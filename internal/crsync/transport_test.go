package crsync

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeChangeStore is an in-memory ChangeStore so the transport is exercised without
// the native extension. Changes returns rows with db_version > since.
type fakeChangeStore struct {
	mu        sync.Mutex
	changes   []Change
	version   int64
	applied   []Change
	sinceSeen int64
}

func (f *fakeChangeStore) Changes(_ context.Context, since int64) ([]Change, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sinceSeen = since
	var out []Change
	for _, c := range f.changes {
		if c.DBVersion > since {
			out = append(out, c)
		}
	}
	return out, f.version, nil
}

func (f *fakeChangeStore) Apply(_ context.Context, changes []Change) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, changes...)
	return nil
}

// fakeWatermarks is an in-memory Watermarks.
type fakeWatermarks struct {
	mu sync.Mutex
	m  map[string]int64
}

func newFakeWatermarks() *fakeWatermarks { return &fakeWatermarks{m: map[string]int64{}} }
func (f *fakeWatermarks) Watermark(_ context.Context, peer string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[peer], nil
}
func (f *fakeWatermarks) SetWatermark(_ context.Context, peer string, v int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v > f.m[peer] {
		f.m[peer] = v
	}
	return nil
}

const testToken = "test-token-0123456789"

func TestHandler_RequiresToken(t *testing.T) {
	h := Handler(&fakeChangeStore{}, testToken, discardLogger())
	srv := httptest.NewServer(http.StripPrefix("/sync", h))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/sync/changes?since=0")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-token GET status = %d, want 401", resp.StatusCode)
	}
}

func TestPuller_PullsAndAdvancesWatermark(t *testing.T) {
	ctx := context.Background()
	// The peer (server) has two changes at db_version 7 and 9.
	peerStore := &fakeChangeStore{
		changes: []Change{
			{Table: "artifacts", PK: []byte("a"), CID: "title", Val: Value{X: "x"}, DBVersion: 7, SiteID: []byte("s")},
			{Table: "artifacts", PK: []byte("b"), CID: "title", Val: Value{X: "y"}, DBVersion: 9, SiteID: []byte("s")},
		},
		version: 9,
	}
	srv := httptest.NewServer(http.StripPrefix("/sync", Handler(peerStore, testToken, discardLogger())))
	defer srv.Close()

	localStore := &fakeChangeStore{}
	wm := newFakeWatermarks()
	p := NewPuller(localStore, wm, []string{srv.URL}, testToken, srv.Client(), discardLogger())

	p.SyncAll(ctx)

	// The peer saw a pull from watermark 0.
	if peerStore.sinceSeen != 0 {
		t.Errorf("peer saw since=%d, want 0 on first pull", peerStore.sinceSeen)
	}
	// Local applied both changes and advanced the watermark to the peer's version.
	if len(localStore.applied) != 2 {
		t.Fatalf("local applied %d changes, want 2", len(localStore.applied))
	}
	if v, _ := wm.Watermark(ctx, srv.URL); v != 9 {
		t.Errorf("watermark = %d, want 9", v)
	}

	// A second pull starts from the advanced watermark and applies nothing new.
	p.SyncAll(ctx)
	if peerStore.sinceSeen != 9 {
		t.Errorf("peer saw since=%d on second pull, want 9", peerStore.sinceSeen)
	}
	if len(localStore.applied) != 2 {
		t.Errorf("second pull applied more changes (%d), want still 2", len(localStore.applied))
	}
}

func TestPuller_WrongTokenDoesNotAdvance(t *testing.T) {
	ctx := context.Background()
	peerStore := &fakeChangeStore{
		changes: []Change{{Table: "artifacts", PK: []byte("a"), CID: "title", DBVersion: 3, SiteID: []byte("s")}},
		version: 3,
	}
	srv := httptest.NewServer(http.StripPrefix("/sync", Handler(peerStore, testToken, discardLogger())))
	defer srv.Close()

	localStore := &fakeChangeStore{}
	wm := newFakeWatermarks()
	p := NewPuller(localStore, wm, []string{srv.URL}, "wrong-token", srv.Client(), discardLogger())

	p.SyncAll(ctx)

	if len(localStore.applied) != 0 {
		t.Errorf("applied %d changes with a wrong token, want 0", len(localStore.applied))
	}
	if v, _ := wm.Watermark(ctx, srv.URL); v != 0 {
		t.Errorf("watermark = %d after a rejected pull, want 0", v)
	}
}
