package respconv

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"log/slog"
	"unicode/utf8"

	"github.com/niuma/kirocc-pro/internal/kiroproto"
)

// ProcessEvent processes a single Kiro event and returns the delta for this event.
func (a *responseAccumulator) ProcessEvent(e kiroproto.Event) EventDelta {
	var d EventDelta

	switch e.Type {
	case kiroproto.EventAssistantResponse:
		delta := ComputeDelta(e.Content, a.lastContent)
		a.lastContent = e.Content
		if delta != "" && !a.LocalStop {
			textOut, thinkingOut := a.parseThinkingTags(delta)
			if thinkingOut != "" {
				a.accumulateThinking(thinkingOut, &d)
			}
			if textOut != "" {
				a.HasText = true
				// Apply stop sequence detection.
				if len(a.stopSequences) > 0 {
					textOut = a.applyStopSequenceFilter(textOut)
				}
				// Apply max_tokens budget enforcement.
				if textOut != "" && !a.LocalStop {
					textOut = a.applyMaxTokensBudget(textOut)
				}
				if textOut != "" {
					a.TextBuf.WriteString(textOut)
					d.TextDelta = textOut
				}
			}
			if a.LocalStop {
				d.StopSignal = true
				d.StopReason = a.StopReason
				d.StopSequence = a.StopSequence
			}
		}

	case kiroproto.EventReasoningContent:
		if e.Signature != "" && e.Signature != a.Signature {
			a.Signature = e.Signature
			d.SignatureDelta = e.Signature
		}
		if e.RedactedContent != "" {
			d.RedactedContent = e.RedactedContent
			return d
		}
		// Guard against double-counting: if thinking tags were already parsed
		// from assistantResponseEvent, skip reasoningContentEvent thinking.
		if a.suppressReasoningContent {
			return d
		}
		delta := ComputeDelta(e.ThinkingText, a.lastThinking)
		a.lastThinking = e.ThinkingText
		if delta != "" && !a.LocalStop {
			a.accumulateThinking(delta, &d)
		}

	case kiroproto.EventToolUse:
		a.processToolUseEvent(e, &d)

	case kiroproto.EventMetadata:
		a.HasMetadata = true
		a.InputTokens = e.InputTokens
		a.OutputTokens = e.OutputTokens
		a.CacheReadInputTokens = e.CacheReadInputTokens
		a.CacheWriteInputTokens = e.CacheWriteInputTokens

	case kiroproto.EventMetering:
		if !a.HasMetadata {
			a.InputTokens = e.InputTokens
			a.OutputTokens = e.OutputTokens
		}

	case kiroproto.EventMessageMetadata:
		a.ConversationID = e.ConversationID

	case kiroproto.EventContextUsage:
		a.HasContextUsage = true
		a.ContextUsagePercentage = e.ContextUsagePercentage

	case kiroproto.EventInvalidState, kiroproto.EventException:
		d.IsError = true
		d.ErrorMessage = e.ErrorText()
	}

	return d
}

// processToolUseEvent handles kiroproto.EventToolUse, recording or filtering the tool call
// and updating the output budget.
func (a *responseAccumulator) processToolUseEvent(e kiroproto.Event, d *EventDelta) {
	if !e.ToolStop || a.LocalStop {
		return
	}
	// Restore original tool name if shortened.
	toolName := e.ToolName
	if mapped, ok := a.toolNameMap[toolName]; ok {
		toolName = mapped
	}
	if toolName == kiroproto.ThinkingToolName {
		var input struct {
			Thought string `json:"thought"`
		}
		if err := json.Unmarshal([]byte(e.ToolInput), &input); err == nil && input.Thought != "" {
			a.accumulateThinking(input.Thought, d)
		}
		return
	}
	// Skip recording filtered tools (e.g. internal ToolSearch).
	if a.DropToolName != "" && e.ToolName == a.DropToolName {
		d.ToolStop = true
		d.ToolUseID = e.ToolUseID
		d.ToolName = toolName
		d.ToolInput = e.ToolInput
		return
	}
	toolInput := e.ToolInput
	// [fork] Added in fork: tool_input_validator pass (fix #3/#4) — invalid
	// tool_use blocks are diverted to InvalidToolCalls instead of being
	// emitted, so invalid_tool_retry / invalid_tool_fallback can take over.
	if a.toolInputValidator != nil {
		ok, normalized, reason := a.toolInputValidator.Validate(toolName, toolInput)
		if !ok {
			slog.Warn("dropping invalid tool_use",
				"tool_name", toolName,
				"tool_use_id", e.ToolUseID,
				"reason", reason,
				"input_head", truncateLogValue(toolInput, 200),
			)
			a.InvalidToolCalls = append(a.InvalidToolCalls, InvalidToolCall{
				ID:     e.ToolUseID,
				Name:   toolName,
				Input:  toolInput,
				Reason: reason,
			})
			return
		}
		toolInput = normalized
	}
	a.HasToolUse = true
	tc := ToolCall{
		ID:    e.ToolUseID,
		Name:  toolName,
		Input: toolInput,
	}
	if !jsontext.Value(tc.Input).IsValid() {
		a.ToolParseError = true
	}
	// Count tool input runes toward budget and enforce max_tokens.
	// Unlike text/thinking, tool input JSON cannot be truncated mid-stream
	// (would produce invalid JSON), so we check the budget inline instead
	// of using applyMaxTokensBudget which truncates at a rune boundary.
	toolRunes := utf8.RuneCountInString(toolInput)
	a.outputRuneCount += toolRunes
	if a.maxTokensBudget > 0 && a.outputRuneCount/4 >= a.maxTokensBudget {
		a.LocalStop = true
		a.StopReason = StopReasonMaxTokens
	}
	a.ToolCalls = append(a.ToolCalls, tc)
	d.ToolStop = true
	d.ToolUseID = e.ToolUseID
	d.ToolName = toolName
	d.ToolInput = toolInput
	if a.LocalStop {
		d.StopSignal = true
		d.StopReason = a.StopReason
	}
}
