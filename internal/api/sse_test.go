package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"kb/internal/events"
)

func TestSSE_DeliversAfterConnect(t *testing.T) {
	bus := events.NewBus()
	srv := httptest.NewServer(SSEHandler(bus))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// The handler subscribes before flushing headers, so once Do() has returned
	// the subscription exists and this event cannot be missed.
	bus.Publish(events.New(events.ArtifactReady, map[string]string{"id": "01ABC"}))

	got := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data:") {
				got <- line
				return
			}
		}
	}()

	select {
	case line := <-got:
		if !strings.Contains(line, string(events.ArtifactReady)) || !strings.Contains(line, "01ABC") {
			t.Errorf("SSE data line = %q, want it to contain %q and the payload id", line, events.ArtifactReady)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no SSE frame received after publish")
	}
}
