# KB Projects Consolidation Report

## The Big Picture

22+ projects in `~/projects/kb/`, all circling the same core idea: **capture information from everywhere, enrich it with AI, and make it searchable/browsable**. The projects vary in maturity from production-ready tools to planning docs, but collectively they've already solved many of the sub-problems the unified system needs.

The projects naturally decompose into the pipeline stages described in the unified vision:

- **Source Connectors** -- arc-tabs-logger, rememberer extension, bookmarks-manager
- **Fetch/Ingest** -- tldr-web, unfurl, shelf importers
- **Enrich/Pipeline** -- summarize-url.sh, radio_hn, bookmark-summarizer
- **Store/Index** -- braindrain, shelf, haus
- **Query/Browse** -- rememberer search, lineout, mindmapper

The unified system is really about wiring these stages together with a common data model.

---

## Tier 1: Production-Ready -- Keep These Concepts

These are mature, working tools with patterns directly applicable to the unified system.

### arc-tabs-logger

**Concept: Passive background capture via OS automation**

- Silent launchd daemon, runs every 60s, queries Arc via JXA
- Append-only deduplicated URL log
- Zero-friction capture -- user does nothing

**Keep**: The passive-capture-via-daemon pattern. This is the model for any PULL data source on a schedule.

### rememberer

**Concept: Capture + semantic search + AI relevance scoring**

- Chrome MV3 extension captures text highlights with context
- Go server with SQLite + sqlite-vec for embeddings
- Ollama for local embedding generation and relevance assessment
- Profile/project-based organization

**Keep**: The full capture-to-semantic-search pipeline. This is the closest thing to the unified vision already working end-to-end. The sqlite-vec approach for local embeddings is proven here.

### braindrain

**Concept: Hierarchical document store with dual search**

- Rust TUI, SQLite, vim-style navigation
- Both fuzzy title search AND semantic embedding search
- Soft-delete with 30-day TTL, rich timestamps (ctime/atime/mtime)

**Keep**: The hierarchical tree model, dual-search strategy, and soft-delete pattern. The schema design (TreeNode with parent refs, timestamp triggers) is solid.

### shelf

**Concept: Universal data aggregation with 50+ importers**

- Encrypted SQLite, multi-user ACL/RBAC
- Importers for Apple, Google, Slack, GitHub, finance, IoT, databases, browsers
- Object lifecycle triggers, audit trail
- Sharing via TailScale + AutoMerge CRDT sync

**Keep**: The importer architecture (one module per source), the ACL model, and the trigger/event system. The importer list itself is a roadmap of data sources worth supporting.

### haus

**Concept: Multi-user JSON object store with triggers**

- REST API + SSE for real-time events
- Object versioning, file uploads to S3
- Scheduled tasks, email notifications
- Flexible JSON schema with validation

**Keep**: The event-driven architecture (SSE for push notifications), trigger system, and the idea of a generic object store that isn't locked to one schema.

### tldr-web

**Concept: Browser-rendered content extraction + LLM summarization**

- Playwright for JS-rendered pages (handles SPAs, lazy-loading)
- Ollama for structured summaries (title, summary, tags via Zod schema)
- Multiple input sources: URLs, files, browser bookmarks/tabs
- Multiple output formats: markdown, JSON, JSONL, plain text

**Keep**: The Playwright-based content extraction (handles modern web), Zod-validated LLM output schema, and multi-source/multi-format pattern.

### gg

**Concept: Markdown files as queryable database**

- Plain markdown files on filesystem -- human-readable, git-friendly
- Query language with keypaths and type inference
- Extensible via JavaScript actions/validators
- Published to NPM

**Keep**: The idea that sometimes the simplest storage IS the filesystem. Good for the "notes" artifact type where you want human-editable files.

---

## Tier 2: Valuable Patterns -- Extract and Reuse

These have useful ideas even if the projects themselves are incomplete.

### bookmarks-manager (shelf-bookmarks + bookmark-summarizer)

**Concept: Parallel metadata extraction with graceful degradation**

- Worker pool via Node.js cluster for bulk operations
- Content-type routing: different handlers for PDF, GitHub repo, generic page, image
- Multi-source title priority: Open Graph > page title > URL > filename
- Mozilla Readability for clean text extraction
- RAKE algorithm for keyword extraction
- Puppeteer screenshots as fallback when metadata is sparse

**Keep**: The content-type dispatcher pattern (route to specialized extractors by MIME type) and the graceful-degradation approach (partial metadata better than failure).

### unfurl

**Concept: Policy-driven resource lifecycle management**

- Design docs describe refresh policies (cron, TTL, iCal RRULE)
- Retry strategies with exponential backoff
- Auto-collection membership via inclusion/exclusion rules
- Hooks system (before/after events on resources)
- Pluggable fetchers per protocol/domain

**Keep**: The policy system concept. Different data sources need different fetch schedules, retry strategies, and freshness requirements. This is the RECURRING SCHEDULES design.

### digidex

**Concept: User-context-aware event classification**

- Architecture diagram showing: Events -> Classifier -> LLM Summarizer -> Semantic Index
- User context (interests, projects, employment, hobbies) shapes processing
- Personalized relevance scoring

**Keep**: The user-profile-as-processing-context idea. This is the "ASSESSED and AUGMENTED by LLM for RELEVANCE to ME PERSONALLY" requirement, architecturally sketched out.

### summarize-url.sh

**Concept: Analysis-at-ingestion with personal context**

- Grabs URL from active Arc tab via osascript
- Sends to GPT with a system prompt embedding personal interests
- Structured output: bullets, relevance score, benefit analysis, critical filters

**Keep**: The "critical filters" concept -- forcing the LLM to identify weaknesses/assumptions in content prevents uncritical information hoarding. Build this into every ingestion pipeline.

### lineout

**Concept: Keyboard-first outline editor with LLM assist**

- React + Vite, vim keybindings, modal editing
- Focus mode for deep work on subtrees
- LLM integration for content generation

**Keep**: The outline/tree UI pattern with focus mode. Relevant for the browsing/review interface.

### mindmapper

**Concept: Spatial knowledge organization**

- Rust + GPUI (Zed's framework), keyboard-driven
- Pan/zoom, inline editing, PNG export
- JSON file storage

**Keep**: Spatial/visual organization as a complementary view to hierarchical and search-based access.

### radio_hn

**Concept: Content-to-audio pipeline**

- Fetch stories -> LLM summarize -> TTS audio -> FFmpeg combine

**Keep**: The multi-modal output pipeline. The AUDIO artifact type needs exactly this: text-to-speech for articles/summaries.

### local-tldr

**Concept: In-browser LLM summarization**

- Firefox/Chrome extension, calls local Ollama
- Single-click page summarization, no API keys

**Keep**: Client-side AI processing pattern. Useful for the PUSH model where the user actively sends content to the system.

### tldw

**Concept: Screen activity capture as a PUSH data source**

- macOS shell script capturing display frames at configurable intervals
- FFmpeg for video assembly (mp4, webm, gif)
- Configurable: capture interval, cursor inclusion, max duration/frames, output format

**Keep -- and expand**: This is far more than a screen recording utility. Combined with a visual analysis pipeline, it becomes a powerful passive PUSH source:

```
SCREEN CAPTURE (frames at interval)
  -> VISUAL ANALYSIS (vision LLM: what is the user doing?)
  -> OCR (extract text from screen content)
  -> EVENT EXTRACTION (app switches, document opens, searches, code edits)
  -> TOPIC MATCHING (relevance to registered interests/projects)
  -> OUTPUT:
     - Summary events (what happened in this session)
     - Actions detected (opened X, read Y, wrote Z)
     - Insights (patterns in behavior, time allocation)
     - New information discovered (URLs visited, docs read, code reviewed)
     - Relevance updates (which topics got attention, emerging interests)
```

This turns ambient user activity into structured knowledge events without any manual capture. The existing tldw handles the first stage (frame capture); the pipeline stages above are the enrichment layers that make it a first-class data source for the unified system.

---

## Tier 3: Reference / Archived

### b-roll

Electron desktop app for data management. Overlaps significantly with shelf but as a desktop app. The importer list is similar. **Superseded by shelf's more flexible architecture.**

### dagger

Python groupware (calendars, notes, bookmarks) using TOML files. **Niche but the TOML-as-storage idea is interesting for human-editable config.**

### vim-notes-manager

Vimscript plugin for managing assets in markdown notes. **Keep as an example of editor integration for the capture layer.**

### notes/

100+ personal markdown files. **This IS the knowledge base in its most raw form. A migration target for the unified system.**

### bin/

`note` script -- fzf + $EDITOR for fuzzy-finding notes. **Tiny but effective. Shows the value of CLI-first access.**

### rag-codebase

TreeSitter-based code indexing with LLM summaries. **Specialized for code, but the AST-extraction + LLM-annotation pattern could apply to structured documents.**

### experiments/

Early prototypes. **Archived R&D.**

### tldr-web2

Incomplete web UI for tldr-web with SQLite persistence and relevance scoring. **The relevance-scoring-against-user-profile feature is the key idea here; absorb into the unified system.**

---

## Output / Render Layer

The pipeline (`Sources -> Fetch -> Enrich -> Store -> Query/Browse`) defines how knowledge gets _in_ and how it gets _found_, but says little about how it gets _out_ in human-presentable form. Search results, RAG syntheses, digests, and curated subsets all need to be rendered into formats a person reads, shares, or presents. This is the egress counterpart to the ingestion layer.

The system already stores artifacts as Markdown (gg, braindrain, notes/). That single fact makes [Pandoc](https://pandoc.org) the natural render engine: it parses Markdown into an abstract document tree and emits any target format. One adapter, every output format.

The contract:

```
artifact(s) -> assemble Markdown -> pandoc -> { terminal slides | PDF | HTML | DOCX | EPUB }
                                            -> TTS -> audio (radio_hn path)
```

Renderers are pure egress. They are not data sources, not ingestion paths, not enrichment steps. They sit at the end of the pipeline and at the "Act" stage of the feedback cycle (Gap 14): generating a deck, a PDF, or a shared HTML page is itself an action the user takes on stored knowledge, and the rendered artifact is shareable output (subject to the privacy tiers in Gap 12).

### radio_hn

**Concept: Content-to-audio rendering** (recategorized from Tier 2 -- it is an output renderer, not an ingestion pattern)

- Fetch stories -> LLM summarize -> TTS -> FFmpeg combine

**Role**: The audio render target. Renders Markdown/summaries to spoken audio. Same architectural position as the document renderers below, different modality.

### pandoc render adapters

**Concept: One conversion hub, many output formats**

Three external tools, all pandoc output targets:

- **patat** (https://github.com/jaspervdj/patat) -- terminal slideshow. Embeds pandoc as a library and drives a live, keyboard-driven presenter. Consistent with the stack's terminal-first tools (braindrain TUI, mindmapper, lineout, `note`). This is a _viewer_, not just an exporter.
- **Eisvogel** (https://github.com/Wandmalfarbe/pandoc-latex-template) -- LaTeX template for polished PDF export.
- **pandoc-bootstrap-adaptive-template** (https://github.com/diversen/pandoc-bootstrap-adaptive-template) -- responsive HTML output.

**Keep**: All three as thin render adapters behind one interface, not as standalone projects. patat is the terminal-viewer face of the render layer; Eisvogel and the Bootstrap template are passive file exporters. Because the store is already Markdown, adding pandoc unlocks PDF, HTML, slides, DOCX, and EPUB at once.

**Connects to**:

- **Gap 6 (Conversational query / RAG)** -- synthesis answers ("key takeaways on topic Y") are rendered to a deck or PDF instead of staying as ephemeral chat output.
- **Gap 5 (Proactive surfacing)** -- the morning digest and weekly review summary are render targets: a Markdown digest piped through pandoc to HTML (email/web) or patat (terminal review).
- **Gap 12 (Privacy boundaries)** -- any rendered/exported artifact must respect privacy tiers; export filtering applies here.

---

## Concept Map: What's Already Solved

| Unified System Concept              | Already Solved By                                              |
| ----------------------------------- | -------------------------------------------------------------- |
| PULL data sources on schedule       | arc-tabs-logger (launchd pattern)                              |
| PUSH data sources (user-initiated)  | rememberer (browser extension), local-tldr                     |
| PUSH data sources (passive/ambient) | tldw (screen capture + visual analysis pipeline)               |
| URL content fetching                | tldr-web (Playwright), bookmarks-manager (Cheerio/Readability) |
| PDF extraction                      | bookmarks-manager (pdf-lib)                                    |
| Structured artifact storage         | shelf/haus (SQLite + JSON), braindrain (tree model)            |
| Semantic indexing                   | rememberer (sqlite-vec + Ollama embeddings)                    |
| LLM relevance assessment            | summarize-url.sh, digidex architecture, tldr-web2              |
| User profile for personalization    | digidex, summarize-url.sh                                      |
| Multi-format output                 | tldr-web (MD/JSON/JSONL/text), radio_hn (audio)                |
| Document / slide rendering          | pandoc render layer (patat terminal, Eisvogel PDF, HTML)       |
| Recurring fetch schedules           | unfurl (policy design), arc-tabs-logger (launchd)              |
| Event/trigger system                | shelf (lifecycle triggers), haus (SSE + triggers)              |
| Hierarchical browsing               | braindrain (tree), lineout (outline)                           |
| Spatial browsing                    | mindmapper                                                     |
| Search (fuzzy + semantic)           | braindrain (dual search), rememberer (vector search)           |
| 50+ service importers               | shelf (iCloud, Google, Slack, GitHub, finance, etc.)           |
| Content-type routing                | bookmarks-manager (PDF/GitHub/page/image handlers)             |
| Parallel processing                 | bookmarks-manager (cluster workers)                            |
| Critical analysis at ingestion      | summarize-url.sh (critical filters)                            |
| Activity capture + visual analysis  | tldw (screen capture -> vision LLM -> events)                  |

---

## Recommendations for the Unified System

1. **rememberer's stack is the foundation** -- Go + SQLite + sqlite-vec + Ollama is proven for local-first semantic search with AI enrichment. Start here.

2. **shelf's importer architecture is the source connector model** -- One module per external service, with credential management and configurable schedules.

3. **unfurl's policy system is the scheduling layer** -- Different sources need different refresh cadences, retry strategies, and staleness thresholds.

4. **tldr-web's Playwright extraction is the web fetcher** -- Modern sites need real browser rendering; curl alone won't cut it.

5. **bookmarks-manager's content-type dispatcher is the pipeline router** -- Route artifacts to type-specific extractors (PDF, video, audio, web page, etc.).

6. **digidex + summarize-url.sh define the enrichment model** -- User profile shapes LLM assessment; structured output (summary, relevance score, tags, critical analysis) at ingestion time.

7. **braindrain's tree model + dual search is the browsing/retrieval layer** -- Hierarchical navigation with both fuzzy and semantic search covers all discovery patterns.

8. **tldw is the ambient activity pipeline** -- Screen capture + vision LLM + OCR + event extraction turns passive user activity into structured knowledge events. This is a unique data source that most PKM systems completely ignore.

---

## Gaps: What's Missing

The existing projects cover ingress and storage well but have significant gaps in several areas. Everything below is required for the unified system.

### Gap 1: Uncovered Data Sources

**Communication channels beyond Slack**:

- Discord, iMessage, Signal, Telegram, WhatsApp -- where link-sharing and idea exchange actually happen
- These are high-value PULL sources (periodic export/sync) and PUSH sources (real-time capture via hooks or bridges)

**Social media feeds**:

- Twitter/X bookmarks and likes
- Reddit saved posts and comment threads
- Mastodon, Bluesky feeds
- These are high-volume PULL sources with terrible native search -- exactly the kind of thing a second brain should absorb

**YouTube**:

- Watch history, liked videos, subscription activity, playlist additions
- Transcripts freely available via API (or Whisper for audio-only)
- Probably one of the biggest learning channels and it's completely unindexed

**Podcasts**:

- Subscription metadata, episode show notes
- Transcription via DICTATOR project (external dependency for all audio transcription)
- Same problem as YouTube: consumed, learned from, then lost to memory

**Documents received** (not created):

- Email attachments, downloads folder monitoring
- Shared Google Docs, Notion pages, Confluence pages
- Meeting recordings and transcripts (Zoom, Meet)

**Physical world capture**:

- Photos as information: whiteboard photos, book pages, conference slides, handwritten notes
- Vision LLM + OCR pipeline (same pattern as tldw but for camera roll)
- Location context: where you were when you encountered information matters for recall (Core Location + reverse geocoding)

### Gap 2: Deduplication and Canonical Resolution

The same article arrives from Twitter, Hacker News, a newsletter, and a Slack message. Nothing in the existing projects handles cross-source deduplication.

Required:

- **Canonical URL resolver** -- strip tracking params (utm\_\*, fbclid, etc.), follow redirects, normalize (www vs non-www, trailing slashes, protocol)
- **Content fingerprinting** -- for cases where the same content lives at different URLs (syndication, mirrors, AMP pages)
- **Merge strategy** -- when the same artifact is encountered from multiple sources, combine metadata (tags, annotations, context) from all encounters rather than discarding duplicates
- **First-seen / encounter-count tracking** -- "you've seen this from 4 different sources" is itself a relevance signal

### Gap 3: Knowledge Graph and Linking

The existing projects store artifacts as isolated records. None of them build connections between artifacts.

Required:

- **Automatic link discovery** -- shared URLs, shared entities (people, companies, projects), shared topics between artifacts
- **Backlinks** -- when artifact A references artifact B, B should know about A
- **Topic clustering** -- group artifacts by detected theme, not just user-assigned tags
- **Entity extraction** -- identify people, organizations, technologies, concepts mentioned across artifacts; build an entity registry
- **Graph query** -- "show me everything connected to X within 2 hops"
- **Visual graph view** -- mindmapper's spatial approach applied to the knowledge graph

### Gap 4: Decay, Resurfacing, and Spaced Repetition

Nothing in the system handles the "saved 8 months ago and forgot about it" problem.

Required:

- **Spaced repetition engine** -- resurface high-relevance items at increasing intervals (1 day, 3 days, 1 week, 2 weeks, 1 month, 3 months)
- **Adaptive scheduling** -- items you engage with get resurfaced more; items you dismiss decay faster
- **Staleness detection** -- "you saved 12 articles about X but never revisited any of them"
- **Interest drift tracking** -- as your user profile evolves, re-score old artifacts against the new profile; surface previously-low-relevance items that are now relevant
- **Review queue** -- daily/weekly queue of items to revisit, sized to be manageable (not 50 items; 5-10)
- **Engagement signals** -- time spent reading, sharing, annotating, acting on an item all feed back into its importance score

### Gap 5: Proactive Surfacing and Context-Aware Suggestions

Everything in the current design is reactive (you search, you browse). A second brain should also PUSH to you.

Required:

- **Morning digest** -- "here's what came in overnight, ranked by relevance to your active projects"
- **Context-aware suggestions** -- detect what you're working on (via tldw activity capture, active git repo, open files) and surface relevant artifacts: "you're working on project X, here are 3 things you saved that might help"
- **Trend detection** -- "you've been saving a lot about topic Y lately" with an option to promote Y to a tracked interest
- **Serendipity engine** -- occasionally surface random high-quality artifacts from outside your current focus to prevent tunnel vision
- **Weekly review summary** -- what you ingested, what you engaged with, what you ignored, how your interests shifted

### Gap 6: Conversational Query (RAG)

Search is keyword or semantic vector. The killer interface is natural language over the entire artifact store.

Required:

- **RAG layer** -- retrieve relevant artifacts, feed to LLM, generate synthesized answer with citations
- **Temporal queries** -- "what did I read last month about Rust async patterns?"
- **Comparative queries** -- "summarize the different approaches to X that I've encountered"
- **Synthesis queries** -- "based on everything I know about Y, what are the key takeaways?"
- **Source attribution** -- every answer must cite which artifacts it drew from, with links back to originals

### Gap 7: Conflict Detection

When two sources say contradictory things, the system should flag it.

Required:

- **Contradiction detection** -- LLM-powered analysis across artifacts on the same topic
- **Claim tracking** -- extract factual claims from artifacts, compare against existing claims in the knowledge base
- **Source credibility signals** -- track which sources have been reliable vs. unreliable over time
- **Flagged conflicts queue** -- surface contradictions for human resolution rather than silently storing both

### Gap 8: Artifact Lifecycle and Garbage Collection

Ingress exists but no concept of when something is no longer worth keeping. Storage grows unbounded.

Required:

- **Relevance decay** -- scores decrease over time unless refreshed by engagement or re-encounter
- **Active / archive / purge tiers** -- artifacts move through tiers based on relevance and age
- **Storage budgets per source** -- prevent one noisy RSS feed from dominating the knowledge base
- **Periodic review prompts** -- for items in the archive tier: "do you still care about these?"
- **Purge policy** -- configurable rules for permanent deletion (e.g., items below relevance threshold X after Y months in archive with no engagement)

### Gap 9: Source Health Monitoring

When a PULL source starts failing, you need to know.

Required:

- **Fetch success/failure tracking** -- per-source uptime metrics
- **Alerting** -- notification when a source has failed N consecutive fetches
- **Auth expiry detection** -- API tokens and OAuth sessions expire; detect and prompt for re-auth
- **Schema change detection** -- when a source's output format changes (API breaking change, RSS format shift), flag it
- **Source dashboard** -- at-a-glance view of all sources, their health, last successful fetch, next scheduled fetch

### Gap 10: User Feedback Loop

The system assesses relevance but has no mechanism to learn from its mistakes.

Required:

- **Explicit feedback** -- thumbs up/down on surfaced items, "more like this / less like this"
- **Implicit signals** -- time spent reading, whether you shared it, whether you took notes, whether you acted on it
- **Profile learning** -- feedback signals update the user profile model over time, not just the static text document
- **Relevance model tuning** -- track prediction accuracy (did the user engage with high-scored items? ignore low-scored ones?) and adjust
- **Correction mechanism** -- "this was marked irrelevant but it's actually important" with explanation that refines the model

### Gap 11: Multi-Device Access

Everything is local-first SQLite on one Mac.

Required:

- **HTTP API with auth** -- read and limited write access from any device
- **Mobile-friendly web UI** -- responsive design for phone/tablet review and triage
- **Push notifications** -- morning digest, high-relevance alerts, review reminders delivered to phone
- **Capture from mobile** -- share sheet integration (iOS/Android) to push URLs, photos, voice memos into the system
- **Sync strategy** -- decide between:
  - Central server (Mac as hub, devices connect via API)
  - Peer sync (CRDTs via AutoMerge, as shelf explored with TailScale)
  - Hybrid (local-first with periodic sync to a shared store)

### Gap 12: Privacy Boundaries

Not all artifacts are equal. Work, personal finance, health data need different treatment.

Required:

- **Privacy tiers** -- public, personal, sensitive, restricted (with per-artifact override)
- **Default tier per source** -- work email defaults to "restricted", HN defaults to "public"
- **Sharing controls** -- when exposing a curated subset ("my favorite articles on X"), privacy boundaries must be enforced
- **LLM processing rules** -- sensitive artifacts may need local-only LLM processing (no cloud API calls)
- **Export filtering** -- any export or API response respects privacy tiers
- **Audit log** -- who/what accessed sensitive artifacts and when

### Gap 13: Transcription Pipeline (DICTATOR Dependency)

Audio and video artifacts require transcription as a prerequisite for all downstream processing.

Required:

- **DICTATOR project integration** -- external dependency for all audio-to-text conversion
- **Applies to**: podcasts, voice memos, meeting recordings, phone calls, voicemails, video narration, YouTube audio
- **Pipeline position** -- transcription happens early in the enrichment pipeline, before summarization, tagging, or semantic indexing
- **Transcript as artifact** -- the transcript itself is a first-class artifact linked to the source audio/video
- **Speaker diarization** -- identify who said what (critical for meetings, phone calls, podcasts with multiple hosts)
- **Timestamp alignment** -- link transcript segments back to source timestamps for "jump to this moment" functionality

### Gap 14: The Feedback Cycle (System-Level Architecture)

The biggest structural gap: the system is currently a one-way funnel. A real second brain is a cycle.

Current (linear):

```
Sources -> Fetch -> Enrich -> Store -> Search/Browse
```

Required (cyclic):

```
Sources -> Fetch -> Enrich -> Store -> Surface -> Act -> Reflect -> Update Profile
                                                                        |
                                              reshapes Fetch priorities <-+
                                              refines Enrichment models <-+
                                              adjusts Surfacing rules   <-+
```

The "Act -> Reflect -> Update Profile" stages are where the system actually learns what matters. The user profile must be a living model that updates based on engagement, not just a static text document declaring interests.

This cycle means:

- **What you engage with** changes what gets fetched next
- **What you dismiss** changes how similar items are scored
- **What you create** (notes, annotations, shares) feeds back as new artifacts
- **How your interests shift** triggers re-scoring of the entire archive
- **Source performance** (which sources produce items you engage with) affects fetch priority

---

## Concept Map: What's Already Solved

| Unified System Concept              | Already Solved By                                              |
| ----------------------------------- | -------------------------------------------------------------- |
| PULL data sources on schedule       | arc-tabs-logger (launchd pattern)                              |
| PUSH data sources (user-initiated)  | rememberer (browser extension), local-tldr                     |
| PUSH data sources (passive/ambient) | tldw (screen capture + visual analysis pipeline)               |
| URL content fetching                | tldr-web (Playwright), bookmarks-manager (Cheerio/Readability) |
| PDF extraction                      | bookmarks-manager (pdf-lib)                                    |
| Structured artifact storage         | shelf/haus (SQLite + JSON), braindrain (tree model)            |
| Semantic indexing                   | rememberer (sqlite-vec + Ollama embeddings)                    |
| LLM relevance assessment            | summarize-url.sh, digidex architecture, tldr-web2              |
| User profile for personalization    | digidex, summarize-url.sh                                      |
| Multi-format output                 | tldr-web (MD/JSON/JSONL/text), radio_hn (audio)                |
| Document / slide rendering          | pandoc render layer (patat terminal, Eisvogel PDF, HTML)       |
| Recurring fetch schedules           | unfurl (policy design), arc-tabs-logger (launchd)              |
| Event/trigger system                | shelf (lifecycle triggers), haus (SSE + triggers)              |
| Hierarchical browsing               | braindrain (tree), lineout (outline)                           |
| Spatial browsing                    | mindmapper                                                     |
| Search (fuzzy + semantic)           | braindrain (dual search), rememberer (vector search)           |
| 50+ service importers               | shelf (iCloud, Google, Slack, GitHub, finance, etc.)           |
| Content-type routing                | bookmarks-manager (PDF/GitHub/page/image handlers)             |
| Parallel processing                 | bookmarks-manager (cluster workers)                            |
| Critical analysis at ingestion      | summarize-url.sh (critical filters)                            |
| Activity capture + visual analysis  | tldw (screen capture -> vision LLM -> events)                  |

## Concept Map: What's NOT Yet Solved

| Unified System Concept               | Status   |
| ------------------------------------ | -------- |
| Cross-source deduplication           | NEW      |
| Canonical URL resolution             | NEW      |
| Knowledge graph / backlinks          | NEW      |
| Entity extraction and registry       | NEW      |
| Spaced repetition / resurfacing      | NEW      |
| Adaptive relevance decay             | NEW      |
| Morning digest / proactive surfacing | NEW      |
| Context-aware suggestions            | NEW      |
| Trend and interest drift detection   | NEW      |
| Conversational query (RAG)           | NEW      |
| Conflict / contradiction detection   | NEW      |
| Source health monitoring             | NEW      |
| User feedback loop / model learning  | NEW      |
| Multi-device access (HTTP API)       | NEW      |
| Mobile capture (share sheet)         | NEW      |
| Privacy tiers and access controls    | PARTIAL  |
| Transcription pipeline (DICTATOR)    | EXTERNAL |
| Speaker diarization                  | NEW      |
| Artifact lifecycle / garbage collect | NEW      |
| Storage budgets per source           | NEW      |
| Feedback cycle (profile learning)    | NEW      |
| Chat platform ingestion              | NEW      |
| YouTube / podcast ingestion          | NEW      |
| Physical world capture (photos/OCR)  | NEW      |
| Location context for artifacts       | NEW      |
| Serendipity engine                   | NEW      |

---

## Recommendations for the Unified System

1. **rememberer's stack is the foundation** -- Go + SQLite + sqlite-vec + Ollama is proven for local-first semantic search with AI enrichment. Start here.

2. **shelf's importer architecture is the source connector model** -- One module per external service, with credential management and configurable schedules.

3. **unfurl's policy system is the scheduling layer** -- Different sources need different refresh cadences, retry strategies, and staleness thresholds.

4. **tldr-web's Playwright extraction is the web fetcher** -- Modern sites need real browser rendering; curl alone won't cut it.

5. **bookmarks-manager's content-type dispatcher is the pipeline router** -- Route artifacts to type-specific extractors (PDF, video, audio, web page, etc.).

6. **digidex + summarize-url.sh define the enrichment model** -- User profile shapes LLM assessment; structured output (summary, relevance score, tags, critical analysis) at ingestion time.

7. **braindrain's tree model + dual search is the browsing/retrieval layer** -- Hierarchical navigation with both fuzzy and semantic search covers all discovery patterns.

8. **tldw is the ambient activity pipeline** -- Screen capture + vision LLM + OCR + event extraction turns passive user activity into structured knowledge events.

9. **DICTATOR is the transcription backbone** -- All audio/video artifacts depend on transcription before any downstream enrichment can happen. This is an external project dependency that must be integrated early.

10. **The feedback cycle is the architecture** -- The system is not a pipeline, it's a loop. Every interaction (engagement, dismissal, annotation, creation) feeds back into the user profile, which reshapes fetching, enrichment, and surfacing. Design for this from day one or it will never be retrofitted.
