# KB Core

The unified knowledge-base daemon: captures information from many sources,
enriches it with local AI, stores it under one CRDT-replicated data model, and
makes it searchable, browsable, and self-surfacing.

- Design: `UNIFIED_SYSTEM_DESIGN_PLAN.md`
- Consolidation context: `CONSOLIDATION_REPORT.md`
- Build plan: `IMPLEMENTATION_PLAN.md` (current: Phase 1 — Ingestion)
- Walkthrough: `docs/walkthrough/`

## Build and test

    make build      # or: CGO_ENABLED=1 go build ./...
    make race       # or: CGO_ENABLED=1 go test -race ./...

Requires CGO (a C toolchain) for the SQLite driver — a plain `go build` without
`CGO_ENABLED=1` produces linker errors. FTS5 search (Phase 3) will need the
driver built with `GO_TAGS=sqlite_fts5`; sqlite-vec embeddings (Phase 2) load as
a runtime SQLite extension.

## Configuration

The daemon is configured by environment variables (it fails fast, naming any
missing required value):

| Env var              | Required | Default          | Description                                                                                                                               |
| -------------------- | -------- | ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| `KB_DB`              | yes      | —                | Path to the SQLite database file                                                                                                          |
| `KB_API_TOKEN`       | yes      | —                | Bearer token for the HTTP API                                                                                                             |
| `KB_LISTEN_ADDR`     | no       | `127.0.0.1:8087` | TCP listen address (binding beyond loopback exposes the API)                                                                              |
| `KB_DEFAULT_PRIVACY` | no       | `personal`       | Default tier: `public` / `personal` / `sensitive` / `restricted`                                                                          |
| `KB_READ_TOKEN`      | no       | —                | Optional lower-trust read-only token (≥16 chars, must differ from `KB_API_TOKEN`); grants ceiling-limited read access on read routes only |
| `KB_READ_TIER`       | no       | `public`         | Privacy ceiling for `KB_READ_TOKEN`: `public` / `personal` / `sensitive` (`restricted` is rejected — it is the owner's ceiling)           |
| `KB_CLOUD_LLM`       | no       | `false`          | Allow cloud LLMs for non-sensitive tiers (Phase 2)                                                                                        |
| `KB_LOG_LEVEL`       | no       | `info`           | `debug` / `info` / `warn` / `error`                                                                                                       |

## Run

    KB_DB=kb.db KB_API_TOKEN=$(openssl rand -hex 32) go run ./cmd/kbd

## API

- `GET /healthz` — liveness + build version (no auth).
- `GET /events` — Server-Sent Events stream of lifecycle events (bearer auth).
- `POST /ingest` — bearer auth; JSON `{ "source_id", "url" | "body", "title"?,
"privacy"? }`. Returns `{ "artifact_id" }`.

```bash
TOKEN=...; A=127.0.0.1:8087
curl -s $A/healthz
curl -s -XPOST $A/ingest -H "Authorization: Bearer $TOKEN" \
  -d '{"source_id":"<id>","url":"https://example.com/article"}'
```

### Read surface and the read token (§8.17)

Read routes — `GET /search`, `/ask`, `/surface`, `/entities`, `/entities/{id}`,
`/artifacts/{id}/backlinks`, `/topics`, `/topics/{id}`, `/conflicts`,
`/graph/{id}` — accept either the owner token (`KB_API_TOKEN`, sees every tier)
or, when configured, the lower-trust `KB_READ_TOKEN`. The read token sees only
artifacts at or below its `KB_READ_TIER` ceiling; every read is filtered in SQL,
so above-ceiling content is never loaded. Write and management routes (`/ingest`,
`/conflicts/{id}`, `/digest`, …) remain owner-only.

For the read token, above-ceiling artifacts, entities, topics, and graph nodes
return `404` (indistinguishable from a missing one — no existence oracle), and
`/surface` clamps a requested `privacy_ceiling` down to the token's ceiling.
`/ask` and `/search` over a ceiling-filtered corpus can still reveal whether a
query matches above-ceiling content (an empty answer) — an inherent property of
answering for a semi-trusted reader; no id, title, or body is exposed.

Every read that surfaces a **sensitive or restricted** artifact (via `/search`,
`/surface`, `/graph`, or `/ask`) is recorded in a local access audit log — actor
(owner or read-token), action, artifact, and tier. `GET /audit` (owner-only)
lists it newest-first, with `?artifact_id=` and `?limit=` filters.
