package api

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestSplitListenAddressRequiresLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8318", "example.com:8318", ":8318", "localhost:8318"} {
		if _, err := splitListenAddress(addr); err == nil {
			t.Fatalf("accepted non-literal-loopback listen %q", addr)
		}
	}
	for _, addr := range []string{"", "127.0.0.1:0", "[::1]:8318"} {
		if _, err := splitListenAddress(addr); err != nil {
			t.Fatalf("rejected loopback %q: %v", addr, err)
		}
	}
}

func TestSplitListenerDispatchesInProcessAndReloadsKey(t *testing.T) {
	const model = "split-api-integration-model"
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("split-api-integration-client", "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { reg.UnregisterClient("split-api-integration-client") })
	engine := gin.New()
	seen := make(chan string, 2)
	engine.POST("/v1/messages", func(c *gin.Context) {
		seen <- c.GetHeader("Authorization")
		if c.GetHeader("Cookie") != "" || c.GetHeader("X-Api-Key") != "" {
			t.Error("Claude identity reached own handler")
		}
		c.Header("Content-Type", "text/event-stream")
		c.String(200, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	cfg := &config.Config{SplitRelay: &config.SplitRelayConfig{Listen: "127.0.0.1:0", OfficialProxyURL: "http://127.0.0.1:1"}}
	cfg.APIKeys = []string{"test-key-one"}
	s := &Server{engine: engine, cfg: cfg}
	s.updateSplitAPIKey(cfg)
	if _, err := s.startSplitRelay(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.closeSplitRelay)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, key := range []string{"test-key-one", "test-key-two"} {
		cfgNext := *cfg
		cfgNext.APIKeys = []string{key}
		s.updateSplitAPIKey(&cfgNext)
		req, err := http.NewRequest("POST", "http://"+s.split.Load().server.Addr+"/v1/messages", strings.NewReader(`{"model":"`+model+`","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer claude-identity")
		req.Header.Set("Cookie", "identity-cookie")
		req.Header.Set("X-Api-Key", "identity-key")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
		if got := <-seen; got != "Bearer "+key {
			t.Fatalf("gateway key did not reload: %q", got)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.split.Load().server.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSplitListenerDisabledAndMisconfiguration(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	if ch, err := s.startSplitRelay(); ch != nil || err != nil {
		t.Fatalf("disabled: %v %v", ch, err)
	}
	s.cfg.SplitRelay = &config.SplitRelayConfig{OfficialProxyURL: "http://127.0.0.1:1"}
	s.updateSplitAPIKey(s.cfg)
	if _, err := s.startSplitRelay(); err == nil {
		t.Fatal("accepted missing gateway key")
	}
	s.cfg.APIKeys = []string{"test"}
	s.updateSplitAPIKey(s.cfg)
	s.cfg.SplitRelay.OfficialProxyURL = "http://outside.invalid:7890"
	if _, err := s.startSplitRelay(); err == nil {
		t.Fatal("accepted external identity proxy")
	}
}
