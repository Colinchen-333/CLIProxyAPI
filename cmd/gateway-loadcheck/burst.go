package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type burstOptions struct {
	VerifySessionCache                      bool
	Mode                                    string
	ContextBytes, Tools, Count, Concurrency int
}

func (o burstOptions) validate() error {
	if o.Mode != "blocked" && o.Mode != "burst" {
		return fmt.Errorf("mode must be blocked or burst")
	}
	if o.ContextBytes < 0 || o.ContextBytes > 2<<20 || o.Tools < 0 || o.Tools > 256 {
		return fmt.Errorf("context-bytes must be 0..2097152 and tools 0..256")
	}
	if o.Mode == "blocked" && (o.ContextBytes != 0 || o.Tools != 0) {
		return fmt.Errorf("context-bytes and tools require burst mode")
	}
	if o.Concurrency < 1 || o.Concurrency > 64 || o.Count < 1 || o.Count > 10000 {
		return fmt.Errorf("burst-concurrency must be 1..64 and burst-requests 1..10000")
	}
	if o.ContextBytes*o.Concurrency > 32<<20 {
		return fmt.Errorf("burst live context budget exceeds 32 MiB; reduce concurrency")
	}
	return nil
}

type burstResult struct {
	CacheVerified int     `json:"verified_session_cache_keys"`
	Requests      int     `json:"requests"`
	Concurrency   int     `json:"concurrency"`
	ContextBytes  int     `json:"context_bytes"`
	Tools         int     `json:"tools"`
	Successes     int     `json:"successes"`
	Verified      int     `json:"verified_upstream_payloads"`
	Errors        int     `json:"errors"`
	FirstError    string  `json:"first_error,omitempty"`
	P50           float64 `json:"first_semantic_p50_ms"`
	P95           float64 `json:"first_semantic_p95_ms"`
	P99           float64 `json:"first_semantic_p99_ms"`
	ArrivalP50    float64 `json:"upstream_arrival_p50_ms"`
	ArrivalP95    float64 `json:"upstream_arrival_p95_ms"`
	ArrivalP99    float64 `json:"upstream_arrival_p99_ms"`
}
type burstPending struct {
	client   string
	started  time.Time
	content  string
	observed bool
}
type burstProbe struct {
	cacheKeys     map[string]string
	cacheVerified int
	opts          burstOptions
	mu            sync.Mutex
	pending       map[string]*burstPending
	arrivals      []float64
}

func newBurstProbe(o burstOptions) *burstProbe {
	return &burstProbe{opts: o, pending: make(map[string]*burstPending), cacheKeys: make(map[string]string)}
}
func burstContent(id string, size int) string {
	return "loadcheck:" + id + "\n" + strings.Repeat("x", size) + "\nReturn bench-ok"
}
func burstBody(id string, o burstOptions) []byte { return burstBodyForClient(id, "test-client", o) }
func burstBodyForClient(id, client string, o burstOptions) []byte {
	tools := make([]any, 0, o.Tools)
	for i := 0; i < o.Tools; i++ {
		tools = append(tools, map[string]any{"name": fmt.Sprintf("bench_tool_%03d", i), "description": "Synthetic verification tool", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "enum": []string{"kept"}}}, "required": []string{"value"}}})
	}
	identity, _ := json.Marshal(map[string]string{"account_uuid": "loadcheck-fake-account", "device_id": "loadcheck-fake-device", "session_id": client})
	body, _ := json.Marshal(map[string]any{"metadata": map[string]string{"user_id": string(identity)}, "model": "bench-fast", "max_tokens": 16, "stream": true, "messages": []any{map[string]any{"role": "user", "content": burstContent(id, o.ContextBytes)}}, "tools": tools})
	return body
}

// Inspect the actual Responses input, not the original Claude request.
func responseTexts(body map[string]any) []string {
	var texts []string
	input, _ := body["input"].([]any)
	for _, v := range input {
		item, _ := v.(map[string]any)
		if item["role"] != "user" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, c := range content {
			part, _ := c.(map[string]any)
			if part["type"] == "input_text" {
				if text, ok := part["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}
func verifyBurstTools(body map[string]any, count int) error {
	allTools, _ := body["tools"].([]any)
	tools := make([]any, 0, len(allTools))
	imageTools := 0
	for _, v := range allTools {
		tool, _ := v.(map[string]any)
		if tool["type"] == "image_generation" {
			imageTools++
			continue
		}
		tools = append(tools, v)
	}
	if imageTools > 1 {
		return fmt.Errorf("duplicate executor image_generation tool")
	}
	if len(tools) != count {
		return fmt.Errorf("upstream tools count: got %d want %d", len(tools), count)
	}
	for i, v := range tools {
		tool, _ := v.(map[string]any)
		if tool["type"] != "function" || tool["name"] != fmt.Sprintf("bench_tool_%03d", i) || tool["description"] != "Synthetic verification tool" {
			return fmt.Errorf("upstream tool %d identity changed", i)
		}
		params, _ := tool["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		value, _ := props["value"].(map[string]any)
		enum, _ := value["enum"].([]any)
		required, _ := params["required"].([]any)
		if params["type"] != "object" || value["type"] != "string" || len(enum) != 1 || enum[0] != "kept" || len(required) != 1 || required[0] != "value" {
			return fmt.Errorf("upstream tool %d schema changed", i)
		}
	}
	return nil
}
func (p *burstProbe) observe(body map[string]any, arrival time.Time) error {
	if body["stream"] != true {
		return fmt.Errorf("upstream stream flag lost")
	}
	if err := verifyBurstTools(body, p.opts.Tools); err != nil {
		return err
	}
	texts := responseTexts(body)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, text := range texts {
		if !strings.HasPrefix(text, "loadcheck:") {
			continue
		}
		id, _, ok := strings.Cut(strings.TrimPrefix(text, "loadcheck:"), "\n")
		if !ok {
			continue
		}
		request := p.pending[id]
		if request == nil || request.observed {
			return fmt.Errorf("unregistered or duplicate upstream request")
		}
		if text != request.content {
			return fmt.Errorf("upstream context changed")
		}
		if p.opts.VerifySessionCache {
			key, _ := body["prompt_cache_key"].(string)
			if key == "" || key == request.client {
				return fmt.Errorf("missing or non-anonymous upstream session cache key")
			}
			if previous, ok := p.cacheKeys[request.client]; ok && previous != key {
				return fmt.Errorf("upstream session cache key changed within client")
			}
			for client, previous := range p.cacheKeys {
				if client != request.client && previous == key {
					return fmt.Errorf("upstream session cache key shared across clients")
				}
			}
			p.cacheKeys[request.client] = key
			p.cacheVerified++
		}
		request.observed = true
		p.arrivals = append(p.arrivals, ms(arrival.Sub(request.started)))
		return nil
	}
	return fmt.Errorf("upstream user context missing")
}
func (p *burstProbe) run(ctx context.Context, client *http.Client, endpoint string) *burstResult {
	r := &burstResult{Requests: p.opts.Count, Concurrency: p.opts.Concurrency, ContextBytes: p.opts.ContextBytes, Tools: p.opts.Tools}
	type observation struct {
		latency time.Duration
		err     error
	}
	jobs := make(chan int)
	results := make(chan observation, p.opts.Concurrency)
	var wg sync.WaitGroup
	for worker := range p.opts.Concurrency {
		wg.Add(1)
		go func() {
			clientID := fmt.Sprintf("loadcheck-client-%d", worker)
			defer wg.Done()
			for i := range jobs {
				id := fmt.Sprint(i)
				body := burstBodyForClient(id, clientID, p.opts)
				p.mu.Lock()
				p.pending[id] = &burstPending{client: clientID, started: time.Now(), content: burstContent(id, p.opts.ContextBytes)}
				p.mu.Unlock()
				latency, err := firstTextBody(ctx, client, endpoint, body)
				p.mu.Lock()
				delete(p.pending, id)
				p.mu.Unlock()
				results <- observation{latency, err}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for i := 0; i < p.opts.Count; i++ {
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()
	var latencies []float64
	for got := range results {
		if got.err != nil {
			r.Errors++
			if r.FirstError == "" {
				r.FirstError = got.err.Error()
			}
		} else {
			latencies = append(latencies, ms(got.latency))
		}
	}
	r.Successes = len(latencies)
	if r.Successes+r.Errors < p.opts.Count {
		r.Errors += p.opts.Count - r.Successes - r.Errors
		if r.FirstError == "" {
			r.FirstError = "deadline prevented remaining requests"
		}
	}
	p.mu.Lock()
	arrivals := append([]float64(nil), p.arrivals...)
	p.mu.Unlock()
	r.Verified = len(arrivals)
	r.CacheVerified = p.cacheVerified
	sort.Float64s(latencies)
	sort.Float64s(arrivals)
	r.P50, r.P95, r.P99 = percentile(latencies, .5), percentile(latencies, .95), percentile(latencies, .99)
	r.ArrivalP50, r.ArrivalP95, r.ArrivalP99 = percentile(arrivals, .5), percentile(arrivals, .95), percentile(arrivals, .99)
	return r
}
