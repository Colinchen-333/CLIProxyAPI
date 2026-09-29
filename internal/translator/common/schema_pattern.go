package common

import (
	"bytes"
	"encoding/json"
	"strings"
)

// PortableRegexPattern rewrites the ECMAScript NUL escape \0 as \x00. Claude Code
// emits patterns such as ^[^\0]*$ from JavaScript regex literals, and OpenAI-style
// schema validators (OpenCode Go among them) reject \0 as "not a regex", failing
// every request that carries the tool. \x00 means the same thing in both dialects.
// An escaped backslash (\\0) and an octal-looking \0 followed by a digit are left alone.
func PortableRegexPattern(pattern string) string {
	if !strings.Contains(pattern, `\0`) {
		return pattern
	}
	var b strings.Builder
	b.Grow(len(pattern) + 8)
	for i := 0; i < len(pattern); i++ {
		if pattern[i] != '\\' || i+1 >= len(pattern) {
			b.WriteByte(pattern[i])
			continue
		}
		next := pattern[i+1]
		if next == '0' && (i+2 >= len(pattern) || pattern[i+2] < '0' || pattern[i+2] > '9') {
			b.WriteString(`\x00`)
		} else {
			b.WriteByte('\\')
			b.WriteByte(next)
		}
		i++
	}
	return b.String()
}

// PortableSchemaPatterns applies PortableRegexPattern to every string "pattern"
// keyword in a raw JSON schema. A property that happens to be named "pattern"
// holds an object, so it is walked rather than rewritten. Schemas without a \0
// escape are returned byte-for-byte unchanged.
func PortableSchemaPatterns(raw []byte) []byte {
	if !bytes.Contains(raw, []byte(`\\0`)) {
		return raw
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var schema any
	if err := decoder.Decode(&schema); err != nil {
		return raw
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(portableSchemaNode(schema)); err != nil {
		return raw
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

func portableSchemaNode(node any) any {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			if pattern, ok := child.(string); ok && key == "pattern" {
				value[key] = PortableRegexPattern(pattern)
				continue
			}
			value[key] = portableSchemaNode(child)
		}
	case []any:
		for i, child := range value {
			value[i] = portableSchemaNode(child)
		}
	}
	return node
}
