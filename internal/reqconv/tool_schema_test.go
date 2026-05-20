package reqconv

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/niuma/kirocc-pro/internal/anthropic"
)

func TestConvertTools_Basic(t *testing.T) {
	tools := []anthropic.Tool{
		{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		},
	}
	entries := ConvertTools(tools, nil)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	spec := entries[0].ToolSpecification
	if spec.Name != "get_weather" || spec.Description != "Get weather" {
		t.Fatalf("unexpected spec: %+v", spec)
	}
}

func TestConvertTools_EmptyDescription(t *testing.T) {
	tools := []anthropic.Tool{{Name: "my_tool", InputSchema: map[string]any{}}}
	entries := ConvertTools(tools, nil)
	if entries[0].ToolSpecification.Description != "Tool: my_tool" {
		t.Fatalf("got %q", entries[0].ToolSpecification.Description)
	}
}

func TestConvertTools_LongDescription(t *testing.T) {
	longDesc := strings.Repeat("x", 50001)
	tools := []anthropic.Tool{{Name: "Bash", Description: longDesc, InputSchema: map[string]any{}}}
	entries := ConvertTools(tools, nil)
	if entries[0].ToolSpecification.Description != longDesc {
		t.Fatal("long description should be kept as-is")
	}
}

func TestConvertTools_LongNameShortened(t *testing.T) {
	longName := strings.Repeat("a", 65)
	tools := []anthropic.Tool{{Name: longName, InputSchema: map[string]any{}}}
	nameMap := NewToolNameMap()
	entries := ConvertTools(tools, nameMap)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	short := entries[0].ToolSpecification.Name
	if len(short) > maxToolNameLen {
		t.Fatalf("shortened name still too long: %d chars", len(short))
	}
	if nameMap.Restore(short) != longName {
		t.Fatal("reverse mapping failed")
	}
}

func TestConvertTools_WebSearchServerToolGetsObjectSchema(t *testing.T) {
	tools := []anthropic.Tool{{Type: anthropic.ToolTypeWebSearch, Name: "web_search"}}
	entries := ConvertTools(tools, nil)
	if len(entries) != 1 {
		t.Fatalf("got %d entries", len(entries))
	}
	spec := entries[0].ToolSpecification
	if spec.Name != "web_search" {
		t.Fatalf("name = %q", spec.Name)
	}
	schema := spec.InputSchema.JSON
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties = %T", schema["properties"])
	}
	if _, ok := props["query"]; !ok {
		t.Fatalf("missing query property: %+v", props)
	}
}

func TestSanitizeJSONSchema_RemovesAdditionalProperties(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"x": map[string]any{"type": "string", "additionalProperties": true}},
	}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["additionalProperties"]; ok {
		t.Fatal("additionalProperties should be removed")
	}
	props := got["properties"].(map[string]any)
	x := props["x"].(map[string]any)
	if _, ok := x["additionalProperties"]; ok {
		t.Fatal("nested additionalProperties should be removed")
	}
}

func TestSanitizeJSONSchema_RemovesEmptyRequired(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{}}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["required"]; ok {
		t.Fatal("empty required should be removed")
	}
}

func TestSanitizeJSONSchema_KeepsNonEmptyRequired(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []any{"x"}}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["required"]; !ok {
		t.Fatal("non-empty required should be kept")
	}
}

func TestSanitizeJSONSchema_ConstToEnum(t *testing.T) {
	schema := map[string]any{"const": "hello"}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["const"]; ok {
		t.Fatal("const should be removed")
	}
	enum, ok := got["enum"].([]any)
	if !ok || len(enum) != 1 || enum[0] != "hello" {
		t.Fatalf("expected enum: [hello], got %v", got["enum"])
	}
}

func TestSanitizeJSONSchema_Nil(t *testing.T) {
	got := SanitizeJSONSchema(nil)
	if got == nil {
		t.Fatal("should return empty map, not nil")
	}
}

func TestSanitizeJSONSchema_FlattensAnyOfEnums(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{
				"anyOf": []any{
					map[string]any{"enum": []any{"pending", "in_progress", "completed"}, "type": "string"},
					map[string]any{"enum": []any{"deleted"}, "type": "string"},
				},
			},
		},
	}
	got := SanitizeJSONSchema(schema)
	props := got["properties"].(map[string]any)
	status := props["status"].(map[string]any)
	if _, ok := status["anyOf"]; ok {
		t.Fatal("anyOf should be flattened")
	}
	enum, ok := status["enum"].([]any)
	if !ok {
		t.Fatal("expected enum field")
	}
	if len(enum) != 4 {
		t.Fatalf("expected 4 enum values, got %d: %v", len(enum), enum)
	}
	if status["type"] != "string" {
		t.Fatalf("expected type string, got %v", status["type"])
	}
}

func TestSanitizeJSONSchema_AnyOfNullable_NoWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "null"},
		},
	}
	got := SanitizeJSONSchema(schema)

	if got["type"] != "string" {
		t.Fatalf("expected type string, got %v", got["type"])
	}
	if _, ok := got["anyOf"]; ok {
		t.Fatal("anyOf should be removed")
	}
	if buf.Len() > 0 {
		t.Fatalf("expected no warning for nullable anyOf, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_OneOfNullable_NoWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "null"},
			map[string]any{"type": "integer", "description": "count"},
		},
	}
	got := SanitizeJSONSchema(schema)

	if got["type"] != "integer" {
		t.Fatalf("expected type integer, got %v", got["type"])
	}
	if got["description"] != "count" {
		t.Fatalf("expected description preserved, got %v", got["description"])
	}
	if buf.Len() > 0 {
		t.Fatalf("expected no warning for nullable oneOf, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_AnyOfNullableMultiNonNull_MergesScalarTypes(t *testing.T) {
	// `[string, integer, null]` previously fell through to first-branch only
	// (type="string"), silently losing the integer branch. The model would
	// then send an int and the proxy/Kiro would reject it. Pure-scalar
	// unions now merge into a JSON Schema type array without warning.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
			map[string]any{"type": "null"},
		},
	}
	got := SanitizeJSONSchema(schema)

	types, ok := got["type"].([]any)
	if !ok {
		t.Fatalf("expected merged type array, got %T %v", got["type"], got["type"])
	}
	want := map[string]bool{"string": true, "integer": true}
	for _, typ := range types {
		delete(want, typ.(string))
	}
	if len(want) > 0 {
		t.Fatalf("missing merged types %v in %v", want, types)
	}
	if strings.Contains(buf.String(), "lossy") {
		t.Fatalf("scalar union should merge cleanly, got warning: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_AnyOfScalarTypes_NoWarning(t *testing.T) {
	// Pure scalar [string, number] unions merge to a type array; no lossy
	// warning should be logged.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "number"},
		},
	}
	got := SanitizeJSONSchema(schema)

	types, ok := got["type"].([]any)
	if !ok {
		t.Fatalf("expected merged type array, got %T %v", got["type"], got["type"])
	}
	if len(types) != 2 {
		t.Fatalf("expected 2 merged types, got %v", types)
	}
	if strings.Contains(buf.String(), "lossy") {
		t.Fatalf("scalar union should not warn, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_OneOfScalarTypes_NoWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"oneOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "number"},
		},
	}
	got := SanitizeJSONSchema(schema)

	if _, ok := got["type"].([]any); !ok {
		t.Fatalf("expected merged type array, got %T %v", got["type"], got["type"])
	}
	if strings.Contains(buf.String(), "lossy") {
		t.Fatalf("scalar oneOf should not warn, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_AnyOfEnum_NoWarning(t *testing.T) {
	// When all branches are enum-based, no warning should be logged.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"enum": []any{"a"}, "type": "string"},
			map[string]any{"enum": []any{"b"}, "type": "string"},
		},
	}
	SanitizeJSONSchema(schema)

	if buf.Len() > 0 {
		t.Fatalf("expected no warning for enum-based anyOf, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_AnyOfScalarWithDescription_MergesTypes(t *testing.T) {
	// Scalar branches with `description` are still pure scalars and should
	// merge into a type array; description is dropped because it cannot be
	// faithfully merged across branches.
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"type": "string", "description": "a string"},
			map[string]any{"type": "number"},
		},
	}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["anyOf"]; ok {
		t.Fatal("anyOf should be removed")
	}
	types, ok := got["type"].([]any)
	if !ok {
		t.Fatalf("expected merged type array, got %T %v", got["type"], got["type"])
	}
	if len(types) != 2 {
		t.Fatalf("expected 2 merged types, got %v", types)
	}
}

func TestSanitizeJSONSchema_AnyOfMixedObjectAndScalar_FallsBackToFirst(t *testing.T) {
	// When a union mixes object schema with scalar shapes, we cannot express
	// it accurately and must keep the first-branch lossy fallback to avoid
	// dropping the object's properties.
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	old := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(old)

	schema := map[string]any{
		"anyOf": []any{
			map[string]any{
				"type":       "object",
				"properties": map[string]any{"x": map[string]any{"type": "string"}},
			},
			map[string]any{"type": "string"},
		},
	}
	got := SanitizeJSONSchema(schema)
	if got["type"] != "object" {
		t.Fatalf("expected fallback to first object branch, got %v", got["type"])
	}
	if !strings.Contains(buf.String(), "lossy") {
		t.Fatalf("expected lossy warning for unmergeable union, got: %q", buf.String())
	}
}

func TestSanitizeJSONSchema_AnyOfObjectBranches_MergesProperties(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      map[string]any{"type": "string"},
					"recursive": map[string]any{"type": "boolean"},
				},
				"required": []any{"path"},
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
					"timeout": map[string]any{"type": "integer"},
				},
				"required": []any{"command"},
			},
		},
	}

	got := SanitizeJSONSchema(schema)
	if got["type"] != "object" {
		t.Fatalf("expected object type, got %v", got["type"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %v", got["properties"])
	}
	for _, name := range []string{"path", "recursive", "command", "timeout"} {
		if _, ok := props[name]; !ok {
			t.Fatalf("missing merged property %q in %v", name, props)
		}
	}
	if _, ok := got["required"]; ok {
		t.Fatalf("branch-specific required fields should not be forced globally: %v", got["required"])
	}
}

func TestSanitizeJSONSchema_OneOfObjectBranches_KeepsCommonRequired(t *testing.T) {
	schema := map[string]any{
		"oneOf": []any{
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":   map[string]any{"type": "string"},
					"path": map[string]any{"type": "string"},
				},
				"required": []any{"id", "path"},
			},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":      map[string]any{"type": "string"},
					"command": map[string]any{"type": "string"},
				},
				"required": []any{"id", "command"},
			},
		},
	}

	got := SanitizeJSONSchema(schema)
	req, ok := got["required"].([]any)
	if !ok || len(req) != 1 || req[0] != "id" {
		t.Fatalf("expected only common required [id], got %v", got["required"])
	}
}

func TestSanitizeJSONSchema_AnyOfConstBranches(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"const": "A"},
			map[string]any{"const": "B"},
			map[string]any{"const": "C"},
		},
	}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["anyOf"]; ok {
		t.Fatal("anyOf should be flattened")
	}
	enum, ok := got["enum"].([]any)
	if !ok {
		t.Fatalf("expected enum field, got %v", got)
	}
	if len(enum) != 3 {
		t.Fatalf("expected 3 enum values, got %d: %v", len(enum), enum)
	}
}

func TestSanitizeJSONSchema_AnyOfMixedTypes_NoType(t *testing.T) {
	schema := map[string]any{
		"anyOf": []any{
			map[string]any{"enum": []any{"hello"}, "type": "string"},
			map[string]any{"enum": []any{42}, "type": "integer"},
		},
	}
	got := SanitizeJSONSchema(schema)
	enum, ok := got["enum"].([]any)
	if !ok {
		t.Fatalf("expected enum field, got %v", got)
	}
	if len(enum) != 2 {
		t.Fatalf("expected 2 enum values, got %d: %v", len(enum), enum)
	}
	if _, ok := got["type"]; ok {
		t.Fatal("type should be omitted for mixed-type enums")
	}
}

func TestSanitizeJSONSchema_AllOfMerged(t *testing.T) {
	schema := map[string]any{
		"allOf": []any{
			map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}}},
			map[string]any{"required": []any{"a"}},
		},
	}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["allOf"]; ok {
		t.Fatal("allOf should be removed")
	}
	if got["type"] != "object" {
		t.Fatalf("expected type object, got %v", got["type"])
	}
	req, ok := got["required"].([]any)
	if !ok || len(req) != 1 {
		t.Fatalf("expected required [a], got %v", got["required"])
	}
}

func TestSanitizeJSONSchema_RemovesValidationKeywords(t *testing.T) {
	keywords := []string{
		"format", "pattern",
		"minLength", "maxLength",
		"minimum", "maximum",
		"minItems", "maxItems",
		"uniqueItems", "multipleOf",
		"not",
	}
	for _, kw := range keywords {
		schema := map[string]any{"type": "string", kw: "value"}
		got := SanitizeJSONSchema(schema)
		if _, ok := got[kw]; ok {
			t.Fatalf("%q should be removed", kw)
		}
		if got["type"] != "string" {
			t.Fatalf("type should be preserved when removing %q", kw)
		}
	}
}

func TestSanitizeJSONSchema_RemovesDollarSchema(t *testing.T) {
	schema := map[string]any{"type": "object", "$schema": "http://json-schema.org/draft-07/schema#"}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["$schema"]; ok {
		t.Fatal("$schema should be removed")
	}
}

func TestSanitizeJSONSchema_RemovesPatternProperties(t *testing.T) {
	schema := map[string]any{"type": "object", "patternProperties": map[string]any{}}
	got := SanitizeJSONSchema(schema)
	if _, ok := got["patternProperties"]; ok {
		t.Fatal("patternProperties should be removed")
	}
}

func TestSanitizeJSONSchema_AnyOfOverridesType_Deterministic(t *testing.T) {
	// anyOf first-branch has type "string", but schema also has type "object".
	// Combinator should always win regardless of map iteration order, and the
	// scalar union should produce the same merged result on every iteration.
	schema := map[string]any{
		"type": "object",
		"anyOf": []any{
			map[string]any{"type": "string", "description": "a string"},
			map[string]any{"type": "number"},
		},
	}
	var first []any
	// Run multiple times to catch map iteration order flakiness.
	for i := range 100 {
		got := SanitizeJSONSchema(schema)
		types, ok := got["type"].([]any)
		if !ok {
			t.Fatalf("iteration %d: expected merged type array, got %T %v", i, got["type"], got["type"])
		}
		if i == 0 {
			first = types
			continue
		}
		if len(types) != len(first) {
			t.Fatalf("iteration %d: type len = %d, first iteration had %d (must be deterministic)", i, len(types), len(first))
		}
		for j := range types {
			if types[j] != first[j] {
				t.Fatalf("iteration %d: type[%d] = %v, first iteration had %v (must be deterministic)", i, j, types[j], first[j])
			}
		}
	}
}
