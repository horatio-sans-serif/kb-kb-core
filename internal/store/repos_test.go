package store

import (
	"context"
	"sort"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db := openTemp(t)
	if _, err := Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(db)
}

func TestULID_Sortable(t *testing.T) {
	const n = 50
	ids := make([]string, n)
	seen := make(map[string]bool, n)
	for i := range ids {
		ids[i] = NewID()
		if seen[ids[i]] {
			t.Fatalf("duplicate ULID minted: %s", ids[i])
		}
		seen[ids[i]] = true
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ULIDs not lexicographically sortable by creation order at %d: %q vs %q", i, ids[i], sorted[i])
		}
	}
}

func TestSource_CreateGet(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	s := &Source{Kind: "PULL", Connector: "hn"}
	if err := st.Sources.Create(ctx, s); err != nil {
		t.Fatalf("create source: %v", err)
	}
	if s.ID == "" {
		t.Fatal("Create did not assign an ID")
	}

	got, err := st.Sources.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got.Kind != "PULL" || got.Connector != "hn" {
		t.Errorf("got %+v, want kind=PULL connector=hn", got)
	}
}

func TestArtifact_UpsertByCanonical_Dedups(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	url := "https://example.com/a"

	a1, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url, Title: "first"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	a2, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url, Title: "second"})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	if a1.ID != a2.ID {
		t.Errorf("upsert created a duplicate: %s != %s", a1.ID, a2.ID)
	}
	list, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("artifact count = %d, want 1 (deduped by canonical URL)", len(list))
	}
}

func TestArtifact_SoftDelete_Hidden(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "note", Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Artifacts.SoftDelete(ctx, a.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	live, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("default list returned %d rows, want 0 (tombstoned excluded)", len(live))
	}

	all, err := st.Artifacts.ListIncludingDeleted(ctx)
	if err != nil {
		t.Fatalf("list incl deleted: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("list-including-deleted returned %d rows, want 1", len(all))
	}
}

func TestEncounter_CountIsRelevanceSignal(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	s1 := &Source{Kind: "PULL", Connector: "hn"}
	s2 := &Source{Kind: "PUSH", Connector: "extension"}
	if err := st.Sources.Create(ctx, s1); err != nil {
		t.Fatalf("create s1: %v", err)
	}
	if err := st.Sources.Create(ctx, s2); err != nil {
		t.Fatalf("create s2: %v", err)
	}

	url := "https://example.com/x"
	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	for _, sid := range []string{s1.ID, s2.ID} {
		if err := st.Encounters.Record(ctx, &Encounter{ArtifactID: a.ID, SourceID: sid}); err != nil {
			t.Fatalf("record encounter: %v", err)
		}
	}

	n, err := st.Encounters.CountForArtifact(ctx, a.ID)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Errorf("encounter count = %d, want 2 (seen from 2 sources)", n)
	}
}

func TestEnrichment_CreateGet(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "note", Title: "x"})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	e := &Enrichment{ArtifactID: a.ID, Kind: "summary", Payload: `{"text":"hi"}`, Model: "llama3.1", PromptVersion: "v1"}
	if err := st.Enrichments.Create(ctx, e); err != nil {
		t.Fatalf("create enrichment: %v", err)
	}

	got, err := st.Enrichments.Get(ctx, e.ID)
	if err != nil {
		t.Fatalf("get enrichment: %v", err)
	}
	if got.Kind != "summary" || got.Payload != `{"text":"hi"}` || got.ArtifactID != a.ID {
		t.Errorf("got %+v, want kind=summary payload={\"text\":\"hi\"} artifact=%s", got, a.ID)
	}
}
