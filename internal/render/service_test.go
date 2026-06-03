package render

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/storetest"
)

func TestService_RenderEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	dir := t.TempDir()

	reg := NewRegistry()
	reg.Register(NewMarkdown(dir))
	svc := NewService(st, reg)

	h1, h2 := "h1", "h2"
	a1, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "First", BodyMarkdown: "one", ContentHash: &h1, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("insert a1: %v", err)
	}
	a2, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "Second", BodyMarkdown: "two", ContentHash: &h2, PrivacyTier: kb.Personal})
	if err != nil {
		t.Fatalf("insert a2: %v", err)
	}

	rec, err := svc.Render(ctx, Request{
		Format:      "markdown",
		ArtifactIDs: []string{a1.ID, a2.ID},
		Title:       "Both",
		SourceQuery: "manual",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if rec.Format != "markdown" || rec.PrivacyMax != kb.Personal {
		t.Errorf("record = %+v, want markdown/personal", rec)
	}
	if len(rec.ArtifactIDs) != 2 {
		t.Errorf("record should reference both artifacts, got %v", rec.ArtifactIDs)
	}

	// The output file exists and contains both bodies.
	b, err := os.ReadFile(rec.OutputPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if doc := string(b); !strings.Contains(doc, "one") || !strings.Contains(doc, "two") {
		t.Errorf("output missing content:\n%s", doc)
	}

	// The render is persisted and retrievable.
	got, err := st.Renders.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("get persisted render: %v", err)
	}
	if got.OutputPath != rec.OutputPath {
		t.Errorf("persisted output path = %q, want %q", got.OutputPath, rec.OutputPath)
	}
}

// errRenderer is a Renderer that always fails, for the no-orphan-row test.
type errRenderer struct{}

func (errRenderer) Format() string { return "boom" }
func (errRenderer) Render(context.Context, []*kb.Artifact, kb.RenderOpts) (kb.Rendered, error) {
	return kb.Rendered{}, errors.New("render exploded")
}

func TestService_RendererFailure_NoRow(t *testing.T) {
	// The "file and provenance row share one identity" invariant depends on the
	// row being written only after a successful render. A renderer failure must
	// leave no orphan row.
	ctx := context.Background()
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register(errRenderer{})
	svc := NewService(st, reg)

	_, err := svc.Render(ctx, Request{Format: "boom"})
	if err == nil {
		t.Fatal("expected the renderer error to propagate")
	}
	if !strings.Contains(err.Error(), "render exploded") {
		t.Errorf("renderer error not wrapped through: %v", err)
	}
	if list, _ := st.Renders.List(ctx); len(list) != 0 {
		t.Errorf("a failed render must not persist a row, got %d", len(list))
	}
}

func TestService_UnknownFormat(t *testing.T) {
	svc := NewService(storetest.New(t), NewRegistry())
	_, err := svc.Render(context.Background(), Request{Format: "wat"})
	if !errors.Is(err, ErrUnknownFormat) {
		t.Errorf("Render(unknown format) = %v, want ErrUnknownFormat", err)
	}
}

func TestService_PrivacyCeilingExcludes(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register(NewMarkdown(t.TempDir()))
	svc := NewService(st, reg)

	hp, hr := "hp", "hr"
	pub, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "Public", BodyMarkdown: "open", ContentHash: &hp, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("insert pub: %v", err)
	}
	restricted, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "Secret", BodyMarkdown: "classified", ContentHash: &hr, PrivacyTier: kb.Restricted})
	if err != nil {
		t.Fatalf("insert restricted: %v", err)
	}

	rec, err := svc.Render(ctx, Request{
		Format:         "markdown",
		ArtifactIDs:    []string{pub.ID, restricted.ID},
		PrivacyCeiling: kb.Personal, // restricted is above this ceiling
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if len(rec.ArtifactIDs) != 1 || rec.ArtifactIDs[0] != pub.ID {
		t.Errorf("ceiling should keep only the public artifact, got %v", rec.ArtifactIDs)
	}
	if rec.PrivacyMax != kb.Public {
		t.Errorf("PrivacyMax = %q, want public (the restricted one was filtered out)", rec.PrivacyMax)
	}
	b, _ := os.ReadFile(rec.OutputPath)
	if strings.Contains(string(b), "classified") {
		t.Error("filtered-out restricted content leaked into the render")
	}
}

func TestService_InvalidCeilingIsRejected(t *testing.T) {
	// A non-empty but invalid ceiling must fail closed: silently including every
	// tier because the string didn't parse would be a fail-open privacy hole.
	ctx := context.Background()
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register(NewMarkdown(t.TempDir()))
	svc := NewService(st, reg)

	h := "h"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "x", BodyMarkdown: "y", ContentHash: &h, PrivacyTier: kb.Restricted})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err = svc.Render(ctx, Request{Format: "markdown", ArtifactIDs: []string{a.ID}, PrivacyCeiling: "bogus"})
	if !errors.Is(err, ErrInvalidCeiling) {
		t.Errorf("Render(invalid ceiling) = %v, want ErrInvalidCeiling", err)
	}
	// And nothing was written for the rejected request.
	if list, _ := st.Renders.List(ctx); len(list) != 0 {
		t.Errorf("a rejected render must not persist a row, got %d", len(list))
	}
}

func TestService_SkipsMissingArtifacts(t *testing.T) {
	ctx := context.Background()
	st := storetest.New(t)
	reg := NewRegistry()
	reg.Register(NewMarkdown(t.TempDir()))
	svc := NewService(st, reg)

	h := "h"
	a, err := st.Artifacts.UpsertByContentHash(ctx, &kb.Artifact{Type: "note", Title: "Here", BodyMarkdown: "x", ContentHash: &h, PrivacyTier: kb.Public})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	rec, err := svc.Render(ctx, Request{Format: "markdown", ArtifactIDs: []string{a.ID, "01HMISSING"}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(rec.ArtifactIDs) != 1 || rec.ArtifactIDs[0] != a.ID {
		t.Errorf("missing artifact should be skipped, got %v", rec.ArtifactIDs)
	}
}
