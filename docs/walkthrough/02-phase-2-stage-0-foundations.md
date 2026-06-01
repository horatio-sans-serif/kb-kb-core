% KB Core — Phase 2 Stage 0: Foundations
% Walkthrough
%

# Phase 2 Stage 0: Foundations

**What:** the contracts and seams the enrichment pipeline plugs into, built
before the first real enricher. Phase 0 built the _mechanisms_ (queue, store,
bus); a comprehensive review found it was under-abstracted for the _contracts_
Phase 2 needs. This stage adds them.

## The pieces

- **`internal/kb` — the domain package.** Pure domain types (`Artifact`,
  `Encounter`, `Enrichment`, `Source`) with typed enums (`PrivacyTier`,
  `SourceKind`, `ArtifactType`, …) and the four plugin contracts (`Fetcher`,
  `Enricher`, `Renderer`, `SourceConnector`). It depends on nothing internal, so
  plugins build against it without importing the SQL layer. `store` keeps **type
  aliases** (`type Artifact = kb.Artifact`) so existing code compiles unchanged.

- **`internal/pipeline` — the enrichment DAG.** Above the dumb job runner.
  Enrichers declare stage dependencies; `Enqueue` topologically expands an
  artifact into one job per stage, wiring each job's `depends_on` from the
  enricher's `DependsOn` — in one transaction. `RegisterHandlers` wraps each
  enricher as a job handler.

- **The privacy gate.** Each pipeline handler enforces `PrivacyOK` _before_
  dispatch: an enricher whose `PrivacyOK` rejects the artifact's tier is skipped
  without ever processing it. This is the §8.17 floor in code — a
  sensitive/restricted artifact never reaches a cloud enricher.

- **`internal/ssrf` — the fetch guard.** The Phase 2 fetcher will request
  arbitrary user URLs from a host sitting next to localhost sidecars and on a
  Tailscale network. `CheckURL` allows only http/https and rejects hosts that
  resolve to loopback, private, link-local (incl. cloud metadata), multicast,
  ULA, or Tailscale CGNAT; `Control` re-checks the dialed IP to defeat DNS
  rebinding.

- **Read pool.** `OpenReadOnly` + `NewWithReadPool`: reads go to a 4-connection
  read-only pool, mutations and `Tx` to the single writer. WAL lets readers run
  concurrently with the writer, so long LLM-bound enricher writes don't serialize
  reads.

- **Source surface + registry.** `SourceRepo.List/Update/SetEnabled` (disable is
  a source's soft-delete — never hard-deleted, to preserve encounters' FK), the
  previously-unmapped `schedule`/`auth_state`/`storage_budget` columns, and an
  `internal/sources` connector registry (the seam; real connectors are Phase 4).

- **Hardening.** A `KB_API_TOKEN` length floor, and UTF-8/control-character
  rejection on ingest text.

## How a future enricher plugs in

```
type Summarizer struct{ ... }                       // implements kb.Enricher
func (Summarizer) Name() string        { return "summarize" }
func (Summarizer) DependsOn() []string { return []string{"extract"} }
func (Summarizer) PrivacyOK(t kb.PrivacyTier) bool { return true } // local LLM
func (Summarizer) Enrich(ctx, a) ([]kb.Enrichment, error) { ... }

p := pipeline.New(); p.Register(Extract{}); p.Register(Summarizer{})
p.RegisterHandlers(st, runner)
// on ingest: p.Enqueue(ctx, st, artifactID)  -> jobs run in dependency order,
// each gated by tier, results persisted.
```

The core never changed to add the stage — that is the point of the four
contracts (design §7).

## Invariants after Stage 0

- A plugin package imports `internal/kb`, never `internal/store`.
- A `sensitive`/`restricted` artifact is never passed to an enricher whose
  `PrivacyOK` rejects that tier (the gate skips it).
- Reads never block the single writer (separate WAL read pool); writes still
  serialize on one connection.
- A user-supplied fetch URL that resolves to an internal address is rejected by
  the SSRF guard before any request leaves the host.

## What it is not yet

No real fetchers or enrichers — those are Phase 2 proper (Playwright fetch behind
the SSRF guard, Ollama summarize/tag/critical-analysis/embed). The `ingest` job is
still a placeholder; wiring the pipeline as the ingest expansion replaces it.
