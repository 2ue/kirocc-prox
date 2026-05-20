// [fork] New file added in fork (not present in upstream d-kuro/kirocc).
// Implements top-level required-field and JSON-type validation for tool_use
// inputs (Write/Edit/AskUserQuestion etc.), preventing malformed tool calls
// from reaching Claude Code and causing InputValidationError stalls.

package respconv

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/niuma/kirocc-pro/internal/anthropic"
	"github.com/niuma/kirocc-pro/internal/reqconv"
)

// ToolInputValidator performs a small, hot-path validation of model-emitted tool
// input against the request tool schemas before forwarding tool_use to clients.
// It intentionally checks only valid top-level JSON object shape and top-level
// JSON-Schema required fields; it does not recursively validate properties.
type ToolInputValidator struct {
	known    map[string]struct{}
	required map[string][]string
	types    map[string]map[string][]string
}

// NewToolInputValidator builds a validator from the tools advertised on the
// current request. Unknown tools are allowed so internal/synthetic tools are not
// blocked accidentally.
func NewToolInputValidator(tools []anthropic.Tool) *ToolInputValidator {
	if len(tools) == 0 {
		return nil
	}
	v := &ToolInputValidator{
		known:    make(map[string]struct{}, len(tools)),
		required: make(map[string][]string, len(tools)),
		types:    make(map[string]map[string][]string, len(tools)),
	}
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		v.known[t.Name] = struct{}{}
		schema := reqconv.SanitizeJSONSchema(t.InputSchema)
		if req := requiredFields(schema); len(req) > 0 {
			v.required[t.Name] = req
		}
		if types := propertyTypes(schema); len(types) > 0 {
			v.types[t.Name] = types
		}
	}
	if len(v.known) == 0 {
		return nil
	}
	return v
}

func requiredFields(schema map[string]any) []string {
	raw, ok := schema["required"]
	if !ok {
		return nil
	}
	var out []string
	switch v := raw.(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func propertyTypes(schema map[string]any) map[string][]string {
	raw, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string][]string)
	for name, prop := range raw {
		propSchema, ok := prop.(map[string]any)
		if !ok || name == "" {
			continue
		}
		types := schemaTypes(propSchema["type"])
		if len(types) > 0 {
			out[name] = types
		}
	}
	return out
}

func schemaTypes(raw any) []string {
	var out []string
	switch v := raw.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	case []string:
		out = append(out, v...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Validate returns false when a known tool call is unsafe to forward. The second
// return value is the normalized JSON input to forward when the call is valid.
func (v *ToolInputValidator) Validate(toolName, raw string) (bool, string, string) {
	if v == nil {
		return true, raw, ""
	}
	if _, ok := v.known[toolName]; !ok {
		return true, raw, ""
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		raw = "{}"
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return false, "", "invalid JSON input"
	}
	if obj == nil {
		obj = map[string]any{}
		raw = "{}"
	}
	required := v.required[toolName]
	var missing []string
	for _, name := range required {
		if _, ok := obj[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return false, "", fmt.Sprintf("missing required field(s): %s", strings.Join(missing, ", "))
	}
	if types := v.types[toolName]; len(types) > 0 {
		for name, expected := range types {
			value, ok := obj[name]
			if !ok {
				continue
			}
			if !matchesJSONTypes(value, expected) {
				return false, "", fmt.Sprintf("field %q expected %s but got %s", name, strings.Join(expected, " or "), jsonType(value))
			}
		}
	}
	return true, raw, ""
}

func matchesJSONTypes(value any, expected []string) bool {
	actual := jsonType(value)
	for _, typ := range expected {
		switch typ {
		case actual:
			return true
		case "number":
			if actual == "integer" {
				return true
			}
		case "integer":
			if n, ok := value.(float64); ok && math.Trunc(n) == n {
				return true
			}
		}
	}
	return false
}

func jsonType(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case float64:
		if math.Trunc(v) == v {
			return "integer"
		}
		return "number"
	default:
		return fmt.Sprintf("%T", value)
	}
}
