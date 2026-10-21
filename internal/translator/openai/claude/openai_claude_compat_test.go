package claude

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertClaudeRequestToOpenAIWithCompatPreservesEmptySignatureThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":""}]}]}`)

	withoutCompat := ConvertClaudeRequestToOpenAI("deepseek-v4", payload, false)
	if gjson.GetBytes(withoutCompat, "messages.0.reasoning_content").Exists() {
		t.Fatalf("default translation preserved empty-signature reasoning: %s", withoutCompat)
	}

	withCompat := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	if gjson.GetBytes(withCompat, "messages.0.reasoning_content").String() != "reason" {
		t.Fatalf("compat translation missing reasoning_content: %s", withCompat)
	}
}

func TestConvertClaudeRequestToOpenAIWithCompatPreservesThinkingWithToolCalls(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":""},{"type":"text","text":"Reading files."},{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"main.go"}}]}]}`)

	result := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	assistant := gjson.GetBytes(result, "messages.0")
	if got := assistant.Get("reasoning_content").String(); got != "reason" {
		t.Fatalf("reasoning_content = %q, want %q; output: %s", got, "reason", result)
	}
	if !assistant.Get("tool_calls").Exists() {
		t.Fatalf("tool_calls missing from compatible translation: %s", result)
	}
}

// Measured 2026-10-20 through the split relay: an assistant tool-call turn with no
// thinking block was rejected 2 of 8 times by opencode-go DeepSeek backends with
// "The reasoning_content in the thinking mode must be passed back"; the same turn
// with a thinking block passed 8 of 8. The field must be present, empty when the
// turn produced no reasoning.
func TestConvertClaudeRequestToOpenAIWithCompatSendsEmptyReasoningForToolCallsWithoutThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]}]}`)

	result := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	assistant := gjson.GetBytes(result, "messages.0")
	reasoning := assistant.Get("reasoning_content")
	if !reasoning.Exists() || reasoning.String() != "" {
		t.Fatalf("compatible translation must pass back empty reasoning_content for a tool-call turn without thinking: %s", result)
	}
	if !assistant.Get("tool_calls").Exists() {
		t.Fatalf("tool_calls missing from compatible translation: %s", result)
	}
}

func TestConvertClaudeRequestToOpenAIWithCompatDoesNotAddReasoningToTextOnlyTurnWithoutThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"text","text":"Done."}]}]}`)

	result := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	if gjson.GetBytes(result, "messages.0.reasoning_content").Exists() {
		t.Fatalf("compatible translation added reasoning_content to a text-only turn: %s", result)
	}
}

func TestConvertClaudeRequestToOpenAIWithCompatPreservesIncompatibleThinking(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"claude#opaque"},{"type":"tool_use","id":"call_1","name":"Read","input":{}}]}]}`)

	result := ConvertClaudeRequestToOpenAIWithCompat("deepseek-v4", payload, false)
	assistant := gjson.GetBytes(result, "messages.0")
	if got := assistant.Get("reasoning_content").String(); got != "reason" {
		t.Fatalf("reasoning_content = %q, want %q; output: %s", got, "reason", result)
	}
	if !assistant.Get("tool_calls").Exists() {
		t.Fatalf("tool_calls missing from compatible translation: %s", result)
	}
}

func TestConvertClaudeRequestToOpenAIWithoutCompatDoesNotAddReasoningForToolCalls(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{}}]}]}`)

	result := ConvertClaudeRequestToOpenAI("deepseek-v4", payload, false)
	if gjson.GetBytes(result, "messages.0.reasoning_content").Exists() {
		t.Fatalf("default translation added reasoning_content: %s", result)
	}
}
