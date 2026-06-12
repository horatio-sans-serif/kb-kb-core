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

func TestTierCeiling_InvalidCeilingFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	_ = seedTiered(t, st, "h1", "apple", "public")
	// An invalid read ceiling must see nothing (not everything).
	hits, err := st.Artifacts.SearchKeyword(ctx, `"apple"`, 10, kb.PrivacyTier("bogus"))
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("invalid ceiling returned %d hits, want 0 (fail closed)", len(hits))
	}
}

func TestTierCeiling_InvalidCeiling_AllQueryFamilies(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	bogus := kb.PrivacyTier("bogus")

	// Wire one public artifact (and a second it links to) into every read structure,
	// then assert each ceiling-aware query family returns nothing for an invalid ceiling
	// — the "1 = 0" fail-closed path, not "1 = 1" (see all uses of tierClause).
	a := seedTiered(t, st, "h1", "alpha apple", "public")
	b := seedTiered(t, st, "h2", "beta", "public")
	if err := st.Graph.AddLink(ctx, a, b, "references", 1.0); err != nil { // a -> b: b's backlink, a's neighbor
		t.Fatal(err)
	}
	ent, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "concept", Name: "Topic"})
	if err := st.Graph.LinkArtifactEntity(ctx, a, ent); err != nil {
		t.Fatal(err)
	}
	top := &Topic{Label: "T"}
	if err := st.Topics.Create(ctx, top); err != nil {
		t.Fatal(err)
	}
	if err := st.Topics.Assign(ctx, a, top.ID, 0.9); err != nil {
		t.Fatal(err)
	}
	ca := &Claim{ArtifactID: a, Statement: "P"}
	cb := &Claim{ArtifactID: b, Statement: "not P"}
	if err := st.Claims.Create(ctx, ca); err != nil {
		t.Fatal(err)
	}
	if err := st.Claims.Create(ctx, cb); err != nil {
		t.Fatal(err)
	}
	if err := st.Conflicts.Create(ctx, &Conflict{ClaimA: ca.ID, ClaimB: cb.ID}); err != nil {
		t.Fatal(err)
	}

	// Each family: invalid ceiling returns zero rows and no error (fail closed, not a
	// query failure). The named sub-test makes a regression point at the exact family.
	checks := []struct {
		name string
		run  func() (int, error)
	}{
		{"SearchKeyword", func() (int, error) { h, e := st.Artifacts.SearchKeyword(ctx, `"alpha"`, 10, bogus); return len(h), e }},
		{"ListEntities", func() (int, error) { x, e := st.Graph.ListEntities(ctx, 10, bogus); return len(x), e }},
		{"EntityArtifacts", func() (int, error) { x, e := st.Graph.EntityArtifacts(ctx, ent, 10, bogus); return len(x), e }},
		{"Backlinks", func() (int, error) { x, e := st.Graph.Backlinks(ctx, b, 10, bogus); return len(x), e }},
		{"ConnectedWithin", func() (int, error) { x, e := st.Graph.ConnectedWithin(ctx, a, 2, bogus); return len(x), e }},
		{"ListWithCounts", func() (int, error) { x, e := st.Topics.ListWithCounts(ctx, 10, bogus); return len(x), e }},
		{"Members", func() (int, error) { x, e := st.Topics.Members(ctx, top.ID, 10, bogus); return len(x), e }},
		{"ListByStatus", func() (int, error) { x, e := st.Conflicts.ListByStatus(ctx, "open", 10, bogus); return len(x), e }},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.run()
			if err != nil {
				t.Fatalf("%s with invalid ceiling errored: %v", c.name, err)
			}
			if n != 0 {
				t.Errorf("%s with invalid ceiling returned %d rows, want 0 (fail closed)", c.name, n)
			}
		})
	}

	// Sanity: the owner ceiling sees the wired structures (proves the test wired them).
	if h, _ := st.Artifacts.SearchKeyword(ctx, `"alpha"`, 10, kb.Restricted); len(h) == 0 {
		t.Error("owner SearchKeyword found nothing — the fixture is not wired")
	}
}

func TestConnectedWithin_CeilingHidesAndDoesNotExpandThrough(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// pub -> sens -> far. far is reachable from pub within 2 hops ONLY through the
	// sensitive node, so a public ceiling must not reach it (no expansion through a
	// hidden node), while the owner reaches both.
	pub := seedTiered(t, st, "h1", "p", "public")
	sens := seedTiered(t, st, "h2", "s", "sensitive")
	far := seedTiered(t, st, "h3", "f", "public")
	if err := st.Graph.AddLink(ctx, pub, sens, "references", 1.0); err != nil {
		t.Fatal(err)
	}
	if err := st.Graph.AddLink(ctx, sens, far, "references", 1.0); err != nil {
		t.Fatal(err)
	}

	owner, err := st.Graph.ConnectedWithin(ctx, pub, 2, kb.Restricted)
	if err != nil {
		t.Fatal(err)
	}
	if len(owner) != 2 {
		t.Errorf("owner ConnectedWithin = %v, want both sens and far", owner)
	}
	pubSet, err := st.Graph.ConnectedWithin(ctx, pub, 2, kb.Public)
	if err != nil {
		t.Fatal(err)
	}
	if len(pubSet) != 0 {
		t.Errorf("public ConnectedWithin = %v, want empty (sens hidden, far unreachable through it)", pubSet)
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
