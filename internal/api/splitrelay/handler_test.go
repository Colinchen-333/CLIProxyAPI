package splitrelay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
)

func makeHandler(t *testing.T, own http.Handler, observe func(Event)) *Handler {
	t.Helper()
	h, err := New(Options{OwnHandler: own, OwnsModel: func(s string) bool { return s == "own" || s == "slow" || strings.HasPrefix(s, "gemini-") }, GatewayKey: func() string { return "gateway-test-key" }, OfficialProxyURL: "http://127.0.0.1:1", Observe: observe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.CloseIdleConnections)
	return h
}
func request(model string) *http.Request {
	r := httptest.NewRequest("POST", "http://127.0.0.1/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[],"stream":true}`, model)))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestThousandConcurrentOwnRequestsAndSlowIsolation(t *testing.T) {
	var arrived atomic.Int64
	release := make(chan struct{})
	h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { arrived.Add(1); <-release; io.WriteString(w, "ok") }), nil)
	var group sync.WaitGroup
	for i := 0; i < 1000; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request("own"))
			if w.Body.String() != "ok" {
				t.Error("bad response")
			}
		}()
	}
	deadline := time.After(5 * time.Second)
	for arrived.Load() != 1000 {
		select {
		case <-deadline:
			close(release)
			group.Wait()
			t.Fatalf("only %d requests entered own handler", arrived.Load())
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	group.Wait()
	slowEntered := make(chan struct{})
	slowRelease := make(chan struct{})
	defer close(slowRelease)
	h = makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if bytes.Contains(b, []byte(`"slow"`)) {
			close(slowEntered)
			<-slowRelease
		}
		io.WriteString(w, "ok")
	}), nil)
	go h.ServeHTTP(httptest.NewRecorder(), request("slow"))
	<-slowEntered
	fast := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), request("own")); close(fast) }()
	select {
	case <-fast:
	case <-time.After(time.Second):
		t.Fatal("slow model blocked fast model")
	}
}
func TestCancellationReachesOwn(t *testing.T) {
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(cancelled) }), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.ServeHTTP(httptest.NewRecorder(), request("own").WithContext(ctx))
	<-entered
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancellation not delivered")
	}
}
func TestIdentityAndSSE(t *testing.T) {
	payload := []byte("event: content_block_start\r\ndata: {\"content_block\":{\"type\":\"tool_use\"}}\r\n\r\ndata: {\"delta\":{\"type\":\"text_delta\",\"text\":\"你好\"}}\n\n")
	var events []Event
	h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gateway-test-key" || len(r.Header) != 3 {
			t.Errorf("unexpected headers: %v", r.Header)
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["metadata"] != nil || b["context_management"] != nil {
			t.Error("identity leaked")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(payload[:19])
		w.(http.Flusher).Flush()
		w.Write(payload[19:])
	}), func(e Event) { events = append(events, e) })
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"own","metadata":{"user_id":"private"},"context_management":{},"output_config":{"effort":"high"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "secret")
	r.Header.Set("Cookie", "secret")
	r.Header.Set("X-Api-Key", "secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !bytes.Equal(w.Body.Bytes(), payload) {
		t.Fatal("SSE bytes changed")
	}
	phases := map[string]bool{}
	for _, e := range events {
		phases[e.Phase] = true
		if e.RequestID != events[0].RequestID {
			t.Error("id changed")
		}
	}
	for _, p := range []string{"ingress", "dispatch", "headers", "first_tool", "first_text", "complete"} {
		if !phases[p] {
			t.Errorf("missing %s", p)
		}
	}
	serialized, _ := json.Marshal(events)
	if bytes.Contains(serialized, []byte("private")) || bytes.Contains(serialized, []byte("你好")) {
		t.Error("content in events")
	}
}
func TestCompressedRoutingAndGeminiSchema(t *testing.T) {
	for _, encoding := range []string{"gzip", "br"} {
		t.Run(encoding, func(t *testing.T) {
			plain := []byte(`{"model":"gemini-test","tools":[{"input_schema":{"type":"array","prefixItems":[{"type":"string"}]}}]}`)
			var encoded bytes.Buffer
			if encoding == "gzip" {
				z := gzip.NewWriter(&encoded)
				z.Write(plain)
				z.Close()
			} else {
				z := brotli.NewWriter(&encoded)
				z.Write(plain)
				z.Close()
			}
			called := false
			h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "prefixItems") || !strings.Contains(string(b), `"items":{"type":"string"}`) {
					t.Error(string(b))
				}
				if r.Header.Get("Content-Encoding") != "" {
					t.Error("stale encoding")
				}
			}), nil)
			r := httptest.NewRequest("POST", "/v1/messages", &encoded)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Content-Encoding", encoding)
			h.ServeHTTP(httptest.NewRecorder(), r)
			if !called {
				t.Fatal("not routed own")
			}
		})
	}
}
func TestOfficialRequiresCONNECTWithoutDirectFallback(t *testing.T) {
	var direct atomic.Int64
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	defer target.Close()
	var connects atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("not CONNECT")
		}
		connects.Add(1)
		http.Error(w, "denied", 502)
	}))
	defer proxy.Close()
	h, err := New(Options{OwnHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("official routed own") }), OwnsModel: func(string) bool { return true }, GatewayKey: func() string { return "key" }, OfficialURL: target.URL, OfficialProxyURL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer h.CloseIdleConnections()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("claude-collision"))
	if connects.Load() != 1 || direct.Load() != 0 || w.Code != 502 {
		t.Fatalf("connects=%d direct=%d status=%d", connects.Load(), direct.Load(), w.Code)
	}
	for _, bad := range []string{"", "http://example.com:8888", "https://127.0.0.1:8888", "http://127.0.0.1"} {
		o := h.options
		o.OfficialProxyURL = bad
		if _, err := New(o); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestOfficialCONNECTPreservesCompressedBodyAndHeaders(t *testing.T) {
	var compressed bytes.Buffer
	zipper := gzip.NewWriter(&compressed)
	zipper.Write([]byte(`{"model":"claude-test","metadata":{"user_id":"official-only"}}`))
	zipper.Close()
	raw := append([]byte(nil), compressed.Bytes()...)
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if !bytes.Equal(b, raw) || r.Header.Get("Content-Encoding") != "gzip" || r.Header.Get("Authorization") != "Bearer official-test" {
			t.Error("official request changed")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"delta\":{\"text\":\"ok\"}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer target.Close()
	var connects atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "CONNECT required", 405)
			return
		}
		connects.Add(1)
		remote, err := net.Dial("tcp", strings.TrimPrefix(target.URL, "https://"))
		if err != nil {
			t.Error(err)
			http.Error(w, "dial", 502)
			return
		}
		local, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			remote.Close()
			t.Error(err)
			return
		}
		io.WriteString(local, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() { defer local.Close(); defer remote.Close(); io.Copy(remote, local) }()
		go func() { defer local.Close(); defer remote.Close(); io.Copy(local, remote) }()
	}))
	defer proxy.Close()
	h, err := New(Options{OwnHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("wrong own route") }), OwnsModel: func(string) bool { return false }, GatewayKey: func() string { return "key" }, OfficialURL: target.URL, OfficialProxyURL: proxy.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer h.CloseIdleConnections()
	h.transport.TLSClientConfig = target.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Content-Encoding", "gzip")
		r.Header.Set("Authorization", "Bearer official-test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 || w.Body.String() != "data: {\"delta\":{\"text\":\"ok\"}}\n\n" {
			t.Errorf("bad response %d %s", w.Code, w.Body.String())
		}
	}
	if connects.Load() != 1 {
		t.Errorf("tunnel not reused: %d", connects.Load())
	}
}

func TestManagementPathsCannotReachOwnHandler(t *testing.T) {
	h := makeHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("management reached own handler") }), nil)
	r := request("own")
	r.URL.Path = "/v0/management/config"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 502 {
		t.Errorf("unexpected status %d", w.Code)
	}
}
