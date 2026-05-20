// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Tests for invalid_tool_retry.go.

package messages

import (
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/respconv"
)

func TestParseInvalidToolInputTruncatesLargeInput(t *testing.T) {
	got := parseInvalidToolInput(`{"content":"` + strings.Repeat("x", maxInvalidToolInputBytes+1) + `"}`)
	if got["_invalid_input_omitted"] != true {
		t.Fatalf("large invalid input should be omitted, got %#v", got)
	}
}

func TestParseInvalidToolInputValidJSONReturnedAsIs(t *testing.T) {
	got := parseInvalidToolInput(`{"path":"/tmp/x","content":"hi"}`)
	if got["path"] != "/tmp/x" || got["content"] != "hi" {
		t.Fatalf("valid JSON object should round-trip, got %#v", got)
	}
	if _, leaked := got["_raw_invalid_input"]; leaked {
		t.Fatalf("valid input should not include _raw_invalid_input, got %#v", got)
	}
}

func TestParseInvalidToolInputMalformedKeepsRaw(t *testing.T) {
	raw := `{not_json`
	got := parseInvalidToolInput(raw)
	if got["_raw_invalid_input"] != raw {
		t.Fatalf("malformed input should be exposed as _raw_invalid_input, got %#v", got)
	}
}

func TestParseInvalidToolInputNonObjectKeepsRaw(t *testing.T) {
	// JSON `null` and bare scalars unmarshal to nil/non-map; we want the raw
	// preserved so the model sees what it actually wrote, not an empty map.
	got := parseInvalidToolInput(`null`)
	if got["_raw_invalid_input"] != "null" {
		t.Fatalf("`null` input should be exposed as _raw_invalid_input, got %#v", got)
	}
}

func TestParseInvalidToolInputEmptyReturnsEmptyMap(t *testing.T) {
	got := parseInvalidToolInput(``)
	if len(got) != 0 {
		t.Fatalf("empty input should produce empty map, got %#v", got)
	}
}

func TestAppendInvalidToolRetryMessagesBuildsErrorToolResult(t *testing.T) {
	msgs := appendInvalidToolRetryMessages(nil, []respconv.InvalidToolCall{{
		ID:     "toolu_bad",
		Name:   "Write",
		Input:  `{}`,
		Reason: "missing required field(s): content",
	}})
	if len(msgs) != 2 {
		t.Fatalf("message count = %d, want 2", len(msgs))
	}
	assistant := msgs[0].Content.Blocks
	if len(assistant) != 1 || assistant[0].Type != "tool_use" || assistant[0].ID != "toolu_bad" || assistant[0].Name != "Write" {
		t.Fatalf("unexpected assistant tool_use: %#v", assistant)
	}
	user := msgs[1].Content.Blocks
	if len(user) != 1 || user[0].Type != "tool_result" || user[0].ToolUseID != "toolu_bad" || !user[0].IsError {
		t.Fatalf("unexpected error tool_result: %#v", user)
	}
	if !strings.Contains(user[0].Content.Text, "Retry with complete valid JSON") {
		t.Fatalf("tool_result text missing retry instruction: %q", user[0].Content.Text)
	}
}
