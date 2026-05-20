// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Wraps invalid model-emitted tool_use blocks as tool_result(is_error=true) and
// reflows them in the next upstream request so the model can self-heal without
// the whole turn dying with a 502 / InputValidationError.

package messages

import (
	"encoding/json/v2"
	"fmt"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/reqconv"
	"github.com/niuma/kirocc-pro/internal/respconv"
)

func prepareInvalidToolUseRetry(inv *invocation, calls []respconv.InvalidToolCall) error {
	if len(calls) == 0 {
		return nil
	}
	inv.req.Messages = appendInvalidToolRetryMessages(inv.req.Messages, calls)
	payload, nameMap, err := reqconv.BuildPayload(inv.req, reqconv.BuildOptions{
		ProfileARN:     inv.creds.ProfileARN,
		ModelID:        inv.model,
		ConversationID: "",
		Thinking:       inv.thinking,
		ThinkingBudget: inv.thinkingBudget,
		ThinkingEffort: inv.req.Effort(),
	})
	if err != nil {
		return err
	}
	inv.payload = payload
	inv.toolNameMap = nameMap.ReverseMap()
	return nil
}

func appendInvalidToolRetryMessages(msgs []anthropic.Message, calls []respconv.InvalidToolCall) []anthropic.Message {
	assistantBlocks := make([]anthropic.ContentBlock, 0, len(calls))
	resultBlocks := make([]anthropic.ContentBlock, 0, len(calls))
	for i, call := range calls {
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("toolu_invalid_%d", i+1)
		}
		assistantBlocks = append(assistantBlocks, anthropic.ContentBlock{
			Type:  anthropic.BlockTypeToolUse,
			ID:    id,
			Name:  call.Name,
			Input: parseInvalidToolInput(call.Input),
		})
		resultBlocks = append(resultBlocks, anthropic.ContentBlock{
			Type:      anthropic.BlockTypeToolResult,
			ToolUseID: id,
			IsError:   true,
			Content: anthropic.MessageContent{Text: fmt.Sprintf(
				"Tool call %q was invalid and was not executed: %s. Retry with complete valid JSON arguments matching the tool schema. If a different tool is more appropriate, use it now.",
				call.Name, call.Reason,
			)},
		})
	}
	return append(msgs,
		anthropic.Message{Role: "assistant", Content: anthropic.MessageContent{Blocks: assistantBlocks}},
		anthropic.Message{Role: "user", Content: anthropic.MessageContent{Blocks: resultBlocks}},
	)
}

const maxInvalidToolInputBytes = 1024

// parseInvalidToolInput converts the model's raw tool input string into the
// map shape that anthropic.ContentBlock.Input expects. When the raw input is
// valid JSON object, the parsed map is returned so the model sees its own
// arguments verbatim on retry. When it is malformed or non-object, the raw
// string is preserved as `_raw_invalid_input` so the retry message includes
// what the model actually wrote — an empty `{}` would mislead the model into
// thinking it called the tool with no arguments.
func parseInvalidToolInput(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	if len(raw) > maxInvalidToolInputBytes {
		return map[string]any{
			"_invalid_input_omitted": true,
			"reason":                 "invalid tool input exceeded retry history limit",
		}
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && obj != nil {
		return obj
	}
	return map[string]any{
		"_raw_invalid_input": raw,
	}
}
