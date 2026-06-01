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

| Env var              | Required | Default          | Description                                                      |
| -------------------- | -------- | ---------------- | ---------------------------------------------------------------- |
| `KB_DB`              | yes      | —                | Path to the SQLite database file                                 |
| `KB_API_TOKEN`       | yes      | —                | Bearer token for the HTTP API                                    |
| `KB_LISTEN_ADDR`     | no       | `127.0.0.1:8087` | TCP listen address (binding beyond loopback exposes the API)     |
| `KB_DEFAULT_PRIVACY` | no       | `personal`       | Default tier: `public` / `personal` / `sensitive` / `restricted` |
| `KB_CLOUD_LLM`       | no       | `false`          | Allow cloud LLMs for non-sensitive tiers (Phase 2)               |
| `KB_LOG_LEVEL`       | no       | `info`           | `debug` / `info` / `warn` / `error`                              |

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
