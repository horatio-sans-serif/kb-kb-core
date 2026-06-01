# KB Core — System Walkthrough

A long-form, build-as-we-go walkthrough of every part of the system. One entry
per phase, written while the work is fresh; together they are the complete
walkthrough.

Each entry covers: **what** was built, **why** (with the design §), **how** it
fits the spine, the **flow** it participates in, and the **decisions/trade-offs**.

## Entries

Tracks every phase of `../UNIFIED_SYSTEM_DESIGN_PLAN.md` §12, so the table doubles
as a progress tracker.

| #   | Phase                                            | Doc                                                | Status      |
| --- | ------------------------------------------------ | -------------------------------------------------- | ----------- |
| 00  | Spine (schema, storage, jobs, events, HTTP API)  | [00-phase-0-spine.md](00-phase-0-spine.md)         | written     |
| 01  | Ingestion: canonicalization + content-hash dedup | [01-phase-1-ingestion.md](01-phase-1-ingestion.md) | written     |
| 02  | Enrichment core (fetch, summarize, embed)        | —                                                  | not started |
| 03  | Retrieval (search + RAG)                         | —                                                  | not started |
| 04  | Sources at scale + scheduling + health           | —                                                  | not started |
| 05  | Knowledge graph + entities                       | —                                                  | not started |
| 06  | The feedback loop (engagement, re-score, decay)  | —                                                  | not started |
| 07  | Surfacing (digest, spaced repetition, context)   | —                                                  | not started |
| 08  | Transcription + ambient capture                  | —                                                  | not started |
| 09  | Output / render layer                            | —                                                  | not started |
| 10  | Multi-device, API, sync                          | —                                                  | not started |
| 11  | Conflict detection + source credibility          | —                                                  | not started |

## Rendering

The entries are Markdown-at-rest, so they feed the design's Output/Render layer
directly:

- **PDF / HTML:** the entries open with a `%`-style title block (patat's format),
  so for pandoc pass `--from markdown+pandoc_title_block`:
  `pandoc --from markdown+pandoc_title_block --template eisvogel 00-phase-0-spine.md -o phase-0.pdf`
  (or `-o phase-0.html` with the Bootstrap template).
- **Terminal slides:** `patat 00-phase-0-spine.md` for a keyboard-driven
  walkthrough.
- **Interactive tutorial:** adapt to the `agent-skills-tutorial` HTML format; use
  `glance`'s "Copy Claude Instructions" prompt pattern to generate it.
- **Concept review:** pipe an entry to `flashcarder` to generate
  spaced-repetition cards for the design.
