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

func topicTestServer(t *testing.T) (*store.Store, http.Handler) {
	t.Helper()
	st := newStore(t)
	h := NewServer(Deps{Store: st, Bus: events.NewBus(), Token: "tok", Logger: discardLogger()}).Handler()
	return st, h
}

func TestTopics_ListAndGet(t *testing.T) {
	ctx := context.Background()
	st, h := topicTestServer(t)
	a := seedReady(t, st, "ha", "Alpha", kb.Public, 0.5)
	b := seedReady(t, st, "hb", "Beta", kb.Public, 0.5)
	tp := &kb.Topic{Label: "things", Centroid: []float32{1}}
	if err := st.Topics.Create(ctx, tp); err != nil {
		t.Fatalf("topic: %v", err)
	}
	_ = st.Topics.Assign(ctx, a, tp.ID, 0.9)
	_ = st.Topics.Assign(ctx, b, tp.ID, 0.6)

	// List.
	rec := doGet(h, "/topics", "tok")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list struct {
		Topics []struct {
			ID          string `json:"id"`
			Label       string `json:"label"`
			MemberCount int    `json:"member_count"`
		} `json:"topics"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Topics) != 1 || list.Topics[0].ID != tp.ID || list.Topics[0].MemberCount != 2 {
		t.Fatalf("topics list = %+v, want one topic with 2 members", list.Topics)
	}

	// Get with members.
	grec := doGet(h, "/topics/"+tp.ID, "tok")
	if grec.Code != http.StatusOK {
		t.Fatalf("get status = %d", grec.Code)
	}
	var got struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		Members []struct {
			ArtifactID string  `json:"artifact_id"`
			Title      string  `json:"title"`
			Score      float64 `json:"score"`
		} `json:"members"`
	}
	_ = json.Unmarshal(grec.Body.Bytes(), &got)
	if got.ID != tp.ID || got.Label != "things" || len(got.Members) != 2 {
		t.Fatalf("topic get = %+v", got)
	}
	if got.Members[0].ArtifactID != a || got.Members[0].Title != "Alpha" || got.Members[0].Score != 0.9 {
		t.Errorf("members[0] = %+v, want Alpha(0.9) first", got.Members[0])
	}
}

func TestTopics_GetUnknownIs404(t *testing.T) {
	_, h := topicTestServer(t)
	if rec := doGet(h, "/topics/01HMISSING", "tok"); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an unknown topic", rec.Code)
	}
}

func TestTopics_RequiresAuth(t *testing.T) {
	_, h := topicTestServer(t)
	if rec := doGet(h, "/topics", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /topics without token = %d, want 401", rec.Code)
	}
}
