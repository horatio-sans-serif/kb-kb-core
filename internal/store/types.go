package store

import "kb/internal/kb"

// The domain types live in internal/kb so plugins can depend on them without
// importing the storage layer (design §7). These aliases keep existing store.X
// usages compiling; they are the same types, not wrappers. Job stays defined in
// this package — it is local-only queue infrastructure, not a domain entity.
type (
	Artifact      = kb.Artifact
	Encounter     = kb.Encounter
	Enrichment    = kb.Enrichment
	Source        = kb.Source
	PrivacyTier   = kb.PrivacyTier
	SourceKind    = kb.SourceKind
	ArtifactType  = kb.ArtifactType
	LifecycleTier = kb.LifecycleTier
)
