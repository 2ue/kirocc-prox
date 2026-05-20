package reqconv

import (
	"strings"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

// ExtractTextContent extracts plain text from message content.
// String content is returned as-is.
// For block arrays: text blocks are joined with space, thinking blocks are ignored,
// unknown blocks are converted to text like [type: name].
func ExtractTextContent(content anthropic.MessageContent) string {
	if content.IsString() {
		return content.Text
	}
	var parts []string
	for _, b := range content.Blocks {
		switch b.Type {
		case anthropic.BlockTypeText:
			parts = append(parts, b.Text)
		case anthropic.BlockTypeThinking, anthropic.BlockTypeToolUse, anthropic.BlockTypeToolResult, anthropic.BlockTypeImage, anthropic.BlockTypeToolReference,
			anthropic.BlockTypeServerToolUse, anthropic.BlockTypeToolSearchToolResult:
			// Skip — handled separately.
		default:
			// Unknown block type → textualize.
			parts = append(parts, textualizeUnknownBlock(b))
		}
	}
	return strings.Join(parts, " ")
}

// textualizeUnknownBlock converts an unknown content block to a text representation.
func textualizeUnknownBlock(b anthropic.ContentBlock) string {
	identifier := b.ToolName // tool_reference uses tool_name
	if identifier == "" {
		identifier = b.Name
	}
	if identifier == "" {
		identifier = b.ID
	}
	if identifier != "" {
		return "[" + b.Type + ": " + identifier + "]"
	}
	return "[" + b.Type + "]"
}

// ExtractSystemPrompt extracts the system prompt text from the SystemPrompt union type.
// String form returns as-is. Array form joins text blocks with "\n".
func ExtractSystemPrompt(system anthropic.SystemPrompt) string {
	if system.IsEmpty() {
		return ""
	}
	if system.Text != "" {
		return cleanSystemPromptText(system.Text)
	}
	var parts []string
	for _, block := range system.Blocks {
		if block.Type == anthropic.BlockTypeText && block.Text != "" {
			if cleaned := cleanSystemPromptText(block.Text); cleaned != "" {
				parts = append(parts, cleaned)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func cleanSystemPromptText(text string) string {
	if strings.HasPrefix(strings.TrimSpace(text), "x-anthropic-billing-header:") {
		return ""
	}
	return text
}

// [fork] Modified from upstream d-kuro/kirocc: added <system-reminder> block
// stripping for MCP Server Instructions (fixes #1 and #2). Removes the bulky
// MCP tool descriptions from history while preserving other system reminders
// (currentDate, etc.). Reduces prompt-token bloat by ~14% on the sample we
// measured and reduces model confusion from stale MCP context.
func cleanCurrentText(text string) string {
	const open = "<system-reminder>"
	const close = "</system-reminder>"

	var out strings.Builder
	for {
		start := strings.Index(text, open)
		if start < 0 {
			out.WriteString(text)
			break
		}
		endRel := strings.Index(text[start+len(open):], close)
		if endRel < 0 {
			out.WriteString(text)
			break
		}
		end := start + len(open) + endRel + len(close)
		blockBody := text[start+len(open) : start+len(open)+endRel]
		if strings.HasPrefix(strings.TrimSpace(blockBody), "# MCP Server Instructions") {
			out.WriteString(text[:start])
			text = strings.TrimLeft(text[end:], " \n\t")
			continue
		}
		out.WriteString(text[:end])
		text = text[end:]
	}
	return out.String()
}
