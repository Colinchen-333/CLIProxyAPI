package executor

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAntigravityIndependentClientsReuseProxyHTTP11Pool(t *testing.T) {
	var connections atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("protocol = %s", r.Proto)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	defer server.Close()
	auth := &cliproxyauth.Auth{ID: t.Name(), ProxyURL: server.URL}
	one := newAntigravityHTTPClient(context.Background(), nil, auth, 0)
	two := newAntigravityHTTPClient(context.Background(), nil, auth, 0)
	if one.Transport != two.Transport {
		t.Fatal("HTTP/1 clone is not reused")
	}
	transport := one.Transport.(*http.Transport)
	if transport.MaxIdleConns != 1024 || transport.MaxIdleConnsPerHost != 1024 {
		t.Fatal("HTTP/1 burst idle capacity missing")
	}
	if transport.ForceAttemptHTTP2 || len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" {
		t.Fatal("HTTP/1 policy changed")
	}
	for _, client := range []*http.Client{one, two} {
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
	other := newAntigravityHTTPClient(context.Background(), nil, &cliproxyauth.Auth{ID: "other", ProxyURL: server.URL}, 0)
	if other.Transport == one.Transport {
		t.Fatal("HTTP/1 pool shared across identities")
	}
}
