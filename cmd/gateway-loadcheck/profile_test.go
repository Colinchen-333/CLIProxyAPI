package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCPUProfileDownloadAndFailure(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "profile", false: "reject invalid response"}[valid], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/debug/pprof/profile" || r.URL.Query().Get("seconds") != "5" {
					t.Errorf("unexpected profile request: %s", r.URL)
				}
				if valid {
					_, _ = w.Write([]byte{0x1f, 0x8b, 0x08, 0})
				} else {
					_, _ = w.Write([]byte("not a profile"))
				}
			}))
			defer upstream.Close()
			path := filepath.Join(t.TempDir(), "cpu.pprof")
			finish, err := startCPUProfile(context.Background(), strings.TrimPrefix(upstream.URL, "http://"), path)
			if err != nil {
				t.Fatal(err)
			}
			err = finish()
			if (err == nil) != valid {
				t.Fatalf("unexpected result: %v", err)
			}
			_, statErr := os.Stat(path)
			if (statErr == nil) != valid {
				t.Fatalf("unexpected saved profile: %v", statErr)
			}
		})
	}
}
func TestCPUProfileRejectsNonLoopback(t *testing.T) {
	if _, err := startCPUProfile(context.Background(), "192.0.2.1:8316", filepath.Join(t.TempDir(), "cpu.pprof")); err == nil {
		t.Fatal("non-loopback profile accepted")
	}
}
