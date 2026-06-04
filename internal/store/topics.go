package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
