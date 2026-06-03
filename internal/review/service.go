package review

import (
	"context"
	"errors"
	"fmt"
	"time"

	"kb/internal/kb"
	"kb/internal/store"
)

// ErrArtifactNotFound is returned when a review targets an artifact that does not
// exist (or is tombstoned), so the API can answer 404.
var ErrArtifactNotFound = errors.New("review: artifact not found")

// DueItem is one entry in the review queue: the schedule joined with enough of the
// artifact to display it.
type DueItem struct {
	ArtifactID   string
	Title        string
	URL          string
	NextReviewAt string
	IntervalDays float64
	Reviews      int
}

// Service applies SM-2 reviews over the store and lists what's due.
type Service struct {
	st  *store.Store
	now func() time.Time
}

// New wires a Service to the store with the wall clock.
func New(st *store.Store) *Service { return NewWithClock(st, time.Now) }

// NewWithClock is New with an injectable clock (for tests).
func NewWithClock(st *store.Store, now func() time.Time) *Service {
	return &Service{st: st, now: now}
}

// Record applies a recall grade to an artifact: it loads the prior schedule
// (default for a never-reviewed artifact), advances it via SM-2, sets the next
// review to now + the new interval, and persists. The artifact must be live.
func (s *Service) Record(ctx context.Context, artifactID string, grade int) (*kb.ReviewState, error) {
	if _, err := s.st.Artifacts.GetLive(ctx, artifactID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrArtifactNotFound, artifactID)
		}
		return nil, fmt.Errorf("review lookup %q: %w", artifactID, err)
	}

	prev := Default()
	switch cur, err := s.st.Reviews.Get(ctx, artifactID); {
	case err == nil:
		prev = State{IntervalDays: cur.IntervalDays, Ease: cur.Ease, Reviews: cur.Reviews}
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}

	next := Schedule(prev, grade)
	rs := &kb.ReviewState{
		ArtifactID:   artifactID,
		NextReviewAt: store.FormatTime(s.now().Add(daysToDuration(next.IntervalDays))),
		IntervalDays: next.IntervalDays,
		Ease:         next.Ease,
		Reviews:      next.Reviews,
	}
	if err := s.st.Reviews.Upsert(ctx, rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// defaultDueLimit bounds Due when the caller passes a non-positive limit, so a
// direct caller (e.g. a future digest job) gets a sane page rather than the
// repo's surface-borrowed fallback.
const defaultDueLimit = 20

// Due returns the artifacts due for review now, soonest first, joined with their
// title/url. A schedule whose artifact has been tombstoned is skipped.
func (s *Service) Due(ctx context.Context, limit int) ([]DueItem, error) {
	if limit <= 0 {
		limit = defaultDueLimit
	}
	now := store.FormatTime(s.now())
	states, err := s.st.Reviews.Due(ctx, now, limit)
	if err != nil {
		return nil, err
	}
	out := make([]DueItem, 0, len(states))
	for _, rs := range states {
		a, err := s.st.Artifacts.GetLive(ctx, rs.ArtifactID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // tombstoned since scheduling — drop from the queue
			}
			return nil, err
		}
		item := DueItem{
			ArtifactID:   rs.ArtifactID,
			Title:        a.Title,
			NextReviewAt: rs.NextReviewAt,
			IntervalDays: rs.IntervalDays,
			Reviews:      rs.Reviews,
		}
		if a.CanonicalURL != nil {
			item.URL = *a.CanonicalURL
		}
		out = append(out, item)
	}
	return out, nil
}

// daysToDuration converts an interval in days (always a whole number in practice,
// and bounded by maxIntervalDays so the multiplication stays inside int64) to a
// Duration.
func daysToDuration(days float64) time.Duration {
	return time.Duration(days * 24 * float64(time.Hour))
}
