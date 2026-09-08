# azure-gateway-api — design

**Date:** 2026-09-08
**Status:** approved, implementing

## Problem

Two Azure AI containers run inside an OpenShift cluster, outside the Azure ecosystem:

- **Document Intelligence `prebuilt-layout`**, `api-version=2024-11-30`
- **Computer Vision Read v3.2**, `model-version=2022-04-30`

Their synchronous behaviour is correct and stable. Their **asynchronous** behaviour is not, because
the async result store is container-local by default. When OpenShift autoscales or reschedules a pod:

- a `resultId` minted by replica A is invisible to replica B — a poll routed elsewhere either 404s or
  stalls on `running` forever;
- a restart destroys in-flight work with **no failure signal**, and invalidates every operation id
  already handed to a client;
- `HealthCheck:MemoryUpperboundInMB` makes the container self-report unhealthy under memory pressure,
  so the kubelet restarts pods *mid-analysis*.

Microsoft's only supported fix is a shared **Azure Blob + Azure Queue** backend
(`"Currently only Azure Storage and Azure Queue are supported"` — no S3, no MinIO; Redis and RabbitMQ
were removed in v3.x). Outside Azure, that fix does not exist. See `docs/api-surface.md` §8.

## Goal

A gateway that is **wire-identical** to both containers, so no client changes anything, while owning
the async lifecycle itself on durable storage.

- Client hits a **sync** endpoint → streaming passthrough.
- Client hits an **async** endpoint → gateway returns `202` with its own operation id, calls the
  container **synchronously**, persists the result to a PVC, and serves the client's polls from its
  own store.

The client-facing contract stays `202 + Operation-Location + poll` because that is what every Azure
SDK is generated against. The gateway owns the LRO fiction; the upstream call underneath is
synchronous whenever the container allows it.

## Non-goals

`:analyzeBatch` (Azure-Blob-bound, unusable air-gapped) · `/formrecognizer` legacy aliases ·
custom models and classifiers · client-facing authentication · Kubernetes/Helm manifests ·
multi-replica gateway.

The gateway is **single-pod by design**. Operation ids are pod-local by construction — that is
precisely the property that makes a PVC the right store and a second gateway replica wrong.

---

## Decisions

| # | Decision | Rationale |
|---|---|---|
| D1 | Go 1.23, stdlib `net/http` | Static binary, distroless image, best-in-class streaming of large PDFs |
| D2 | SQLite via `modernc.org/sqlite` (pure Go, no cgo) | Keeps the binary static; SQL + indexes for TTL/GC and job queries |
| D3 | Upstream call is always synchronous where possible | The user's requirement, and the documented field workaround for the multi-replica problem |
| D4 | `429` the moment workers are busy; queue depth configurable, default `0` | Explicit user choice |
| D5 | 24h TTL, Azure-identical | Matches the cloud service; expired ids `404` exactly like Azure |
| D6 | Read always uses `syncAnalyze` | Explicit user choice. ⚠️ makes the OpenShift Route timeout the binding constraint |
| D7 | Gateway requires no client API key; injects its own upstream | Explicit user choice |
| D8 | Gateway serves its own `/status` and `/ready` | Both containers claim those paths at root — passthrough would collide |

---

## Architecture

```
                      ┌─────────────────────────── single pod ───────────────────────────┐
  unmodified          │                                                                  │
  Azure SDK    ──►    │  surface/di    ┐                     ┌─ upstream/di  ──► Layout   │
  clients             │  surface/read  ├─► jobs (workers) ──►├─ upstream/read──► Read     │
                      │  admin /_gw/*  ┘        │            └                            │
                      │                         ▼                                         │
                      │                 store: SQLite + blobs                             │
                      └─────────────────────────┬────────────────────────────────────────┘
                                                ▼
                                          PVC  /data
```

### Packages

| Package | Responsibility |
|---|---|
| `cmd/gateway` | wiring, lifecycle, graceful shutdown, `probe` subcommand |
| `internal/config` | env-driven config, fail-fast validation |
| `internal/logging` | `log/slog` JSON, request-scoped logger |
| `internal/httpx` | recover, request-id, access log, body cap, timeouts, streaming helpers |
| `internal/azerr` | the **two** error shapes + code catalogue + per-surface emitters |
| `internal/ids` | canonical 36-char lowercase GUIDs, result-id validation |
| `internal/store` | SQLite job store; PVC blob store (atomic write, shard, stream, GC) |
| `internal/upstream` | blocking `Analyze` per surface; capability detection; affinity fallback |
| `internal/jobs` | worker pool, admission control, lease/heartbeat, crash recovery, TTL sweeper |
| `internal/surface/di` | DI handlers + envelope composition |
| `internal/surface/read` | Read handlers + envelope composition |
| `internal/admin` | `/_gw/health,ready,live,jobs,metrics,version,config` |
| `internal/probe` | live-container capability probe (subcommand + startup) |
| `internal/mockazure` | faithful fakes of **both** containers, including the failure modes |

Each package is independently testable and depends only on those above it. `store`, `azerr`, `ids`
and `config` are leaves with no gateway-internal dependencies.

---

## Upstream call strategy

The single most important finding: **both containers expose a native synchronous analyze route.**
Read's is documented; DI's is not, and was found by forensics on the image itself
(`"SyncAnalyzePath"` in `/app/appsettings.api.json`, a compiled route literal in
`Microsoft.CloudAI.Containers.VDI.Endpoint.Analyze.dll`, MVC action `AnalyzeController.SynchronousAnalyze`).

```go
// One interface. The worker cannot tell which strategy ran.
type Analyzer interface {
    Analyze(ctx context.Context, doc Document, p Params) (json.RawMessage, error)
}
```

### DI (`upstream/di.go`) — dual-mode with mandatory fallback

```
POST {up}/documentintelligence/documentModels/{modelId}:syncAnalyze?api-version=2024-11-30
  200  -> done. Fully stateless; load-balances correctly.
  202  -> container degraded to async under memory pressure (observed on 3.1; MS: 202 is
          "not standard or documented behavior"). Pin to the replica that answered and poll
          with affinity until terminal.
  404 | 500 UnhandledEndpointException
       -> this image build does not serve the route. Latch capability=off process-wide and
          use :analyze + affinity poll permanently.
```

### Affinity fallback

Only reached on the degraded paths above.

```
transport := per-job (MaxIdleConnsPerHost:1, keep-alives on)  // same TCP conn -> same HAProxy backend
jar       := per-job cookie jar                                // replays the OpenShift router cookie
loop until DI_UPSTREAM_TIMEOUT (backoff 500ms -> 5s):
    GET analyzeResults/{upstreamId}   (jar + same conn; URL rebuilt against configured base)
      200 running|notStarted -> continue
      200 succeeded          -> return
      200 failed             -> return upstream error
      404                    -> miss++; miss > DI_BLIND_POLL_BUDGET ? fail : continue
```

`sessionAffinity: ClientIP` on a Kubernetes Service is **useless here** — every request from the one
gateway pod shares a source IP, so all traffic would pin to a single backend.

### Read (`upstream/read.go`)

`POST {up}/vision/v3.2/read/syncAnalyze` — always, per D6. Same 202-degradation guard.

⚠️ **Deployment constraints, both real and both documented in the README:**
- OpenShift Routes default to a **30s** HAProxy timeout; a large PDF holds the connection for
  minutes. Set `haproxy.router.openshift.io/timeout`.
- **No `vision` substring in the upstream Service name.** The Read container corrupts its own
  `Operation-Location` when the inbound authority contains `vision`, stripping the port and the
  `/vision` segment. The gateway mints its own header so it is immune, but the constraint is real.

---

## Fidelity contract

Every rule below is traceable to a specific client's source and becomes a named conformance test.
Full derivation in `docs/api-surface.md` §4, §5, §6, §10.

### `Operation-Location` — differs per surface

**DI** — must be absolute, and must satisfy four different parsers at once:

```
{public}/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}?api-version=2024-11-30
```

- The literal segment `/documentintelligence/` **must** be present — Python `_patch.py`, .NET
  `OperationWithId.cs`, Java `PollingUtils.java` and JS `pollingHelper.ts` all hard-code
  `[^:]+://[^/]+/documentintelligence/.+/([^?/]+)` to derive the operation id.
- `{modelId}` must sit **4 segments from the end**, `{resultId}` **2 from the end**, query present —
  `Azure.AI.FormRecognizer` 4.x discards the URL and counts backwards after `Split('/','?')`.
- `?api-version=` must be emitted — Python never re-appends it; .NET and Java replace it; JS appends.
- Percent-encode everything. A literal `{`/`}` raises in Python's `str.format()`; an unencoded
  `{`, space, `|` or `^` makes Java's `new URI(path)` throw and silently demote polling.

**Read** — must be a **bare** URL ending in a parseable GUID:

```
{public}/vision/v3.2/read/analyzeResults/{36-char lowercase GUID}
```

No query string, no trailing slash, no fragment. The canonical Python sample does `split("/")[-1]`
and percent-encodes the junk into the path; the .NET sample does `Substring(len-36)` + `Guid.Parse`.

Never propagate the container's own `Operation-Location` — it may name an unroutable internal host,
or be corrupted by the `vision`-substring bug. Extract the id, discard the rest, mint our own.

### Response rules

| Rule | Why |
|---|---|
| `POST …:analyze` returns **exactly `202`** | All four SDK families require exactly 202 |
| Polls return **exactly `200`** | A `202` on the CV poll is a hard client error |
| `Retry-After` is **integer seconds, never an HTTP-date** | An HTTP-date hard-fails the Python DI client (`_deserialize("int", …)` → `DeserializationError`); .NET silently ignores it |
| **Never** attach `Retry-After` to a non-retriable error | Python retries *any* ≥400 carrying it, 10 times, including 404 |
| **No `Location` header** on the 202 | Python and JS issue a bogus extra final GET to it and parse *that* as the result |
| **No `resourceLocation`** in a poll body | Same, for Python, Java and JS |
| Terminal DI body nests `analyzeResult` **inside** the envelope | .NET `GetProperty("analyzeResult")` throws if it is absent |
| Poll body is always JSON with a top-level `status` | Python raises `BadResponse` on an empty body; Java NPEs |
| Status strings exactly `notStarted \| running \| succeeded \| failed` | `cancelled` is terminal in JS only; `skipped` in none |
| **Never redirect** | .NET builds its handler with `AllowAutoRedirect = false`; any 3xx is terminal |
| Compress only what `Accept-Encoding` permits; **never `br`** | .NET sends no `Accept-Encoding` and cannot decompress |
| Accept chunked request bodies with no `Content-Length` | SDKs stream file-like bodies |

### 404 policy

A `404` on an **in-flight** poll makes Java clients die with an opaque `NullPointerException`, and
marks the operation terminally failed in .NET and JS. Therefore:

- **mid-flight failure** → `200 {"status":"failed", …, "error":{…}}`
- **unknown or TTL-expired id** → `404` with the surface's error body, exactly like Azure.

We never 404 a live id — guaranteed by construction, because a single gateway pod owns every id it
mints.

### Error shapes — the two surfaces differ

`azerr` emits the DI nested shape (`{"error":{"code","message","target","details","innererror"}}`)
and the CV Read shape separately. Each swagger declares exactly one `default` error response
covering every non-2xx, so the shape is per-surface, not per-status.

**The dominant failure mode on both surfaces is HTTP 200** with `"status":"failed"` in the body. Code
that switches on HTTP status alone mis-maps every analysis failure.

---

## Persistence

SQLite at `${DATA_DIR}/gateway.db`:

```
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = FULL;    -- the 202 is a durability promise; NORMAL can roll back a
                               -- committed job on node power loss
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
```

One dedicated writer connection (`SetMaxOpenConns(1)`), a separate read pool. Startup warns if
`DATA_DIR` is on a network filesystem, where WAL is unsafe; `ALLOW_NETWORK_FS` overrides.

Blobs at `${DATA_DIR}/blobs/{xx}/{yy}/{id}.{in,json,pdf,fig-N}` — written temp → `fsync` → `rename`
→ `fsync` dir, so a partial file is never observable.

**Submit ordering is the whole ballgame:**

```
stream body to blob → fsync → INSERT job → COMMIT → *then* 202
```

Returning `202` before the commit hands the client an operation id that does not exist.

### Schema

```sql
CREATE TABLE jobs (
  id                TEXT PRIMARY KEY,   -- gateway operation id (canonical GUID)
  surface           TEXT NOT NULL,      -- 'di' | 'read'
  model_id          TEXT,
  api_version       TEXT,
  status            TEXT NOT NULL,      -- notStarted | running | succeeded | failed
  created_at        TEXT NOT NULL,      -- RFC3339 UTC, the client-visible createdDateTime
  updated_at        TEXT NOT NULL,
  expires_at        INTEGER NOT NULL,   -- unix seconds
  request_query     TEXT,               -- canonical query forwarded upstream
  content_type      TEXT,
  input_path        TEXT,               -- deleted on success
  input_bytes       INTEGER,
  result_path       TEXT,               -- the exact envelope we will serve
  result_bytes      INTEGER,
  error_json        TEXT,               -- Azure-shaped error object
  attempts          INTEGER NOT NULL DEFAULT 0,
  lease_owner       TEXT,
  lease_until       INTEGER,
  upstream_mode     TEXT,               -- 'sync' | 'degraded-202' | 'async-fallback'
  upstream_op_url   TEXT,               -- set only on the degraded path
  upstream_status   INTEGER,
  upstream_ms       INTEGER,
  client_request_id TEXT
);
CREATE INDEX jobs_status  ON jobs(status, created_at);
CREATE INDEX jobs_expiry  ON jobs(expires_at);
```

### Result envelope

At completion the worker composes and stores **the exact bytes it will later serve**:

```json
{"status":"succeeded","createdDateTime":"…","lastUpdatedDateTime":"…","analyzeResult":{…}}
```

A succeeded poll is then a pure file stream with an exact `Content-Length` — no re-serialisation, no
parse cost on the hot path. In-progress and failed responses are tiny and generated on the fly.

### Crash recovery

On boot:
- `running` with an expired lease → requeue if the input blob survives and `attempts < max`,
  else `failed` with an Azure-shaped `InternalServerError`;
- `notStarted` → recovery queue, processed as slots free. These were already `202`-acked, so they
  bypass the admission 429 — a promise already made must be kept.

### TTL and disk

Sweeper every `GC_INTERVAL` deletes jobs past `expires_at` and their blobs. At
`DISK_HIGH_WATERMARK` (0.90) it evicts oldest-succeeded-first. On a full PVC, submits fail cleanly
with a `503` + integer `Retry-After` rather than corrupting the store.

---

## Admission control

`MAX_INFLIGHT` slots per surface, acquired at admission and **held to completion**, so the number of
concurrent upstream calls is bounded by construction. No slot → `429` with an integer `Retry-After`
and the correct per-surface error body. `QUEUE_DEPTH` defaults to `0` per D4. Sync passthrough takes
a slot from the same pool, so total upstream concurrency is one number.

---

## Configuration

```
GATEWAY_ADDR=:8080
PUBLIC_BASE_URL=                 # overrides scheme/authority in Operation-Location
TRUST_FORWARDED_HEADERS=true     # else derive from Host
DATA_DIR=/data
ALLOW_NETWORK_FS=false

DI_UPSTREAM_URL=                 # e.g. http://layout.ns.svc.cluster.local:5000
DI_UPSTREAM_API_KEY=
DI_MAX_INFLIGHT=4
DI_UPSTREAM_TIMEOUT=15m
DI_BLIND_POLL_BUDGET=60
DI_SYNC_ANALYZE=auto             # auto | force | off

READ_UPSTREAM_URL=               # NOTE: must not contain the substring "vision"
READ_UPSTREAM_API_KEY=
READ_MAX_INFLIGHT=4
READ_SYNC_TIMEOUT=10m

QUEUE_DEPTH=0
RESULT_TTL=24h
GC_INTERVAL=5m
DISK_HIGH_WATERMARK=0.90
MAX_REQUEST_BYTES=524288000      # 500 MB, the DI S0 ceiling
LOG_LEVEL=info
LOG_FORMAT=json
```

Secrets arrive as env vars from a Kubernetes Secret. Nothing is read from disk, nothing is logged.

---

## Testing

`internal/mockazure` implements both containers faithfully **including their failure modes** —
`syncAnalyze` present/absent/degrading-to-202, cross-replica 404, stalled `running`, the
`vision`-substring corruption. The whole gateway is therefore provable on a laptop.

| Suite | Covers |
|---|---|
| `test/conformance` | one named test per fidelity rule above |
| upstream | sync happy path, 202 degradation, capability latch-off, affinity poll, blind-poll budget |
| store | atomic blob write, torn-write recovery, WAL durability, TTL sweep, high-water eviction |
| jobs | admission 429, lease expiry, crash recovery mid-job, recovery-queue bypass |
| streaming | multi-hundred-MB body in and out without buffering |
| `test/sdk` | **the real Python Azure SDKs** driven against the gateway — the definitive proof |

Gates: `go test -race ./...`, `go vet`, `staticcheck`, `gofmt -l`.

---

## Open questions

These are runtime-unverifiable without the live containers and are settled by `gateway probe`
(see `docs/api-surface.md` §11 for the full list and the exact curl for each):

| # | Question | Impact if the answer is bad |
|---|---|---|
| Q-A1 | Does `layout-4.0:2024-11-30` actually serve `:syncAnalyze` at runtime? | Falls back to `:analyze` + affinity. Design already handles it; the happy path just gets slower |
| Q-A2 | Is its 200 body the bare `AnalyzeResult` or the `AnalyzeOperation` wrapper? | Envelope composition differs; `probe` reports which, adapter handles both |
| Q-A3 | At what size/load does `:syncAnalyze` degrade to 202? | Tunes `MAX_INFLIGHT` and the timeout |
| Q-A4 | Does it serve `/analyzeResults/{id}/pdf` and `/figures/{id}`? | Those endpoints return the upstream's own error if unsupported |
| Q-B5 | Confirm `syncAnalyze` returns 200 (implied, never documented) | Adapter treats any 2xx-with-body as success |

`gateway probe` pulls each container's own `/swagger` JSON, which is ground truth for the exact
build and supersedes every Microsoft doc where they disagree. It closes most of the above in minutes.

## Rollback

The gateway is additive: it sits beside the containers rather than replacing them. Rollback is
repointing clients at the container Routes directly, which restores exactly today's behaviour —
including today's async fragility. No data migration, no schema in any shared system.
