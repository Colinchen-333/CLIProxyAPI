package helps

import (
	"context"
	"errors"
	tls "github.com/refraction-networking/utls"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Parallel()

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "chatgpt.com" {
			t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

type contextTestDialer func(context.Context, string, string) (net.Conn, error)

func (d contextTestDialer) Dial(network, addr string) (net.Conn, error) {
	return d(context.Background(), network, addr)
}
func (d contextTestDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d(ctx, network, addr)
}

func TestUtlsClientsReuseConnectionAndCancelOneStream(t *testing.T) {
	var connections atomic.Int32
	started := make(chan struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.StartTLS()
	defer server.Close()
	auth := &cliproxyauth.Auth{ID: t.Name()}
	first := NewUtlsHTTPClient(context.Background(), nil, auth, 0)
	second := NewUtlsHTTPClient(context.Background(), nil, auth, 0)
	rt := first.Transport.(*fallbackRoundTripper).utls.(*utlsRoundTripper)
	if second.Transport.(*fallbackRoundTripper).utls != rt {
		t.Fatal("independent clients did not share transport")
	}
	rt.tlsConfig = &tls.Config{InsecureSkipVerify: true}
	rt.dialer = contextTestDialer(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})
	defer rt.CloseIdleConnections()
	get := func(client *http.Client) {
		t.Helper()
		resp, err := client.Get("https://chatgpt.com/ok")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	get(first)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://chatgpt.com/cancel", nil)
	finished := make(chan error, 1)
	go func() { _, err := first.Do(req); finished <- err }()
	<-started
	get(second)
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	get(second)
	if n := connections.Load(); n != 1 {
		t.Fatalf("TCP connections = %d, want 1", n)
	}
	resp, err := second.Get("https://chatgpt.com:8443/ok")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if n := connections.Load(); n != 2 {
		t.Fatalf("different ports shared a TCP connection: %d", n)
	}
}

func TestUtlsConnectionWaitAndDialCancellation(t *testing.T) {
	rt := newUtlsRoundTripper("")
	started := make(chan struct{})
	rt.dialer = contextTestDialer(func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := rt.getOrCreateConnection(ctx, "example.com", "example.com:443"); first <- err }()
	<-started
	waiterCtx, waiterCancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() { _, err := rt.getOrCreateConnection(waiterCtx, "example.com", "example.com:443"); second <- err }()
	waiterCancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ignored cancellation")
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUtlsHandshakeCancellation(t *testing.T) {
	rt := newUtlsRoundTripper("")
	client, server := net.Pipe()
	defer server.Close()
	rt.dialer = contextTestDialer(func(context.Context, string, string) (net.Conn, error) { return client, nil })
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { _, err := rt.createConnection(ctx, "example.com", "example.com:443"); finished <- err }()
	buf := make([]byte, 4096)
	if _, err := server.Read(buf); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("handshake ignored cancellation")
	}
}
