package splitrelay

import "encoding/json"

// Match the existing relay's Gemini schema repair without changing other models.
func repairTools(body map[string]json.RawMessage) {
	var tools []map[string]any
	if json.Unmarshal(body["tools"], &tools) != nil {
		return
	}
	for _, tool := range tools {
		if schema, ok := tool["input_schema"]; ok && schema != nil {
			tool["input_schema"] = repairSchema(schema)
		}
	}
	if encoded, err := json.Marshal(tools); err == nil {
		body["tools"] = encoded
	}
}
func repairSchema(value any) any {
	if list, ok := value.([]any); ok {
		for i, v := range list {
			list[i] = repairSchema(v)
		}
		return list
	}
	node, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if len(node) == 0 {
		return map[string]any{"type": "string"}
	}
	for _, key := range []string{"properties", "$defs", "definitions"} {
		if entries, ok := node[key].(map[string]any); ok {
			for k, v := range entries {
				entries[k] = repairSchema(v)
			}
		}
	}
	if items, ok := node["prefixItems"].([]any); ok {
		node["prefixItems"] = repairSchema(items)
	}
	switch node["items"].(type) {
	case bool, []any:
		node["items"] = map[string]any{"type": "string"}
	}
	kind, _ := node["type"].(string)
	array := kind == "array"
	if types, ok := node["type"].([]any); ok {
		for _, v := range types {
			if v == "array" {
				array = true
			}
		}
	}
	if array {
		if node["items"] == nil {
			node["items"] = map[string]any{"type": "string"}
		}
		delete(node, "prefixItems")
	}
	if node["items"] != nil {
		node["items"] = repairSchema(node["items"])
	}
	if extra, ok := node["additionalProperties"].(map[string]any); ok {
		node["additionalProperties"] = repairSchema(extra)
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf"} {
		if list, ok := node[key].([]any); ok {
			node[key] = repairSchema(list)
		}
	}
	return node
}
