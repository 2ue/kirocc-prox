package reqconv

import (
	"log/slog"
	"maps"
)

// unsupportedKeywords lists JSON Schema keywords that Kiro API rejects.
var unsupportedKeywords = map[string]struct{}{
	"additionalProperties":  {},
	"$schema":               {},
	"propertyNames":         {},
	"default":               {},
	"exclusiveMinimum":      {},
	"exclusiveMaximum":      {},
	"$defs":                 {},
	"$ref":                  {},
	"patternProperties":     {},
	"if":                    {},
	"then":                  {},
	"else":                  {},
	"dependentRequired":     {},
	"dependentSchemas":      {},
	"prefixItems":           {},
	"unevaluatedProperties": {},
	"unevaluatedItems":      {},
	"contentMediaType":      {},
	"contentEncoding":       {},
	"format":                {},
	"pattern":               {},
	"minLength":             {},
	"maxLength":             {},
	"minimum":               {},
	"maximum":               {},
	"minItems":              {},
	"maxItems":              {},
	"uniqueItems":           {},
	"multipleOf":            {},
	"not":                   {},
}

// SanitizeJSONSchema recursively removes fields that Kiro API rejects.
func SanitizeJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}

	result := make(map[string]any, len(schema))

	// First pass: process all non-combinator keys.
	for key, value := range schema {
		if _, drop := unsupportedKeywords[key]; drop {
			continue
		}
		switch key {
		case "const":
			result["enum"] = []any{value}
		case "required":
			if arr, ok := value.([]any); ok && len(arr) == 0 {
				continue
			}
			result[key] = value
		case "anyOf", "oneOf", "allOf":
			// Handled in second pass.
		default:
			switch v := value.(type) {
			case map[string]any:
				result[key] = SanitizeJSONSchema(v)
			case []any:
				sanitized := make([]any, len(v))
				for i, item := range v {
					if m, ok := item.(map[string]any); ok {
						sanitized[i] = SanitizeJSONSchema(m)
					} else {
						sanitized[i] = item
					}
				}
				result[key] = sanitized
			default:
				result[key] = value
			}
		}
	}

	// Second pass: apply combinators last so they deterministically override.
	for key, value := range schema {
		switch key {
		case "anyOf", "oneOf":
			if arr, ok := value.([]any); ok && len(arr) > 0 {
				branches := dropNullBranches(arr)
				if len(branches) == 0 {
					continue
				}
				if merged := flattenEnumBranches(branches); merged != nil {
					maps.Copy(result, merged)
				} else if merged := flattenObjectBranches(branches); merged != nil {
					slog.Warn("approximate schema conversion: merging object union branches",
						"combinator", key, "branches", len(branches))
					maps.Copy(result, merged)
				} else if merged := flattenScalarBranches(branches); merged != nil {
					maps.Copy(result, merged)
				} else if len(branches) == 1 {
					if m, ok := branches[0].(map[string]any); ok {
						maps.Copy(result, SanitizeJSONSchema(m))
					}
				} else if first, ok := branches[0].(map[string]any); ok {
					slog.Warn("lossy schema conversion: using first branch only",
						"combinator", key, "branches", len(branches))
					maps.Copy(result, SanitizeJSONSchema(first))
				}
			}
		case "allOf":
			if arr, ok := value.([]any); ok {
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						maps.Copy(result, SanitizeJSONSchema(m))
					}
				}
			}
		}
	}

	return result
}

// flattenObjectBranches approximates an anyOf/oneOf union of object shapes by
// exposing the union of all properties to Kiro. This is less precise than JSON
// Schema, but it preserves tool parameters that would otherwise be dropped when
// falling back to the first branch only. Required fields are kept only when they
// are required by every branch, avoiding impossible cross-branch requirements.
func flattenObjectBranches(branches []any) map[string]any {
	if len(branches) < 2 {
		return nil
	}

	properties := map[string]any{}
	var commonRequired map[string]struct{}
	var requiredOrder []string
	for i, branch := range branches {
		m, ok := branch.(map[string]any)
		if !ok {
			return nil
		}
		sanitized := SanitizeJSONSchema(m)
		props, _ := sanitized["properties"].(map[string]any)
		typ, _ := sanitized["type"].(string)
		if typ != "object" && len(props) == 0 {
			return nil
		}
		for name, prop := range props {
			if _, exists := properties[name]; !exists {
				properties[name] = prop
			}
		}

		req := requiredSet(sanitized["required"])
		if i == 0 {
			commonRequired = req
			requiredOrder = requiredNames(sanitized["required"])
			continue
		}
		for name := range commonRequired {
			if _, ok := req[name]; !ok {
				delete(commonRequired, name)
			}
		}
	}
	if len(properties) == 0 {
		return nil
	}

	merged := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if len(commonRequired) > 0 {
		required := make([]any, 0, len(commonRequired))
		for _, name := range requiredOrder {
			if _, ok := commonRequired[name]; ok {
				required = append(required, name)
			}
		}
		if len(required) > 0 {
			merged["required"] = required
		}
	}
	return merged
}

func requiredSet(value any) map[string]struct{} {
	result := map[string]struct{}{}
	for _, name := range requiredNames(value) {
		result[name] = struct{}{}
	}
	return result
}

func requiredNames(value any) []string {
	arr, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if name, ok := item.(string); ok {
			result = append(result, name)
		}
	}
	return result
}

// dropNullBranches returns branches that are not {type: "null"}.
func dropNullBranches(branches []any) []any {
	var result []any
	for _, b := range branches {
		m, ok := b.(map[string]any)
		if !ok || m["type"] != "null" {
			result = append(result, b)
		}
	}
	return result
}

// flattenScalarBranches handles `anyOf`/`oneOf` unions whose branches are all
// plain scalar shapes (e.g. `[{"type":"string"}, {"type":"integer"}]`). Such
// unions previously fell through to the lossy "first branch only" fallback,
// which silently dropped half of the legal types — the model would then send
// an integer for a string-only signature and the proxy/Kiro would reject it.
//
// Each branch is sanitized once. A branch qualifies only when it carries no
// schema constraints other than `type` (and optionally a free-text
// `description`); any other key would be a substantive constraint that cannot
// be expressed by a `type` array. Returns nil when the union is not pure
// scalars, so the caller can fall through to the existing fallbacks.
func flattenScalarBranches(branches []any) map[string]any {
	if len(branches) < 2 {
		return nil
	}
	seen := map[string]struct{}{}
	var types []any
	for _, branch := range branches {
		m, ok := branch.(map[string]any)
		if !ok {
			return nil
		}
		sanitized := SanitizeJSONSchema(m)
		for k := range sanitized {
			if k != "type" && k != "description" {
				return nil
			}
		}
		typ, ok := sanitized["type"].(string)
		if !ok || typ == "" || typ == "object" || typ == "array" {
			return nil
		}
		if _, dup := seen[typ]; dup {
			continue
		}
		seen[typ] = struct{}{}
		types = append(types, typ)
	}
	if len(types) == 0 {
		return nil
	}
	if len(types) == 1 {
		return map[string]any{"type": types[0]}
	}
	return map[string]any{"type": types}
}

// flattenEnumBranches merges anyOf/oneOf branches when all branches have enum values.
// Each branch is sanitized exactly once and the sanitized result is reused for
// enum/type extraction, avoiding the double SanitizeJSONSchema call that the
// previous combinator pass performed per branch.
// Returns a merged schema with combined enum, or nil if not all branches are enum-based.
func flattenEnumBranches(branches []any) map[string]any {
	if len(branches) == 0 {
		return nil
	}
	var allEnums []any
	var typ string
	typConsistent := true
	for _, branch := range branches {
		m, ok := branch.(map[string]any)
		if !ok {
			return nil
		}
		sanitized := SanitizeJSONSchema(m)
		enumVal, hasEnum := sanitized["enum"]
		if !hasEnum {
			return nil
		}
		arr, ok := enumVal.([]any)
		if !ok {
			return nil
		}
		allEnums = append(allEnums, arr...)
		if t, ok := sanitized["type"].(string); ok {
			if typ == "" {
				typ = t
			} else if typ != t {
				typConsistent = false
			}
		} else {
			typConsistent = false
		}
	}
	merged := map[string]any{"enum": allEnums}
	if typ != "" && typConsistent {
		merged["type"] = typ
	}
	return merged
}
