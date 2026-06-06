package messages

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/httpx"
	"github.com/niuma/kirocc-pro/internal/kiroclient"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/respconv"
)

const retryReasonEmptyVisibleEndTurn = "empty_visible_end_turn"

// [fork] Added in fork (fix #5/#6): retryReasonInvalidToolUse, the
// InvalidToolCalls field on retryOutcome, and the retryInvalidToolUse
// parameter on handleStreamingResponse / handleNonStreamingResponse below.
// Carries withheld invalid tool_use calls from the response layer up to
// execute.go, where invalid_tool_retry / invalid_tool_fallback decide what
// to do with them.
const retryReasonInvalidToolUse = "invalid_tool_use"

type retryOutcome struct {
	Reason           string
	InvalidToolCalls []respconv.InvalidToolCall
	RetryCount       int

	// TerminalErr carries the upstream error when callAndHandle failed at
	// the transport layer (for example Kiro returned 400 / 429 / 5xx).
	// Keeping the error structured lets pool cooldown honor Retry-After.
	TerminalErr error
}

func (s *Service) handleStreamingResponse(ctx context.Context, w http.ResponseWriter, apiResp *kiroclient.Response, model string, contextWindowSize int, stopSequences []string, maxTokens int, preCountedInputTokens int, capture *upstreamAttemptCapture, toolNameMap map[string]string, tools []anthropic.Tool, cacheAttempt *promptCacheAttempt, retryInvalidToolUse bool) retryOutcome {
	traceID, short := logging.TraceIDs(ctx)

	gw := NewGateWriter(w)
	sw := respconv.NewSSEWriter(ctx, gw, model, contextWindowSize, stopSequences, maxTokens, preCountedInputTokens)
	sw.OnVisibleOutput = func() {
		markMetricsFirstToken(w)
		gw.Promote()
	}
	sw.SetToolNameMap(toolNameMap)
	sw.SetToolInputValidator(respconv.NewToolInputValidator(tools))
	sw.SetUsageAdjuster(cacheAttempt.usageAdjuster())

	var streamErr bool
	var localStop bool
	var invalidReason string
	var isException bool
	err := kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
		capture.recordEvent(e)
		if streamErr || localStop {
			return true
		}
		// Stop early if the client disconnected (write failed).
		if sw.WriteErr() {
			streamErr = true
			return true
		}
		if e.Type == kiroproto.EventInvalidState {
			invalidReason = e.InvalidStateReason
			slog.ErrorContext(ctx, "invalid state",
				"trace_id", short,
				"reason", e.InvalidStateReason,
				"message", e.ErrorMessage,
			)
		}
		if e.Type == kiroproto.EventException {
			isException = true
			slog.ErrorContext(ctx, "upstream exception",
				"trace_id", short,
				"reason", e.InvalidStateReason,
				"message", e.ErrorMessage,
			)
		}
		shouldStop := sw.HandleEvent(e)
		if sw.WriteErr() {
			streamErr = true
			return true
		}
		if !shouldStop {
			return false
		}
		// Distinguish adapter-side stop (Finish already called) from error.
		if sw.LocalStop() {
			localStop = true
			return true
		}
		streamErr = true
		return true
	})

	if streamErr && !sw.Started() {
		return retryOutcome{Reason: handleUpstreamError(w, isException, invalidReason)}
	}

	// If the stream started (thinking events) but GateWriter was never promoted
	// (no visible output reached the client), we can still discard and write error JSON.
	if streamErr && sw.Started() && !gw.IsPromoted() {
		gw.Discard()
		return retryOutcome{Reason: handleUpstreamError(w, isException, invalidReason)}
	}

	if err != nil {
		slog.ErrorContext(ctx, "stream error", "trace_id", short, "err", err)
		writeStreamingOrJSONError(gw, sw, w, http.StatusBadGateway, errTypeStreamError, "upstream stream error")
		return retryOutcome{}
	}

	if calls := sw.InvalidToolCalls(); len(calls) > 0 && !gw.IsPromoted() {
		gw.Discard()
		if retryInvalidToolUse {
			slog.WarnContext(ctx, "retrying after invalid tool_use",
				"trace_id", short,
				"invalid_tool_calls", len(calls),
			)
		}
		return retryOutcome{Reason: retryReasonInvalidToolUse, InvalidToolCalls: calls}
	}

	if !streamErr && !localStop {
		sw.Finish()
	}

	// Detect empty visible end_turn (thinking-only response with no visible text).
	// If the GateWriter hasn't been promoted yet, we can safely discard and retry.
	if !streamErr && !localStop && sw.IsEmptyVisibleEndTurn() && !gw.IsPromoted() {
		gw.Discard()
		args := []any{
			"trace_id", short,
			"thinking_chars", sw.ThinkingLen(),
			"has_tool_use", false,
			"retry", true,
		}
		args = append(args, capture.logAttrs()...)
		slog.WarnContext(ctx, "empty visible end_turn detected", args...)
		return retryOutcome{Reason: retryReasonEmptyVisibleEndTurn}
	}

	// Log response completion (only on success).
	if !streamErr {
		slog.DebugContext(ctx, "client response headers",
			"trace_id", traceID,
			"session_id", logging.SessionIDFromContext(ctx),
			"headers", logging.SafeHeaders{H: gw.Header()},
		)
		inputTokens, outputTokens := sw.Usage()
		rawInputTokens, rawOutputTokens := sw.RawUsage()
		pct := resolveContextPercent(sw.ContextUsagePercentage(), sw.HasContextUsage(), inputTokens, contextWindowSize)
		logResponseStats(ctx, short, inputTokens, outputTokens, sw.HasContextUsage(), sw.ContextUsagePercentage(), contextWindowSize)
		if mw, ok := w.(*metricsResponseWriter); ok {
			mw.setUsageDetailed(
				inputTokens, outputTokens, sw.FinalCacheReadInputTokens(), sw.FinalCacheWriteInputTokens(),
				rawInputTokens, rawOutputTokens, sw.RawCacheReadInputTokens(), sw.RawCacheWriteInputTokens(),
				pct,
			)
		}
		cacheAttempt.commitIfApplied()
	}
	return retryOutcome{}
}

func (s *Service) handleNonStreamingResponse(ctx context.Context, w http.ResponseWriter, apiResp *kiroclient.Response, model string, contextWindowSize int, stopSequences []string, maxTokens int, preCountedInputTokens int, capture *upstreamAttemptCapture, toolNameMap map[string]string, tools []anthropic.Tool, cacheAttempt *promptCacheAttempt, retryInvalidToolUse bool) retryOutcome {
	traceID, short := logging.TraceIDs(ctx)
	acc := respconv.NewNonStreamingAccumulator(contextWindowSize, stopSequences, maxTokens, preCountedInputTokens)
	acc.SetToolNameMap(toolNameMap)
	acc.SetToolInputValidator(respconv.NewToolInputValidator(tools))
	acc.SetUsageAdjuster(cacheAttempt.usageAdjuster())

	var invalidReason string
	var hasError bool
	var isException bool
	err := kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
		capture.recordEvent(e)
		d := acc.ProcessEvent(e)
		markMetricsFirstTokenForDelta(w, d)
		if d.IsError {
			hasError = true
			switch e.Type {
			case kiroproto.EventException:
				isException = true
				slog.ErrorContext(ctx, "upstream exception",
					"trace_id", short,
					"reason", e.InvalidStateReason,
					"message", e.ErrorMessage,
				)
			case kiroproto.EventInvalidState:
				invalidReason = e.InvalidStateReason
				slog.ErrorContext(ctx, "invalid state",
					"trace_id", short,
					"reason", e.InvalidStateReason,
					"message", e.ErrorMessage,
				)
			}
			return true // stop parsing
		}
		return false
	})
	if err != nil {
		slog.ErrorContext(ctx, "stream parse error", "trace_id", short, "err", err)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream stream error")
		return retryOutcome{}
	}

	if hasError {
		return retryOutcome{Reason: handleUpstreamError(w, isException, invalidReason)}
	}

	if calls := acc.InvalidToolCalls(); len(calls) > 0 {
		if retryInvalidToolUse {
			slog.WarnContext(ctx, "retrying after invalid tool_use",
				"trace_id", short,
				"invalid_tool_calls", len(calls),
			)
		}
		return retryOutcome{Reason: retryReasonInvalidToolUse, InvalidToolCalls: calls}
	}

	resp, stats := acc.BuildResponse(model)

	// Detect empty visible end_turn (thinking-only response with no visible text).
	if acc.IsEmptyVisibleEndTurn() {
		args := []any{
			"trace_id", short,
			"thinking_chars", acc.ThinkingLen(),
			"has_tool_use", false,
			"retry", true,
		}
		args = append(args, capture.logAttrs()...)
		slog.WarnContext(ctx, "empty visible end_turn detected", args...)
		return retryOutcome{Reason: retryReasonEmptyVisibleEndTurn}
	}

	w.Header().Set("Content-Type", "application/json")
	slog.DebugContext(ctx, "client response headers",
		"trace_id", traceID,
		"session_id", logging.SessionIDFromContext(ctx),
		"headers", logging.SafeHeaders{H: w.Header()},
	)
	if err := json.MarshalWrite(w, resp); err != nil {
		slog.ErrorContext(ctx, "write non-streaming response failed", "err", err)
		return retryOutcome{}
	}
	_, _ = w.Write([]byte("\n"))
	if content, ok := resp["content"].([]any); ok && len(content) > 0 {
		markMetricsFirstToken(w)
	}

	logResponseStats(ctx, short, stats.InputTokens, stats.OutputTokens, stats.HasContextUsage, stats.ContextUsagePercentage, contextWindowSize)
	if mw, ok := w.(*metricsResponseWriter); ok {
		pct := resolveContextPercent(stats.ContextUsagePercentage, stats.HasContextUsage, stats.InputTokens, contextWindowSize)
		mw.setUsageDetailed(
			stats.InputTokens, stats.OutputTokens, stats.CacheReadInputTokens, stats.CacheWriteInputTokens,
			stats.RawInputTokens, stats.RawOutputTokens, stats.RawCacheReadTokens, stats.RawCacheWriteTokens,
			pct,
		)
	}
	cacheAttempt.commitIfApplied()
	return retryOutcome{}
}

// logResponseStats logs response completion and warns on context limit exceeded.
func logResponseStats(ctx context.Context, short string, inputTokens, outputTokens int, hasContextUsage bool, contextUsagePct float64, contextWindowSize int) {
	hasUsage := inputTokens > 0 || outputTokens > 0 || hasContextUsage
	pct := resolveContextPercent(contextUsagePct, hasContextUsage, inputTokens, contextWindowSize)
	contextUsage := "unknown"
	if hasUsage {
		contextUsage = fmt.Sprintf("%.1fk(%.1f%%)", float64(inputTokens)/1000, pct)
	}
	slog.InfoContext(ctx, "<-- POST /v1/messages",
		"trace_id", short,
		"session_id", logging.ShortID(logging.SessionIDFromContext(ctx)),
		"status", 200,
		"input_tokens", inputTokens,
		"output_tokens", outputTokens,
		"context_usage", contextUsage,
	)
	if hasUsage && pct >= 100 {
		slog.WarnContext(ctx, "context limit exceeded",
			"trace_id", short,
			"context_usage", fmt.Sprintf("%.1fk(%.1f%%)", float64(inputTokens)/1000, pct),
		)
	}
}

// resolveContextPercent returns the context usage percentage, falling back to
// an estimate from inputTokens/windowSize when the reported value is not available.
func resolveContextPercent(reported float64, hasContextUsage bool, inputTokens, windowSize int) float64 {
	if hasContextUsage || windowSize == 0 {
		return reported
	}
	return float64(inputTokens) * 100 / float64(windowSize)
}
