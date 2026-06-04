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

func entityServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	return st, h
}

func TestEntities_ListAndGet(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	a := seedReady(t, st, "ha", "Doc A", kb.Public, 0.5)
	b := seedReady(t, st, "hb", "Doc B", kb.Public, 0.5)
	id, _ := st.Graph.UpsertEntity(ctx, &kb.Entity{Kind: "technology", Name: "Go", Aliases: []string{"Golang"}})
	_ = st.Graph.LinkArtifactEntity(ctx, a, id)
	_ = st.Graph.LinkArtifactEntity(ctx, b, id)

	rec := doGet(h, "/entities", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list struct {
		Entities []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			Kind          string `json:"kind"`
			ArtifactCount int    `json:"artifact_count"`
		} `json:"entities"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Entities) != 1 || list.Entities[0].Name != "Go" || list.Entities[0].ArtifactCount != 2 {
		t.Fatalf("entities = %+v, want Go(2)", list.Entities)
	}

	grec := doGet(h, "/entities/"+id, "tok")
	if grec.Code != http.StatusOK {
		t.Fatalf("get status = %d", grec.Code)
	}
	var got struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		Aliases   []string `json:"aliases"`
		Artifacts []struct {
			ArtifactID string `json:"artifact_id"`
			Title      string `json:"title"`
		} `json:"artifacts"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &got)
	if got.Name != "Go" || len(got.Aliases) != 1 || len(got.Artifacts) != 2 {
		t.Errorf("entity detail = %+v, want Go with 2 artifacts", got)
	}
}

func TestEntities_GetUnknownIs404(t *testing.T) {
	_, h := entityServer(t)
	if rec := doGet(h, "/entities/01HMISSING", "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestEntities_Backlinks(t *testing.T) {
	ctx := context.Background()
	st, h := entityServer(t)
	target := seedReady(t, st, "ht", "Target", kb.Public, 0.5)
	a := seedReady(t, st, "ha", "Referrer", kb.Public, 0.5)
	_ = st.Graph.AddLink(ctx, a, target, "references", 1.0)

	rec := doGet(h, "/artifacts/"+target+"/backlinks", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var out struct {
		Backlinks []struct {
			ArtifactID string `json:"artifact_id"`
			Title      string `json:"title"`
			Relation   string `json:"relation"`
		} `json:"backlinks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Backlinks) != 1 || out.Backlinks[0].ArtifactID != a || out.Backlinks[0].Title != "Referrer" || out.Backlinks[0].Relation != "references" {
		t.Errorf("backlinks = %+v, want [Referrer/references]", out.Backlinks)
	}
}

func TestEntities_RequiresAuth(t *testing.T) {
	_, h := entityServer(t)
	if rec := doGet(h, "/entities", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /entities without token = %d, want 401", rec.Code)
	}
}
