package main

import (
	"encoding/json"
	"fmt"
	claude "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/codex/claude"
	"testing"
	"time"
)

func TestBurstValidatesTranslatedPayload(t *testing.T) {
	opts := burstOptions{Mode: "burst", ContextBytes: 400000, Tools: 89, Count: 8, Concurrency: 4}
	if err := opts.validate(); err != nil {
		t.Fatal(err)
	}
	raw := claude.ConvertClaudeRequestToCodex("bench-fast", burstBody("42", opts), true)
	var translated map[string]any
	if err := json.Unmarshal(raw, &translated); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"none", "context", "tools", "schema", "stream", "duplicate"} {
		t.Run(mutation, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			probe := newBurstProbe(opts)
			probe.pending["42"] = &burstPending{started: time.Now(), content: burstContent("42", opts.ContextBytes)}
			switch mutation {
			case "context":
				body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] = "loadcheck:42\ntruncated"
			case "tools":
				body["tools"] = []any{}
			case "schema":
				body["tools"].([]any)[0].(map[string]any)["parameters"] = map[string]any{}
			case "stream":
				body["stream"] = false
			case "duplicate":
				probe.pending["42"].observed = true
			}
			err := probe.observe(body, time.Now())
			if (err == nil) != (mutation == "none") {
				t.Fatalf("unexpected validation result: %v", err)
			}
		})
	}
}
func TestBurstRejectsUnboundedWork(t *testing.T) {
	base := burstOptions{Mode: "burst", Count: 32, Concurrency: 8}
	for _, change := range []func(*burstOptions){func(o *burstOptions) { o.Concurrency = 65 }, func(o *burstOptions) { o.Count = 10001 }, func(o *burstOptions) { o.ContextBytes = 1 << 22 }, func(o *burstOptions) { o.Tools = 257 }, func(o *burstOptions) { o.ContextBytes = 1 << 20; o.Concurrency = 64 }, func(o *burstOptions) { o.Mode = "blocked"; o.Tools = 1 }} {
		o := base
		change(&o)
		if o.validate() == nil {
			t.Fatalf("unbounded or ignored options accepted: %+v", o)
		}
	}
}

func TestBurstSessionCacheValidation(t *testing.T) {
	for _, scenario := range []string{"stable", "missing", "changed", "shared", "raw-session"} {
		t.Run(scenario, func(t *testing.T) {
			opts := burstOptions{Mode: "burst", VerifySessionCache: true}
			probe := newBurstProbe(opts)
			for i := 0; i < 2; i++ {
				id := fmt.Sprint(i)
				client := "client-a"
				key := "anonymous-a"
				if i == 1 {
					switch scenario {
					case "missing":
						key = ""
					case "changed":
						key = "anonymous-b"
					case "shared":
						client = "client-b"
					case "raw-session":
						key = client
					}
				}
				var body map[string]any
				if err := json.Unmarshal(claude.ConvertClaudeRequestToCodex("bench-fast", burstBodyForClient(id, client, opts), true), &body); err != nil {
					t.Fatal(err)
				}
				body["prompt_cache_key"] = key
				probe.pending[id] = &burstPending{client: client, started: time.Now(), content: burstContent(id, 0)}
				err := probe.observe(body, time.Now())
				wantError := i == 1 && scenario != "stable"
				if (err != nil) != wantError {
					t.Fatalf("unexpected result: %v", err)
				}
			}
		})
	}
}
