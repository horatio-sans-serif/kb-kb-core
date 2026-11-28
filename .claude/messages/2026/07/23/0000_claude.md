# Claude — 2026-07-23T08:11:13.366000-04:00

> **Complexity: High** — The AI assistant is describing the detailed process of merging, reviewing, and hardening three new connectors (Reddit, Mastodon, and Audit CSV export) into a larger project. This includes handling transient server rate limits, resolving merge conflicts, writing tests for fixes, and addressing remaining design decisions.

Continuing the backlog. Next batch of independent, well-scoped items: the Reddit and Mastodon public connectors (mirror the just-merged GitHub one) and audit-log export. I'll dispatch them as parallel worktree agents, then merge + review as before. (DICTATOR transcription and auth-bearing connectors need a backend/design decision — I'll surface those after this batch rather than guess.)

Reddit succeeded. The other two hit a transient server rate-limit (not my usage). Let me check for partial worktrees, clean up, and re-dispatch those two.

Only Reddit's worktree remains — the two failed agents left nothing behind. Re-dispatching them fresh:

All three succeeded. Merging sequentially — audit export first (no `main.go`), then the two connectors:

Mastodon conflicts with Reddit's `main.go` additions (same region). Let me resolve:

Now the second conflict (the register lines):

Conflict resolved, merge committed. Now the combined verification across the three features:

Combined tree green, five connectors registered. Clean up worktrees, then run a combined review:

Solid review. Findings to realize: P1-1 (stripHTML unescape order — triggers on ordinary content), P1-2 (script/style content), P1-3 (Mastodon instance validation), P2-1 (Reddit permalink host-escape), P2-2 (dedup), P2-3 (rune-safe title). Let me check whether the HTML tokenizer is available and read the code:

`x/net` isn't vendored, so I'll fix `stripHTML` as a correct single pass (unescape per text-run, drop `script`/`style` content) rather than add a dependency. Let me read the surrounding helpers:

Now realize the Mastodon fixes — a robust single-pass `stripHTML` (per-run unescape + drop script/style content), rune-safe title, and instance validation:

Now rune-safe title and instance validation:

Now the Reddit permalink guard + dedup. Read the emit loop region:

Build and run the connector tests to check the fixes against existing assertions:

Both green. Let me pin the fixes with tests. Read the existing stripHTML test:

Extend the tests to pin the fixes:

The tests passed after my `parseConfig` change — I need to understand how the test harness points at httptest without tripping the new instance validation. Let me check `testConnector` and how `base` vs `instance` interact:

The injected `base` (bare host:port) overrides `instance` in tests, and my validation allows host:port — that's why the tests pass, and it's correct for production too. Now add the pinning tests:

Now the Reddit host-escape guard test. Let me read the test file structure:

All fixes pinned. Full verification + lint, then docs + commit:

Clean. Update the docs (connectors + audit export), then commit:

A stray `ssrf_test.go` appeared at the repo root — that shouldn't be there. Let me inspect before committing:

That's a scratch file a review agent left in the working tree (`package main` at root, a copy of the old `stripHTML` — never tracked, not part of any feature). Remove it:

The build error is the intentional `sqlite_fts5` guard (building without the tag fails by design). Confirm the tagged build + commit:

Final verification of the committed tree, then report:

Phase 20 backlog batch is complete — three more features built in parallel, merged, reviewed, and hardened.

## What landed

| Feature | § | Surface |
|---------|---|---------|
| **Reddit connector** | 8.1 | `internal/sources/reddit` — a subreddit's new posts; config `{"subreddit":"..."}` |
| **Mastodon connector** | 8.1 | `internal/sources/mastodon` — an account's public posts (lookup→statuses); config `{"instance":"...","acct":"..."}` |
| **Audit CSV export** | 8.17 | `GET /audit?format=csv` — RFC 4180 export, owner-only, filters apply |

The five PULL connectors are now `rss`, `hackernews`, `github`, `reddit`, `mastodon`.

**Process notes:** two agents initially hit a transient server-side rate limit and returned nothing; I re-dispatched them and they succeeded. The Reddit/Mastodon `main.go` register lines conflicted on merge (same region) — resolved by hand.

**Review outcome (no P0):** the combined review found and I realized —
- **Mastodon `stripHTML`** rewritten as a correct single pass: drops `script`/`style` element *content* and decodes entities per text-run (no final global unescape, so an encoded `<script>` in visible text can't re-form a live tag); rune-safe title truncation; `parseConfig` rejects a non-bare-host instance up front.
- **Reddit** skips a permalink that isn't host-relative (`//x` / `@x` would escape the reddit host into the canonical URL); permalink URL built once.
- Removed **`ssrf_test.go`**, a `package main` scratch file a review agent had left at the repo root (never tracked, not part of any feature — caught it before it got committed).

Audit CSV export reviewed clean. New tests pin every fix. Final state: full suite green under `sqlite_fts5`, gofmt + lint clean, working tree clean, worktrees/branches removed.

**Remaining backlog** — both need a design decision before I dispatch, so I'm flagging rather than guessing:
- **Auth-bearing connectors** — per-source credentials/OAuth + token storage/refresh. Would unlock Reddit-saved, GitHub-private/notifications, Mastodon-home. Needs a decision on secret storage (there's an `AuthState`-style notion referenced in the design) and the OAuth flow surface.
- **DICTATOR** (Gap 13) — audio/video → transcript + diarization + timestamps. Needs a transcription-backend choice (local whisper.cpp/whisper CLI via a pluggable transcriber interface with a no-op fallback, vs. an external service).

Want me to pick one of those and propose an approach, or keep going another direction?
