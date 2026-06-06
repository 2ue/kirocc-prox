package messages

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/httpx"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/promptcache"
)

// invocation bundles everything callAndHandle needs for one upstream attempt.
// Replaces the former 11-argument callAndHandle signature.
type invocation struct {
	req               *anthropic.Request
	payload           *kiroproto.Payload
	creds             *auth.Credentials
	credID            string
	conversationID    string
	model             string
	responseModel     string
	contextWindowSize int
	thinking          bool
	thinkingBudget    int
	toolNameMap       map[string]string
	tools             []anthropic.Tool
	reportProfile     *promptcache.MatchedProfile
}

// [fork] Modified from upstream d-kuro/kirocc: added the retryInvalidToolUse
// parameter (fix #5/#6) so callAndHandle can be invoked in either "first try,
// invalid tool_use is retriable" or "retry attempt, invalid tool_use is fatal"
// mode. The two-stage dispatch in executeWithRetry below uses this to give the
// model one self-heal round before falling back to a visible error.
//
// callAndHandle performs one upstream call for the invocation and streams or
// buffers the response to w. Returns a non-empty reason if the request failed
// with a retryable invalidStateEvent before any bytes were written to w.
func (s *Service) callAndHandle(ctx context.Context, w http.ResponseWriter, inv *invocation, attempt int, retryInvalidToolUse bool) retryOutcome {
	_, short := logging.TraceIDs(ctx)
	capture := newUpstreamAttemptCapture(ctx, s.captureEnabled, inv.payload, inv.model, inv.thinking, inv.req.Stream, attempt)

	apiResp, err := s.client.GenerateAssistantResponse(ctx, inv.creds.AccessToken, inv.payload, inv.creds.Region)
	if err != nil {
		logUpstreamError(ctx, short, err)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream API error")
		// [fork] Propagate the upstream error up so executeWithRetry's
		// caller can record the real status (rate_limited / auth_error /
		// upstream_error) in usage_records instead of silently labelling
		// the request as "success".
		return retryOutcome{TerminalErr: err}
	}
	body := apiResp.Body
	defer func() { _ = body.Close() }()
	if capture != nil {
		capture.setResponseHeaders(apiResp.Header)
	}

	if inv.req.Stream {
		out := s.handleStreamingResponse(ctx, w, apiResp, inv.responseModel, inv.contextWindowSize, inv.req.StopSequences, inv.req.MaxTokens, apiResp.PromptTokens, capture, inv.toolNameMap, inv.tools, s.promptCacheAttempt(inv, apiResp.PromptTokens), retryInvalidToolUse)
		if out.Reason == retryReasonEmptyVisibleEndTurn {
			capture.logCapture(ctx, out.Reason)
		}
		return out
	} else {
		out := s.handleNonStreamingResponse(ctx, w, apiResp, inv.responseModel, inv.contextWindowSize, inv.req.StopSequences, inv.req.MaxTokens, apiResp.PromptTokens, capture, inv.toolNameMap, inv.tools, s.promptCacheAttempt(inv, apiResp.PromptTokens), retryInvalidToolUse)
		if out.Reason == retryReasonEmptyVisibleEndTurn {
			capture.logCapture(ctx, out.Reason)
		}
		return out
	}
}

// executeWithRetry runs the invocation and handles retryable invalidStateEvent
// responses by clearing ConversationID and attempting once more. Terminal error
// responses are written to w and the function returns. Returns the number of
// retries performed and the terminal error (nil when the call ultimately
// succeeded — letting the caller record the correct usage status).
func (s *Service) executeWithRetry(ctx context.Context, w http.ResponseWriter, inv *invocation) (int, error) {
	_, short := logging.TraceIDs(ctx)

	out := s.callAndHandle(ctx, w, inv, 1, true)
	if out.Reason == "" && out.TerminalErr == nil {
		return 0, nil
	}
	if out.TerminalErr != nil {
		// Upstream call itself failed (HTTP layer). No retry — the error
		// was already written to w by callAndHandle.
		return 0, out.TerminalErr
	}

	// [fork] Added in fork (fix #5/#6): two-stage invalid-tool recovery.
	// First retry feeds tool_result(is_error=true) back to the model so it can
	// self-heal. If the retry also produces invalid tool_use, fall back to
	// writeInvalidToolUseFallback (visible error) instead of looping forever.
	if out.Reason == retryReasonInvalidToolUse {
		slog.WarnContext(ctx, "retrying upstream request with invalid tool result",
			"trace_id", short,
			"invalid_tool_calls", len(out.InvalidToolCalls),
		)
		if err := prepareInvalidToolUseRetry(inv, out.InvalidToolCalls); err != nil {
			slog.ErrorContext(ctx, "invalid tool retry preparation failed", "trace_id", short, "err", err)
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "invalid tool retry preparation failed")
			return 1, errors.New("invalid tool retry preparation failed")
		}
		out2 := s.callAndHandle(ctx, w, inv, 2, false)
		if out2.Reason == "" && out2.TerminalErr == nil {
			return 1, nil
		}
		if out2.TerminalErr != nil {
			return 1, out2.TerminalErr
		}
		if out2.Reason == retryReasonEmptyVisibleEndTurn {
			slog.ErrorContext(ctx, "invalid tool retry returned empty visible end_turn", "trace_id", short)
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
			return 1, errors.New("upstream returned empty response")
		}
		if out2.Reason == retryReasonInvalidToolUse {
			slog.ErrorContext(ctx, "invalid tool retry still returned invalid tool_use",
				"trace_id", short,
				"invalid_tool_calls", len(out2.InvalidToolCalls),
			)
			writeInvalidToolUseFallback(ctx, w, inv.responseModel, inv.req.Stream, out2.InvalidToolCalls)
			return 1, errors.New("invalid_tool_use_persistent")
		}
		slog.ErrorContext(ctx, "invalid tool retry failed", "trace_id", short, "reason", out2.Reason)
		httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "invalid state: "+out2.Reason)
		return 1, errors.New("invalid state: " + out2.Reason)
	}

	slog.WarnContext(ctx, "retrying upstream request",
		"trace_id", short,
		"reason", out.Reason,
	)
	// Clear conversation ID to break out of stuck state (empty_visible_end_turn
	// or retryable invalidStateEvent like CONTENT_LENGTH_EXCEEDS_THRESHOLD).
	inv.payload.ConversationState.ConversationID = ""

	out2 := s.callAndHandle(ctx, w, inv, 2, true)
	if out2.Reason == "" && out2.TerminalErr == nil {
		return 1, nil
	}
	if out2.TerminalErr != nil {
		return 1, out2.TerminalErr
	}
	if out2.Reason == retryReasonEmptyVisibleEndTurn {
		slog.ErrorContext(ctx, "retry also returned empty visible end_turn",
			"trace_id", short, "reason", out2.Reason)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
		return 1, errors.New("upstream returned empty response")
	}
	// Retry ended with a different (final) error — report it as invalid state.
	slog.ErrorContext(ctx, "retry failed",
		"trace_id", short, "first_reason", out.Reason, "second_reason", out2.Reason)
	httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, "invalid state: "+out2.Reason)
	return 1, errors.New("invalid state: " + out2.Reason)
}
