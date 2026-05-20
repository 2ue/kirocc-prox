package reqconv

import (
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

func TestExtractSystemPrompt(t *testing.T) {
	tests := []struct {
		name   string
		prompt anthropic.SystemPrompt
		want   string
	}{
		{
			name:   "string",
			prompt: anthropic.SystemPrompt{Text: "You are helpful."},
			want:   "You are helpful.",
		},
		{
			name: "array",
			prompt: anthropic.SystemPrompt{
				Blocks: []anthropic.SystemBlock{
					{Type: "text", Text: "Part 1"},
					{Type: "text", Text: "Part 2"},
				},
			},
			want: "Part 1\nPart 2",
		},
		{
			name: "drops billing header block",
			prompt: anthropic.SystemPrompt{
				Blocks: []anthropic.SystemBlock{
					{Type: "text", Text: "x-anthropic-billing-header: cc_version=2.1"},
					{Type: "text", Text: "You are helpful."},
				},
			},
			want: "You are helpful.",
		},
		{
			name:   "empty",
			prompt: anthropic.SystemPrompt{},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSystemPrompt(tt.prompt)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanCurrentText_DropsMCPServerInstructionsReminder(t *testing.T) {
	input := "<system-reminder>\n# MCP Server Instructions\n\nHuge irrelevant docs.\n</system-reminder>\n <system-reminder>\nAs you answer the user's questions, you can use the following context:\n# currentDate\nToday's date is 2026/05/11.\n</system-reminder>\n\n真实问题"

	got := cleanCurrentText(input)
	if strings.Contains(got, "MCP Server Instructions") {
		t.Fatalf("MCP instructions should be removed: %q", got)
	}
	if !strings.Contains(got, "currentDate") {
		t.Fatalf("currentDate reminder should be preserved: %q", got)
	}
	if !strings.Contains(got, "真实问题") {
		t.Fatalf("user content should be preserved: %q", got)
	}
}
