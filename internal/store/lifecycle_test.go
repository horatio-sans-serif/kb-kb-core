package store

import (
	"context"
	"testing"

	"kb/internal/kb"
)

func TestArtifactRepo_LifecycleCounts(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	mk := func(hash string, tier kb.LifecycleTier, tombstone bool) string {
		a, err := st.Artifacts.UpsertByContentHash(ctx, &Artifact{Type: "note", ContentHash: &hash, PrivacyTier: kb.Public})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}
		_ = st.Artifacts.Patch(ctx, a.ID, kb.ArtifactPatch{LifecycleTier: &tier})
		if tombstone {
			_ = st.Artifacts.SoftDelete(ctx, a.ID)
		}
		return a.ID
	}
	mk("h1", kb.Active, false)
	mk("h2", kb.Active, false)
	mk("h3", kb.Archive, false)
	mk("h4", kb.Purged, true)

	c, err := st.Artifacts.LifecycleCounts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.Active != 2 || c.Archive != 1 || c.Purged != 1 {
		t.Errorf("counts = %+v, want active 2, archive 1, purged 1", c)
	}
}
