package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// A configured openai-compatibility model with is-compat sends reasoning_content
// for a signatureless thinking block that sits next to tool_use, and still
// emits the tool call.
func TestOpenAICompatExecutorUsesCompatibleClaudeTranslation(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	payload := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":""},{"type":"tool_use","id":"call_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`)

	newExecutor := func(isCompat bool) *OpenAICompatExecutor {
		cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
			Name:    "opencode-go-ds",
			BaseURL: server.URL,
			Models: []config.OpenAICompatibilityModel{{
				Name:     "deepseek-v4-flash",
				Alias:    "deepseek-v4.1-flash",
				IsCompat: isCompat,
			}},
		}}}
		return NewOpenAICompatExecutor("openai-compatibility", cfg)
	}
	auth := &cliproxyauth.Auth{
		Provider: "opencode-go-ds",
		Attributes: map[string]string{
			"base_url":    server.URL,
			"api_key":     "test-key",
			"compat_name": "opencode-go-ds",
		},
	}
	request := cliproxyexecutor.Request{
		Model:   "deepseek-v4-flash",
		Payload: payload,
	}
	options := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatOpenAI,
	}

	for _, tc := range []struct {
		name     string
		isCompat bool
		want     string
	}{
		{name: "is-compat true sends reasoning_content", isCompat: true, want: "prior reasoning"},
		{name: "is-compat false sends nothing", isCompat: false, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamBody = nil
			if _, errExecute := newExecutor(tc.isCompat).Execute(context.Background(), auth, request, options); errExecute != nil {
				t.Fatalf("Execute error: %v", errExecute)
			}
			assistant := gjson.GetBytes(upstreamBody, "messages.0")
			if got := assistant.Get("reasoning_content").String(); got != tc.want {
				t.Fatalf("reasoning_content = %q, want %q; body=%s", got, tc.want, upstreamBody)
			}
			if !assistant.Get("tool_calls").Exists() {
				t.Fatalf("tool_calls missing from upstream request: %s", upstreamBody)
			}
		})
	}
}

// When entries share an alias to form an upstream pool, the flag must come from
// the entry whose upstream name this attempt dispatches to, not from a sibling
// entry earlier in the list.
func TestOpenAICompatExecutorScopesIsCompatPerUpstreamEntry(t *testing.T) {
	var upstreamBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	cfg := &config.Config{OpenAICompatibility: []config.OpenAICompatibility{{
		Name:    "commandcode",
		BaseURL: server.URL,
		Models: []config.OpenAICompatibilityModel{
			{Name: "deepseek/deepseek-v4.1-flash", Alias: "deepseek-v4.1-flash", IsCompat: true},
			{Name: "vendor/native-only", Alias: "deepseek-v4.1-flash"},
		},
	}}}
	executor := NewOpenAICompatExecutor("openai-compatibility", cfg)
	auth := &cliproxyauth.Auth{
		Provider:   "commandcode",
		Attributes: map[string]string{"base_url": server.URL, "api_key": "test-key", "compat_name": "commandcode"},
	}
	payload := []byte(`{"model":"x","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"prior reasoning","signature":""},{"type":"tool_use","id":"call_1","name":"Read","input":{}}]}]}`)
	options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatOpenAI}

	// Dispatched by upstream name to the non-compat entry of the shared alias pool.
	if _, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "vendor/native-only", Payload: payload}, options); errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.reasoning_content").String(); got != "" {
		t.Fatalf("reasoning_content = %q, want empty for the non-compat pool entry; body=%s", got, upstreamBody)
	}

	// Dispatched by upstream name to the compat entry of the same pool.
	if _, errExecute := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "deepseek/deepseek-v4.1-flash", Payload: payload}, options); errExecute != nil {
		t.Fatalf("Execute error: %v", errExecute)
	}
	if got := gjson.GetBytes(upstreamBody, "messages.0.reasoning_content").String(); got != "prior reasoning" {
		t.Fatalf("reasoning_content = %q, want %q; body=%s", got, "prior reasoning", upstreamBody)
	}
}
