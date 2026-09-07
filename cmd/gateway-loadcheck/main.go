// gateway-loadcheck exercises a real gateway process using only synthetic credentials
// and a local Responses provider. It never contacts an actual model provider.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	Mode              string       `json:"mode"`
	Burst             *burstResult `json:"burst,omitempty"`
	BinarySHA256      string       `json:"gateway_binary_sha256"`
	Success           bool         `json:"success"`
	Requested         int          `json:"requested_slow_concurrency"`
	Arrived           int64        `json:"arrived_slow_requests"`
	Peak              int64        `json:"peak_slow_active"`
	ActiveDuringFast  int64        `json:"slow_active_during_fast"`
	ActiveAfterCancel int64        `json:"slow_active_after_cancel"`
	FastCount         int          `json:"fast_successes"`
	P50               float64      `json:"first_real_text_p50_ms"`
	P95               float64      `json:"first_real_text_p95_ms"`
	P99               float64      `json:"first_real_text_p99_ms"`
	RecoveryMS        float64      `json:"recovery_first_real_text_ms"`
	Errors            int          `json:"errors"`
	SlowErrors        int          `json:"slow_request_errors"`
	FirstSlowError    string       `json:"first_slow_request_error,omitempty"`
	Failure           string       `json:"failure,omitempty"`
	ElapsedMS         float64      `json:"elapsed_ms"`
}

func main() {
	binary := flag.String("gateway-binary", "", "path to the actual compiled gateway server")
	concurrency := flag.Int("concurrency", 1000, "number of simultaneous blocked slow model requests")
	timeout := flag.Duration("timeout", 90*time.Second, "overall acceptance deadline")
	opts := burstOptions{}
	flag.StringVar(&opts.Mode, "mode", "blocked", "blocked or burst")
	flag.IntVar(&opts.ContextBytes, "context-bytes", 0, "synthetic context bytes per burst request (max 2097152)")
	flag.IntVar(&opts.Tools, "tools", 0, "synthetic tool definitions per burst request (max 256)")
	flag.IntVar(&opts.Count, "burst-requests", 32, "total burst requests")
	flag.IntVar(&opts.Concurrency, "burst-concurrency", 8, "maximum simultaneous burst requests (max 64)")
	flag.BoolVar(&opts.VerifySessionCache, "verify-session-cache", false, "require stable Responses prompt_cache_key per synthetic client")
	flag.Parse()
	started := time.Now()
	r := result{Requested: *concurrency, Mode: opts.Mode}
	if *binary == "" || *concurrency < 1 {
		r.Failure = "gateway-binary and positive concurrency are required"
	} else if err := opts.validate(); err != nil {
		r.Failure = err.Error()
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		err := run(ctx, *binary, *concurrency, opts, &r)
		cancel()
		if err != nil {
			r.Failure = err.Error()
		}
	}
	if r.Failure != "" {
		r.Errors++
	}
	r.Success = r.Errors == 0 && r.FastCount == 32 && r.ActiveAfterCancel == 0
	if opts.Mode == "burst" {
		r.Success = r.Errors == 0 && r.Burst != nil && r.Burst.Successes == opts.Count && r.Burst.Verified == opts.Count
	}
	r.ElapsedMS = ms(time.Since(started))
	_ = json.NewEncoder(os.Stdout).Encode(r)
	if !r.Success {
		os.Exit(1)
	}
}

func run(ctx context.Context, binary string, concurrency int, opts burstOptions, out *result) error {
	binary, err := filepath.Abs(binary)
	if err != nil {
		return err
	}
	binaryFile, err := os.Open(binary)
	if err != nil {
		return err
	}
	digest := sha256.New()
	_, hashErr := io.Copy(digest, binaryFile)
	_ = binaryFile.Close()
	if hashErr != nil {
		return hashErr
	}
	out.BinarySHA256 = fmt.Sprintf("%x", digest.Sum(nil))
	temp, err := os.MkdirTemp("", "gateway-loadcheck-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	if err := os.Mkdir(filepath.Join(temp, "auths"), 0700); err != nil {
		return err
	}
	probe := newBurstProbe(opts)
	var active, arrived, peak atomic.Int64
	defer func() { out.Arrived = arrived.Load(); out.Peak = peak.Load(); out.ActiveAfterCancel = active.Load() }()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected provider path", 404)
			return
		}
		var body map[string]any
		arrival := time.Now()
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if body["model"] == "bench-slow" {
			now := active.Add(1)
			arrived.Add(1)
			for previous := peak.Load(); now > previous; previous = peak.Load() {
				if peak.CompareAndSwap(previous, now) {
					break
				}
			}
			defer active.Add(-1)
			<-req.Context().Done()
			return
		}
		if body["model"] != "bench-fast" {
			http.Error(w, "unexpected model", 400)
			return
		}
		if opts.Mode == "burst" {
			if err := probe.observe(body, arrival); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.created","response":{"id":"resp_bench","model":"bench-fast"}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_bench","type":"message","role":"assistant","content":[]}}`,
			`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`,
			`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"bench-ok"}`,
			`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"bench-ok"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_bench","type":"message","role":"assistant","content":[{"type":"output_text","text":"bench-ok"}]}}`,
			`{"type":"response.completed","response":{"id":"resp_bench","model":"bench-fast","status":"completed","output":[{"id":"msg_bench","type":"message","role":"assistant","content":[{"type":"output_text","text":"bench-ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		}
		for _, event := range events {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	apiAddr, err := freeAddress()
	if err != nil {
		return err
	}
	splitAddr, err := freeAddress()
	if err != nil {
		return err
	}
	_, apiPort, _ := net.SplitHostPort(apiAddr)
	config := fmt.Sprintf(`host: 127.0.0.1
port: %s
auth-dir: %q
api-keys: ["loadcheck-fake-gateway-key"]
remote-management:
  disable-control-panel: true
  disable-auto-update-panel: true
logging-to-file: false
usage-statistics-enabled: false
request-retry: 0
split-relay:
  listen: %q
  official-proxy-url: "http://127.0.0.1:1"
codex-api-key:
  - api-key: "loadcheck-fake-provider-key"
    base-url: %q
    proxy-url: direct
    is-compat: true
    models:
      - name: bench-fast
      - name: bench-slow
`, apiPort, filepath.Join(temp, "auths"), splitAddr, upstream.URL+"/v1")
	configPath := filepath.Join(temp, "config.yaml")
	if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
		return err
	}
	logFile, err := os.Create(filepath.Join(temp, "gateway.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(binary, "--config", configPath, "--local-model")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + temp, "TMPDIR=" + temp}
	cmd.Dir = temp
	cmd.Stdout, cmd.Stderr = logFile, logFile
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		interruptProcess(cmd)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			killProcess(cmd)
			<-done
		}
	}()
	transport := &http.Transport{MaxIdleConns: 4096, MaxIdleConnsPerHost: 4096}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	endpoint := "http://" + splitAddr + "/v1/messages"
	if err := waitFor(ctx, 15*time.Second, func() bool {
		conn, err := net.DialTimeout("tcp", splitAddr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return true
		}
		return false
	}); err != nil {
		logBytes, _ := os.ReadFile(logFile.Name())
		if len(logBytes) > 4000 {
			logBytes = logBytes[len(logBytes)-4000:]
		}
		return fmt.Errorf("gateway startup: %w; %s", err, logBytes)
	}
	if opts.Mode == "burst" {
		out.Burst = probe.run(ctx, client, endpoint)
		out.Errors += out.Burst.Errors
		return nil
	}
	// A real translated warmup validates model registration before measuring load.
	if _, err := firstText(ctx, client, endpoint, "bench-fast"); err != nil {
		return fmt.Errorf("warmup: %w", err)
	}
	slowCtx, cancelSlow := context.WithCancel(ctx)
	defer cancelSlow()
	var slowWG sync.WaitGroup
	var slowErrors atomic.Int64
	var firstSlowError string
	var firstSlowErrorOnce sync.Once
	for range concurrency {
		slowWG.Add(1)
		go func() {
			defer slowWG.Done()
			_, err := firstText(slowCtx, client, endpoint, "bench-slow")
			if slowCtx.Err() == nil && err != nil {
				slowErrors.Add(1)
				firstSlowErrorOnce.Do(func() { firstSlowError = err.Error() })
			}
		}()
	}
	defer func() {
		cancelSlow()
		slowWG.Wait()
		out.SlowErrors = int(slowErrors.Load())
		out.Errors += out.SlowErrors
		out.FirstSlowError = firstSlowError
	}()
	if err := waitFor(ctx, 45*time.Second, func() bool { return active.Load() == int64(concurrency) }); err != nil {
		return fmt.Errorf("slow barrier: active=%d arrived=%d errors=%d: %w", active.Load(), arrived.Load(), slowErrors.Load(), err)
	}
	type observation struct {
		latency time.Duration
		err     error
	}
	observations := make(chan observation, 32)
	fastCtx, cancelFast := context.WithTimeout(ctx, 10*time.Second)
	defer cancelFast()
	for range 32 {
		go func() {
			latency, err := firstText(fastCtx, client, endpoint, "bench-fast")
			observations <- observation{latency, err}
		}()
	}
	var latencies []float64
	for range 32 {
		got := <-observations
		if got.err != nil {
			out.Errors++
		} else {
			latencies = append(latencies, ms(got.latency))
		}
	}
	out.FastCount = len(latencies)
	out.ActiveDuringFast = active.Load()
	sort.Float64s(latencies)
	out.P50, out.P95, out.P99 = percentile(latencies, .50), percentile(latencies, .95), percentile(latencies, .99)
	if out.ActiveDuringFast != int64(concurrency) {
		return fmt.Errorf("slow requests escaped barrier during fast model measurement")
	}
	cancelSlow()
	slowWG.Wait()
	if err := waitFor(ctx, 10*time.Second, func() bool { return active.Load() == 0 }); err != nil {
		return fmt.Errorf("canceled upstream requests stayed active: %d", active.Load())
	}
	latency, err := firstText(ctx, client, endpoint, "bench-fast")
	if err != nil {
		return fmt.Errorf("post-cancel recovery: %w", err)
	}
	out.RecoveryMS = ms(latency)
	return nil
}

func firstText(ctx context.Context, client *http.Client, endpoint, model string) (time.Duration, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 16, "stream": true, "messages": []map[string]string{{"role": "user", "content": "Return bench-ok"}}})
	return firstTextBody(ctx, client, endpoint, body)
}

func firstTextBody(ctx context.Context, client *http.Client, endpoint string, body []byte) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "loadcheck-fake-gateway-key")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	scanner := bufio.NewScanner(resp.Body)
	var first time.Duration
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if first == 0 && event.Type == "content_block_delta" && event.Delta.Type == "text_delta" && event.Delta.Text == "bench-ok" {
			first = time.Since(started)
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, err
	}
	if first == 0 {
		return 0, errors.New("no real translated text delta received")
	}
	return first, nil
}
func freeAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := listener.Addr().String()
	return addr, listener.Close()
}
func waitFor(ctx context.Context, limit time.Duration, ready func() bool) error {
	bound, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return nil
		}
		select {
		case <-bound.Done():
			return bound.Err()
		case <-ticker.C:
		}
	}
}
func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1)*fraction + .5)
	return sorted[index]
}
func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
