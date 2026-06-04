package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSyncRepo_Watermark(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// An unknown peer has watermark 0 (pull from the start).
	if v, err := st.Sync.Watermark(ctx, "http://peer"); err != nil || v != 0 {
		t.Fatalf("unknown peer watermark = %d (err %v), want 0", v, err)
	}

	if err := st.Sync.SetWatermark(ctx, "http://peer", 5); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Sync.Watermark(ctx, "http://peer"); v != 5 {
		t.Errorf("watermark = %d, want 5", v)
	}

	// Monotonic: a lower value never rewinds the watermark.
	if err := st.Sync.SetWatermark(ctx, "http://peer", 3); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Sync.Watermark(ctx, "http://peer"); v != 5 {
		t.Errorf("watermark = %d after a lower set, want 5 (monotonic)", v)
	}

	// A higher value advances it.
	if err := st.Sync.SetWatermark(ctx, "http://peer", 9); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Sync.Watermark(ctx, "http://peer"); v != 9 {
		t.Errorf("watermark = %d, want 9", v)
	}

	// Peers are tracked independently.
	if v, _ := st.Sync.Watermark(ctx, "http://other"); v != 0 {
		t.Errorf("other peer watermark = %d, want 0", v)
	}
}

func TestSyncRepo_PersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kb.db")

	// Set a watermark, then close the database.
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(db1); err != nil {
		t.Fatal(err)
	}
	if err := New(db1).Sync.SetWatermark(ctx, "http://peer", 42); err != nil {
		t.Fatal(err)
	}
	_ = db1.Close()

	// Reopen the same file: the watermark survives, so a restart does not re-pull
	// the whole history.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if _, err := Migrate(db2); err != nil {
		t.Fatal(err)
	}
	if v, _ := New(db2).Sync.Watermark(ctx, "http://peer"); v != 42 {
		t.Errorf("watermark after reopen = %d, want 42 (persisted)", v)
	}
}
