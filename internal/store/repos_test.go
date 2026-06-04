package store

import (
	"context"
	"errors"
	"path/filepath"
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

func ptrStr(s string) *string { return &s }

// TestReadPool_SnapshotIsolation pins the read/write split the daemon relies on:
// an uncommitted write is invisible to the read pool, and a committed one is
// visible. The pipeline's finalize hook reads job state through the read pool
// right after the writer commits a terminal status, so read-after-commit
// visibility is a correctness requirement, not just an optimization.
func TestReadPool_SnapshotIsolation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.db")
	wdb, err := Open(path)
	if err != nil {
		t.Fatalf("open write: %v", err)
	}
	t.Cleanup(func() { _ = wdb.Close() })
	if _, err := Migrate(wdb); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	rdb, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Close() })
	st := NewWithReadPool(wdb, rdb)

	// An uncommitted write must not be visible through the read pool.
	rollback := errors.New("rollback")
	h1 := "h1"
	err = st.Tx(ctx, func(tx *Store) error {
		if _, err := tx.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "x", ContentHash: &h1}); err != nil {
			return err
		}
		arts, err := st.Artifacts.List(ctx) // read pool, mid-transaction
		if err != nil {
			t.Fatalf("read during tx: %v", err)
		}
		if len(arts) != 0 {
			t.Errorf("read pool saw %d artifacts during an open write tx, want 0", len(arts))
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("tx returned %v, want the rollback sentinel", err)
	}

	// After a committed write, the read pool sees it.
	h2 := "h2"
	committed, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "y", ContentHash: &h2})
	if err != nil {
		t.Fatalf("commit write: %v", err)
	}
	got, err := st.Artifacts.Get(ctx, committed.ID) // read pool
	if err != nil {
		t.Fatalf("read after commit: %v", err)
	}
	if got.ID != committed.ID {
		t.Errorf("read pool got %q, want the committed artifact %q", got.ID, committed.ID)
	}
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

func TestEncounter_DistinctSourceCount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	s1 := &Source{Kind: "PULL", Connector: "a"}
	s2 := &Source{Kind: "PUSH", Connector: "b"}
	if err := st.Sources.Create(ctx, s1); err != nil {
		t.Fatalf("s1: %v", err)
	}
	if err := st.Sources.Create(ctx, s2); err != nil {
		t.Fatalf("s2: %v", err)
	}
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "b", ContentHash: ptrStr("h")})
	if err != nil {
		t.Fatalf("artifact: %v", err)
	}
	// s1 encounters it twice (a re-poll), s2 once.
	for _, sid := range []string{s1.ID, s1.ID, s2.ID} {
		if err := st.Encounters.Record(ctx, &Encounter{ArtifactID: a.ID, SourceID: sid}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if total, _ := st.Encounters.CountForArtifact(ctx, a.ID); total != 3 {
		t.Errorf("total encounters = %d, want 3", total)
	}
	distinct, err := st.Encounters.DistinctSourceCountForArtifact(ctx, a.ID)
	if err != nil {
		t.Fatalf("distinct: %v", err)
	}
	if distinct != 2 {
		t.Errorf("distinct sources = %d, want 2 (a re-poll from one source must not inflate)", distinct)
	}
}

func TestEncounter_MaxSourceWeight(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// No encounters yet → neutral default 1.0.
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "b", ContentHash: ptrStr("h")})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}
	if w, err := st.Encounters.MaxSourceWeightForArtifact(ctx, a.ID); err != nil || w != 1.0 {
		t.Errorf("no-encounter weight = %v (err %v), want 1.0", w, err)
	}

	light := &Source{Kind: "PULL", Connector: "a", SourceWeight: 0.5}
	heavy := &Source{Kind: "PUSH", Connector: "b", SourceWeight: 3.0}
	if err := st.Sources.Create(ctx, light); err != nil {
		t.Fatalf("create light: %v", err)
	}
	if err := st.Sources.Create(ctx, heavy); err != nil {
		t.Fatalf("create heavy: %v", err)
	}
	// Source.Create takes the schema default (1.0) and ignores SourceWeight, so
	// set the weights explicitly via Update.
	light.SourceWeight, heavy.SourceWeight = 0.5, 3.0
	if err := st.Sources.Update(ctx, light); err != nil {
		t.Fatalf("update light: %v", err)
	}
	if err := st.Sources.Update(ctx, heavy); err != nil {
		t.Fatalf("update heavy: %v", err)
	}
	for _, sid := range []string{light.ID, heavy.ID} {
		if err := st.Encounters.Record(ctx, &Encounter{ArtifactID: a.ID, SourceID: sid}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	w, err := st.Encounters.MaxSourceWeightForArtifact(ctx, a.ID)
	if err != nil {
		t.Fatalf("max weight: %v", err)
	}
	if w != 3.0 {
		t.Errorf("max source weight = %v, want 3.0 (the most authoritative source)", w)
	}
}

func TestEnrichment_LatestByKind(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "b", ContentHash: ptrStr("h")})
	if err != nil {
		t.Fatalf("create artifact: %v", err)
	}

	if _, err := st.Enrichments.LatestByKind(ctx, a.ID, "embedding"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing kind should be ErrNotFound, got %v", err)
	}

	for _, p := range []string{`{"v":1}`, `{"v":2}`} {
		if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: p, Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("create enrichment: %v", err)
		}
	}
	// A different kind must not be returned.
	if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: a.ID, Kind: "summary", Payload: `{"v":9}`, Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("create summary: %v", err)
	}

	got, err := st.Enrichments.LatestByKind(ctx, a.ID, "embedding")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if got.Kind != "embedding" || got.Payload != `{"v":2}` {
		t.Errorf("latest embedding = %+v, want the most recent embedding payload {\"v\":2}", got)
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

	// App-level dedup (the find-then-insert upsert) is atomic only inside a Tx, the
	// way the ingest path uses it — the immediate transaction serializes the
	// find+insert so concurrent callers still collapse to one artifact.
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = st.Tx(ctx, func(tx *Store) error {
				_, err := tx.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", CanonicalURL: &url})
				return err
			})
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

func TestArtifact_ConcurrentUpsert_AutoCommit_NoDuplicates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	url := "https://example.com/autocommit-race"

	// Called directly (no enclosing Tx), the upsert wraps itself in an immediate
	// transaction, so the find-then-insert stays atomic and concurrent direct
	// callers still collapse to one artifact — the contract is enforced, not just
	// documented.
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
			t.Errorf("auto-commit upsert %d failed: %v", i, err)
		}
	}
	list, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("concurrent auto-commit upserts created %d artifacts, want 1", len(list))
	}
}

func TestArtifact_Patch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	a, err := st.Artifacts.UpsertByCanonical(ctx, &Artifact{Type: "webpage", Title: "x"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	body := "# fetched"
	status := ArtifactStatus("ready")
	score := 0.75
	if err := st.Artifacts.Patch(ctx, a.ID, ArtifactPatch{BodyMarkdown: &body, Status: &status, RelevanceScore: &score}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, _ := st.Artifacts.Get(ctx, a.ID)
	if got.BodyMarkdown != body || got.Status != "ready" || got.RelevanceScore == nil || *got.RelevanceScore != score {
		t.Errorf("after patch: body=%q status=%q score=%v", got.BodyMarkdown, got.Status, got.RelevanceScore)
	}
	if got.Title != "x" {
		t.Errorf("narrow patch changed an unset field: title = %q", got.Title)
	}
	if err := st.Artifacts.Patch(ctx, a.ID, ArtifactPatch{}); err != nil {
		t.Errorf("empty patch should be a no-op: %v", err)
	}
}

func TestArtifact_UpsertByContentHash_Dedups(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	h := "0000000000000000000000000000000000000000000000000000000000000001"

	a1, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", ContentHash: &h, BodyMarkdown: "hello"})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	a2, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", ContentHash: &h, BodyMarkdown: "hello reflowed"})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if a1.ID != a2.ID {
		t.Errorf("content-hash upsert created a duplicate: %s != %s", a1.ID, a2.ID)
	}
	list, err := st.Artifacts.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("artifacts = %d, want 1 (deduped by content hash)", len(list))
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

func TestSource_ListUpdateSetEnabled(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sched := `{"cron":"0 * * * *"}`
	budget := 1000
	s := &Source{Kind: "PULL", Connector: "rss", DefaultPrivacy: "sensitive", Schedule: &sched, StorageBudget: &budget}
	if err := st.Sources.Create(ctx, s); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The previously-unmapped columns round-trip.
	got, err := st.Sources.Get(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Schedule == nil || *got.Schedule != sched {
		t.Errorf("schedule = %v, want %q", got.Schedule, sched)
	}
	if got.StorageBudget == nil || *got.StorageBudget != budget {
		t.Errorf("storage_budget = %v, want %d", got.StorageBudget, budget)
	}
	if !got.Enabled || got.SourceWeight != 1.0 {
		t.Errorf("new source defaults: enabled=%v weight=%v, want true/1.0", got.Enabled, got.SourceWeight)
	}

	if list, err := st.Sources.List(ctx); err != nil || len(list) != 1 || list[0].ID != s.ID {
		t.Errorf("list = %v (err %v), want one source", list, err)
	}

	got.DefaultPrivacy = "restricted"
	got.SourceWeight = 2.5
	if err := st.Sources.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	if re, _ := st.Sources.Get(ctx, s.ID); re.DefaultPrivacy != "restricted" || re.SourceWeight != 2.5 {
		t.Errorf("after update: %+v, want privacy=restricted weight=2.5", re)
	}

	if err := st.Sources.SetEnabled(ctx, s.ID, false); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	if re, _ := st.Sources.Get(ctx, s.ID); re.Enabled {
		t.Error("source should be disabled after SetEnabled(false)")
	}
}

func TestSource_GetNotFound(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Sources.Get(context.Background(), "01NONE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEnrichment_GetNotFound(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.Enrichments.Get(context.Background(), "01NONE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestEncounter_NoForeignKeyEnforcement(t *testing.T) {
	st := newTestStore(t)
	// Synced tables carry no CHECKED foreign keys: cr-sqlite (§8.16) forbids them,
	// because a replica can receive a child row before its parent. So a dangling
	// reference inserts WITHOUT a DB error — referential integrity is maintained by
	// the application (parent-before-child writes in a Tx) and the reconcile sweep,
	// not the engine. This guards against re-adding a FK that would break
	// crsql_as_crr (migration 0013).
	err := st.Encounters.Record(context.Background(),
		&Encounter{ArtifactID: "01NOSUCH", SourceID: "01NOSUCH"})
	if err != nil {
		t.Errorf("encounters must not enforce a foreign key (cr-sqlite requirement), got %v", err)
	}
}

func TestStore_TxPanicRollsBack(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected the panic to propagate out of Tx")
			}
		}()
		_ = st.Tx(ctx, func(tx *Store) error {
			if err := tx.Sources.Create(ctx, &Source{Kind: "PULL", Connector: "x"}); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	var n int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("sources = %d after a panicking Tx, want 0 (rolled back)", n)
	}
}

func TestEnrichment_EngagedEmbeddings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	mk := func(hash string, engage bool) string {
		a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: hash, ContentHash: &hash})
		if err != nil {
			t.Fatalf("create %s: %v", hash, err)
		}
		if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: a.ID, Kind: "embedding", Payload: `{"vector":[1,0]}`, Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("embed %s: %v", hash, err)
		}
		if engage {
			if _, err := st.Artifacts.RecordEngagement(ctx, a.ID); err != nil {
				t.Fatalf("engage %s: %v", hash, err)
			}
		}
		return a.ID
	}
	engaged := mk("h1", true)
	mk("h2", false)            // not engaged → excluded
	excluded := mk("h3", true) // engaged but passed as excludeID

	rows, err := st.Enrichments.EngagedEmbeddings(ctx, excluded, 10)
	if err != nil {
		t.Fatalf("engaged embeddings: %v", err)
	}
	if len(rows) != 1 || rows[0].ArtifactID != engaged {
		t.Errorf("rows = %+v, want only the engaged, non-excluded artifact %s", rows, engaged)
	}
}

func TestArtifact_RecordEngagement(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	h := "h"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "b", ContentHash: &h})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.LastEngagedAt != nil {
		t.Fatal("a fresh artifact must have no engagement")
	}

	ok, err := st.Artifacts.RecordEngagement(ctx, a.ID)
	if err != nil || !ok {
		t.Fatalf("record engagement: ok=%v err=%v", ok, err)
	}
	got, _ := st.Artifacts.Get(ctx, a.ID)
	if got.LastEngagedAt == nil {
		t.Error("last_engaged_at not stamped after engagement")
	}

	// Unknown (or tombstoned) artifact → false, no error.
	if ok, err := st.Artifacts.RecordEngagement(ctx, "01NONEXISTENT"); err != nil || ok {
		t.Errorf("unknown artifact: ok=%v err=%v, want false/nil", ok, err)
	}
}

func TestArtifact_KeywordSearch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	h1, h2 := "h1", "h2"
	rust, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: "Rust async", BodyMarkdown: "tokio runtime", ContentHash: &h1})
	if err != nil {
		t.Fatalf("create rust: %v", err)
	}
	if _, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", Title: "Go channels", BodyMarkdown: "goroutines", ContentHash: &h2}); err != nil {
		t.Fatalf("create go: %v", err)
	}

	hits, err := st.Artifacts.SearchKeyword(ctx, `"rust"`, 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].ArtifactID != rust.ID {
		t.Fatalf("hits = %+v, want only the rust artifact", hits)
	}
	if hits[0].Score <= 0 {
		t.Errorf("score = %v, want positive (negated bm25)", hits[0].Score)
	}

	// A body patch is reindexed: the new term is searchable, the old one is gone.
	body := "now mentions kubernetes"
	if err := st.Artifacts.Patch(ctx, rust.ID, ArtifactPatch{BodyMarkdown: &body}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if hits, _ := st.Artifacts.SearchKeyword(ctx, `"kubernetes"`, 10); len(hits) != 1 || hits[0].ArtifactID != rust.ID {
		t.Errorf("patched body term should be searchable, got %+v", hits)
	}
	if hits, _ := st.Artifacts.SearchKeyword(ctx, `"tokio"`, 10); len(hits) != 0 {
		t.Errorf("old body term should not match after reindex, got %+v", hits)
	}

	// Tombstoned artifacts are excluded from results.
	if err := st.Artifacts.SoftDelete(ctx, rust.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if hits, _ := st.Artifacts.SearchKeyword(ctx, `"kubernetes"`, 10); len(hits) != 0 {
		t.Errorf("tombstoned artifact must not appear in search, got %+v", hits)
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

func TestEnrichment_LatestEmbeddings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	live, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "a", ContentHash: ptrStr("h1")})
	if err != nil {
		t.Fatalf("create live: %v", err)
	}
	gone, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", BodyMarkdown: "b", ContentHash: ptrStr("h2")})
	if err != nil {
		t.Fatalf("create gone: %v", err)
	}

	// live: two embeddings (only the latest should be returned) + a non-embedding.
	for _, p := range []string{`{"v":1}`, `{"v":2}`} {
		if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: live.ID, Kind: "embedding", Payload: p, Model: "m", PromptVersion: "v1"}); err != nil {
			t.Fatalf("create embedding: %v", err)
		}
	}
	if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: live.ID, Kind: "summary", Payload: `{"x":1}`, Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("create summary: %v", err)
	}
	// tombstoned artifact's embedding must be excluded.
	if err := st.Enrichments.Create(ctx, &Enrichment{ArtifactID: gone.ID, Kind: "embedding", Payload: `{"v":9}`, Model: "m", PromptVersion: "v1"}); err != nil {
		t.Fatalf("create gone embedding: %v", err)
	}
	if err := st.Artifacts.SoftDelete(ctx, gone.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	type row struct{ id, payload string }
	var rows []row
	if err := st.Enrichments.ForEachLatestEmbedding(ctx, func(artifactID, payload string) error {
		rows = append(rows, row{artifactID, payload})
		return nil
	}); err != nil {
		t.Fatalf("for each latest embedding: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (live artifact, latest only, tombstoned excluded)", len(rows))
	}
	if rows[0].id != live.ID || rows[0].payload != `{"v":2}` {
		t.Errorf("row = %+v, want live artifact with the latest payload {\"v\":2}", rows[0])
	}
}
