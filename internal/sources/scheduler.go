package sources

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"kb/internal/canon"
	"kb/internal/events"
	"kb/internal/ingest"
	"kb/internal/kb"
	"kb/internal/store"
)

// defaultTTL is how often a PULL source refreshes when its schedule says nothing.
const defaultTTL = time.Hour

// failureAlertThreshold is how many consecutive failed polls raise a
// source.failed alert. The event fires once, when the count first reaches the
// threshold, so a persistently-broken source does not spam every poll (§8.15).
const failureAlertThreshold = 3

// Scheduler polls due PULL sources via their registered connectors and ingests the
// new items through the shared ingest path (design §8.1).
type Scheduler struct {
	st       *store.Store
	reg      *Registry
	ingester *ingest.Ingester
	bus      *events.Bus
	now      func() time.Time
	logger   *slog.Logger
}

// NewScheduler builds a scheduler. logger defaults to slog.Default(); bus may be
// nil (no health alerts).
func NewScheduler(st *store.Store, reg *Registry, ingester *ingest.Ingester, bus *events.Bus, logger *slog.Logger) *Scheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{st: st, reg: reg, ingester: ingester, bus: bus, now: time.Now, logger: logger}
}

// Run polls on a ticker until ctx is cancelled. A tick error is logged (a wedged
// source is visible, not fatal) and the loop continues.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			switch n, err := s.Tick(ctx); {
			case err != nil && ctx.Err() == nil:
				s.logger.Warn("source scheduler tick", "err", err)
			case n > 0:
				s.logger.Info("source scheduler ingested", "items", n)
			}
		}
	}
}

// Tick polls every enabled PULL source that is due and returns the number of items
// ingested. A per-source failure is collected and the others still run.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	srcs, err := s.st.Sources.ListEnabledByKind(ctx, string(kb.Pull))
	if err != nil {
		return 0, err
	}
	now := s.now()
	ingested := 0
	var errs error
	for _, src := range srcs {
		if err := ctx.Err(); err != nil {
			return ingested, err
		}
		if !due(src, now) {
			continue
		}
		conn, ok := s.reg.Get(src.Connector)
		if !ok {
			s.logger.Warn("source connector not registered", "source", src.ID, "connector", src.Connector)
			continue
		}
		n, err := s.poll(ctx, conn, src, now)
		ingested += n
		if err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return ingested, errs
}

// poll fetches one source, ingests its new items, and persists the fetch state.
// On a fetch error the cursor is kept (so nothing is skipped) and the health is
// still recorded; per-item ingest errors are logged and skipped.
func (s *Scheduler) poll(ctx context.Context, conn kb.SourceConnector, src *kb.Source, now time.Time) (int, error) {
	var since kb.Cursor
	if src.Cursor != nil {
		since = kb.Cursor(*src.Cursor)
	}
	items, newCursor, ferr := conn.Fetch(ctx, src, since)
	ts := store.FormatTime(now)
	h := s.nextHealth(src.Health, ferr, ts)
	health := healthJSON(h)

	if ferr != nil {
		// Keep the old cursor; record the failed attempt and health.
		if err := s.st.Sources.UpdateFetchState(ctx, src.ID, src.Cursor, &ts, &health); err != nil {
			return 0, err
		}
		// Alert once, when the failures first reach the threshold.
		if h.ConsecutiveFailures == failureAlertThreshold && s.bus != nil {
			s.bus.Publish(events.New(events.SourceFailed, map[string]string{
				"source_id": src.ID, "connector": src.Connector, "detail": h.Detail,
			}))
		}
		return 0, ferr
	}

	n := 0
	for _, raw := range items {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		item, ok := toItem(src, raw)
		if !ok {
			continue // unusable item (no content / invalid text / bad url)
		}
		if _, err := s.ingester.Ingest(ctx, item); err != nil {
			s.logger.Warn("source item ingest failed", "source", src.ID, "err", err)
			continue
		}
		n++
	}

	cursor := string(newCursor)
	if err := s.st.Sources.UpdateFetchState(ctx, src.ID, &cursor, &ts, &health); err != nil {
		return n, err
	}
	return n, nil
}

// DeriveStatus turns a source's health snapshot into a coarse operator status:
// unknown (never polled), healthy, degraded (some recent failures, below the
// alert threshold), or failing (at/over the threshold). §8.15 dashboard.
func DeriveStatus(h kb.Health, polled bool) string {
	switch {
	case !polled:
		return "unknown"
	case h.OK:
		return "healthy"
	case h.ConsecutiveFailures >= failureAlertThreshold:
		return "failing"
	default:
		return "degraded"
	}
}

// NextFetchAt returns when a PULL source is next due (last poll + its refresh
// interval) as a store-format timestamp, or ("", false) if it has never been
// polled (due now).
func NextFetchAt(lastFetchedAt, schedule *string) (string, bool) {
	if lastFetchedAt == nil {
		return "", false
	}
	last, err := time.Parse(store.TimeFormat, *lastFetchedAt)
	if err != nil {
		return "", false
	}
	return store.FormatTime(last.Add(ttl(schedule))), true
}

// due reports whether a source's refresh interval has elapsed.
func due(src *kb.Source, now time.Time) bool {
	if src.LastFetchedAt == nil {
		return true // never polled
	}
	last, err := time.Parse(time.RFC3339Nano, *src.LastFetchedAt)
	if err != nil {
		return true
	}
	return now.Sub(last) >= ttl(src.Schedule)
}

// ttl parses the refresh interval from a source's schedule JSON
// ({"every_seconds": N}), falling back to defaultTTL.
func ttl(schedule *string) time.Duration {
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

// toItem converts a connector's RawItem into a validated, canonicalized ingest
// Item (or ok=false to skip it). A URL item dedups by canonical URL; a body-only
// item dedups by content hash. The privacy tier is the source's default.
func toItem(src *kb.Source, raw kb.RawItem) (ingest.Item, bool) {
	if !ingest.ValidText(raw.Title, false) || !ingest.ValidText(raw.Body, true) {
		return ingest.Item{}, false
	}
	item := ingest.Item{SourceID: src.ID, Title: raw.Title, Tier: src.DefaultPrivacy}
	// Prefer a usable article URL; if it's missing or not canonicalizable, fall
	// back to the body as a note rather than dropping a content-bearing item.
	if raw.URL != "" {
		if c, err := canon.URL(raw.URL); err == nil {
			item.Type, item.CanonicalURL, item.RawURL = kb.Webpage, c, raw.URL
			return item, true
		}
	}
	if raw.Body != "" {
		item.Type, item.Body, item.ContentHash = kb.Note, raw.Body, canon.Content(raw.Body)
		return item, true
	}
	return ingest.Item{}, false
}

// maxHealthDetail bounds the error text stored in a source's health JSON so a
// pathological connector error can't bloat the row.
const maxHealthDetail = 512

func errString(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > maxHealthDetail {
		s = s[:maxHealthDetail]
	}
	return s
}

// nextHealth derives the new health snapshot from the prior one and this poll's
// outcome: a success resets the consecutive-failure count and stamps the success
// time; a failure increments the count and carries the prior success time forward
// (so "failing since" is derivable).
func (s *Scheduler) nextHealth(prior *string, ferr error, ts string) kb.Health {
	var prev kb.Health
	if prior != nil && *prior != "" {
		_ = json.Unmarshal([]byte(*prior), &prev) // a corrupt prior degrades to zero value
	}
	if ferr == nil {
		return kb.Health{OK: true, LastSuccessAt: ts}
	}
	return kb.Health{
		OK:                  false,
		Detail:              errString(ferr),
		ConsecutiveFailures: prev.ConsecutiveFailures + 1,
		LastSuccessAt:       prev.LastSuccessAt,
	}
}

func healthJSON(h kb.Health) string {
	b, err := json.Marshal(h)
	if err != nil {
		return `{"ok":false}`
	}
	return string(b)
}
