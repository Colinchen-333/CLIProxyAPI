package proxyutil

import (
	"bufio"
	"context"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestConnectDialerCancellationClosesPendingTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	for _, scheme := range []string{"http", "https", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			dialer, _, err := BuildDialer(scheme + "://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", "example.com:443")
				if conn != nil {
					_ = conn.Close()
				}
				finished <- err
			}()
			conn, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			buf := make([]byte, 4096)
			if _, err := conn.Read(buf); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-finished:
				if err == nil {
					t.Fatal("cancel succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("dial ignored cancellation")
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := conn.Read(buf); err == nil {
				t.Fatal("cancel did not close tunnel")
			} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
				t.Fatal("tunnel stayed open")
			}
		})
	}
}

func TestConnectCancellationDoesNotCloseEstablishedTunnel(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer conn.Close()
		_, err = http.ReadRequest(bufio.NewReader(conn))
		if err == nil {
			_, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		}
		if err == nil {
			buf := make([]byte, 4)
			_, err = io.ReadFull(conn, buf)
		}
		if err == nil {
			_, err = io.WriteString(conn, "pong")
		}
		serverDone <- err
	}()
	dialer, _, err := BuildDialer("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := dialer.(proxy.ContextDialer).DialContext(ctx, "tcp", "example.com:443")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "pong" {
		t.Fatalf("response = %q", buf)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
