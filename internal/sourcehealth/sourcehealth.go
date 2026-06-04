// Package sourcehealth holds the source health/scheduling helpers shared by the
// scheduler (which produces health) and the API (which presents it). It is a leaf
// package (kb + store only) so the API need not import the concrete sources
// feature package — preserving the ConnectorRegistry interface boundary (§8.15).
package sourcehealth

import (
	"encoding/json"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
)

// AlertThreshold is how many consecutive failed polls raise a source.failed
// alert. The event fires once, when the count first reaches it, so a persistently-
// broken source does not spam every poll.
const AlertThreshold = 3

// defaultTTL is how often a PULL source refreshes when its schedule says nothing.
const defaultTTL = time.Hour

// RefreshInterval is a source's poll interval, parsed from its schedule JSON
// ({"every_seconds": N}), falling back to defaultTTL.
func RefreshInterval(schedule *string) time.Duration {
	if schedule == nil {
		return defaultTTL
	}
	var sc struct {
		EverySeconds int `json:"every_seconds"`
	}
	if json.Unmarshal([]byte(*schedule), &sc) != nil || sc.EverySeconds <= 0 {
		return defaultTTL
	}
	return time.Duration(sc.EverySeconds) * time.Second
}

// DeriveStatus turns a source's health snapshot into a coarse operator status:
// unknown (never polled), healthy, degraded (some recent failures, below the
// alert threshold), or failing (at/over the threshold).
func DeriveStatus(h kb.Health, polled bool) string {
	switch {
	case !polled:
		return "unknown"
	case h.OK:
		return "healthy"
	case h.ConsecutiveFailures >= AlertThreshold:
		return "failing"
	default:
		return "degraded"
	}
}

// NextFetchAt returns when a PULL source is next due (last poll + its refresh
// interval) as a store-format timestamp, or ("", false) if it has never been
// polled (due now). last_fetched_at is canonically store.TimeFormat.
func NextFetchAt(lastFetchedAt, schedule *string) (string, bool) {
	if lastFetchedAt == nil {
		return "", false
	}
	last, err := time.Parse(store.TimeFormat, *lastFetchedAt)
	if err != nil {
		return "", false
	}
	return store.FormatTime(last.Add(RefreshInterval(schedule))), true
}
