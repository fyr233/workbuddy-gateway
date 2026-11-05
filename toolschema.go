package main

import "sort"

// normalizeRequestToolSchemas repairs tool JSON Schemas in an inbound request
// body. It walks body["tools"] and rewrites `parameters` objects in place.
// Returns the number of tool definitions whose schema had to be repaired.
//
// Why this exists: some clients serialize an empty `required` array as `{}`.
// WorkBuddy rejects such a definition with HTTP 400 / code 11129
// ("invalid function call parameters") and the gateway used to misread that as
// a model rate limit, cooling every account in the pool for 60s. Repairing the
// schema on the way in keeps a malformed client from taking the pool down.
func normalizeRequestToolSchemas(body map[string]any) int {
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) == 0 {
		return 0
	}
	repaired := 0
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// OpenAI chat shape: {"type":"function","function":{"parameters":{...}}}
		if fn, ok := tool["function"].(map[string]any); ok {
			if params, exists := fn["parameters"]; exists {
				fixed, n := normalizeToolSchema(params)
				fn["parameters"] = fixed
				repaired += n
			}
		}
		// Flat shape: {"type":"function","parameters":{...}}
		if params, exists := tool["parameters"]; exists {
			fixed, n := normalizeToolSchema(params)
			tool["parameters"] = fixed
			repaired += n
		}
	}
	return repaired
}

// normalizeToolSchema walks a decoded JSON value and coerces every `required`
// that is not an array into an array. It returns the repaired value and the
// number of `required` fields rewritten. Maps and slices are mutated in place.
func normalizeToolSchema(node any) (any, int) {
	switch v := node.(type) {
	case map[string]any:
		fixed := 0
		for key, child := range v {
			if key == "required" {
				if replacement, ok := normalizeRequiredValue(child); ok {
					v[key] = replacement
					fixed++
					continue
				}
			}
			childFixed, n := normalizeToolSchema(child)
			if n > 0 {
				v[key] = childFixed
				fixed += n
			}
		}
		return v, fixed
	case []any:
		fixed := 0
		for i, child := range v {
			childFixed, n := normalizeToolSchema(child)
			if n > 0 {
				v[i] = childFixed
				fixed += n
			}
		}
		return v, fixed
	default:
		return node, 0
	}
}

// normalizeRequiredValue turns a non-array `required` into an array. A nil or
// empty object becomes an empty array; a non-empty object keeps its keys (sorted
// for stable output, since schema validation does not depend on order).
func normalizeRequiredValue(value any) (any, bool) {
	switch req := value.(type) {
	case nil:
		return []any{}, true
	case map[string]any:
		keys := make([]string, 0, len(req))
		for key := range req {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, key)
		}
		return out, true
	default:
		return value, false
	}
}
