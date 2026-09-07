package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"time"
)

// Only an address freshly allocated for this harness's child process is passed in.
func startCPUProfile(ctx context.Context, addr, path string) (func() error, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return nil, fmt.Errorf("profile address must be isolated IPv4 loopback")
	}
	if err := waitFor(ctx, 5*time.Second, func() bool {
		c, e := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if e == nil {
			_ = c.Close()
			return true
		}
		return false
	}); err != nil {
		return nil, fmt.Errorf("profile listener: %w", err)
	}
	// A fresh transport disables environment proxies and cannot reach a live endpoint.
	transport := &http.Transport{Proxy: nil}
	client := &http.Client{Transport: transport}
	profileCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	started := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	go func() {
		defer cancel()
		defer transport.CloseIdleConnections()
		defer once.Do(func() { close(started) })
		req, err := http.NewRequestWithContext(profileCtx, "GET", "http://"+addr+"/debug/pprof/profile?seconds=5", nil)
		if err != nil {
			done <- err
			return
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { once.Do(func() { close(started) }) }}))
		resp, err := client.Do(req)
		if err != nil {
			done <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			done <- fmt.Errorf("profile HTTP status %d", resp.StatusCode)
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
		if err != nil {
			done <- err
			return
		}
		if len(data) < 2 || len(data) > 16<<20 || data[0] != 0x1f || data[1] != 0x8b {
			done <- fmt.Errorf("invalid or oversized CPU profile")
			return
		}
		done <- os.WriteFile(path, data, 0600)
	}()
	<-started
	// Callers defer this after gateway cleanup was registered, so the download
	// finishes before the isolated gateway is interrupted and its directory removed.
	return func() error {
		if err := <-done; err != nil {
			return fmt.Errorf("CPU profile: %w", err)
		}
		return nil
	}, nil
}
