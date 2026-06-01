// Package api holds the daemon's HTTP handlers.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"kb/internal/events"
)

// sseHeartbeat is the interval between keep-alive comments, which detect a dead
// connection (the write fails) and defeat idle proxy/Tailscale timeouts.
const sseHeartbeat = 25 * time.Second

// SSEHandler streams bus events to the client as Server-Sent Events. It
// subscribes before flushing the response headers, so a client that has
// received the response will not miss an event published thereafter. The stream
// ends when the client disconnects (request context done) or a write fails (a
// wedged client is reaped rather than leaking a goroutine + connection).
func SSEHandler(bus *events.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch, cancel := bus.Subscribe()
		defer cancel()

		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Content-Type-Options", "nosniff")
		flusher.Flush() // commit headers now, before any event

		beat := time.NewTicker(sseHeartbeat)
		defer beat.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-beat.C:
				if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
					return // client gone
				}
				flusher.Flush()
			case e, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue // skip an unencodable event rather than break the stream
				}
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data); err != nil {
					return // client gone; stop streaming
				}
				flusher.Flush()
			}
		}
	}
}
