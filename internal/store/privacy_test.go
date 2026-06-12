package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"kb/internal/kb"
)

func seedTiered(t *testing.T, st *Store, hash, title, tier string) string {
	t.Helper()
	a, err := st.Artifacts.UpsertByContentHash(context.Background(),
		&Artifact{Type: "note", Title: title, BodyMarkdown: title, ContentHash: &hash, PrivacyTier: kb.PrivacyTier(tier)})
	if err != nil {
		t.Fatalf("seed %s: %v", tier, err)
	}
	return a.ID
}

func TestSearchKeyword_TierCeiling(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	pub := seedTiered(t, st, "h1", "secret apple", "public")
	_ = seedTiered(t, st, "h2", "secret apple", "sensitive")

	// A public ceiling sees only the public artifact.
	hits, err := st.Artifacts.SearchKeyword(ctx, `"secret"`, 10, kb.Public)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ArtifactID != pub {
		t.Errorf("public ceiling = %+v, want only the public artifact %s", hits, pub)
	}
	// The owner (Restricted) sees both.
	hits, _ = st.Artifacts.SearchKeyword(ctx, `"secret"`, 10, kb.Restricted)
	if len(hits) != 2 {
		t.Errorf("owner ceiling = %d hits, want 2", len(hits))
	}
}

func TestGetLiveWithin_Ceiling(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sens := seedTiered(t, st, "h1", "x", "sensitive")

	// Below the artifact's tier → ErrNotFound (no read, no existence leak).
	for _, ceiling := range []kb.PrivacyTier{kb.Public, kb.Personal} {
		if _, err := st.Artifacts.GetLiveWithin(ctx, sens, ceiling); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetLiveWithin(sensitive, %s) err = %v, want ErrNotFound", ceiling, err)
		}
	}
	// At or above the artifact's tier → returned.
	for _, ceiling := range []kb.PrivacyTier{kb.Sensitive, kb.Restricted} {
		if a, err := st.Artifacts.GetLiveWithin(ctx, sens, ceiling); err != nil || a.ID != sens {
			t.Errorf("GetLiveWithin(sensitive, %s) = %v, %v, want the artifact", ceiling, a, err)
		}
	}
}

func TestForEachLatestEmbeddingWithin_Ceiling(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	seedEmbed := func(hash, tier string) string {
		id := seedTiered(t, st, hash, "t", tier)
		payload, _ := json.Marshal(kb.EmbeddingPayload{Model: "m", Dim: 2, Vector: []float32{1, 0}})
		if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: id, Kind: "embedding", Payload: string(payload), Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	pub := seedEmbed("h1", "public")
	_ = seedEmbed("h2", "restricted")

	var ids []string
	if err := st.Enrichments.ForEachLatestEmbeddingWithin(ctx, kb.Public, func(id, _ string) error {
		ids = append(ids, id)
		return nil
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(ids) != 1 || ids[0] != pub {
		t.Errorf("public ceiling streamed %v, want only the public artifact %s", ids, pub)
	}
	// The unfiltered (owner) variant streams both.
	ids = nil
	_ = st.Enrichments.ForEachLatestEmbedding(ctx, func(id, _ string) error { ids = append(ids, id); return nil })
	if len(ids) != 2 {
		t.Errorf("unfiltered streamed %d, want 2", len(ids))
	}
}
