package helps

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// utlsRoundTripper implements http.RoundTripper using utls with Chrome fingerprint
// to bypass Cloudflare's TLS fingerprinting on Anthropic domains.
type utlsRoundTripper struct {
	mu          sync.Mutex
	connections map[string]*http2.ClientConn
	pending     map[string]chan struct{}
	dialer      proxy.Dialer
	tlsConfig   *tls.Config
}

func newUtlsRoundTripper(proxyURL string) *utlsRoundTripper {
	var dialer proxy.Dialer = proxy.Direct
	if proxyURL != "" {
		proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
		if errBuild != nil {
			log.Errorf("utls: failed to configure proxy dialer for %q: %v", proxyutil.Redact(proxyURL), errBuild)
		} else if mode != proxyutil.ModeInherit && proxyDialer != nil {
			dialer = proxyDialer
		}
	}
	return &utlsRoundTripper{
		connections: make(map[string]*http2.ClientConn),
		pending:     make(map[string]chan struct{}),
		dialer:      dialer,
	}
}

func (t *utlsRoundTripper) getOrCreateConnection(ctx context.Context, host, addr string) (*http2.ClientConn, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		t.mu.Lock()
		if c := t.connections[addr]; c != nil && c.ReserveNewRequest() {
			t.mu.Unlock()
			return c, nil
		}
		if done := t.pending[addr]; done != nil {
			t.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		t.pending[addr] = done
		t.mu.Unlock()
		c, err := t.createConnection(ctx, host, addr)
		t.mu.Lock()
		delete(t.pending, addr)
		if err == nil {
			// Keep authority entries bounded; idle connections close automatically.
			for key, old := range t.connections {
				if old.State().Closed {
					delete(t.connections, key)
				}
			}
			if len(t.connections) < 128 {
				t.connections[addr] = c
			}
			c.ReserveNewRequest()
		}
		close(done)
		t.mu.Unlock()
		return c, err
	}
}

// CloseIdleConnections preserves streams already using a shared connection.
func (t *utlsRoundTripper) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for addr, c := range t.connections {
		state := c.State()
		if state.StreamsActive == 0 && state.StreamsPending == 0 && state.StreamsReserved == 0 {
			_ = c.Close()
			delete(t.connections, addr)
		}
	}
}

func (t *utlsRoundTripper) createConnection(ctx context.Context, host, addr string) (result *http2.ClientConn, err error) {
	conn, err := t.dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// Include the initial HTTP/2 preface write in establishment cancellation.
	// Once returned, stream cancellation must never close the shared socket.
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); close(cancelled) })
	defer func() {
		if !stop() {
			<-cancelled
			result = nil
			err = ctx.Err()
		}
	}()

	tlsConfig := &tls.Config{ServerName: host}
	if t.tlsConfig != nil {
		tlsConfig = t.tlsConfig.Clone()
		tlsConfig.ServerName = host
	}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloChrome_Auto)

	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}

	tr := &http2.Transport{IdleConnTimeout: 90 * time.Second}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, err
	}

	return h2Conn, nil
}

func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	hostname := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		port = "443"
	}
	addr := net.JoinHostPort(hostname, port)

	h2Conn, err := t.getOrCreateConnection(req.Context(), hostname, addr)
	if err != nil {
		return nil, err
	}

	resp, err := h2Conn.RoundTrip(req)
	if err != nil && !h2Conn.CanTakeNewRequest() {
		t.mu.Lock()
		if cached, ok := t.connections[addr]; ok && cached == h2Conn {
			delete(t.connections, addr)
		}
		t.mu.Unlock()
		return nil, err
	}

	return resp, err
}

// utlsProtectedHosts contains the hosts that should use utls Chrome TLS fingerprint
// to bypass Cloudflare's TLS fingerprinting.
var utlsProtectedHosts = map[string]struct{}{
	"api.anthropic.com": {},
	"chatgpt.com":       {},
}

// fallbackRoundTripper uses utls for protected HTTPS hosts and falls back to
// standard transport for all other requests.
type fallbackRoundTripper struct {
	utls     http.RoundTripper
	fallback http.RoundTripper
}

func (f *fallbackRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme == "https" {
		if _, ok := utlsProtectedHosts[strings.ToLower(req.URL.Hostname())]; ok {
			return f.utls.RoundTrip(req)
		}
	}
	return f.fallback.RoundTrip(req)
}

// NewUtlsHTTPClient creates an HTTP client using utls Chrome TLS fingerprint.
// Use this for provider requests that need a Chrome-like TLS fingerprint.
// Falls back to standard transport for non-HTTPS requests.
func NewUtlsHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var ctxRoundTripper http.RoundTripper
	if ctx != nil {
		ctxRoundTripper, _ = ctx.Value("cliproxy.roundtripper").(http.RoundTripper)
	}

	var utlsRT http.RoundTripper = sharedTransport(transportKey("utls", proxyURL, auth, nil), func() http.RoundTripper { return newUtlsRoundTripper(proxyURL) })
	var standardTransport http.RoundTripper = http.DefaultTransport
	if proxyURL != "" {
		if transport := sharedProxyTransport(proxyURL, auth); transport != nil {
			standardTransport = transport
		}
	} else if ctxRoundTripper != nil {
		utlsRT = ctxRoundTripper
		standardTransport = ctxRoundTripper
	}

	client := &http.Client{
		Transport: &fallbackRoundTripper{
			utls:     utlsRT,
			fallback: standardTransport,
		},
	}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}
