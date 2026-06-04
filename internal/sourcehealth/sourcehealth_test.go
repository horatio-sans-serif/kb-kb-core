package sourcehealth

import (
	"testing"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
)

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		h      kb.Health
		polled bool
		want   string
	}{
		{kb.Health{}, false, "unknown"},
		{kb.Health{OK: true}, true, "healthy"},
		{kb.Health{ConsecutiveFailures: 1}, true, "degraded"},
		{kb.Health{ConsecutiveFailures: AlertThreshold}, true, "failing"},
		{kb.Health{ConsecutiveFailures: AlertThreshold + 2}, true, "failing"},
	}
	for _, c := range cases {
		if got := DeriveStatus(c.h, c.polled); got != c.want {
			t.Errorf("DeriveStatus(%+v, %v) = %q, want %q", c.h, c.polled, got, c.want)
		}
	}
}

func TestRefreshInterval(t *testing.T) {
	if RefreshInterval(nil) != defaultTTL {
		t.Errorf("nil schedule should use the default TTL")
	}
	sched := `{"every_seconds":90}`
	if RefreshInterval(&sched) != 90*time.Second {
		t.Errorf("schedule not parsed")
	}
	bad := `{"every_seconds":-1}`
	if RefreshInterval(&bad) != defaultTTL {
		t.Errorf("a non-positive interval should fall back to the default")
	}
}

func TestNextFetchAt(t *testing.T) {
	if _, ok := NextFetchAt(nil, nil); ok {
		t.Errorf("a never-polled source has no next-fetch time")
	}
	last := store.FormatTime(time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC))
	sched := `{"every_seconds":3600}`
	next, ok := NextFetchAt(&last, &sched)
	if !ok || next != store.FormatTime(time.Date(2025, 6, 1, 1, 0, 0, 0, time.UTC)) {
		t.Errorf("NextFetchAt = (%q, %v), want last + 1h", next, ok)
	}
}
