// Package events is the in-process publish/subscribe bus that is the system's
// nervous system (design Section 9 / Principle 5): lifecycle events fan out to
// best-effort subscribers such as the SSE endpoint, surfacing, and UI observers.
//
// Durability rule: the bus is for NOTIFICATIONS only. Anything that must durably
// happen (e.g. enqueueing enrichment, re-scoring on profile change) is a
// transactionally-enqueued job, never a bus subscriber — a lossy, in-process,
// non-durable bus cannot drive durable work. See design §9 and the walkthrough.
package events

import "time"

// EventType identifies a lifecycle notification (design Section 9).
type EventType string

// Event type constants (design Section 9).
const (
	ArtifactIngested   EventType = "artifact.ingested"
	ArtifactReady      EventType = "artifact.ready"
	EngagementRecorded EventType = "engagement.recorded"
	ProfileUpdated     EventType = "profile.updated"
	SourceFailed       EventType = "source.failed"
	ConflictDetected   EventType = "conflict.detected"
	ReviewDue          EventType = "review.due"
	DigestReady        EventType = "digest.ready"
	ActivityObserved   EventType = "activity.observed"
)

// Event is one lifecycle notification. Data is any JSON-serializable payload.
type Event struct {
	Type EventType `json:"type"`
	Data any       `json:"data,omitempty"`
	Time time.Time `json:"time"`
}

// New builds an Event stamped with the current UTC time.
func New(eventType EventType, data any) Event {
	return Event{Type: eventType, Data: data, Time: time.Now().UTC()}
}
