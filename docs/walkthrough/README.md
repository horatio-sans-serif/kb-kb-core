# KB Core — System Walkthrough

A long-form, build-as-we-go walkthrough of every part of the system. One entry
per phase, written while the work is fresh; together they are the complete
walkthrough.

Each entry covers: **what** was built, **why** (with the design §), **how** it
fits the spine, the **flow** it participates in, and the **decisions/trade-offs**.

## Entries

| #   | Phase                                            | Doc                                                | Status  |
| --- | ------------------------------------------------ | -------------------------------------------------- | ------- |
| 00  | Spine (schema, storage, jobs, events, HTTP API)  | [00-phase-0-spine.md](00-phase-0-spine.md)         | written |
| 01  | Ingestion: canonicalization + content-hash dedup | [01-phase-1-ingestion.md](01-phase-1-ingestion.md) | written |

(Rows are appended as phases complete.)

## Rendering

The entries are Markdown-at-rest, so they feed the design's Output/Render layer
directly:

- **PDF / HTML:** `pandoc 00-phase-0-spine.md -o phase-0.pdf` (Eisvogel template)
  or `... -o phase-0.html` (Bootstrap template).
- **Terminal slides:** `patat 00-phase-0-spine.md` for a keyboard-driven
  walkthrough.
- **Interactive tutorial:** adapt to the `agent-skills-tutorial` HTML format; use
  `glance`'s "Copy Claude Instructions" prompt pattern to generate it.
- **Concept review:** pipe an entry to `flashcarder` to generate
  spaced-repetition cards for the design.
