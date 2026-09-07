package api

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api/splitrelay"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

type splitRuntime struct {
	server  *http.Server
	handler *splitrelay.Handler
}

func (s *Server) updateSplitAPIKey(cfg *config.Config) {
	key := ""
	if cfg != nil && len(cfg.APIKeys) != 0 {
		key = cfg.APIKeys[0]
	}
	s.splitAPIKey.Store(key)
}

func splitListenAddress(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		raw = "127.0.0.1:8318"
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", errors.New("split-relay.listen must be a loopback host:port")
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("split-relay.listen must use a literal loopback address")
	}
	return net.JoinHostPort(host, port), nil
}

func (s *Server) startSplitRelay() (<-chan error, error) {
	if s.cfg == nil || s.cfg.SplitRelay == nil {
		return nil, nil
	}
	cfg := *s.cfg.SplitRelay
	addr, err := splitListenAddress(cfg.Listen)
	if err != nil {
		return nil, err
	}
	key, _ := s.splitAPIKey.Load().(string)
	if key == "" {
		return nil, errors.New("split-relay requires a configured own-gateway API key")
	}
	handler, err := splitrelay.New(splitrelay.Options{
		OwnHandler: s.engine,
		OwnsModel: func(model string) bool {
			return len(registry.GetGlobalRegistry().GetModelProviders(model)) > 0
		},
		GatewayKey: func() string {
			key, _ := s.splitAPIKey.Load().(string)
			return key
		},
		OfficialProxyURL: cfg.OfficialProxyURL,
		MaxRequestBytes:  cfg.MaxRequestBytes,
		Observe: func(event splitrelay.Event) {
			if event.Phase == "complete" || event.Phase == "cancel" || event.Phase == "error" {
				log.WithFields(log.Fields{
					"request_id": event.RequestID, "route": event.Route,
					"model": event.Model, "phase": event.Phase,
					"elapsed_ms":        float64(event.Elapsed.Microseconds()) / 1000,
					"status":            event.Status,
					"headers_ms":        float64(event.HeadersAfter.Microseconds()) / 1000,
					"first_text_ms":     float64(event.FirstTextAfter.Microseconds()) / 1000,
					"first_thinking_ms": float64(event.FirstThinkingAfter.Microseconds()) / 1000,
					"first_tool_ms":     float64(event.FirstToolAfter.Microseconds()) / 1000,
				}).Info("split relay request")
			} else {
				log.WithFields(log.Fields{
					"request_id": event.RequestID, "phase": event.Phase,
					"elapsed_ms": float64(event.Elapsed.Microseconds()) / 1000,
				}).Debug("split relay phase")
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("configure split relay: %w", err)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		handler.CloseIdleConnections()
		return nil, fmt.Errorf("listen split relay: %w", err)
	}
	runtime := &splitRuntime{server: &http.Server{Addr: listener.Addr().String(), Handler: handler}, handler: handler}
	s.split.Store(runtime)
	errorsCh := make(chan error, 1)
	go func() {
		err := runtime.server.Serve(listener)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errorsCh <- err
		}
	}()
	log.Infof("split relay listening on %s (own providers dispatched in process)", listener.Addr())
	return errorsCh, nil
}

func (s *Server) closeSplitRelay() {
	if split := s.split.Load(); split != nil {
		_ = split.server.Close()
		split.handler.CloseIdleConnections()
	}
}
