package store

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

func TestRenderRepo_CreateGetList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	r := &RenderRecord{
		Format:      "markdown",
		SourceQuery: "tag:go",
		ArtifactIDs: []string{"01H0", "01H1"},
		OutputPath:  "/tmp/renders/x.md",
		PrivacyMax:  kb.Personal,
	}
	if err := st.Renders.Create(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.ID == "" {
		t.Fatal("Create should assign an ID")
	}
	if r.CreatedAt == "" {
		t.Fatal("Create should stamp CreatedAt")
	}

	got, err := st.Renders.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Format != "markdown" || got.SourceQuery != "tag:go" ||
		got.OutputPath != "/tmp/renders/x.md" || got.PrivacyMax != kb.Personal {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if len(got.ArtifactIDs) != 2 || got.ArtifactIDs[0] != "01H0" || got.ArtifactIDs[1] != "01H1" {
		t.Errorf("artifact_ids did not round-trip: %v", got.ArtifactIDs)
	}

	list, err := st.Renders.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != r.ID {
		t.Errorf("List = %d rows, want the one created", len(list))
	}
}

func TestRenderRepo_GetMissing(t *testing.T) {
	st := newTestStore(t)
	_, err := st.Renders.Get(context.Background(), "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestRenderRepo_EmptyArtifactIDs(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	r := &RenderRecord{Format: "markdown", PrivacyMax: kb.Public}
	if err := st.Renders.Create(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.Renders.Get(ctx, r.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.ArtifactIDs) != 0 {
		t.Errorf("empty render should round-trip to no artifact ids, got %v", got.ArtifactIDs)
	}
}
