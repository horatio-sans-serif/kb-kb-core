package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/review"
	"kb/internal/store"
)

func reviewTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Review: review.New(st)}).Handler()
	return st, h
}

func TestReview_RecordSchedules(t *testing.T) {
	st, h := reviewTestServer(t)
	id := seedReady(t, st, "h1", "Topic", kb.Public, 0.5)

	rec := doPost(h, "/review", `{"artifact_id":"`+id+`","grade":5}`, "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		ArtifactID   string  `json:"artifact_id"`
		NextReviewAt string  `json:"next_review_at"`
		Reviews      int     `json:"reviews"`
		IntervalDays float64 `json:"interval_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ArtifactID != id || got.Reviews != 1 || got.IntervalDays != 1 || got.NextReviewAt == "" {
		t.Errorf("review = %+v, want reviews 1, interval 1, a next time", got)
	}
}

func TestReview_UnknownArtifactIs404(t *testing.T) {
	_, h := reviewTestServer(t)
	rec := doPost(h, "/review", `{"artifact_id":"01HMISSING","grade":5}`, "tok")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown artifact", rec.Code)
	}
}

func TestReview_InvalidGradeIs400(t *testing.T) {
	st, h := reviewTestServer(t)
	id := seedReady(t, st, "h1", "T", kb.Public, 0.5)
	if rec := doPost(h, "/review", `{"artifact_id":"`+id+`","grade":7}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an out-of-range grade", rec.Code)
	}
}

func TestReview_MissingArtifactIDIs400(t *testing.T) {
	_, h := reviewTestServer(t)
	if rec := doPost(h, "/review", `{"grade":5}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 when artifact_id is missing", rec.Code)
	}
}

func TestReview_DueListsScheduled(t *testing.T) {
	st, h := reviewTestServer(t)
	id := seedReady(t, st, "h1", "Due Topic", kb.Public, 0.5)
	// Schedule it in the past so it is due regardless of the wall clock.
	if err := st.Reviews.Upsert(context.Background(), &kb.ReviewState{
		ArtifactID: id, NextReviewAt: "2000-01-01T00:00:00.000000000Z", IntervalDays: 1, Ease: 2.5, Reviews: 1,
	}); err != nil {
		t.Fatalf("seed schedule: %v", err)
	}

	rec := doGet(h, "/review/due", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Due []struct {
			ArtifactID string `json:"artifact_id"`
			Title      string `json:"title"`
		} `json:"due"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Due) != 1 || out.Due[0].ArtifactID != id || out.Due[0].Title != "Due Topic" {
		t.Errorf("due = %+v, want the scheduled artifact", out.Due)
	}
}

func TestReview_DueLimitValidation(t *testing.T) {
	_, h := reviewTestServer(t)
	for _, bad := range []string{"0", "-1", "201", "abc"} {
		if rec := doGet(h, "/review/due?limit="+bad, "tok"); rec.Code != http.StatusBadRequest {
			t.Errorf("limit=%s = %d, want 400", bad, rec.Code)
		}
	}
}

func TestReview_NotConfiguredIs503(t *testing.T) {
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	if rec := doPost(h, "/review", `{"artifact_id":"x","grade":5}`, "tok"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when review is not configured", rec.Code)
	}
}

func TestReview_RequiresAuth(t *testing.T) {
	_, h := reviewTestServer(t)
	if rec := doPost(h, "/review", `{"artifact_id":"x","grade":5}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a token", rec.Code)
	}
	if rec := doGet(h, "/review/due", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /review/due without token = %d, want 401", rec.Code)
	}
}
