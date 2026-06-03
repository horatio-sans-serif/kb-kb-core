package ingest

import (
	"context"
	"testing"
	"time"

	"kb/internal/events"
	"kb/internal/kb"
	"kb/internal/store"
	"kb/internal/storetest"
)

type countingEnqueuer struct{ n int }

func (c *countingEnqueuer) EnqueueTx(_ context.Context, _ *store.Store, _ string) error {
	c.n++
	return nil
}

func newSource(t *testing.T, st *store.Store) string {
	t.Helper()
	src := &store.Source{Kind: "PULL", Connector: "x"}
	if err := st.Sources.Create(context.Background(), src); err != nil {
		t.Fatalf("create source: %v", err)
	}
	return src.ID
}

func TestIngest_InsertEnqueuesAndPublishes(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	bus := events.NewBus()
	sub, cancel := bus.Subscribe()
	defer cancel()
	enq := &countingEnqueuer{}
	rescores := 0
	ing := New(st, enq, func(context.Context, *store.Store, string) error { rescores++; return nil }, bus)

	src := newSource(t, st)
	id, err := ing.Ingest(ctx, Item{SourceID: src, Type: kb.Webpage, Title: "X", CanonicalURL: "https://e.com/a", RawURL: "https://e.com/a?utm=1", Tier: kb.Personal})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if id == "" {
		t.Fatal("no artifact id")
	}
	if enq.n != 1 || rescores != 0 {
		t.Errorf("insert: enqueue=%d rescore=%d, want 1/0", enq.n, rescores)
	}
	if n, _ := st.Encounters.CountForArtifact(ctx, id); n != 1 {
		t.Errorf("encounters = %d, want 1", n)
	}
	select {
	case e := <-sub:
		if e.Type != events.ArtifactIngested {
			t.Errorf("event = %q, want artifact.ingested", e.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no artifact.ingested event")
	}
}

func TestIngest_ReEncounterRescoresNotReenqueues(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	enq := &countingEnqueuer{}
	rescores := 0
	ing := New(st, enq, func(context.Context, *store.Store, string) error { rescores++; return nil }, events.NewBus())
	src := newSource(t, st)

	item := Item{SourceID: src, Type: kb.Webpage, Title: "X", CanonicalURL: "https://e.com/a", Tier: kb.Personal}
	id1, _ := ing.Ingest(ctx, item)
	id2, err := ing.Ingest(ctx, item)
	if err != nil {
		t.Fatalf("re-ingest: %v", err)
	}
	if id1 != id2 {
		t.Errorf("re-encounter created a duplicate: %q vs %q", id1, id2)
	}
	if enq.n != 1 || rescores != 1 {
		t.Errorf("enqueue=%d rescore=%d, want 1/1 (insert enqueues, re-encounter re-scores)", enq.n, rescores)
	}
	if n, _ := st.Encounters.CountForArtifact(ctx, id1); n != 2 {
		t.Errorf("encounters = %d, want 2 (one per sighting)", n)
	}
}

func TestIngest_RejectsItemWithoutKey(t *testing.T) {
	st := storetest.New(t)
	ing := New(st, &countingEnqueuer{}, nil, events.NewBus())
	src := newSource(t, st)
	// Neither CanonicalURL nor ContentHash — must fail, not insert content_hash="".
	if _, err := ing.Ingest(context.Background(), Item{SourceID: src, Type: kb.Note, Body: "x", Tier: kb.Personal}); err == nil {
		t.Error("an item with no dedup key must be rejected (would corrupt content-hash dedup)")
	}
}

func TestIngest_NoteDedupByContentHash(t *testing.T) {
	st := storetest.New(t)
	ctx := context.Background()
	ing := New(st, &countingEnqueuer{}, nil, events.NewBus())
	src := newSource(t, st)

	item := Item{SourceID: src, Type: kb.Note, Body: "hello", ContentHash: "h1", Tier: kb.Personal}
	id1, _ := ing.Ingest(ctx, item)
	id2, _ := ing.Ingest(ctx, item)
	if id1 != id2 {
		t.Errorf("same content hash made two artifacts: %q vs %q", id1, id2)
	}
	if got, _ := st.Artifacts.Get(ctx, id1); got.BodyMarkdown != "hello" || got.Type != "note" {
		t.Errorf("note not stored: %+v", got)
	}
}
