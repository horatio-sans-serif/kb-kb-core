// Package api holds the daemon's HTTP handlers. Phase 0 Stage 4 provides the SSE
// event stream; Stage 5 adds the rest of the API (health, ingest, auth).
package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"kb/internal/events"
)

// SSEHandler streams bus events to the client as Server-Sent Events. It
// subscribes before flushing the response headers, so a client that has
// received the response will not miss an event published thereafter. The stream
// ends when the client disconnects (request context done).
func SSEHandler(bus *events.Bus) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		ch, cancel := bus.Subscribe()
		defer cancel()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher.Flush() // commit headers now, before any event

		for {
			select {
			case <-r.Context().Done():
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				data, err := json.Marshal(e)
				if err != nil {
					continue // skip an unencodable event rather than break the stream
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
				flusher.Flush()
			}
		}
	}
}
