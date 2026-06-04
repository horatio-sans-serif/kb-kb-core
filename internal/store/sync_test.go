package store

import (
	"context"
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
