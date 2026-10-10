# Large-document splitting and merging — design

**Date:** 2026-10-10
**Status:** approved in conversation, spec awaiting review
**Builds on:** [sync-only upstream design](2026-10-10-sync-only-upstream-design.md). Its upstream
call, outcome classes, parameter policy, settings and migrations are assumed throughout.

## Problem

Large documents are slow and fragile on the Azure pods. A document with many pages can take a long
time, and sometimes the call hangs. The same pods analyse 3–5 pages in under 2 seconds. With one
gateway in front of up to 8 autoscaling pods, a single large document also ties up one pod while
the others sit idle.

## Goal

- **Split large PDFs.** A PDF over `SPLIT_PAGES` pages (default 5) is split into parts of 3–5
  pages. The parts are analysed in parallel through HAProxy, and the part results are merged into
  one result.
- **The merged JSON matches a whole-document analysis.**
  - Everything Azure determines page by page is **identical**: words, lines, selection marks, the
    tables and figures on each page, page size and angle, and every offset, page number, figure id
    and cross-reference.
  - Document-wide judgements (§6) are **measured** against whole-document results on the user's
    real documents. Every difference found gets a merge rule that reproduces Azure's
    whole-document answer.
- **Failures are absorbed.** Hung or failing parts are retried, hedged and, if needed, broken into
  smaller parts. A document fails only when Azure would have failed it.
- **Fast and efficient.**
  - The Azure slots are shared fairly between jobs.
  - Memory per job is bounded regardless of document size.
  - Uploads stay proportional to the document's size.
  - The merge is a single streaming pass.

## Non-goals

- Splitting anything but PDF: images, TIFF, Office and HTML documents are sent whole.
- Fetching `urlSource` documents in the gateway. The container fetches them itself, as today, so
  they are sent whole.
- Persisting part progress across a gateway restart. Parts take seconds, so a restarted job is
  re-run from its stored input.
- Changing Wega's vision service. Its own split-and-merge stays exactly as it is. A vision slice
  over 5 pages is simply split again by the gateway. Both merges are page-exact, so the two compose.

## Design

### 1. When a document is split

The decision is made per job, on the stored input. The engine (§3) serves both entry routes:

- **`:analyze`**, from the input already on the PVC;
- **a client's `:syncAnalyze`**, whose body the gateway now saves to the job's work folder first
  (bounded by `MAX_REQUEST_BYTES` and `UPLOAD_TIMEOUT`). This replaces the single-attempt
  passthrough of the sync-only design, so sync clients also get retries and splitting.

A document is split when **all** of these hold:

1. `SPLIT_PAGES` > 0.
2. The body is a PDF:
   - `Content-Type: application/pdf`; or
   - `application/octet-stream` whose bytes contain `%PDF-` within the first 1024 bytes; or
   - a JSON `base64Source` that decodes to such bytes.
3. The splitter can open it: it parses, it is not encrypted, and it opens within the split time
   budget (§2).
4. The pages to analyse number more than `SPLIT_PAGES`. These are all pages, or the client's
   `pages=` selection if it gave one.
5. Any client `pages=` value parses as Azure's grammar (`1-3,5,7-9`) and names only existing pages.

Otherwise the document is sent **whole**, as one call, exactly as the client sent it. That call
gets the sync-only design's per-attempt timeout (`DI_UPSTREAM_TIMEOUT` / `READ_SYNC_TIMEOUT`) and
attempt rules. When a client `pages=` names pages that don't exist, Azure therefore produces its
own error.

### 2. Splitting (`internal/pdfsplit`)

- **Library.** [`pdfcpu`](https://github.com/pdfcpu/pdfcpu): pure Go, Apache-2.0, no cgo.
- **Parse once, cut many.** The PDF is read into a `model.Context` once. Each part is produced
  with `pdfcpu.ExtractPages(ctx, pageNrs, true)` and written with `api.WriteContext`. Parts copy the
  original page objects and resources, so each page renders exactly as in the original.
- **Planner.** The selected pages (in order) are cut into consecutive runs:
  - `k = ceil(n / SPLIT_PAGES)` parts;
  - the first `n mod k` parts get one page more than the rest.

  With the default 5:

  | Pages | Parts |
  |---|---|
  | 6 | 3 + 3 |
  | 7 | 4 + 3 |
  | 11 | 4 + 4 + 3 |
  | 16 | 4 + 4 + 4 + 4 |
  | 21 | 5 + 4 + 4 + 4 + 4 |

  Every part has between `ceil(SPLIT_PAGES/2)` and `SPLIT_PAGES` pages. A part may hold
  non-consecutive original pages when the client's `pages=` skipped some. Each part records its
  original page numbers in order.
- **Limits.**
  - At most **2** PDFs are parsed and cut at once (a semaphore). This bounds the gateway's memory.
  - Parsing and cutting together have a **60-second budget**.
  - A panic inside `pdfcpu` is recovered.

  Any of these failures sends the document whole. The reason is counted (§8).
- **Where the parts go.** Part files are written into the job's work folder on the PVC,
  `${DATA_DIR}/work/<jobId>/`, and the parsed context is released as soon as the parts exist.

### 3. The engine (`internal/engine`) and scheduling (`internal/sched`)

**Engine.** For each job, the engine:

1. decides between whole and split (§1);
2. splits (§2);
3. runs the parts through the scheduler (below) with the part rules of §4;
4. merges the part results (§5) into one result file;
5. hands that file to the existing completion path (`finish`), so the poll serves it unchanged.

A whole document goes through the same scheduler as a single "part" with the long timeout.

**Scheduler.**

- **One scheduler per surface**, holding `*_MAX_INFLIGHT` slots. Every call to Azure takes a
  slot: whole documents, parts and hedges alike. It replaces today's `AdmitUpstream` 100 ms
  busy-wait with a proper wait queue.
- **Fair sharing.** Waiting calls are grouped by job, and a freed slot goes to the **next job in
  round-robin order** that has a call waiting. A 2-page job never waits behind a 2,000-page one,
  and a single large job alone gets every slot.
- **Hedges use idle capacity only.** `TryAcquireIdle` succeeds only when a slot is free *and* no
  call is waiting.
- **Workers.** Workers per surface stay at `*_MAX_INFLIGHT` (active jobs), and admission
  (`*_MAX_INFLIGHT + QUEUE_DEPTH`) is unchanged.
- **Job deadline.** A whole job, all parts included, has **60 minutes**, the containers' own
  maximum task time (`Task:MaxRunningTimeSpanInMinutes`). Past it, the job fails with a timeout
  error shaped for its surface.

### 4. Part rules (`internal/engine`)

Each part attempt is one `:syncAnalyze` call with the part PDF as the body (`application/pdf`) and
the client's forwarded parameters (the sync-only parameter policy), minus `pages`.

| Situation | Rule |
|---|---|
| **Attempt timeout** | **60 seconds** per part attempt; a 3–5 page part normally takes about 2 s. A hung pod costs one minute, not fifteen. |
| **Hedge** | An attempt still running after `clamp(4 × median, 5s, 30s)` gets **one** duplicate, if `TryAcquireIdle` succeeds. The median is of the surface's last 200 successful part durations; with fewer than 20 samples the delay is 30s. The first success wins; the other is cancelled and its file deleted. A hedge belongs to its attempt: the attempt succeeds if either call succeeds, and counts as one failed attempt only if both fail. |
| **Retriable outcome** (sync-only §1 rows 5–8: timeouts, 5xx, connection errors, 202) | Up to **3 attempts** per part at its current size, with the sync-only backoff. |
| **Still failing after 3 attempts** | **Halve the part.** The first half is `ceil(n/2)` pages, cut again from the part's own PDF. Each half gets 3 fresh attempts, so the sequence is 5 → 3 + 2 → 2 + 1 / 1 + 1 → single pages. |
| **A single page exhausts its attempts** | The job fails with that attempt's error, as the whole document would have. |
| **Terminal outcome** (rows 3, 9, 10: analysis `failed`, other 4xx, unusable 200) | **Re-check against the original.** Send the *original* file with `pages=<this part's original pages>`. If that succeeds, use its result; a splitting artefact can never fail a good document. If it fails terminally too, the job fails with **that** error, Azure's own answer about the original. |
| **Route not served** (row 4) | The job fails immediately (a deployment fault). |
| **Shutdown** (row 1) | Every part attempt is cancelled. The job stays `running`, and recovery re-runs it from the stored input. |

Part results are written to the work folder, named by original page range.

### 5. Merge (`internal/merge`)

**Safety checks before merging.** If any check fails, the engine analyses the **whole** document
instead (one call, long timeout) and counts the fallback (§8):

- `apiVersion`, `modelId`, `stringIndexType` and `contentFormat` are equal across parts;
- every part has an `analyzeResult`;
- the pages returned cover the selected pages exactly once, in ascending order, after mapping;
- any top-level member that is not an array is equal across parts.

**Page mapping.** For each part, its returned page numbers map to original page numbers:

- results numbered `1..k` (a physically split part) map through the part's page list;
- results already numbered in the original range (a `pages=` re-check) map to themselves.

**Streaming rewrite (`internal/jsonx`).**
- A path-aware tokenizer copies each part's bytes verbatim and replaces only targeted scalar
  values. Numbers, string escapes, member order and unknown fields therefore stay exactly as Azure
  wrote them.
- Memory is a few buffers whatever the result size.
- It runs in one pass to measure (content length, element counts, member order) and one pass to
  write.

**Document Intelligence rules.** The `analyzeResult` members are written in the order of the first
part's `analyzeResult`, with members seen only in later parts appended.

| What | Rule |
|---|---|
| Scalars (`apiVersion`, `modelId`, `stringIndexType`, `contentFormat`) | From the first part. |
| `content` | The parts' raw JSON string bodies concatenated, with the page separator between consecutive parts: `\n` for `text`, `\n<!-- PageBreak -->\n` for `markdown`. No decode or re-encode. The separators are confirmed by the §6 probe. |
| Every `offset` inside a `span` object or a `spans[]` element, anywhere | `+ shift(part)`. `shift` is the length of all earlier parts' content plus separators, counted in the request's `stringIndexType`:<br>• `textElements` (the default): extended grapheme clusters per UAX #29, via `github.com/rivo/uniseg` (MIT), as .NET's `StringInfo` counts them;<br>• `unicodeCodePoint`: runes;<br>• `utf16CodeUnit`: UTF-16 units. |
| `pages[].pageNumber`, and every `pageNumber` inside a `boundingRegions[]` element | Mapped to the original page. |
| Strings inside any `elements` array matching `^/(paragraphs\|tables\|figures\|sections\|keyValuePairs)/\d+$` | Index `+` the earlier parts' element count for that member. `/sections/N` uses the section rule below. |
| `figures[].id` (`"<page>.<n>"`) | The page mapped to the original; `<n>` is kept. |
| `sections` | **One root**, as Azure produces. The merged `sections[0].elements` is every part's root `elements` in order, shifted; its `spans` is one span from offset 0 to the end of the last part's root span. Non-root sections follow in part order. Part *k*'s section *i* ≥ 1 becomes `1 + Σ_{j<k}(sections_j − 1) + (i − 1)`. |
| Other arrays (`paragraphs`, `tables`, `figures`, `styles`, `warnings`, and any unknown array) | Concatenated in part order, with the rules above applied structurally. |

**Read rules.**
- `analyzeResult.version` and `modelVersion` must agree across parts and are taken from the first.
- `readResults[]` are concatenated in part order with `page` mapped to the original. Nothing else
  changes. Read results have no spans or cross-references, so the Read merge is exact by
  construction.

**Output.**
- The merged `analyzeResult` goes into the gateway's normal envelope for an `:analyze` job.
- For a client `:syncAnalyze` it uses the same shape the container's sync route returned for the
  parts: bare result, or wrapper.
- It is checked against `MAX_RESULT_BYTES` and moved into the blob store with the existing atomic
  write.

### 6. Measure, then fix

**`gateway probe --compare-split <folder>`.** For every PDF in the folder, the probe:

1. analyses it **whole** through the container's `:syncAnalyze`;
2. analyses it **split**, through the gateway's own planner, splitter, part calls and merge code,
   linked into the probe binary;
3. diffs the two JSONs field by field.

It writes a report (JSON plus a text summary) grouped by path pattern, for example
`paragraphs[*].role: 3 differences`, with the first examples. It labels each pattern:

- **page-level:** anything under `pages[]`, page-local bounding regions, figures, and the tables on
  a page;
- **document-level:** paragraph roles, `sections`, the table count, `styles`, and `content`
  differences at part boundaries.

**Go-live gate.** On a representative corpus the user supplies (at least 30 PDFs across their
document types):

- **zero page-level differences**;
- **every document-level difference pattern** either gets a merge rule that reproduces Azure's
  whole-document answer, with a regression test built from that document, or is explicitly
  accepted by the user.

The likely document-level candidates are:
- which heading Azure marks `title`;
- section nesting that continues across a split point;
- page header/footer recognition;
- a table that Azure joins across a page break.

Rules are written only after the probe shows what Azure actually does. None is guessed in advance.

### 7. Configuration

**One new setting:** `SPLIT_PAGES`, default `5`; `0` turns splitting off; must be ≥ 0. The gateway
then has 22 settings.

**Constants**, documented in OPERATIONS:

| Constant | Value |
|---|---|
| Part attempt timeout | 60s |
| Attempts per part size | 3 |
| Hedge delay | `clamp(4 × median, 5s, 30s)` |
| Job deadline | 60m |
| Concurrent splits | 2 |
| Split budget | 60s |

`*_MAX_INFLIGHT` now means concurrent Azure calls of any kind, sized as max pods × per-pod
concurrency (16 for 8 × 2).

**Migration 0002** (sync-only design) also adds `upstream_parts INTEGER NOT NULL DEFAULT 0`, the
number of parts the job was analysed in (1 = whole). `/_gw/jobs` shows it.

### 8. Observability

**`/_gw/metrics`.** Histograms are rendered by hand, like the existing metrics.

| Metric | Type |
|---|---|
| `gateway_part_seconds{surface}` | histogram |
| `gateway_job_parts{surface}` | histogram |
| `gateway_merge_seconds{surface}` | histogram |
| `gateway_parts_total{surface,outcome}`, where `outcome` ∈ `succeeded` · `retried` · `halved` · `hedged` · `hedge_won` · `rechecked` | counter |
| `gateway_split_fallback_total{surface,reason}`, where `reason` ∈ `unparseable` · `encrypted` · `budget` · `panic` · `pages_param` · `merge_check` | counter |

**Logs.** Every engine log line carries `job`, `surface` and, for parts, `pages` (for example
`12-15`) and `attempt`. A job's final line adds `parts`, `halvings`, `hedges` and `fallback`.

### 9. Invariants

- The client's 202, the poll states and the result envelope are unchanged. A merged result is
  served exactly like a whole one.
- Every call to Azure, whether a part, a hedge, a re-check or a whole document, holds a scheduler
  slot. The slot is released on every return path.
- Hedges never take a slot that a waiting call could use.
- A merged result is written only when every §5 safety check passes. Otherwise the whole document
  is analysed.
- A job's work folder is deleted when the job finishes. The sweeper removes work folders that have
  no live job, which covers crashes.
- Cancellation is classified on the context first. An interrupted job stays `running` for recovery.

### 10. Testing

**Unit tests.**
- **Planner:** page counts 1–2,000, client `pages=` selections, and the part-size bounds.
- **`pages=` parser:** valid and invalid forms.
- **Unit counting:** graphemes (ZWJ emoji, combining marks, flags), code points, and UTF-16
  surrogates.
- **Streaming rewriter:** output identical to input when nothing is rewritten; each targeted
  rewrite; escapes inside strings; nested arrays; a 100 MB benchmark with a memory bound.
- **Merge safety checks,** and the section-root rule.

**Round-trip test.** A recorded whole-document result is cut into synthetic parts by the inverse
transform: offsets un-shifted, pages renumbered from 1, pointers re-based, sections split at the
root. Merging the parts must reproduce the original **byte for byte**. This runs for both
surfaces and all three `stringIndexType` values.

**`internal/mockazure` paged fake.**
- Test PDFs are generated with a distinct page width per page, so the fake identifies a part's
  original pages from the part PDF's MediaBoxes.
- It answers with the matching slice of a recorded whole result.
- Hangs, failures and terminal errors can be injected per page.

**Engine tests.**
- A hung part is hedged and the hedge wins.
- Repeated timeouts halve a part down to a single page.
- A terminal part error triggers the `pages=` re-check.
- An unparseable PDF goes whole.
- A failed merge check falls back to whole.
- The job deadline.
- A shutdown mid-fan-out followed by recovery.

**Scheduler tests.**
- Round-robin fairness.
- Hedges take only idle slots.
- No slot leaks, under `-race`.

**Conformance.** A 40-page PDF through the SDK flows of both surfaces gives a merged result with
the expected shape, and the gateway's sync route returns the same `analyzeResult` as its async
route.

**Benchmarks.** Splitting a 500-page PDF (time and peak memory); merging 100 parts of 1 MB each;
scheduler overhead per call.

**Gate.** As in the sync-only design, plus the §6 compare-split report.

## Rollout

1. **Implement after the sync-only design,** on the same branch. This depends on its upstream
   call, outcome classes, settings and migrations. Docs updated in the same change:
   - the README settings table;
   - `ARCHITECTURE.md`, with a new splitting section and an updated worker sequence;
   - `OPERATIONS.md`: memory sizing for 2 concurrent splits, the constants, and the new metrics;
   - `TESTING.md` and `HANDOFF.md`.
2. **The user runs `gateway probe --compare-split`** on a representative corpus. Rules are added
   for the differences it reports, and the gate is met before go-live.
3. **Wega:** vision's own split-and-merge is **kept as is**. The Wega follow-up only sets
   `SPLIT_PAGES` and the slot sizing in the gateway manifests.
