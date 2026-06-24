package store

import (
	"context"
	"testing"

	"kb/internal/kb"
)

func TestAuditRepo_RecordAndRecent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// Empty Record is a no-op.
	if err := st.Audit.Record(ctx); err != nil {
		t.Fatalf("empty record: %v", err)
	}
	if rows, _ := st.Audit.Recent(ctx, 10); len(rows) != 0 {
		t.Fatalf("recent after no records = %d, want 0", len(rows))
	}

	// A batch of three. Recent returns them newest first (id is a monotonic ULID).
	err := st.Audit.Record(ctx,
		AuditEntry{Actor: "owner", Action: "search", ArtifactID: "01A", PrivacyTier: kb.Sensitive},
		AuditEntry{Actor: "read-token", Action: "ask", ArtifactID: "01B", PrivacyTier: kb.Restricted},
		AuditEntry{Actor: "read-token", Action: "graph", ArtifactID: "01A", PrivacyTier: kb.Sensitive},
	)
	if err != nil {
		t.Fatalf("record: %v", err)
	}

	rows, err := st.Audit.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("recent = %d, want 3", len(rows))
	}
	// Each row is assigned an id and a timestamp; the fields round-trip.
	for _, r := range rows {
		if r.ID == "" || r.CreatedAt == "" {
			t.Errorf("row missing id/created_at: %+v", r)
		}
	}
	// Newest first: the third recorded (graph) has the highest ULID.
	if rows[0].Action != "graph" || rows[0].Actor != "read-token" || rows[0].PrivacyTier != kb.Sensitive {
		t.Errorf("newest row = %+v, want the graph entry first", rows[0])
	}

	// Limit caps the result.
	if capped, _ := st.Audit.Recent(ctx, 1); len(capped) != 1 || capped[0].Action != "graph" {
		t.Errorf("limit 1 = %+v, want only the newest", capped)
	}
}

func TestAuditRepo_NullableArtifactAndTier(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// An entry not tied to a single artifact stores NULL artifact_id/privacy_tier and
	// reads back as empty strings.
	if err := st.Audit.Record(ctx, AuditEntry{Actor: "owner", Action: "export"}); err != nil {
		t.Fatalf("record: %v", err)
	}
	rows, err := st.Audit.Recent(ctx, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("recent = %d, want 1", len(rows))
	}
	if rows[0].ArtifactID != "" || rows[0].PrivacyTier != "" {
		t.Errorf("nullable round-trip = %+v, want empty artifact_id and tier", rows[0])
	}
}

func TestAuditRepo_ByArtifact(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	if err := st.Audit.Record(ctx,
		AuditEntry{Actor: "owner", Action: "search", ArtifactID: "01A", PrivacyTier: kb.Sensitive},
		AuditEntry{Actor: "read-token", Action: "ask", ArtifactID: "01B", PrivacyTier: kb.Restricted},
		AuditEntry{Actor: "owner", Action: "graph", ArtifactID: "01A", PrivacyTier: kb.Sensitive},
	); err != nil {
		t.Fatalf("record: %v", err)
	}

	a, err := st.Audit.ByArtifact(ctx, "01A", 10)
	if err != nil {
		t.Fatalf("by artifact: %v", err)
	}
	if len(a) != 2 {
		t.Fatalf("ByArtifact(01A) = %d, want 2", len(a))
	}
	for _, r := range a {
		if r.ArtifactID != "01A" {
			t.Errorf("ByArtifact returned a row for %q, want only 01A", r.ArtifactID)
		}
	}
}
