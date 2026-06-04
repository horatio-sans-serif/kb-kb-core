package crsync

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ChangeStore is the changeset side of a replica the transport needs: read this
// replica's changes since a peer's watermark, and apply a peer's changes. *sql.DB
// satisfies it via DBChangeStore; tests use a fake so the transport is exercised
// without the native extension.
type ChangeStore interface {
	Changes(ctx context.Context, since int64) ([]Change, int64, error)
	Apply(ctx context.Context, changes []Change) error
}

// DBChangeStore adapts a cr-sqlite-loaded *sql.DB to ChangeStore.
type DBChangeStore struct{ DB *sql.DB }

func (d DBChangeStore) Changes(ctx context.Context, since int64) ([]Change, int64, error) {
	return Changes(ctx, d.DB, since, DefaultPageRows)
}

// Apply validates that every change targets a synced (CRR) table before merging it.
// A peer's changeset is untrusted: a buggy, forked, or compromised peer could name
// jobs/audit_log or an arbitrary table. The whole batch is rejected — not partially
// applied — if any change is out of bounds.
func (d DBChangeStore) Apply(ctx context.Context, changes []Change) error {
	for _, c := range changes {
		if !IsSynced(c.Table) {
			return fmt.Errorf("crsync: refusing change to non-synced table %q", c.Table)
		}
	}
	return Apply(ctx, d.DB, changes)
}

// Watermarks persists, per peer, the highest db_version pulled from it.
// store.SyncRepo satisfies this.
type Watermarks interface {
	Watermark(ctx context.Context, peer string) (int64, error)
	SetWatermark(ctx context.Context, peer string, version int64) error
}

// changesResponse is the GET /sync/changes body: a peer's changes plus the highest
// db_version among them (the watermark the puller stores).
type changesResponse struct {
	Changes []Change `json:"changes"`
	Version int64    `json:"version"`
}

// applyRequest is the POST /sync/changes body.
type applyRequest struct {
	Changes []Change `json:"changes"`
}

// Handler serves the changeset-exchange endpoints under the mount point (the daemon
// mounts it at /sync), guarded by a constant-time bearer-token check (peers share
// the API token). Routes:
//
//	GET  /changes?since=<n>  -> {changes, version}
//	POST /changes            <- {changes}, 204 on success
func Handler(cs ChangeStore, token string, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /changes", requireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.URL.Query().Get("since")
		since := int64(0)
		if raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v < 0 {
				http.Error(w, "invalid since", http.StatusBadRequest)
				return
			}
			since = v
		}
		changes, version, err := cs.Changes(r.Context(), since)
		if err != nil {
			logger.Warn("sync: read changes", "err", err)
			http.Error(w, "read changes", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, changesResponse{Changes: changes, Version: version})
	})))
	mux.Handle("POST /changes", requireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req applyRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, maxApplyBytes)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := cs.Apply(r.Context(), req.Changes); err != nil {
			logger.Warn("sync: apply changes", "err", err, "count", len(req.Changes))
			http.Error(w, "apply changes", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	return mux
}

// maxApplyBytes caps an inbound changeset body (a defensive bound; personal-scale
// changesets are far smaller).
const maxApplyBytes = 64 << 20 // 64 MiB

func requireToken(token string, next http.Handler) http.Handler {
	const prefix = "Bearer "
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		if !strings.HasPrefix(h, prefix) || token == "" ||
			subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeJSON encodes to a buffer first so a marshal error (e.g. an unexpected
// changeset value type) becomes a clean 500 rather than a 200 with a truncated body
// the puller cannot decode.
func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "encode", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// Puller pulls changesets from configured peers and applies them locally, advancing
// each peer's watermark. Pull-only: every replica pulls from every peer, so changes
// propagate (and cr-sqlite forwards third-party changes, so a gossip topology
// converges without a full mesh).
type Puller struct {
	cs     ChangeStore
	wm     Watermarks
	peers  []string
	token  string
	client *http.Client
	logger *slog.Logger
}

// NewPuller builds a Puller. client may be nil (a default client with a timeout, and
// redirects disabled so a peer cannot bounce the token-bearing request elsewhere, is
// used).
func NewPuller(cs ChangeStore, wm Watermarks, peers []string, token string, client *http.Client, logger *slog.Logger) *Puller {
	if client == nil {
		client = &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	return &Puller{cs: cs, wm: wm, peers: peers, token: token, client: client, logger: logger}
}

// maxPagesPerSync bounds one syncPeer call so a pathological non-advancing peer
// cannot loop forever; at DefaultPageRows rows per page this is a very large
// changeset before the sync simply resumes on the next tick.
const maxPagesPerSync = 100_000

// Run pulls from every peer on a ticker until ctx is cancelled (one pull at start).
func (p *Puller) Run(ctx context.Context, interval time.Duration) {
	p.SyncAll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.SyncAll(ctx)
		}
	}
}

// SyncAll pulls from every peer once, logging and continuing past a per-peer error.
func (p *Puller) SyncAll(ctx context.Context) {
	for _, peer := range p.peers {
		if ctx.Err() != nil {
			return
		}
		if err := p.syncPeer(ctx, peer); err != nil {
			p.logger.Warn("sync: pull from peer failed", "peer", peer, "err", err)
		}
	}
}

// syncPeer pulls the peer's changes in pages, applying each and advancing the
// watermark, until a page comes back empty (caught up). Apply is committed before the
// watermark moves, so a crash between them only re-pulls (idempotent), never loses
// changes.
func (p *Puller) syncPeer(ctx context.Context, peer string) error {
	since, err := p.wm.Watermark(ctx, peer)
	if err != nil {
		return fmt.Errorf("read watermark: %w", err)
	}
	for page := 0; page < maxPagesPerSync; page++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		got, err := p.pullPage(ctx, peer, since)
		if err != nil {
			return err
		}
		if len(got.Changes) == 0 {
			return nil // caught up
		}
		if err := p.cs.Apply(ctx, got.Changes); err != nil {
			return fmt.Errorf("apply: %w", err)
		}
		if got.Version <= since {
			// Non-empty page that does not advance the clock would loop forever.
			return fmt.Errorf("peer %q returned %d changes but version %d did not advance past %d",
				peer, len(got.Changes), got.Version, since)
		}
		if err := p.wm.SetWatermark(ctx, peer, got.Version); err != nil {
			return fmt.Errorf("advance watermark: %w", err)
		}
		since = got.Version
	}
	return fmt.Errorf("peer %q: page limit reached, resuming next tick", peer)
}

// pullPage fetches one page of changes from the peer starting after since.
func (p *Puller) pullPage(ctx context.Context, peer string, since int64) (changesResponse, error) {
	u, err := url.Parse(peer)
	if err != nil {
		return changesResponse{}, fmt.Errorf("invalid peer url: %w", err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/sync/changes"
	u.RawQuery = "since=" + strconv.FormatInt(since, 10)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return changesResponse{}, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return changesResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10))
		return changesResponse{}, fmt.Errorf("peer returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var got changesResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxApplyBytes)).Decode(&got); err != nil {
		return changesResponse{}, fmt.Errorf("decode changes: %w", err)
	}
	return got, nil
}
