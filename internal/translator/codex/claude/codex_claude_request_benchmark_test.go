package claude

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func longClaudeRequest(messageCount, toolCount int) []byte {
	messages := make([]any, 0, messageCount)
	for i := 0; i < messageCount; i++ {
		role := "user"
		if i%2 != 0 {
			role = "assistant"
		}
		var content any = strings.Repeat("history ", 256)
		if i%4 == 1 {
			content = []any{map[string]any{"type": "text", "text": "before tool"}, map[string]any{"type": "tool_use", "id": fmt.Sprint("call_", i), "name": "read_file", "input": map[string]any{"path": "README.md"}}}
		}
		if i%4 == 2 {
			content = []any{map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprint("call_", i-1), "content": strings.Repeat("result ", 256)}}
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	tools := make([]any, 0, toolCount)
	for i := 0; i < toolCount; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprint("read_file_", i), "description": strings.Repeat("tool description ", 128), "input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}, "cache_control": map[string]any{"type": "ephemeral"}})
	}
	request, _ := json.Marshal(map[string]any{"model": "muse", "system": []any{map[string]any{"type": "text", "text": "Keep all historical context."}}, "messages": messages, "tools": tools, "tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": false}, "thinking": map[string]any{"type": "enabled", "budget_tokens": 4096}, "max_tokens": 1024})
	return request
}

// These digests were captured before aggregation to preserve exact wire bytes,
// including field order that can affect upstream prefix caching.
func TestLongClaudeRequestByteGolden(t *testing.T) {
	for _, spec := range []struct {
		messages, tools int
		digest          string
	}{
		{0, 0, "4c1987eeac6d881a52f4501caae21b3a5f7eabe7220f784e66238927eda2b682"}, {32, 0, "aeab9c29ab824d5db99414af2660a1ce697cc7247fc4f0446214dc521a1fe81c"}, {256, 89, "7192a7c8a83121dd4e4786dd1fd51276677d999f80554afc45c25d8c76889026"},
	} {
		t.Run(fmt.Sprintf("messages%d_tools%d", spec.messages, spec.tools), func(t *testing.T) {
			got := fmt.Sprintf("%x", sha256.Sum256(ConvertClaudeRequestToCodex("muse", longClaudeRequest(spec.messages, spec.tools), true)))
			if got != spec.digest {
				t.Fatalf("request wire bytes changed: sha256=%s want=%s", got, spec.digest)
			}
		})
	}
}

var benchmarkClaudeRequestOutput []byte

func BenchmarkLongClaudeRequest(b *testing.B) {
	for _, spec := range []struct{ messages, tools int }{{32, 0}, {128, 89}, {256, 89}} {
		b.Run(fmt.Sprintf("messages%d_tools%d", spec.messages, spec.tools), func(b *testing.B) {
			payload := longClaudeRequest(spec.messages, spec.tools)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				benchmarkClaudeRequestOutput = ConvertClaudeRequestToCodex("muse", payload, true)
			}
		})
	}
}
