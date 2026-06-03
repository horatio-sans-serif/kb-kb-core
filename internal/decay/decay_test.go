package decay

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"kb/internal/kb"
	"kb/internal/relevance"
	"kb/internal/store"
	"kb/internal/storetest"
)

func TestSweep_DecaysByAge(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// firstSeen is the recency anchor for an unengaged artifact.
	seed := func(hash string, firstSeen time.Time) string {
		a, err := st.Artifacts.UpsertByContentHash(ctx, &store.Artifact{
			Type: "note", BodyMarkdown: hash, ContentHash: &hash, FirstSeenAt: store.FormatTime(firstSeen),
		})
		if err != nil {
			t.Fatalf("seed %s: %v", hash, err)
		}
		payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 2, Vector: []float32{1, 0}})
		if err := st.Enrichments.Create(ctx, &store.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("embed %s: %v", hash, err)
		}
		return a.ID
	}
	fresh := seed("fresh", now)
	old := seed("old", now.Add(-60*24*time.Hour)) // 60 days → two half-lives

	prof := relevance.NewProfile([]relevance.WeightedVector{{Vector: []float32{1, 0}, Weight: 1}})
	s := NewSweeper(st, prof)
	s.now = func() time.Time { return now }

	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 {
		t.Errorf("re-scored %d, want 2 (both were unscored)", n)
	}

	fa, _ := st.Artifacts.Get(ctx, fresh)
	oa, _ := st.Artifacts.Get(ctx, old)
	if fa.RelevanceScore == nil || oa.RelevanceScore == nil {
		t.Fatal("relevance scores not set by the sweep")
	}
	if !(*fa.RelevanceScore > *oa.RelevanceScore) {
		t.Errorf("fresh score %v should exceed the age-decayed old score %v", *fa.RelevanceScore, *oa.RelevanceScore)
	}

	// The sweep patches only — it writes no relevance enrichment rows.
	if ens, _ := st.Enrichments.ListForArtifact(ctx, old); len(ens) != 1 {
		t.Errorf("enrichments for old = %d, want 1 (just the seeded embedding; sweep adds none)", len(ens))
	}

	// A second sweep at the same instant changes nothing (the epsilon skip).
	if n2, err := s.Sweep(ctx); err != nil || n2 != 0 {
		t.Errorf("second sweep changed %d (err %v), want 0 — unchanged scores are skipped", n2, err)
	}
}

func TestSweep_ResightingDoesNotResetDecay(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Entered the KB 60 days ago, but re-sighted just now (last_seen_at recent) and
	// never engaged. Recency must still decay (anchored on first_seen_at), not reset.
	hash := "resighted"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &store.Artifact{
		Type:         "note",
		BodyMarkdown: hash,
		ContentHash:  &hash,
		FirstSeenAt:  store.FormatTime(now.Add(-60 * 24 * time.Hour)),
		LastSeenAt:   store.FormatTime(now), // a fresh re-sighting
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 2, Vector: []float32{1, 0}})
	_ = st.Enrichments.Create(ctx, &store.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"})

	prof := relevance.NewProfile([]relevance.WeightedVector{{Vector: []float32{1, 0}, Weight: 1}})
	s := NewSweeper(st, prof)
	s.now = func() time.Time { return now }
	if _, err := s.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	got, _ := st.Artifacts.Get(ctx, a.ID)
	// 60 days = two half-lives → recency 0.25; match 1, other factors 1 → score ~0.25.
	if got.RelevanceScore == nil || *got.RelevanceScore > 0.5 {
		t.Errorf("score = %v, want decayed (~0.25) — a re-sighting must not reset recency", got.RelevanceScore)
	}
}

func TestSweep_SkipsPoisonRowAndContinues(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	mk := func(hash, payload string) string {
		a, err := st.Artifacts.UpsertByContentHash(ctx, &store.Artifact{Type: "note", BodyMarkdown: hash, ContentHash: &hash})
		if err != nil {
			t.Fatalf("seed %s: %v", hash, err)
		}
		if err := st.Enrichments.Create(ctx, &store.Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: payload, Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("embed %s: %v", hash, err)
		}
		return a.ID
	}
	// One artifact has a corrupt embedding payload (unmarshal error in Signals);
	// ULIDs are monotonic so "aaa" sorts before "zzz" — the poison row comes first.
	mk("aaa-poison", `not json`)
	good := mk("zzz-good", `{"vector":[1,0]}`)

	s := NewSweeper(st, relevance.NewProfile(nil))
	n, err := s.Sweep(ctx)
	if err == nil {
		t.Error("expected the joined per-artifact error to be reported")
	}
	if n != 1 {
		t.Errorf("changed = %d, want 1 (the good artifact scored despite the poison row)", n)
	}
	if got, _ := st.Artifacts.Get(ctx, good); got.RelevanceScore == nil {
		t.Error("the good artifact after the poison row was not scored — the sweep wedged")
	}
}

func TestSweep_SkipsArtifactsWithoutEmbedding(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	h := "h"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &store.Artifact{Type: "note", BodyMarkdown: "b", ContentHash: &h})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	s := NewSweeper(st, relevance.NewProfile(nil))
	n, err := s.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("re-scored %d, want 0 (no embedding yet)", n)
	}
	if got, _ := st.Artifacts.Get(ctx, a.ID); got.RelevanceScore != nil {
		t.Error("an unembedded artifact must not get a score from the sweep")
	}
}
