// Package events is the in-process publish/subscribe bus that is the system's
// nervous system (design Section 9 / Principle 5): lifecycle events fan out to
// subscribers such as the enrichment runner, surfacing, and the SSE endpoint.
package events

import "time"

// Event type constants (design Section 9).
const (
	ArtifactIngested   = "artifact.ingested"
	ArtifactReady      = "artifact.ready"
	EngagementRecorded = "engagement.recorded"
	ProfileUpdated     = "profile.updated"
	SourceFailed       = "source.failed"
	ConflictDetected   = "conflict.detected"
	ReviewDue          = "review.due"
	DigestReady        = "digest.ready"
	ActivityObserved   = "activity.observed"
)

// Event is one lifecycle notification. Data is any JSON-serializable payload.
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	Time string `json:"time"`
}

// New builds an Event stamped with the current UTC time.
func New(eventType string, data any) Event {
	return Event{Type: eventType, Data: data, Time: time.Now().UTC().Format(time.RFC3339Nano)}
}
