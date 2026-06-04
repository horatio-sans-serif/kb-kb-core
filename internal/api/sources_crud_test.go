package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kb/internal/events"
	"kb/internal/store"
)

func crudServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	reg := fakeRegistry{have: map[string]bool{"rss": true}}
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger(), Connectors: reg}).Handler()
	return st, h
}

func mkSource(t *testing.T, st *store.Store) string {
	t.Helper()
	src := &store.Source{Kind: "PULL", Connector: "rss", Config: `{"url":"https://e.com/feed"}`}
	if err := st.Sources.Create(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return src.ID
}

func TestSources_Update(t *testing.T) {
	st, h := crudServer(t)
	id := mkSource(t, st)

	budget := 50
	body := `{"default_privacy":"sensitive","storage_budget":50,"schedule":{"every_seconds":1800}}`
	rec := doPatch(h, "/sources/"+id, body, "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.Sources.Get(context.Background(), id)
	if got.DefaultPrivacy != "sensitive" || got.StorageBudget == nil || *got.StorageBudget != budget {
		t.Errorf("update not applied: %+v", got)
	}
	if got.Schedule == nil || *got.Schedule != `{"every_seconds":1800}` {
		t.Errorf("schedule not updated: %v", got.Schedule)
	}
}

func TestSources_UpdateInvalidPrivacy(t *testing.T) {
	st, h := crudServer(t)
	id := mkSource(t, st)
	if rec := doPatch(h, "/sources/"+id, `{"default_privacy":"bogus"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid privacy", rec.Code)
	}
}

func TestSources_UpdateNonObjectConfig(t *testing.T) {
	st, h := crudServer(t)
	id := mkSource(t, st)
	if rec := doPatch(h, "/sources/"+id, `{"config":"not-an-object"}`, "tok"); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a non-object config", rec.Code)
	}
}

func TestSources_UpdateUnknownIs404(t *testing.T) {
	_, h := crudServer(t)
	if rec := doPatch(h, "/sources/01HMISSING", `{"default_privacy":"public"}`, "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown source", rec.Code)
	}
}

func TestSources_Disable(t *testing.T) {
	st, h := crudServer(t)
	id := mkSource(t, st)

	rec := doDelete(h, "/sources/"+id, "tok")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, _ := st.Sources.Get(context.Background(), id)
	if got.Enabled {
		t.Error("DELETE should disable the source (soft delete)")
	}
	// Enable it back via update.
	rec = doPatch(h, "/sources/"+id, `{"enabled":true}`, "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable status = %d", rec.Code)
	}
	got, _ = st.Sources.Get(context.Background(), id)
	if !got.Enabled {
		t.Error("update enabled:true should re-enable the source")
	}
}

func TestSources_DisableUnknownIs404(t *testing.T) {
	_, h := crudServer(t)
	if rec := doDelete(h, "/sources/01HMISSING", "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSources_CrudRequiresAuth(t *testing.T) {
	st, h := crudServer(t)
	id := mkSource(t, st)
	if rec := doPatch(h, "/sources/"+id, `{}`, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("PATCH without token = %d, want 401", rec.Code)
	}
	if rec := doDelete(h, "/sources/"+id, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without token = %d, want 401", rec.Code)
	}
}

func doPatch(h http.Handler, path, body, token string) *httptest.ResponseRecorder {
	return doMethod(h, http.MethodPatch, path, body, token)
}
func doDelete(h http.Handler, path, token string) *httptest.ResponseRecorder {
	return doMethod(h, http.MethodDelete, path, "", token)
}
func doMethod(h http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
