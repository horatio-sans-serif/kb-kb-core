package store

import (
	"context"
	"errors"
	"testing"

	"kb/internal/kb"
)

func TestTopicRepo_CreateListAssign(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := mkArtifact(t, st, "h1")

	tp := &Topic{Label: "go", Centroid: []float32{0.1, 0.2, 0.3}}
	if err := st.Topics.Create(ctx, tp); err != nil {
		t.Fatalf("create: %v", err)
	}
	if tp.ID == "" {
		t.Fatal("Create should assign an ID")
	}
	// A topic without a centroid is excluded from the nearest-search set.
	if err := st.Topics.Create(ctx, &Topic{Label: "no-centroid"}); err != nil {
		t.Fatalf("create 2: %v", err)
	}

	withC, err := st.Topics.ListWithCentroids(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(withC) != 1 || withC[0].Label != "go" || len(withC[0].Centroid) != 3 || withC[0].Centroid[2] != 0.3 {
		t.Errorf("centroid topics = %+v, want [go(0.1,0.2,0.3)]", withC)
	}

	// Assign the artifact (idempotent), count, and read back.
	for range 2 {
		if err := st.Topics.Assign(ctx, a, tp.ID, 0.9); err != nil {
			t.Fatalf("assign: %v", err)
		}
	}
	if n, _ := st.Topics.MemberCount(ctx, tp.ID); n != 1 {
		t.Errorf("member count = %d, want 1 (idempotent assign)", n)
	}
	got, err := st.Topics.TopicsForArtifact(ctx, a)
	if err != nil {
		t.Fatalf("topics for artifact: %v", err)
	}
	if len(got) != 1 || got[0].ID != tp.ID {
		t.Errorf("topics for artifact = %+v, want the assigned topic", got)
	}
}

func TestTopicRepo_ListWithCountsGetMembers(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	a := mkArtifact(t, st, "h1")
	b := mkArtifact(t, st, "h2")
	gone := mkArtifact(t, st, "h3")

	big := &Topic{Label: "big", Centroid: []float32{1}}
	small := &Topic{Label: "small", Centroid: []float32{2}}
	_ = st.Topics.Create(ctx, big)
	_ = st.Topics.Create(ctx, small)
	_ = st.Topics.Assign(ctx, a, big.ID, 0.9)
	_ = st.Topics.Assign(ctx, b, big.ID, 0.7)
	_ = st.Topics.Assign(ctx, gone, small.ID, 0.5)
	_ = st.Artifacts.SoftDelete(ctx, gone) // small now has 0 live members

	// ListWithCounts: only topics with >=1 live member, biggest first.
	list, err := st.Topics.ListWithCounts(ctx, 10, kb.Restricted)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Topic.ID != big.ID || list[0].MemberCount != 2 {
		t.Fatalf("list = %+v, want only big with 2 members", list)
	}

	got, err := st.Topics.Get(ctx, big.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Label != "big" {
		t.Errorf("get = %+v", got)
	}
	if _, err := st.Topics.Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want ErrNotFound", err)
	}

	// Members: live members, highest score first; tombstoned excluded.
	members, err := st.Topics.Members(ctx, big.ID, 10, kb.Restricted)
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 2 || members[0].ArtifactID != a || members[0].Score != 0.9 {
		t.Errorf("members = %+v, want [a(0.9), b(0.7)]", members)
	}
}

func TestTopicRepo_SetCentroid(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	tp := &Topic{Label: "t", Centroid: []float32{1}}
	_ = st.Topics.Create(ctx, tp)
	if err := st.Topics.SetCentroid(ctx, tp.ID, []float32{2, 3}); err != nil {
		t.Fatalf("set centroid: %v", err)
	}
	got, _ := st.Topics.ListWithCentroids(ctx)
	if len(got) != 1 || len(got[0].Centroid) != 2 || got[0].Centroid[1] != 3 {
		t.Errorf("centroid not updated: %+v", got)
	}
}
