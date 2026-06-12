package surface

import (
	"context"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

func ready(t *testing.T, st *store.Store, hash, title string, tier kb.PrivacyTier, score float64, status string) string {
	t.Helper()
	ctx := context.Background()
	got, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: title, BodyMarkdown: "b", ContentHash: &hash, PrivacyTier: tier})
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	stt := kb.ArtifactStatus(status)
	if err := st.Artifacts.Patch(ctx, got.ID, kb.ArtifactPatch{Status: &stt, RelevanceScore: &score}); err != nil {
		t.Fatalf("patch %s: %v", title, err)
	}
	return got.ID
}

func TestSurfacer_CeilingAndReadyOnly(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	hi := ready(t, st, "ha", "High", kb.Public, 0.9, "ready")
	sens := ready(t, st, "hs", "Sensitive", kb.Sensitive, 0.8, "ready")
	lo := ready(t, st, "hl", "Low", kb.Public, 0.4, "ready")
	ready(t, st, "hd", "Draft", kb.Public, 0.95, "ingested") // not ready → never surfaced

	s := New(st)

	// Default-ish: a personal ceiling excludes the sensitive one; ready-only excludes the draft.
	got, err := s.Surface(ctx, Options{PrivacyCeiling: kb.Personal, Limit: 10})
	if err != nil {
		t.Fatalf("surface: %v", err)
	}
	if len(got) != 2 || got[0].ID != hi || got[1].ID != lo {
		t.Errorf("personal ceiling = %v, want [High, Low]", titles(got))
	}

	// A sensitive ceiling lets the sensitive artifact through, ranked by score.
	got2, _ := s.Surface(ctx, Options{PrivacyCeiling: kb.Sensitive, Limit: 10})
	if len(got2) != 3 || got2[0].ID != hi || got2[1].ID != sens || got2[2].ID != lo {
		t.Errorf("sensitive ceiling = %v, want [High, Sensitive, Low]", titles(got2))
	}

	// The owner ceiling (Restricted) = all tiers.
	all, _ := s.Surface(ctx, Options{PrivacyCeiling: kb.Restricted, Limit: 10})
	if len(all) != 3 {
		t.Errorf("restricted ceiling should include all tiers, got %d", len(all))
	}

	// No / invalid ceiling now fails CLOSED (surfaces nothing) — it must not fall open
	// to all tiers. A read path that forgets the ceiling shows too little, never too much.
	none, _ := s.Surface(ctx, Options{Limit: 10})
	if len(none) != 0 {
		t.Errorf("an unset ceiling must fail closed (0 artifacts), got %d", len(none))
	}
}

func titles(arts []*kb.Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Title
	}
	return out
}
