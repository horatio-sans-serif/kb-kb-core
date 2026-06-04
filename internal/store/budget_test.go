package store

import (
	"context"
	"testing"

	"kb/internal/kb"
)

func budgetArtifact(t *testing.T, st *Store, sourceID, hash string, score float64) string {
	t.Helper()
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: "t", BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	rdy := kb.StatusReady
	_ = st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{Status: &rdy, RelevanceScore: &score})
	if err := st.Encounters.Record(ctx, &Encounter{ArtifactID: a.ID, SourceID: sourceID, SeenAt: now()}); err != nil {
		t.Fatalf("encounter: %v", err)
	}
	return a.ID
}

func TestBudget_CountAndLowestRelevance(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	budget := 2
	src := &Source{Kind: "PULL", Connector: "rss", StorageBudget: &budget}
	if err := st.Sources.Create(ctx, src); err != nil {
		t.Fatalf("source: %v", err)
	}

	lo := budgetArtifact(t, st, src.ID, "h1", 0.1)
	mid := budgetArtifact(t, st, src.ID, "h2", 0.2)
	budgetArtifact(t, st, src.ID, "h3", 0.3)
	budgetArtifact(t, st, src.ID, "h4", 0.4)

	// A source with a budget set is found by ListWithBudget.
	withB, err := st.Sources.ListWithBudget(ctx)
	if err != nil {
		t.Fatalf("list with budget: %v", err)
	}
	if len(withB) != 1 || withB[0].ID != src.ID || *withB[0].StorageBudget != 2 {
		t.Fatalf("ListWithBudget = %+v, want the budgeted source", withB)
	}

	n, err := st.Encounters.ActiveArtifactCountForSource(ctx, src.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Errorf("active count = %d, want 4", n)
	}

	// The 2 lowest-relevance artifacts are the excess to archive.
	over, err := st.Encounters.LowestRelevanceActiveForSource(ctx, src.ID, n-budget)
	if err != nil {
		t.Fatalf("lowest: %v", err)
	}
	if len(over) != 2 || over[0] != lo || over[1] != mid {
		t.Errorf("excess = %v, want [lo, mid] (lowest relevance first)", over)
	}
}

func TestBudget_IgnoresArchivedAndUnscored(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	src := &Source{Kind: "PULL", Connector: "rss"}
	_ = st.Sources.Create(ctx, src)

	active := budgetArtifact(t, st, src.ID, "h1", 0.5)
	archived := budgetArtifact(t, st, src.ID, "h2", 0.1)
	arch := kb.Archive
	_ = st.Artifacts.Patch(ctx, archived, kb.ArtifactPatch{LifecycleTier: &arch})
	// An unscored (still-enriching) artifact with an encounter.
	un, _ := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", ContentHash: ptrStr("h3"), PrivacyTier: kb.Public})
	_ = st.Encounters.Record(ctx, &Encounter{ArtifactID: un.ID, SourceID: src.ID, SeenAt: now()})

	n, _ := st.Encounters.ActiveArtifactCountForSource(ctx, src.ID)
	if n != 1 {
		t.Errorf("count = %d, want 1 (only the active, scored artifact)", n)
	}
	_ = active
}
