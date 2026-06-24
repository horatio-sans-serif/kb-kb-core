package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"kb/internal/kb"
)

// AuditRepo records and queries the access audit log (§8.17): which principal accessed
// which sensitive/restricted artifact, when. The audit_log table is local-only (never
// synced, no FK to artifacts), so each device keeps its own access history and a row
// survives the artifact's eventual compaction.
type AuditRepo struct{ q, rq Querier } // q: write pool; rq: read pool

// AuditEntry is one access record. ID and CreatedAt are assigned by Record when empty.
// ArtifactID and PrivacyTier may be empty (stored NULL) for an access not tied to a
// single artifact.
type AuditEntry struct {
	ID          string
	Actor       string // the principal: "owner" | "read-token"
	Action      string // the operation, typically the endpoint (e.g. "search", "ask")
	ArtifactID  string
	PrivacyTier kb.PrivacyTier
	CreatedAt   string
}

const auditCols = `id, actor, action, artifact_id, privacy_tier, created_at`

const auditDefaultLimit = 100

// Record inserts the entries in one batched statement (the read paths can produce
// several per request). Empty input is a no-op. Each entry gets a ULID and timestamp
// if it lacks one.
func (r *AuditRepo) Record(ctx context.Context, entries ...AuditEntry) error {
	if len(entries) == 0 {
		return nil
	}
	ph := make([]string, len(entries))
	args := make([]any, 0, len(entries)*6)
	for i := range entries {
		e := &entries[i]
		if e.ID == "" {
			e.ID = NewID()
		}
		if e.CreatedAt == "" {
			e.CreatedAt = now()
		}
		ph[i] = "(?, ?, ?, ?, ?, ?)"
		args = append(args, e.ID, e.Actor, e.Action,
			nullIfEmpty(e.ArtifactID), nullIfEmpty(string(e.PrivacyTier)), e.CreatedAt)
	}
	if _, err := r.q.ExecContext(ctx,
		`INSERT INTO audit_log(`+auditCols+`) VALUES `+strings.Join(ph, ", "), args...); err != nil {
		return fmt.Errorf("record %d audit entries: %w", len(entries), err)
	}
	return nil
}

// Recent returns the newest audit entries first, capped at limit.
func (r *AuditRepo) Recent(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent audit: %w", err)
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

// ByArtifact returns the access history for one artifact, newest first.
func (r *AuditRepo) ByArtifact(ctx context.Context, artifactID string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	rows, err := r.rq.QueryContext(ctx,
		`SELECT `+auditCols+` FROM audit_log WHERE artifact_id = ? ORDER BY created_at DESC, id DESC LIMIT ?`,
		artifactID, limit)
	if err != nil {
		return nil, fmt.Errorf("audit by artifact %q: %w", artifactID, err)
	}
	defer rows.Close()
	return scanAuditEntries(rows)
}

func scanAuditEntries(rows *sql.Rows) ([]AuditEntry, error) {
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var artifactID, tier sql.NullString
		if err := rows.Scan(&e.ID, &e.Actor, &e.Action, &artifactID, &tier, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit entry: %w", err)
		}
		e.ArtifactID = artifactID.String
		e.PrivacyTier = kb.PrivacyTier(tier.String)
		out = append(out, e)
	}
	return out, rows.Err()
}
