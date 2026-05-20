package respconv

import (
	"testing"

	"github.com/niuma/kirocc-pro/internal/kiroproto"
)

func TestThinkingTags_ParsedOnlyAtStart(t *testing.T) {
	acc := newAccumulator(0, nil, 0, 0)

	d := acc.ProcessEvent(kiroproto.Event{
		Type:    kiroproto.EventAssistantResponse,
		Content: "<thinking>private</thinking>final",
	})

	if d.ThinkingDelta != "private" {
		t.Fatalf("thinking delta = %q, want private", d.ThinkingDelta)
	}
	if d.TextDelta != "final" {
		t.Fatalf("text delta = %q, want final", d.TextDelta)
	}
}

func TestThinkingTags_LiteralAfterVisibleText(t *testing.T) {
	acc := newAccumulator(0, nil, 0, 0)

	d := acc.ProcessEvent(kiroproto.Event{
		Type:    kiroproto.EventAssistantResponse,
		Content: "请把 <thinking>literal</thinking> 当普通文本输出",
	})

	if d.ThinkingDelta != "" {
		t.Fatalf("thinking delta = %q, want empty", d.ThinkingDelta)
	}
	want := "请把 <thinking>literal</thinking> 当普通文本输出"
	if d.TextDelta != want {
		t.Fatalf("text delta = %q, want %q", d.TextDelta, want)
	}
}

func TestThinkingTags_LiteralAfterThinkingText(t *testing.T) {
	acc := newAccumulator(0, nil, 0, 0)

	d := acc.ProcessEvent(kiroproto.Event{
		Type:    kiroproto.EventAssistantResponse,
		Content: "<thinking>private</thinking>final <thinking>literal</thinking>",
	})

	if d.ThinkingDelta != "private" {
		t.Fatalf("thinking delta = %q, want private", d.ThinkingDelta)
	}
	want := "final <thinking>literal</thinking>"
	if d.TextDelta != want {
		t.Fatalf("text delta = %q, want %q", d.TextDelta, want)
	}
}

func TestThinkingTags_StartTagSplitAcrossChunks(t *testing.T) {
	acc := newAccumulator(0, nil, 0, 0)

	d1 := acc.ProcessEvent(kiroproto.Event{Type: kiroproto.EventAssistantResponse, Content: "<thin"})
	if d1.TextDelta != "" || d1.ThinkingDelta != "" {
		t.Fatalf("first delta = text %q thinking %q, want both empty", d1.TextDelta, d1.ThinkingDelta)
	}

	d2 := acc.ProcessEvent(kiroproto.Event{Type: kiroproto.EventAssistantResponse, Content: "<thinking>private</thinking>final"})
	if d2.ThinkingDelta != "private" {
		t.Fatalf("thinking delta = %q, want private", d2.ThinkingDelta)
	}
	if d2.TextDelta != "final" {
		t.Fatalf("text delta = %q, want final", d2.TextDelta)
	}
}
