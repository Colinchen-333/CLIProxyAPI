package helps

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}

func TestProxyClientsReuseConnectionsAndPartitionRouteAndIdentity(t *testing.T) {
	var connections atomic.Int32
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	proxy.Start()
	defer proxy.Close()
	auth := &cliproxyauth.Auth{ID: t.Name(), ProxyURL: proxy.URL}
	for range 2 {
		client := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0)
		resp, err := client.Get("http://upstream.invalid/test")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if n := connections.Load(); n != 1 {
		t.Fatalf("TCP connections = %d, want 1", n)
	}
	one := NewProxyAwareHTTPClient(context.Background(), nil, auth, 0).Transport
	otherAuth := &cliproxyauth.Auth{ID: t.Name() + "other", ProxyURL: proxy.URL}
	if NewProxyAwareHTTPClient(context.Background(), nil, otherAuth, 0).Transport == one {
		t.Fatal("shared across identities")
	}
	otherRoute := &cliproxyauth.Auth{ID: auth.ID, ProxyURL: "direct"}
	if NewProxyAwareHTTPClient(context.Background(), nil, otherRoute, 0).Transport == one {
		t.Fatal("shared across routes")
	}
}

func TestTransportCacheIsBounded(t *testing.T) {
	for i := 0; i < 140; i++ {
		sharedProxyTransport("direct", &cliproxyauth.Auth{ID: fmt.Sprintf("%s-%d", t.Name(), i)})
	}
	transportCache.Lock()
	defer transportCache.Unlock()
	if len(transportCache.entries) > 128 {
		t.Fatalf("cache entries = %d", len(transportCache.entries))
	}
}

func TestSharedTransportPreservesContextAndProxyPriority(t *testing.T) {
	injected := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", injected)
	if got := NewProxyAwareHTTPClient(ctx, nil, nil, 0).Transport; got != injected {
		t.Fatal("context transport lost")
	}
	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global.invalid:8080"}}
	auth := &cliproxyauth.Auth{ID: t.Name(), ProxyURL: "direct"}
	for _, client := range []*http.Client{NewProxyAwareHTTPClient(ctx, cfg, auth, 0), NewUtlsHTTPClient(ctx, cfg, auth, 0)} {
		rt := client.Transport
		if fallback, ok := rt.(*fallbackRoundTripper); ok {
			rt = fallback.fallback
			if fallback.utls == injected {
				t.Fatal("context bypassed explicit route")
			}
		}
		transport, ok := rt.(*http.Transport)
		if !ok || transport == injected || transport.Proxy != nil {
			t.Fatal("auth direct route lost priority")
		}
	}
	for _, client := range []*http.Client{NewProxyAwareHTTPClient(ctx, cfg, nil, 0), NewUtlsHTTPClient(ctx, cfg, nil, 0)} {
		rt := client.Transport
		if fallback, ok := rt.(*fallbackRoundTripper); ok {
			rt = fallback.fallback
		}
		transport := rt.(*http.Transport)
		req, _ := http.NewRequest("GET", "https://example.com", nil)
		route, err := transport.Proxy(req)
		if err != nil || route.Host != "global.invalid:8080" {
			t.Fatal("global route lost priority")
		}
	}
}
