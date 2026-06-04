package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"kb/internal/kb"
)

// Topic is defined in internal/kb (aliased in types.go).
type TopicRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// Create inserts a topic, assigning a ULID. The centroid is stored as a JSON float
// array, or NULL when absent.
func (r *TopicRepo) Create(ctx context.Context, t *Topic) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	centroid, err := marshalVec(t.Centroid)
	if err != nil {
		return fmt.Errorf("marshal topic centroid: %w", err)
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO topics(id, label, centroid) VALUES (?, ?, ?)`, t.ID, t.Label, centroid); err != nil {
		return fmt.Errorf("create topic %q: %w", t.Label, err)
	}
	return nil
}

// ListWithCentroids returns every topic that has a centroid — the nearest-search
// set the clustering compares a new artifact against.
func (r *TopicRepo) ListWithCentroids(ctx context.Context) ([]*Topic, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT id, label, centroid FROM topics WHERE centroid IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list topics: %w", err)
	}
	defer rows.Close()
	return scanTopics(rows)
}

// SetCentroid replaces a topic's centroid (the updated running mean).
func (r *TopicRepo) SetCentroid(ctx context.Context, topicID string, centroid []float32) error {
	c, err := marshalVec(centroid)
	if err != nil {
		return fmt.Errorf("marshal centroid: %w", err)
	}
	if _, err := r.q.ExecContext(ctx, `UPDATE topics SET centroid = ? WHERE id = ?`, c, topicID); err != nil {
		return fmt.Errorf("set topic %q centroid: %w", topicID, err)
	}
	return nil
}

// Assign links an artifact to a topic with a membership score. Idempotent.
func (r *TopicRepo) Assign(ctx context.Context, artifactID, topicID string, score float64) error {
	if _, err := r.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO artifact_topics(artifact_id, topic_id, score) VALUES (?, ?, ?)`,
		artifactID, topicID, score); err != nil {
		return fmt.Errorf("assign artifact %q topic %q: %w", artifactID, topicID, err)
	}
	return nil
}

// MemberCount returns how many live artifacts are assigned to a topic — the N a
// running-mean centroid update needs.
func (r *TopicRepo) MemberCount(ctx context.Context, topicID string) (int, error) {
	var n int
	err := r.rq.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM artifact_topics at
		 JOIN artifacts a ON a.id = at.artifact_id AND a.deleted_at IS NULL
		 WHERE at.topic_id = ?`, topicID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("member count for topic %q: %w", topicID, err)
	}
	return n, nil
}

// TopicWithCount is a topic plus its live member count (the topic list view).
type TopicWithCount struct {
	Topic       *Topic
	MemberCount int
}

// ListWithCounts returns topics that have at least one live member, largest first,
// each with its live member count.
func (r *TopicRepo) ListWithCounts(ctx context.Context, limit int) ([]TopicWithCount, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT t.id, t.label, COUNT(*) AS n FROM topics t
		 JOIN artifact_topics at ON at.topic_id = t.id
		 JOIN artifacts a ON a.id = at.artifact_id AND a.deleted_at IS NULL
		 GROUP BY t.id, t.label ORDER BY n DESC, t.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list topics with counts: %w", err)
	}
	defer rows.Close()

	var out []TopicWithCount
	for rows.Next() {
		var t Topic
		var n int
		if err := rows.Scan(&t.ID, &t.Label, &n); err != nil {
			return nil, fmt.Errorf("scan topic count: %w", err)
		}
		out = append(out, TopicWithCount{Topic: &t, MemberCount: n})
	}
	return out, rows.Err()
}

// Get returns a topic by id, or ErrNotFound.
func (r *TopicRepo) Get(ctx context.Context, id string) (*Topic, error) {
	rows, err := r.rq.QueryContext(ctx, `SELECT id, label, centroid FROM topics WHERE id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("get topic %q: %w", id, err)
	}
	defer rows.Close()
	ts, err := scanTopics(rows)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("get topic %q: %w", id, ErrNotFound)
	}
	return ts[0], nil
}

// TopicMember is one artifact's membership in a topic, with its display fields
// (joined here so the caller needs no per-member lookup).
type TopicMember struct {
	ArtifactID string
	Title      string
	URL        string
	Score      float64
}

// Members returns up to limit of a topic's live member artifacts, highest
// membership score first, with their title/url.
func (r *TopicRepo) Members(ctx context.Context, topicID string, limit int) ([]TopicMember, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT at.artifact_id, a.title, a.canonical_url, at.score FROM artifact_topics at
		 JOIN artifacts a ON a.id = at.artifact_id AND a.deleted_at IS NULL
		 WHERE at.topic_id = ? ORDER BY at.score DESC, at.artifact_id LIMIT ?`, topicID, limit)
	if err != nil {
		return nil, fmt.Errorf("members of topic %q: %w", topicID, err)
	}
	defer rows.Close()

	var out []TopicMember
	for rows.Next() {
		var m TopicMember
		var title, url sql.NullString
		if err := rows.Scan(&m.ArtifactID, &title, &url, &m.Score); err != nil {
			return nil, fmt.Errorf("scan topic member: %w", err)
		}
		m.Title, m.URL = title.String, url.String
		out = append(out, m)
	}
	return out, rows.Err()
}

// TopicsForArtifact returns the topics a live artifact is assigned to.
func (r *TopicRepo) TopicsForArtifact(ctx context.Context, artifactID string) ([]*Topic, error) {
	rows, err := r.rq.QueryContext(ctx,
		`SELECT t.id, t.label, t.centroid FROM topics t
		 JOIN artifact_topics at ON at.topic_id = t.id
		 WHERE at.artifact_id = ? ORDER BY t.id`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("topics for artifact %q: %w", artifactID, err)
	}
	defer rows.Close()
	return scanTopics(rows)
}

// ClusterCandidate is a ready, embedded, not-yet-clustered artifact — the input
// to the topic-assignment sweep.
type ClusterCandidate struct {
	ArtifactID string
	Title      string
	Embedding  []float32
}

// UnclusteredEmbeddings returns up to limit ready, live artifacts that have an
// embedding but no topic assignment yet, with their latest embedding vector — the
// incremental work list for the clusterer. Oldest first (by id) so clustering is
// deterministic and stable.
func (r *TopicRepo) UnclusteredEmbeddings(ctx context.Context, limit int) ([]ClusterCandidate, error) {
	if limit <= 0 {
		limit = surfaceDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT e.artifact_id, a.title, e.payload
		 FROM enrichments e
		 JOIN artifacts a ON a.id = e.artifact_id AND a.deleted_at IS NULL AND a.status = 'ready'
		 WHERE e.kind = 'embedding'
		   AND e.id = (SELECT id FROM enrichments e2
		               WHERE e2.artifact_id = e.artifact_id AND e2.kind = 'embedding'
		               ORDER BY created_at DESC, id DESC LIMIT 1)
		   AND a.id NOT IN (SELECT artifact_id FROM artifact_topics)
		 ORDER BY a.id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("unclustered embeddings: %w", err)
	}
	defer rows.Close()

	var out []ClusterCandidate
	for rows.Next() {
		var c ClusterCandidate
		var title sql.NullString
		var payload string
		if err := rows.Scan(&c.ArtifactID, &title, &payload); err != nil {
			return nil, fmt.Errorf("scan cluster candidate: %w", err)
		}
		var p kb.EmbeddingPayload
		if err := json.Unmarshal([]byte(payload), &p); err != nil {
			return nil, fmt.Errorf("unmarshal embedding for %q: %w", c.ArtifactID, err)
		}
		if len(p.Vector) == 0 || isZeroVec(p.Vector) {
			continue // an empty or zero-magnitude embedding is not clusterable
		}
		c.Title = title.String
		c.Embedding = p.Vector
		out = append(out, c)
	}
	return out, rows.Err()
}

func scanTopics(rows *sql.Rows) ([]*Topic, error) {
	var out []*Topic
	for rows.Next() {
		var t Topic
		var centroid sql.NullString
		if err := rows.Scan(&t.ID, &t.Label, &centroid); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		if centroid.Valid && centroid.String != "" {
			if err := json.Unmarshal([]byte(centroid.String), &t.Centroid); err != nil {
				return nil, fmt.Errorf("unmarshal topic %q centroid: %w", t.ID, err)
			}
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// isZeroVec reports whether every component is zero — a degenerate embedding that
// has no cosine direction, so it would found a permanent singleton topic.
func isZeroVec(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// marshalVec encodes a float vector as a JSON array, or nil (→ SQL NULL) when empty.
func marshalVec(v []float32) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}
