package messages

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"net/http"
	"os"
	"slices"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/auth"
	"github.com/niuma/kirocc-pro/internal/httpx"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/logging"
	"github.com/niuma/kirocc-pro/internal/reqconv"
	"github.com/niuma/kirocc-pro/internal/respconv"
)

const maxThinkingToolRounds = 4

type thinkingToolOrchestrator struct {
	service           *Service
	req               *anthropic.Request
	creds             *auth.Credentials
	buildOpts         reqconv.BuildOptions
	contextWindowSize int
	responseModel     string
}

func (s *Service) runThinkingTool(ctx context.Context, w http.ResponseWriter, req *anthropic.Request, creds *auth.Credentials, kiroModel, responseModel string, contextWindowSize int, thinkingBudget int, ccSessionID, short string) {
	orch := &thinkingToolOrchestrator{
		service: s,
		req:     req,
		creds:   creds,
		buildOpts: reqconv.BuildOptions{
			ProfileARN:     creds.ProfileARN,
			ModelID:        kiroModel,
			ConversationID: ccSessionID,
			Thinking:       true,
			ThinkingBudget: thinkingBudget,
			ThinkingEffort: req.Effort(),
		},
		contextWindowSize: contextWindowSize,
		responseModel:     responseModel,
	}
	reason := orch.run(ctx, w)
	if reason != retryReasonEmptyVisibleEndTurn {
		return
	}
	slog.WarnContext(ctx, "retrying thinking tool after empty visible end_turn", "trace_id", short)
	if r2 := orch.run(ctx, w); r2 == retryReasonEmptyVisibleEndTurn {
		slog.ErrorContext(ctx, "thinking tool retry also returned empty visible end_turn", "trace_id", short)
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
	}
}

func (o *thinkingToolOrchestrator) run(ctx context.Context, w http.ResponseWriter) string {
	if o.req.Stream {
		return o.handleStreaming(ctx, w)
	}
	return o.handleNonStreaming(ctx, w)
}

func (o *thinkingToolOrchestrator) handleStreaming(ctx context.Context, w http.ResponseWriter) string {
	_, short := logging.TraceIDs(ctx)

	gw := NewGateWriter(w)
	sw := respconv.NewSSEWriter(ctx, gw, o.responseModel, o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
	sw.OnVisibleOutput = func() { gw.Promote() }

	msgs := slices.Clone(o.req.Messages)
	var cumulativeInputTokens, cumulativeOutputTokens int
	// [fork] Added in fork (fix #5/#6): invalidToolRetried gates the one-shot
	// invalid-tool reflow inside the thinking-tool round loop. Mirrors the
	// same pattern in toolsearch.go and the non-streaming branch below.
	var invalidToolRetried bool

	for round := range maxThinkingToolRounds {
		payload, nameMap, err := o.buildPayload(msgs)
		if err != nil {
			slog.WarnContext(ctx, "thinking tool payload build error", "trace_id", short, "err", err)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
			return ""
		}
		sw.SetToolNameMap(nameMap.ReverseMap())
		sw.SetToolInputValidator(respconv.NewToolInputValidator(o.req.Tools))

		apiResp, err := o.service.client.GenerateAssistantResponse(ctx, o.creds.AccessToken, payload, o.creds.Region)
		if err != nil {
			logUpstreamError(ctx, short, err, "round", round+1)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadGateway, errTypeAPI, "upstream API error")
			return ""
		}

		if round > 0 {
			in, out := sw.Usage()
			cumulativeInputTokens += in
			cumulativeOutputTokens += out
			sw.ResetAccumulator(o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
		}

		var foundThinking bool
		var thinkingToolUseID, thinkingToolInput string
		var streamErr, localStop bool

		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			if streamErr || localStop || foundThinking {
				return true
			}
			if sw.WriteErr() {
				streamErr = true
				return true
			}
			if e.Type == kiroproto.EventToolUse && e.ToolStop && e.ToolName == kiroproto.ThinkingToolName {
				foundThinking = true
				thinkingToolUseID = e.ToolUseID
				thinkingToolInput = e.ToolInput
				return true
			}
			shouldStop := sw.HandleEvent(e)
			if sw.WriteErr() {
				streamErr = true
				return true
			}
			if !shouldStop {
				return false
			}
			if sw.LocalStop() {
				localStop = true
				return true
			}
			streamErr = true
			return true
		})
		_ = apiResp.Body.Close()

		if err != nil && !foundThinking {
			slog.ErrorContext(ctx, "stream error", "trace_id", short, "round", round+1, "err", err)
			writeStreamingOrJSONError(gw, sw, w, http.StatusBadGateway, errTypeAPI, "upstream stream error")
			return ""
		}

		if foundThinking {
			slog.InfoContext(ctx, "thinking tool continuation", "trace_id", short, "round", round+1, "tool_use_id", thinkingToolUseID)
			msgs = appendThinkingToolMessages(msgs, thinkingToolUseID, thinkingToolInput)
			o.buildOpts.PreserveToolBlocks = true
			continue
		}

		if calls := sw.InvalidToolCalls(); len(calls) > 0 && !gw.IsPromoted() {
			if invalidToolRetried {
				gw.Discard()
				slog.ErrorContext(ctx, "thinking tool invalid retry still returned invalid tool_use",
					"trace_id", short,
					"round", round+1,
					"invalid_tool_calls", len(calls),
				)
				writeInvalidToolUseFallback(ctx, w, o.responseModel, true, calls)
				return ""
			}
			gw.Discard()
			slog.WarnContext(ctx, "retrying thinking tool after invalid tool_use",
				"trace_id", short,
				"round", round+1,
				"invalid_tool_calls", len(calls),
			)
			msgs = appendInvalidToolRetryMessages(msgs, calls)
			invalidToolRetried = true
			o.buildOpts.ConversationID = ""
			o.buildOpts.PreserveToolBlocks = true
			continue
		}

		if !streamErr && !localStop {
			sw.Finish()
		}
		if !streamErr && !localStop && sw.IsEmptyVisibleEndTurn() && !gw.IsPromoted() {
			gw.Discard()
			slog.WarnContext(ctx, "empty visible end_turn detected in thinking tool", "trace_id", short)
			return retryReasonEmptyVisibleEndTurn
		}
		if streamErr && !gw.IsPromoted() {
			gw.Discard()
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream stream error")
		}
		if !streamErr {
			inputTokens, outputTokens := sw.Usage()
			logResponseStats(ctx, short, inputTokens+cumulativeInputTokens, outputTokens+cumulativeOutputTokens, sw.HasContextUsage(), sw.ContextUsagePercentage(), o.contextWindowSize)
		}
		return ""
	}

	slog.WarnContext(ctx, "thinking tool max rounds reached", "trace_id", short, "max_rounds", maxThinkingToolRounds)
	if !gw.IsPromoted() {
		gw.Discard()
		return retryReasonEmptyVisibleEndTurn
	}
	sw.Finish()
	return ""
}

func (o *thinkingToolOrchestrator) handleNonStreaming(ctx context.Context, w http.ResponseWriter) string {
	_, short := logging.TraceIDs(ctx)
	msgs := slices.Clone(o.req.Messages)

	var totalInputTokens, totalOutputTokens int
	var finalResp map[string]any
	var finalStats respconv.NonStreamingStats
	var invalidToolRetried bool

	for round := range maxThinkingToolRounds {
		payload, nameMap, err := o.buildPayload(msgs)
		if err != nil {
			httpx.WriteError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
			return ""
		}
		apiResp, err := o.service.client.GenerateAssistantResponse(ctx, o.creds.AccessToken, payload, o.creds.Region)
		if err != nil {
			logUpstreamError(ctx, short, err, "round", round+1)
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream API error")
			return ""
		}

		acc := respconv.NewNonStreamingAccumulator(o.contextWindowSize, o.req.StopSequences, o.req.MaxTokens, 0)
		acc.SetToolNameMap(nameMap.ReverseMap())
		acc.SetToolInputValidator(respconv.NewToolInputValidator(o.req.Tools))

		var hasError bool
		var foundThinking bool
		var thinkingToolUseID, thinkingToolInput string
		err = kiroproto.ParseStream(ctx, apiResp.Body, func(e kiroproto.Event) bool {
			if e.Type == kiroproto.EventToolUse && e.ToolStop && e.ToolName == kiroproto.ThinkingToolName {
				foundThinking = true
				thinkingToolUseID = e.ToolUseID
				thinkingToolInput = e.ToolInput
				return true
			}
			d := acc.ProcessEvent(e)
			if d.IsError {
				hasError = true
				return true
			}
			return false
		})
		_ = apiResp.Body.Close()

		if (err != nil || hasError) && !foundThinking {
			httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream error")
			return ""
		}

		if calls := acc.InvalidToolCalls(); len(calls) > 0 && !foundThinking {
			if invalidToolRetried {
				slog.ErrorContext(ctx, "thinking tool invalid retry still returned invalid tool_use",
					"trace_id", short,
					"round", round+1,
					"invalid_tool_calls", len(calls),
				)
				writeInvalidToolUseFallback(ctx, w, o.responseModel, false, calls)
				return ""
			}
			slog.WarnContext(ctx, "retrying thinking tool after invalid tool_use",
				"trace_id", short,
				"round", round+1,
				"invalid_tool_calls", len(calls),
			)
			msgs = appendInvalidToolRetryMessages(msgs, calls)
			invalidToolRetried = true
			o.buildOpts.ConversationID = ""
			o.buildOpts.PreserveToolBlocks = true
			continue
		}

		resp, stats := acc.BuildResponse(o.responseModel)
		totalInputTokens += stats.InputTokens
		totalOutputTokens += stats.OutputTokens
		finalResp = resp
		finalStats = stats

		if foundThinking {
			slog.InfoContext(ctx, "thinking tool continuation", "trace_id", short, "round", round+1, "tool_use_id", thinkingToolUseID)
			msgs = appendThinkingToolMessages(msgs, thinkingToolUseID, thinkingToolInput)
			o.buildOpts.PreserveToolBlocks = true
			continue
		}

		if acc.IsEmptyVisibleEndTurn() {
			slog.WarnContext(ctx, "empty visible end_turn detected in thinking tool", "trace_id", short)
			return retryReasonEmptyVisibleEndTurn
		}
		break
	}

	if finalResp == nil {
		httpx.WriteError(w, http.StatusBadGateway, errTypeAPI, "upstream returned empty response")
		return ""
	}
	if usage, ok := finalResp["usage"].(map[string]any); ok {
		usage["input_tokens"] = totalInputTokens
		usage["output_tokens"] = totalOutputTokens
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.MarshalWrite(w, finalResp); err != nil {
		slog.ErrorContext(ctx, "write non-streaming response failed", "err", err)
		return ""
	}
	_, _ = w.Write([]byte("\n"))
	logResponseStats(ctx, short, totalInputTokens, totalOutputTokens, finalStats.HasContextUsage, finalStats.ContextUsagePercentage, o.contextWindowSize)
	return ""
}

func (o *thinkingToolOrchestrator) buildPayload(msgs []anthropic.Message) (*kiroproto.Payload, *reqconv.ToolNameMap, error) {
	tmpReq := *o.req
	tmpReq.Messages = msgs
	return reqconv.BuildPayload(&tmpReq, o.buildOpts)
}

func appendThinkingToolMessages(msgs []anthropic.Message, toolUseID, toolInput string) []anthropic.Message {
	var input map[string]any
	if err := json.Unmarshal([]byte(toolInput), &input); err != nil || input == nil {
		input = map[string]any{"thought": toolInput}
	}
	msgs = append(msgs, anthropic.Message{
		Role: "assistant",
		Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
			Type:  anthropic.BlockTypeToolUse,
			ID:    toolUseID,
			Name:  kiroproto.ThinkingToolName,
			Input: input,
		}}},
	})
	if os.Getenv("KIROCC_EXPERIMENT_THINKING_TOOL_CONTINUE") == "assistant_only" {
		return msgs
	}
	return append(msgs, anthropic.Message{
		Role: "user",
		Content: anthropic.MessageContent{Blocks: []anthropic.ContentBlock{{
			Type:      anthropic.BlockTypeToolResult,
			ToolUseID: toolUseID,
			Content:   anthropic.MessageContent{Text: "ok"},
		}, {
			Type: anthropic.BlockTypeText,
			Text: "Now provide the final answer to the user's original request. Do not call the thinking tool again.",
		}}},
	})
}
