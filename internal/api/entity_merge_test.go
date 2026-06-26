package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/kb"
	"kb/internal/store"
)

func doPut(h http.Handler, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEntities_Merge(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	a := seedReady(t, st, "ha", "Doc A", kb.Public, 0.5)
	b := seedReady(t, st, "hb", "Doc B", kb.Public, 0.5)
	survivor, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go", Aliases: []string{"Go lang"}})
	loser, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Golang", Aliases: []string{"golang.org"}})
	_ = st.Graph.LinkArtifactEntity(ctx, a, survivor)
	_ = st.Graph.LinkArtifactEntity(ctx, b, loser)

	rec := doPost(h, "/entities/"+survivor+"/merge", `{"loser_id":"`+loser+`"}`, "tok")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("merge status = %d: %s", rec.Code, rec.Body.String())
	}

	// Loser gone.
	if _, err := st.Graph.GetEntity(ctx, loser); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("loser should be deleted, err = %v", err)
	}
	// Survivor mentions both artifacts and unions aliases.
	arts, _ := st.Graph.ArtifactsForEntity(ctx, survivor)
	if len(arts) != 2 {
		t.Errorf("survivor should mention 2 artifacts, got %v", arts)
	}
	e, _ := st.Graph.GetEntity(ctx, survivor)
	if len(e.Aliases) != 2 {
		t.Errorf("survivor aliases = %v, want union of 2", e.Aliases)
	}
}

func TestEntities_MergeUnknownIs404(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	survivor, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Ada"})

	// Unknown loser.
	if rec := doPost(h, "/entities/"+survivor+"/merge", `{"loser_id":"01HMISSING"}`, "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown loser = %d, want 404", rec.Code)
	}
	// Unknown survivor (path id).
	if rec := doPost(h, "/entities/01HMISSING/merge", `{"loser_id":"`+survivor+`"}`, "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown survivor = %d, want 404", rec.Code)
	}
}

func TestEntities_MergeSelfIs400(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "person", Name: "Ada"})
	if rec := doPost(h, "/entities/"+id+"/merge", `{"loser_id":"`+id+`"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("self-merge = %d, want 400", rec.Code)
	}
	// Missing loser_id is a bad request.
	if rec := doPost(h, "/entities/"+id+"/merge", `{}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("missing loser_id = %d, want 400", rec.Code)
	}
}

func TestEntities_SetAliases(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "org", Name: "Acme", Aliases: []string{"old"}})

	rec := doPut(h, "/entities/"+id+"/aliases", `{"aliases":["Acme Inc","ACME"]}`, "tok")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("set aliases status = %d: %s", rec.Code, rec.Body.String())
	}
	e, _ := st.Graph.GetEntity(ctx, id)
	if len(e.Aliases) != 2 || e.Aliases[0] != "Acme Inc" {
		t.Errorf("aliases = %v, want [Acme Inc, ACME]", e.Aliases)
	}

	// Empty list clears.
	if rec := doPut(h, "/entities/"+id+"/aliases", `{"aliases":[]}`, "tok"); rec.Code != http.StatusNoContent {
		t.Fatalf("clear aliases = %d", rec.Code)
	}
	e, _ = st.Graph.GetEntity(ctx, id)
	if len(e.Aliases) != 0 {
		t.Errorf("aliases after clear = %v, want empty", e.Aliases)
	}
}

func TestEntities_SetAliasesUnknownIs404(t *testing.T) {
	_, h := entityServer(t)
	if rec := doPut(h, "/entities/01HMISSING/aliases", `{"aliases":["x"]}`, "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestEntities_MergeAndAliasesRequireAuth(t *testing.T) {
	_, h := entityServer(t)
	if rec := doPost(h, "/entities/x/merge", `{"loser_id":"y"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("merge without token = %d, want 401", rec.Code)
	}
	if rec := doPut(h, "/entities/x/aliases", `{"aliases":[]}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("set aliases without token = %d, want 401", rec.Code)
	}
}
