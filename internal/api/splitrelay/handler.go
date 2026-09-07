// Package splitrelay routes Claude requests without an intermediate HTTP hop.
package splitrelay

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/google/uuid"
)

type Event struct {
	HeadersAfter       time.Duration
	FirstTextAfter     time.Duration
	FirstThinkingAfter time.Duration
	FirstToolAfter     time.Duration
	RequestID          string
	Route              string
	Model              string
	Phase              string
	Elapsed            time.Duration
	Status             int
}

type Options struct {
	OwnHandler       http.Handler
	OwnsModel        func(string) bool
	GatewayKey       func() string
	OfficialProxyURL string
	OfficialRelayURL string
	OfficialURL      string
	MaxRequestBytes  int64
	Observe          func(Event)
}

type Handler struct {
	options   Options
	official  *httputil.ReverseProxy
	transport *http.Transport
}

type buffers struct{ pool sync.Pool }

func (b *buffers) Get() []byte {
	if p := b.pool.Get(); p != nil {
		return p.([]byte)
	}
	return make([]byte, 32*1024)
}
func (b *buffers) Put(p []byte) { b.pool.Put(p) }

func New(options Options) (*Handler, error) {
	if options.OwnHandler == nil || options.OwnsModel == nil || options.GatewayKey == nil {
		return nil, fmt.Errorf("own handler, model registry and gateway key are required")
	}
	var target, proxy *url.URL
	var err error
	if options.OfficialRelayURL != "" {
		target, err = url.Parse(options.OfficialRelayURL)
		if err != nil || target.Scheme != "http" || net.ParseIP(target.Hostname()) == nil || !loopback(target.Hostname()) || target.Port() == "" || target.User != nil || target.RawQuery != "" || target.Fragment != "" || (target.Path != "" && target.Path != "/") {
			return nil, fmt.Errorf("official relay must be HTTP on a literal loopback address with an explicit port")
		}
	} else {
		proxy, err = url.Parse(options.OfficialProxyURL)
		if err != nil || proxy.Scheme != "http" || !loopback(proxy.Hostname()) || proxy.Port() == "" || proxy.User != nil || proxy.RawQuery != "" || (proxy.Path != "" && proxy.Path != "/") {
			return nil, fmt.Errorf("official proxy must be an HTTP loopback proxy with an explicit port")
		}
		if options.OfficialURL == "" {
			options.OfficialURL = "https://api.anthropic.com"
		}
		target, err = url.Parse(options.OfficialURL)
		if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil {
			return nil, fmt.Errorf("official target must be HTTPS")
		}
	}
	if options.MaxRequestBytes <= 0 {
		options.MaxRequestBytes = 64 << 20
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if proxy != nil {
		transport.Proxy = http.ProxyURL(proxy)
	}
	transport.MaxIdleConns = 1024
	transport.MaxIdleConnsPerHost = 1024
	transport.MaxConnsPerHost = 0
	transport.DisableCompression = true
	transport.ResponseHeaderTimeout = 0
	transport.TLSHandshakeTimeout = 0
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = transport
	reverse.FlushInterval = -1
	reverse.BufferPool = &buffers{}
	director := reverse.Director
	reverse.Director = func(r *http.Request) {
		director(r)
		r.Host = target.Host
		r.Header.Del("X-Forwarded-For")
		r.Header["X-Forwarded-For"] = nil
	}
	reverse.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		http.Error(w, "official identity proxy request failed", http.StatusBadGateway)
	}
	return &Handler{options: options, official: reverse, transport: transport}, nil
}
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || ip != nil && ip.IsLoopback()
}
func (h *Handler) CloseIdleConnections() { h.transport.CloseIdleConnections() }

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("request exceeds size limit")
	}
	return data, nil
}
func decoded(data []byte, encoding string, limit int64) ([]byte, error) {
	var reader io.Reader = bytes.NewReader(data)
	var closer io.ReadCloser
	var err error
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return data, nil
	case "gzip":
		closer, err = gzip.NewReader(reader)
	case "br":
		reader = brotli.NewReader(reader)
	case "deflate":
		closer, err = zlib.NewReader(reader)
		if err != nil {
			closer = flate.NewReader(bytes.NewReader(data))
			err = nil
		}
	default:
		return nil, fmt.Errorf("unsupported content encoding")
	}
	if err != nil {
		return nil, err
	}
	if closer != nil {
		defer closer.Close()
		reader = closer
	}
	return readBounded(reader, limit)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	event := Event{RequestID: uuid.NewString(), Phase: "ingress"}
	emit := func(phase string, status int) {
		elapsed := time.Since(started)
		switch phase {
		case "headers":
			event.HeadersAfter = elapsed
		case "first_text":
			event.FirstTextAfter = elapsed
		case "first_thinking":
			event.FirstThinkingAfter = elapsed
		case "first_tool":
			event.FirstToolAfter = elapsed
		}
		if h.options.Observe != nil {
			next := event
			next.Phase = phase
			next.Status = status
			next.Elapsed = time.Since(started)
			h.options.Observe(next)
		}
	}
	emit("ingress", 0)
	writer := &observedWriter{ResponseWriter: w, emit: emit}
	defer func() {
		phase := "complete"
		caught := recover()
		if caught != nil {
			phase = "error"
		}
		if r.Context().Err() != nil {
			phase = "cancel"
		}
		if phase != "cancel" && (writer.failed || writer.status >= 500) {
			phase = "error"
		}
		emit(phase, writer.status)
		if caught != nil {
			panic(caught)
		}
	}()
	bodyReader := io.Reader(http.NoBody)
	if r.Body != nil {
		bodyReader = r.Body
	}
	raw, err := readBounded(bodyReader, h.options.MaxRequestBytes)
	if err != nil {
		http.Error(writer, "request body unavailable or too large", http.StatusRequestEntityTooLarge)
		return
	}
	plain, err := decoded(raw, r.Header.Get("Content-Encoding"), h.options.MaxRequestBytes)
	if err != nil {
		http.Error(writer, "invalid encoded request body", http.StatusBadRequest)
		return
	}
	var parsed map[string]json.RawMessage
	if strings.Contains(r.Header.Get("Content-Type"), "json") {
		if json.Unmarshal(plain, &parsed) != nil {
			parsed = nil
		}
	}
	var model string
	_ = json.Unmarshal(parsed["model"], &model)
	event.Model = model
	event.Route = "official"
	own := r.Method == http.MethodPost && (r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/messages/count_tokens") && model != "" && !strings.HasPrefix(strings.ToLower(model), "claude-") && h.options.OwnsModel(strings.ToLower(model))
	cloned := r.Clone(r.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(raw))
	cloned.ContentLength = int64(len(raw))
	if own {
		event.Route = "own"
		key := h.options.GatewayKey()
		if key == "" {
			http.Error(writer, "own gateway credential unavailable", http.StatusServiceUnavailable)
			return
		}
		filtered := make(map[string]json.RawMessage)
		for k, v := range parsed {
			if ownFields[k] {
				filtered[k] = v
			}
		}
		if strings.HasPrefix(strings.ToLower(model), "gemini-") {
			repairTools(filtered)
		}
		body, err := json.Marshal(filtered)
		if err != nil {
			http.Error(writer, "invalid own request", http.StatusBadRequest)
			return
		}
		cloned.Header = make(http.Header)
		cloned.Header.Set("Authorization", "Bearer "+key)
		cloned.Header.Set("Content-Type", "application/json")
		version := r.Header.Get("Anthropic-Version")
		if version == "" {
			version = "2023-06-01"
		}
		cloned.Header.Set("Anthropic-Version", version)
		cloned.Body = io.NopCloser(bytes.NewReader(body))
		cloned.ContentLength = int64(len(body))
		cloned.TransferEncoding = nil
		emit("dispatch", 0)
		h.options.OwnHandler.ServeHTTP(writer, cloned)
	} else {
		emit("dispatch", 0)
		h.official.ServeHTTP(writer, cloned)
	}
}

var ownFields = map[string]bool{"model": true, "messages": true, "system": true, "max_tokens": true, "temperature": true, "top_p": true, "top_k": true, "stop_sequences": true, "stream": true, "tools": true, "tool_choice": true, "thinking": true, "output_config": true}

// Unwrap lets ResponseController reach the original server writer. Flush keeps
// Gin and streaming handlers compatible without buffering response bytes.
type observedWriter struct {
	http.ResponseWriter
	emit                             func(string, int)
	status                           int
	pending                          []byte
	overflow                         bool
	seenText, seenThinking, seenTool bool
	failed                           bool
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.emit("headers", status)
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *observedWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.failed = true
	}
	if strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
		w.observe(data[:n])
	}
	return n, err
}
func (w *observedWriter) observe(data []byte) {
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if len(w.pending)+len(data) <= 65536 && !w.overflow {
				w.pending = append(w.pending, data...)
			} else {
				w.pending = nil
				w.overflow = true
			}
			return
		}
		if !w.overflow && len(w.pending)+i <= 65536 {
			w.pending = append(w.pending, data[:i]...)
			w.line(w.pending)
		}
		w.pending = w.pending[:0]
		w.overflow = false
		data = data[i+1:]
	}
}
func (w *observedWriter) line(line []byte) {
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	var event struct {
		ContentBlock struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content_block"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	}
	if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
		return
	}
	if !w.seenTool && event.ContentBlock.Type == "tool_use" {
		w.seenTool = true
		w.emit("first_tool", w.status)
	}
	if !w.seenText && (event.ContentBlock.Text != "" || event.Delta.Text != "") {
		w.seenText = true
		w.emit("first_text", w.status)
	}
	if !w.seenThinking && (event.ContentBlock.Thinking != "" || event.Delta.Thinking != "") {
		w.seenThinking = true
		w.emit("first_thinking", w.status)
	}
}
