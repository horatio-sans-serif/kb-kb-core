package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
)

func conflictTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	return st, h
}

func seedConflict(t *testing.T, st *store.Store) (conflictID, claimAID string) {
	t.Helper()
	ctx := context.Background()
	a := seedReady(t, st, "h1", "Doc", kb.Public, 0.5)
	ca := &kb.Claim{ArtifactID: a, Statement: "X is true"}
	cb := &kb.Claim{ArtifactID: a, Statement: "X is false"}
	if err := st.Claims.Create(ctx, ca); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	if err := st.Claims.Create(ctx, cb); err != nil {
		t.Fatalf("claim b: %v", err)
	}
	cf := &kb.Conflict{ClaimA: ca.ID, ClaimB: cb.ID, Rationale: "mutually exclusive"}
	if err := st.Conflicts.Create(ctx, cf); err != nil {
		t.Fatalf("conflict: %v", err)
	}
	open, _ := st.Conflicts.ListByStatus(ctx, "open", 10, kb.Restricted)
	return open[0].ID, ca.ID
}

func TestConflicts_ListOpen(t *testing.T) {
	st, h := conflictTestServer(t)
	id, _ := seedConflict(t, st)

	rec := doGet(h, "/conflicts", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Conflicts []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			ClaimA struct {
				Statement string `json:"statement"`
			} `json:"claim_a"`
			ClaimB struct {
				Statement string `json:"statement"`
			} `json:"claim_b"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Conflicts) != 1 || out.Conflicts[0].ID != id || out.Conflicts[0].Status != "open" {
		t.Fatalf("conflicts = %+v, want one open", out.Conflicts)
	}
	if out.Conflicts[0].ClaimA.Statement == "" || out.Conflicts[0].ClaimB.Statement == "" {
		t.Errorf("conflict should include both claim statements: %+v", out.Conflicts[0])
	}
}

func TestConflicts_Resolve(t *testing.T) {
	st, h := conflictTestServer(t)
	id, _ := seedConflict(t, st)

	rec := doPost(h, "/conflicts/"+id, `{"status":"resolved","rationale":"A is correct"}`, "tok")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if open, _ := st.Conflicts.ListByStatus(context.Background(), "open", 10, kb.Restricted); len(open) != 0 {
		t.Errorf("resolved conflict should not be open, got %d", len(open))
	}
	resolved, _ := st.Conflicts.ListByStatus(context.Background(), "resolved", 10, kb.Restricted)
	if len(resolved) != 1 || resolved[0].Rationale != "A is correct" {
		t.Errorf("resolved = %+v", resolved)
	}
}

func TestConflicts_ResolveUnknownIs404(t *testing.T) {
	_, h := conflictTestServer(t)
	if rec := doPost(h, "/conflicts/01HMISSING", `{"status":"dismissed"}`, "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown conflict", rec.Code)
	}
}

func TestConflicts_InvalidStatusIs400(t *testing.T) {
	st, h := conflictTestServer(t)
	id, _ := seedConflict(t, st)
	if rec := doPost(h, "/conflicts/"+id, `{"status":"open"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (can only resolve/dismiss)", rec.Code)
	}
	if rec := doGet(h, "/conflicts?status=bogus", "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("list status=bogus = %d, want 400", rec.Code)
	}
}

func TestConflicts_RequiresAuth(t *testing.T) {
	st, h := conflictTestServer(t)
	id, _ := seedConflict(t, st)
	if rec := doGet(h, "/conflicts", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /conflicts without token = %d, want 401", rec.Code)
	}
	if rec := doPost(h, "/conflicts/"+id, `{"status":"resolved"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without token = %d, want 401", rec.Code)
	}
}
