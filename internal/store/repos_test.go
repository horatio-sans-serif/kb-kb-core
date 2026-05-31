package store

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
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
	st := newTestStore(t)
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
	st := newTestStore(t)
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
	st := newTestStore(t)
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
	st := newTestStore(t)
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

func TestArtifact_UpsertAfterSoftDelete(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	url := "https://example.com/resurrect"

	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := st.Artifacts.SoftDelete(ctx, a.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Re-seeing a tombstoned URL must not hard-fail; the tombstone frees the
	// canonical slot, so a new artifact is created.
	b, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url})
	if err != nil {
		t.Fatalf("upsert after soft delete: %v", err)
	}
	if b.ID == a.ID {
		t.Errorf("expected a new artifact after tombstone, got the deleted one (%s)", a.ID)
	}

	live, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(live) != 1 || live[0].ID != b.ID {
		t.Errorf("live list = %d rows, want only the new artifact %s", len(live), b.ID)
	}
	all, err := st.Artifacts.ListIncludingDeleted(ctx)
	if err != nil {
		t.Fatalf("list incl deleted: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("list-including-deleted = %d rows, want 2", len(all))
	}
}

func TestArtifact_ConcurrentUpsert_NoDuplicates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	url := "https://example.com/race"

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent upsert %d failed: %v", i, err)
		}
	}
	list, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("concurrent upserts created %d artifacts, want 1", len(list))
	}
}

func TestArtifact_GetNotFound(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Artifacts.Get(context.Background(), "01NONEXISTENT")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestArtifact_NullTitleBodyRead(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Simulate a row written with NULL title/body (e.g. by a merge from another
	// replica). Reads must not fail.
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO artifacts(id, type, title, body_markdown, first_seen_at, last_seen_at)
		 VALUES ('01NULLROW', 'note', NULL, NULL, '2026-05-31', '2026-05-31')`)
	if err != nil {
		t.Fatalf("insert NULL row: %v", err)
	}

	a, err := st.Artifacts.Get(ctx, "01NULLROW")
	if err != nil {
		t.Fatalf("Get on NULL title/body row: %v", err)
	}
	if a.Title != "" || a.BodyMarkdown != "" {
		t.Errorf("NULL title/body should read as empty strings, got %q / %q", a.Title, a.BodyMarkdown)
	}
}

func TestStore_TxCommit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	var id string
	err := st.Tx(ctx, func(tx *Store) error {
		s := &Source{Kind: "PULL", Connector: "x"}
		if err := tx.Sources.Create(ctx, s); err != nil {
			return err
		}
		id = s.ID
		return nil
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	if _, err := st.Sources.Get(ctx, id); err != nil {
		t.Errorf("source not committed: %v", err)
	}
}

func TestStore_TxRollback(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := st.Tx(ctx, func(tx *Store) error {
		if err := tx.Sources.Create(ctx, &Source{Kind: "PULL", Connector: "x"}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Tx error = %v, want boom", err)
	}

	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rollback failed: %d sources persisted, want 0", n)
	}
}

func TestStore_NestedTxDisallowed(t *testing.T) {
	st := newTestStore(t)
	err := st.Tx(context.Background(), func(tx *Store) error {
		return tx.Tx(context.Background(), func(*Store) error { return nil })
	})
	if err == nil {
		t.Fatal("nested Tx should return an error, got nil")
	}
}

func TestEnrichment_CreateGet(t *testing.T) {
	st := newTestStore(t)
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
