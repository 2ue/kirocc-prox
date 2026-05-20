// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Tests for tool_input_validator.go.

package respconv

import (
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

func TestToolInputValidator_KnownToolMissingRequired(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "Write",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"file_path", "content"},
		},
	}})
	ok, normalized, reason := v.Validate("Write", `{"file_path":"/tmp/a"}`)
	if ok {
		t.Fatal("expected invalid tool input")
	}
	if normalized != "" {
		t.Fatalf("normalized = %q, want empty", normalized)
	}
	if !strings.Contains(reason, "content") {
		t.Fatalf("reason = %q, want missing content", reason)
	}
}

func TestToolInputValidator_EmptyInputAllowedWhenNoRequired(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name:        "Ping",
		InputSchema: map[string]any{"type": "object"},
	}})
	ok, normalized, reason := v.Validate("Ping", "")
	if !ok {
		t.Fatalf("expected valid, reason=%q", reason)
	}
	if normalized != "{}" {
		t.Fatalf("normalized = %q, want {}", normalized)
	}
}

func TestToolInputValidator_NullInputAllowedWhenNoRequired(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{Name: "Ping"}})
	ok, normalized, reason := v.Validate("Ping", "null")
	if !ok {
		t.Fatalf("expected valid, reason=%q", reason)
	}
	if normalized != "{}" {
		t.Fatalf("normalized = %q, want {}", normalized)
	}
}

func TestToolInputValidator_BadJSONRejectedForKnownTool(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{Name: "Edit"}})
	ok, _, reason := v.Validate("Edit", `{bad`)
	if ok {
		t.Fatal("expected invalid JSON to be rejected")
	}
	if !strings.Contains(reason, "invalid JSON") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestToolInputValidator_TopLevelFieldTypeMismatch(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "AskUserQuestion",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"questions"},
			"properties": map[string]any{
				"questions": map[string]any{"type": "array"},
			},
		},
	}})
	ok, normalized, reason := v.Validate("AskUserQuestion", `{"questions":"what should I do?"}`)
	if ok {
		t.Fatal("expected type mismatch to be rejected")
	}
	if normalized != "" {
		t.Fatalf("normalized = %q, want empty", normalized)
	}
	if !strings.Contains(reason, `field "questions" expected array but got string`) {
		t.Fatalf("reason = %q", reason)
	}
}

func TestToolInputValidator_TopLevelFieldTypeMatch(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "AskUserQuestion",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questions": map[string]any{"type": "array"},
			},
		},
	}})
	ok, normalized, reason := v.Validate("AskUserQuestion", `{"questions":[]}`)
	if !ok {
		t.Fatalf("expected valid input, reason=%q", reason)
	}
	if normalized != `{"questions":[]}` {
		t.Fatalf("normalized = %q", normalized)
	}
}

func TestToolInputValidator_OptionalTopLevelFieldTypeMismatch(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "OptionalTyped",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{"type": "array"},
			},
		},
	}})
	ok, _, reason := v.Validate("OptionalTyped", `{"items":"bad"}`)
	if ok {
		t.Fatal("expected optional type mismatch to be rejected")
	}
	if !strings.Contains(reason, `field "items" expected array but got string`) {
		t.Fatalf("reason = %q", reason)
	}
}

func TestToolInputValidator_AskUserQuestionSanitizedAnyOf(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "AskUserQuestion",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"questions"},
			"properties": map[string]any{
				"questions": map[string]any{
					"anyOf": []any{
						map[string]any{"type": "array"},
						map[string]any{"type": "null"},
					},
				},
			},
		},
	}})
	ok, _, reason := v.Validate("AskUserQuestion", `{"questions":"what should I do?"}`)
	if ok {
		t.Fatal("expected sanitized anyOf type mismatch to be rejected")
	}
	if !strings.Contains(reason, `field "questions" expected array but got string`) {
		t.Fatalf("reason = %q", reason)
	}
}

func TestToolInputValidator_EditSanitizedOneOf(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "Edit",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"file_path", "old_string", "new_string"},
			"properties": map[string]any{
				"file_path":  map[string]any{"type": "string"},
				"old_string": map[string]any{"oneOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}},
				"new_string": map[string]any{"type": "string"},
			},
		},
	}})
	ok, _, reason := v.Validate("Edit", `{"file_path":"/tmp/a","old_string":42,"new_string":"x"}`)
	if ok {
		t.Fatal("expected sanitized oneOf type mismatch to be rejected")
	}
	if !strings.Contains(reason, `field "old_string" expected string but got integer`) {
		t.Fatalf("reason = %q", reason)
	}
}

func TestToolInputValidator_BashSanitizedNullableInteger(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{
		Name: "Bash",
		InputSchema: map[string]any{
			"type":     "object",
			"required": []any{"command"},
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"timeout": map[string]any{"anyOf": []any{
					map[string]any{"type": "integer"},
					map[string]any{"type": "null"},
				}},
			},
		},
	}})
	ok, _, reason := v.Validate("Bash", `{"command":"ls","timeout":"fast"}`)
	if ok {
		t.Fatal("expected sanitized nullable integer mismatch to be rejected")
	}
	if !strings.Contains(reason, `field "timeout" expected integer but got string`) {
		t.Fatalf("reason = %q", reason)
	}

	ok, _, reason = v.Validate("Bash", `{"command":"ls","timeout":5}`)
	if !ok {
		t.Fatalf("expected integer timeout to pass, reason=%q", reason)
	}
}

func TestToolInputValidator_UnknownToolPasses(t *testing.T) {
	v := NewToolInputValidator([]anthropic.Tool{{Name: "Write"}})
	ok, normalized, reason := v.Validate("FutureInternalTool", `{bad`)
	if !ok {
		t.Fatalf("unknown tool should pass, reason=%q", reason)
	}
	if normalized != `{bad` {
		t.Fatalf("normalized = %q", normalized)
	}
}
