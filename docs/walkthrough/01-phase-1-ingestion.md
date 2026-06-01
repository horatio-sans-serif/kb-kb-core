% KB Core — Phase 1: Ingestion (Canonicalization + Dedup)
% Walkthrough
%

# Phase 1: Ingestion — canonicalization + content-hash dedup

**What:** make ingestion _correct_ (design §8.2). The same article arriving at
differently-decorated URLs, or as a re-formatted copy, now collapses to **one**
canonical artifact with many encounters — the dedup keystone made real.

**Why:** Phase 0's `/ingest` stored whatever URL it was handed. So
`example.com/x?utm_source=twitter` and `example.com/x?utm_source=hn` looked like
two different artifacts — defeating the one-artifact-many-encounters model the
whole relevance signal (§8.9) depends on.

## The package: `internal/canon`

- **`URL(raw) (string, error)`** — pure, offline URL canonicalization: lowercase
  scheme/host, drop userinfo, drop default ports, strip leading `www.`, strip the
  fragment, remove tracking params (`utm_*` prefix + a denylist: `fbclid`,
  `gclid`, `igshid`, ...), sort the remaining query for stable output, remove a
  trailing slash (except root), and a conservative de-AMP (`/amp` path, `amp=`
  query). Idempotent: `URL(URL(x)) == URL(x)`.
- **`Content(body) string`** — a whitespace-insensitive SHA-256 fingerprint, so
  verbatim and reflowed copies hash equal. (Near-duplicate detection —
  SimHash/MinHash — is a later refinement.)

Redirect-following is **not** here: it needs the network and is done at fetch
time (Phase 2), which re-canonicalizes the resolved URL.

## Storage: two dedup paths, one insert

`ArtifactRepo.upsert` is shared between two atomic `INSERT … ON CONFLICT …
RETURNING` entry points, differing only in the conflict target:

- **`UpsertByCanonical`** — `ON CONFLICT(canonical_url)` (the Phase 0 path).
- **`UpsertByContentHash`** — `ON CONFLICT(content_hash)` (new this phase).

Migration **`0002`** swaps the non-unique `content_hash` index for a _partial
unique_ one scoped to live rows (`WHERE content_hash IS NOT NULL AND deleted_at
IS NULL`), so the content-hash upsert is race-free like the canonical one. This
is also the first multi-migration, exercising the runner end to end.

The two paths never collide: a URL artifact carries a NULL `content_hash` until
it is fetched (Phase 2), and a body artifact carries a NULL `canonical_url`.

## Flow change in `/ingest`

```
1. validate fields
2. if url:  canonicalURL = canon.URL(url)   -> 400 on a bad URL
3. store.Tx:
     - url present  -> CanonicalURL = canonicalURL ; UpsertByCanonical
     - body present -> ContentHash  = canon.Content(body) ; UpsertByContentHash
     - Encounters.Record(raw_url = the ORIGINAL url)   <- as-seen URL preserved
     - Jobs.Enqueue(ingest)
4. (after commit) publish artifact.ingested
```

So `…?utm_source=a` and `…?utm_source=b&fbclid=z` both store
`https://example.com/p` → one artifact, two encounters; the un-normalized URLs
survive on the encounters for provenance.

## Decisions / trade-offs

- **Canonicalization is offline and conservative.** It never changes scheme
  (no forced https) and de-AMP only strips an unambiguous trailing `/amp`, to
  avoid collapsing genuinely-distinct URLs.
- **Content dedup is exact-after-whitespace-normalization**, not near-dup yet —
  enough for reflowed copies; SimHash is deferred.
- **A real second migration** (`0002`) was chosen over editing `0001`, both to
  model append-only migrations and to exercise the multi-migration runner; the
  migrate tests were made count-robust so they don't break each phase.

## What it is not yet

No fetching/extraction (Phase 2): a URL `/ingest` stores only the canonical URL,
not page content, so URL artifacts have no `content_hash` until fetched. Cross-URL
content dedup (syndication/mirrors) therefore activates in Phase 2, when fetched
bodies get fingerprinted and a fetched URL's hash can match an existing artifact.
