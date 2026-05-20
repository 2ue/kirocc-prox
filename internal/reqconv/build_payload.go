package reqconv

import (
	"fmt"
	"os"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/kiroproto"
	"github.com/niuma/kirocc-pro/internal/toolsearch"
	"github.com/google/uuid"
)

// defaultThinkingBudget is the default thinking token budget (medium).
const defaultThinkingBudget = anthropic.ThinkingBudgetMedium

// BuildOptions controls how an Anthropic request is mapped to a Kiro payload.
type BuildOptions struct {
	ProfileARN     string
	ModelID        string
	ConversationID string
	Thinking       bool
	ThinkingBudget int
	ThinkingEffort string
	ToolSearchCtx  *toolsearch.Context
	// PreserveToolBlocks keeps tool_use/tool_result blocks structured even when
	// no tool definitions are exposed on the current turn.
	PreserveToolBlocks bool
}

// BuildPayload converts an Anthropic request into a Kiro API payload.
func BuildPayload(req *anthropic.Request, options BuildOptions) (*kiroproto.Payload, *ToolNameMap, error) {
	nameMap := NewToolNameMap()

	// 1. Build system prompt and convert tools.
	systemPrompt, toolEntries := buildSystemAndTools(req, options.ToolSearchCtx, nameMap)

	// 2. Normalize and split messages.
	useThinkingTool := options.Thinking && os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT") == "tool"
	hasTools := len(req.Tools) > 0 || useThinkingTool || options.PreserveToolBlocks
	if options.ToolSearchCtx != nil {
		hasTools = true
	}
	msgs := Normalize(req.Messages, hasTools)
	historyMsgs, lastMsg := splitMessages(msgs)

	// 3. Build history and place system prompt.
	history := buildHistory(historyMsgs, nameMap)
	history, lastContent := placeSystemPrompt(systemPrompt, history, cleanCurrentText(ExtractTextContent(lastMsg.Content)))

	// 4. Build currentMessage.
	// Extract tool_use IDs from the preceding assistant message for reordering tool results.
	var precedingToolUseIDs []string
	if len(historyMsgs) > 0 {
		precedingToolUseIDs = extractToolUseIDs(historyMsgs[len(historyMsgs)-1])
	}
	userInputMessage := buildCurrentMessage(lastMsg, lastContent, options.ModelID, toolEntries, options.Thinking, options.ThinkingBudget, options.ThinkingEffort, precedingToolUseIDs)

	convState := kiroproto.ConversationState{
		ConversationID:  options.ConversationID,
		ChatTriggerType: kiroproto.ChatTriggerTypeManual,
		AgentTaskType:   kiroproto.AgentTaskTypeVibe,
		CurrentMessage:  kiroproto.CurrentMessage{UserInputMessage: userInputMessage},
	}
	if len(history) > 0 {
		convState.History = history
	}
	payload := &kiroproto.Payload{ConversationState: convState}
	if options.ProfileARN != "" {
		payload.ProfileARN = options.ProfileARN
	}
	return payload, nameMap, nil
}

func upstreamOrigin() string {
	switch os.Getenv("KIROCC_UPSTREAM_ORIGIN") {
	case kiroproto.OriginAIEditor:
		return kiroproto.OriginAIEditor
	default:
		return kiroproto.OriginKiroCLI
	}
}

// buildSystemAndTools extracts the system prompt and converts tools.
func buildSystemAndTools(req *anthropic.Request, tsCtx *toolsearch.Context, nameMap *ToolNameMap) (string, []kiroproto.ToolEntry) {
	systemPrompt := ExtractSystemPrompt(req.System)

	var toolEntries []kiroproto.ToolEntry
	if tsCtx != nil {
		// Tool search mode: convert only active tools, inject ToolSearch tool.
		toolEntries = ConvertTools(tsCtx.ActiveTools, nameMap)
		toolEntries = ApplyToolCachePoints(tsCtx.ActiveTools, toolEntries)
		toolEntries = append(toolEntries, toolsearch.KiroToolSearchEntry())
	} else if len(req.Tools) > 0 {
		toolEntries = ConvertTools(req.Tools, nameMap)
		toolEntries = ApplyToolCachePoints(req.Tools, toolEntries)
	}
	return systemPrompt, toolEntries
}

// splitMessages splits normalized messages into history messages and the last message.
// If the last message is from the assistant, all messages go to history and a
// synthetic "Continue" user message is returned.
func splitMessages(msgs []anthropic.Message) (history []anthropic.Message, last anthropic.Message) {
	if len(msgs) == 0 {
		return nil, anthropic.Message{}
	}
	if msgs[len(msgs)-1].Role == "assistant" {
		return msgs, anthropic.Message{
			Role:    "user",
			Content: anthropic.MessageContent{Text: syntheticContinue},
		}
	}
	return msgs[:len(msgs)-1], msgs[len(msgs)-1]
}

// syntheticAck is the synthetic assistant acknowledgment that kiro-cli always
// inserts after the system prompt in history. v2 captures confirm this is present
// in every request.
const syntheticAck = "I will fully incorporate this information when generating my responses, and explicitly acknowledge relevant parts of the summary when answering questions."

// syntheticAckMessageID is a deterministic UUID for the synthetic ack, computed once since the input is constant.
var syntheticAckMessageID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("synthetic-ack:"+syntheticAck)).String()

// placeSystemPrompt inserts the system prompt as a dedicated history entry pair
// (user message + synthetic assistant ack), matching the v2 kiro-cli structure.
// v2 captures show this pair is present in every request, even the first one.
// Returns a new history slice (original is not mutated) and the updated lastContent.
func placeSystemPrompt(systemPrompt string, history []kiroproto.HistoryEntry, lastContent string) ([]kiroproto.HistoryEntry, string) {
	if systemPrompt == "" {
		return history, lastContent
	}
	switch os.Getenv("KIROCC_EXPERIMENT_SYSTEM_MODE") {
	case "drop":
		return history, lastContent
	case "current":
		if lastContent != "" {
			lastContent = "<system_context>\n" + systemPrompt + "\n</system_context>\n\n" + lastContent
		} else {
			lastContent = "<system_context>\n" + systemPrompt + "\n</system_context>"
		}
		return history, lastContent
	case "noack":
		systemPair := []kiroproto.HistoryEntry{{
			UserInputMessage: &kiroproto.HistoryUserInputMessage{
				Content: systemPrompt,
				Origin:  upstreamOrigin(),
			},
		}}
		newHistory := make([]kiroproto.HistoryEntry, 0, len(systemPair)+len(history))
		newHistory = append(newHistory, systemPair...)
		newHistory = append(newHistory, history...)
		return newHistory, lastContent
	}
	// Always build the system prompt pair: user message + synthetic assistant ack.
	systemPair := []kiroproto.HistoryEntry{
		{UserInputMessage: &kiroproto.HistoryUserInputMessage{
			Content: systemPrompt,
			Origin:  upstreamOrigin(),
		}},
		{AssistantResponseMessage: &kiroproto.AssistantResponseMessage{
			MessageID: syntheticAckMessageID,
			Content:   syntheticAck,
		}},
	}
	newHistory := make([]kiroproto.HistoryEntry, 0, len(systemPair)+len(history))
	newHistory = append(newHistory, systemPair...)
	newHistory = append(newHistory, history...)
	return newHistory, lastContent
}

// buildCurrentMessage constructs the Kiro UserInputMessage from the last Anthropic message.
func buildCurrentMessage(lastMsg anthropic.Message, lastContent, modelID string, toolEntries []kiroproto.ToolEntry, thinking bool, thinkingBudget int, thinkingEffort string, precedingToolUseIDs []string) kiroproto.UserInputMessage {
	msg := kiroproto.UserInputMessage{
		Content: lastContent,
		ModelID: modelID,
		Origin:  upstreamOrigin(),
	}
	useThinkingTool := thinking && os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT") == "tool"
	if useThinkingTool {
		toolEntries = append([]kiroproto.ToolEntry{ThinkingToolEntry()}, toolEntries...)
	}

	// Single-pass scan of lastMsg.Content to extract both tool_results and images.
	toolResults, images := scanCurrentMessage(lastMsg.Content)
	toolResults = ReorderToolResults(toolResults, precedingToolUseIDs)
	if len(toolEntries) > 0 || len(toolResults) > 0 {
		ctx := &kiroproto.UserInputMessageContext{}
		if len(toolEntries) > 0 {
			ctx.Tools = toolEntries
		}
		if len(toolResults) > 0 {
			ctx.ToolResults = toolResults
		}
		msg.UserInputMessageContext = ctx
	}

	// Match the observed kiro-cli continuation shape:
	// tool-result-only turns keep empty currentMessage.content instead of "Continue".
	if msg.Content == "" && len(toolResults) == 0 {
		msg.Content = syntheticContinue
	}

	if len(images) > 0 {
		msg.Images = images
	}

	// Inject thinking mode XML tags after all content decisions are finalized.
	// Skip injection for tool-result-only continuations (content="" with toolResults)
	// to preserve the empty content shape that upstream expects.
	if thinking && !useThinkingTool && shouldInjectThinkingPrefix(thinkingEffort) && (msg.Content != "" || len(toolResults) == 0) {
		budget := thinkingBudget
		if budget <= 0 {
			budget = defaultThinkingBudget
		}
		effort := resolveThinkingEffort(thinkingEffort, budget)
		prefix := thinkingPromptPrefix(os.Getenv("KIROCC_EXPERIMENT_THINKING_PROMPT"), effort, budget)
		if msg.Content != "" && prefix != "" {
			msg.Content = prefix + "\n\n" + msg.Content
		} else if prefix != "" {
			msg.Content = prefix
		}
	}

	return msg
}

func shouldInjectThinkingPrefix(explicitEffort string) bool {
	if validThinkingBudgetFloor() {
		return true
	}
	return explicitEffort == anthropic.EffortMax || explicitEffort == anthropic.EffortXHigh
}

// [fork] Modified from upstream d-kuro/kirocc: KIROCC_FORCE_THINKING_BUDGET
// gate (fix #8) — when set to a positive integer, the proxy unconditionally
// injects <thinking_mode>/<max_thinking_length> tags so the upstream actually
// receives the requested budget regardless of the client's effort hint.
func validThinkingBudgetFloor() bool {
	v := os.Getenv("KIROCC_FORCE_THINKING_BUDGET")
	if v == "" {
		return false
	}
	var budget int
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
		budget = budget*10 + int(r-'0')
	}
	return budget > 0
}

func thinkingPromptPrefix(mode, effort string, budget int) string {
	switch mode {
	case "off":
		return ""
	case "legacy":
		return fmt.Sprintf("<thinking_mode>adaptive</thinking_mode>\n<thinking_effort>%s</thinking_effort>\n<thinking_output_contract>Reason internally at the requested effort. If any draft, plan, self-talk, or analysis is written before the final answer, wrap it inside <thinking>...</thinking>. Visible output must contain only the final answer or tool call requested by the user; never expose scratchpad text as normal prose.</thinking_output_contract>", effort)
	case "minimal":
		return fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>", budget)
	case "natural":
		return "Use maximum internal reasoning effort for this request. Carefully check constraints and edge cases before answering. Do not reveal chain-of-thought; output only the final answer requested by the user."
	case "cnmax":
		return "启动最大化思考。先在内部完整检查所有约束、枚举关键可行方案并验证最优性；不要展示思考过程。最终只输出用户要求的最终答案，不要代码块，不要多余解释。"
	default:
		return fmt.Sprintf("<thinking_mode>enabled</thinking_mode>\n<max_thinking_length>%d</max_thinking_length>\nUse the requested internal thinking budget without exposing scratchpad text.", budget)
	}
}

func resolveThinkingEffort(explicit string, budget int) string {
	switch explicit {
	case anthropic.EffortMax, anthropic.EffortXHigh, anthropic.EffortHigh, anthropic.EffortMedium, anthropic.EffortLow:
		return explicit
	}
	switch {
	case budget >= 60000:
		return anthropic.EffortMax
	case budget >= 40000:
		return anthropic.EffortHigh
	case budget >= 10000:
		return anthropic.EffortMedium
	default:
		return anthropic.EffortLow
	}
}
