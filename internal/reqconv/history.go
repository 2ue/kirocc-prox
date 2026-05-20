package reqconv

import (
	"log/slog"
	"strings"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/google/uuid"
)

// extractToolUseIDs returns the IDs of all tool_use blocks in a message's content.
func extractToolUseIDs(msg anthropic.Message) []string {
	if msg.Content.IsString() {
		return nil
	}
	var ids []string
	for _, b := range msg.Content.Blocks {
		if b.IsToolUse() {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// buildHistory converts normalized Anthropic messages to Kiro history entries.
func buildHistory(msgs []anthropic.Message, nameMap *ToolNameMap) []kiroproto.HistoryEntry {
	var history []kiroproto.HistoryEntry

	for i, msg := range msgs {
		switch msg.Role {
		case "user":
			content := cleanCurrentText(ExtractTextContent(msg.Content))
			userMsg := &kiroproto.HistoryUserInputMessage{
				Content: content,
				Origin:  upstreamOrigin(),
			}
			// Warn if images are present in history — Kiro history type does not support images.
			if images := ExtractImages(msg.Content); len(images) > 0 {
				slog.Warn("images in history messages are not supported and will be dropped", "image_count", len(images))
			}
			toolResults := ExtractToolResults(msg.Content)
			// Reorder tool results to match the preceding assistant's tool_use order.
			if len(toolResults) > 1 && i > 0 && msgs[i-1].Role == "assistant" {
				toolResults = ReorderToolResults(toolResults, extractToolUseIDs(msgs[i-1]))
			}
			if len(toolResults) > 0 {
				userMsg.UserInputMessageContext = &kiroproto.UserInputMessageContext{
					ToolResults: toolResults,
				}
			}
			history = append(history, kiroproto.HistoryEntry{UserInputMessage: userMsg})

		case "assistant":
			content := ExtractTextContent(msg.Content)
			// Generate a deterministic messageId from content + toolUseIDs.
			// v3 captures show messageId must be stable across requests for the same
			// assistant history entry. Using SHA1-based UUID ensures this.
			allToolUses := ExtractToolUses(msg.Content)
			for i := range allToolUses {
				allToolUses[i].Name = nameMap.Shorten(allToolUses[i].Name)
			}
			var idSeedBuilder strings.Builder
			idSeedBuilder.WriteString("assistant-msg:")
			idSeedBuilder.WriteString(content)
			for _, tu := range allToolUses {
				idSeedBuilder.WriteByte(':')
				idSeedBuilder.WriteString(tu.ToolUseID)
			}
			arm := &kiroproto.AssistantResponseMessage{
				MessageID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(idSeedBuilder.String())).String(),
				Content:   content,
			}
			if reasoning := ExtractReasoningContent(msg.Content); reasoning != nil {
				arm.ReasoningContent = reasoning
			}

			// v2 captures show thinking blocks are NOT included in history toolUses.
			// Only real tool_use blocks are included.
			if len(allToolUses) > 0 {
				arm.ToolUses = allToolUses
			}

			history = append(history, kiroproto.HistoryEntry{AssistantResponseMessage: arm})
		}
	}
	return history
}

func ExtractReasoningContent(content anthropic.MessageContent) *kiroproto.ReasoningContent {
	if content.IsString() {
		return nil
	}
	for _, b := range content.Blocks {
		if b.Type != anthropic.BlockTypeThinking || b.Thinking == "" {
			continue
		}
		if b.Signature == "" {
			// Kiro/Bedrock rejects history thinking blocks without a signature:
			// messages.N.content.M.thinking.signature: Field required.
			// Some Claude Code sessions can contain older/synthetic thinking blocks
			// without signatures, so drop those instead of poisoning the request.
			slog.Warn("dropping unsigned thinking block from assistant history")
			continue
		}
		return &kiroproto.ReasoningContent{
			ReasoningText: &kiroproto.ReasoningText{
				Text:      b.Thinking,
				Signature: b.Signature,
			},
		}
	}
	return nil
}
