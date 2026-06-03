package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"kb/internal/config"
)

func TestLogLevel(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug, "warn": slog.LevelWarn, "error": slog.LevelError,
		"": slog.LevelInfo, "bogus": slog.LevelInfo, " DEBUG ": slog.LevelDebug,
	}
	for in, want := range cases {
		if got := logLevel(in); got != want {
			t.Errorf("logLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestServe_BadDBPath(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	cfg := &config.Config{
		DBPath: "/nonexistent-dir-xyz/kb.db", ListenAddr: ln.Addr().String(),
		APIToken: "t", DefaultPrivacy: "personal",
	}
	if err := serve(context.Background(), ln, cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("expected an error opening a DB in a nonexistent directory")
	}
}

func TestServe_HealthzThenGracefulShutdown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	dir := t.TempDir()
	cfg := &config.Config{
		DBPath:         filepath.Join(dir, "kb.db"),
		ListenAddr:     addr,
		APIToken:       "tok",
		DefaultPrivacy: "personal",
		RenderDir:      filepath.Join(dir, "renders"),
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- serve(ctx, ln, cfg, logger) }()

	// Poll /healthz until the daemon is serving.
	ready := false
	for range 100 {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatal("/healthz never became ready")
	}

	cancel() // trigger graceful shutdown
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("serve returned %v, want nil after shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
}
