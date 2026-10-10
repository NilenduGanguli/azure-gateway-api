# Sync-only upstream, single gateway pod — design

**Date:** 2026-10-10
**Status:** approved in conversation, revised after review (migrations, exact parity, settings),
spec awaiting review
**Changes:** the upstream strategy of the [2026-09-08 design](2026-09-08-azure-gateway-api-design.md)
(its "Upstream decision tree"), the store's schema handling, and the configuration surface. The
client-facing contract is unchanged except where this document says otherwise.

## Problem

The target deployment has changed:

- **One gateway pod** per OpenShift cluster, in front of an Azure Document Intelligence
  `prebuilt-layout` deployment, a Computer Vision Read v3.2 deployment, or both.
- The Azure deployment **autoscales on load, to at most 8 pods**.
- The gateway reaches those pods **only through the HAProxy load balancer**. It cannot address or
  count pods, and it cannot use Kubernetes APIs.
- The gateway talks to the Azure pods **only through synchronous requests**. For analysis these are
  `POST {prefix}/documentModels/{modelId}:syncAnalyze` (DI, `prefix` = `/documentintelligence` or
  `/formrecognizer`) and `POST /vision/v3.2/read/syncAnalyze` (Read).

Today's gateway does not fit:

1. **It still makes async calls to the containers.**
   - `output=pdf|figures` forces `:analyze` + polling.
   - Read falls back to `/read/analyze` when `syncAnalyze` answers 404 or 405.
   - `DI_SYNC_ANALYZE=auto` latches onto `:analyze` after one failed or slow sync attempt.
   - `DI_SYNC_ANALYZE=off`, which Wega's `ocr-cell` sets, never uses the sync route.

   With autoscaling pods, an async operation dies with the pod that owns it.
2. **A 60-second ceiling breaks long analyses.** `DI_SYNC_PROBE_TIMEOUT` (default 60s) bounds the
   wait for the sync route's response headers. `:syncAnalyze` sends its headers only when the
   analysis is finished, so any analysis over 60s fails with a 500 in `force` mode and switches the
   route off for the whole process in `auto` mode.
3. **No retries.** One failed upstream call fails the job. Scale-in kills pods mid-analysis, and
   those jobs fail even though their input is still on the PVC.
4. **Both upstreams are mandatory.** `DI_UPSTREAM_URL` and `READ_UPSTREAM_URL` must both be set.
5. **No schema migrations.** The schema is applied with `CREATE TABLE IF NOT EXISTS`, so a changed
   schema never reaches an existing volume. Dead columns therefore cannot be dropped, and new ones
   cannot be added.
6. **Too many settings.** 32 environment settings, several of them dead or redundant.
7. **Gaps against the real pods.**
   - Some container routes are not served at all: `/`, `/swagger…`, `/records/usage-logs…`,
     `/documentintelligence/operations…`.
   - Some responses differ from what the containers send: an `apim-request-id` header the
     containers are not known to send, and no `Server: Kestrel`.
   - Nothing in the repository records the containers' exact responses to compare against.

## Goal

- **Indistinguishable from the real pods.** An Azure SDK client, or any client written for the real
  containers, must not be able to tell the gateway from a real DI or Read pod: the same routes,
  status codes, headers and body shapes. The gateway may serve **extra** endpoints of its own
  (`/_gw/*`) on the same port, because no Azure route can collide with them.
  - **Documented exceptions:**
    - `output=pdf|figures` and the not-yet-proven sync parameters are rejected (§2);
    - the gateway answers its own 429 when its queue is full (§3);
    - the Azure-Blob-bound batch and model-lifecycle routes are not served (§3).
- **Every upstream analysis is one synchronous call through HAProxy.** Apart from it, the gateway
  sends the Azure pods only small synchronous GETs (health and the passthrough routes in §3).
- **Jobs survive pod churn.** Transient failures are retried from the stored input.
- **One surface is enough.** A gateway can front a single surface.
- **One store.** All job state lives in a single SQLite table on the PVC, with versioned
  migrations that run at startup only when the schema version changed.
- **Fewer settings.** Configuration drops from 32 settings to 21.

## Non-goals

- Discovering, counting or addressing Azure pods; adaptive concurrency.
- The searchable PDF and cropped figures (`output=pdf`, `output=figures`). Clients consume the JSON
  result; neither option changes it.
- More than one gateway replica.
- Changing the Wega deployment. That is a separate follow-up (see Rollout).

## Design

### 1. The upstream call (`internal/upstream`)

`DIClient.Analyze` and `ReadClient.Analyze` each run a loop of **attempts**. One attempt is one
`POST` of the stored input (`Document.Open`, re-opened per attempt) to the surface's sync route. It
uses a **new connection per attempt**: a fresh transport with keep-alive off and no cookie jar. So
HAProxy balances every attempt on its own, and a retry is free to land on a different pod.

**Attempt outcomes.** These are checked in order; the first match wins.

| # | Outcome | Class |
|---|---|---|
| 1 | The job's own context is cancelled (shutdown) | returned as-is; the worker leaves the job for recovery |
| 2 | 200 whose body has an `analyzeResult` | **success** |
| 3 | 200 whose `status` is `failed` (case-insensitive) | **terminal**: the analysis itself failed |
| 4 | 404, 405 or 501 whose body is not a parseable Azure error, or a 500 matching `looksLikeMissingEndpoint` | **terminal**, with a message saying the container does not serve the sync route (a deployment fault) |
| 5 | The attempt's own deadline expired (`DI_UPSTREAM_TIMEOUT` / `READ_SYNC_TIMEOUT`), or a 500 or 504 | **retriable once per job** |
| 6 | Dial error (including a dial timeout), connection refused or reset, EOF, TLS failure, body cut short | **retriable** |
| 7 | 408, 429, 502, 503 (HAProxy answers 503 when the Route has no ready pod or its queue times out) | **retriable** |
| 8 | 202 (the container degraded under memory pressure) | **retriable**; the body is drained and the container's operation is abandoned, never polled |
| 9 | Any other 4xx, e.g. a 404 carrying `ModelNotFound` | **terminal**; the container's error is relayed in the surface's shape |
| 10 | Anything else, e.g. a 200 with neither `analyzeResult` nor `failed`, or a result over `MAX_RESULT_BYTES` | **terminal**, as today |

Row 5 is checked before row 6 so that a slow analysis is not mistaken for a broken connection. The
per-attempt deadline is the attempt's own context, which keeps it apart from both the job's context
(row 1) and the 10-second dial timeout (row 6).

**Backoff.** Before attempt *n*+1 the gateway waits a random duration between 50% and 100% of
`min(60s, 2s × 2^(n−1))`. A parseable integer `Retry-After` on the failed response replaces that
wait, capped at 60s. Waiting stops early on context cancellation. The jitter source is injected so
tests are deterministic.

**Limits.** At most **5 attempts per job**. This is a constant, not a setting. The row-5 class
allows at most two attempts in a job, and they count toward the 5.

**Exhaustion.** When attempts run out, the job fails with the **last** attempt's error, shaped for
the surface. The message says how many attempts were made, e.g. *"The document-intelligence
service was unavailable after 5 attempts."*

**Slots.** A job keeps its upstream slot while it backs off. A deployment that answers 503
therefore lowers the gateway's request rate with no extra mechanism.

**Result.** `Result` gains `Attempts int`. Every result now comes from the sync route.

### 2. Query parameters on the sync route

Per the operator's own DI container swagger, `:syncAnalyze` declares `api-version` (required),
`pages`, `locale`, `stringIndexType` and a body: JSON `urlSource` / `base64Source`, or raw bytes
chosen by `Content-Type`. `:analyze` additionally accepts `features`, `queryFields`,
`outputContentFormat` and `output`. Whether the sync route honours those four is not recorded
anywhere (`docs/api-surface.md` Q-A2). ASP.NET silently ignores query parameters an action does not
bind, so forwarding them blindly could give a client a result that silently lacks what it asked
for.

The gateway applies one policy on both DI analyze routes (`:analyze` and `:syncAnalyze`), checked
before admission so nothing is stored. Each rejection is a **400 `InvalidArgument`** (inner
`InvalidParameter`) whose message names the parameter.

| Parameter | Policy |
|---|---|
| `api-version`, `pages`, `locale`, `stringIndexType` | forwarded |
| `outputContentFormat` absent or `text` | forwarded (`text` is the default; Wega's normal attempts send it) |
| `outputContentFormat=markdown` | **rejected** until proven |
| `features` absent or empty | forwarded |
| `features` with any value | **rejected** until proven |
| `queryFields` | **rejected** until proven |
| `output` containing `pdf` or `figures` | **always rejected**. These files are fetched by an operation id, which a sync call never creates. |
| the doc-artifact `_overload` parameter and any other unknown parameter | forwarded, as today |

**Proving a parameter.** The probe (§8) runs the same document through the container's
`:syncAnalyze` and `:analyze` with the parameter set, and compares the two `analyzeResult`s.
Equal means honoured, and accepting that parameter is then a one-line change to the allowlist with
its probe evidence cited.

**Read.** `language`, `pages`, `readingOrder` and `model-version` are forwarded. Read's sync route
is documented as returning the same object graph as its async route, and the probe checks the
same four.

### 3. Matching the real pods (`internal/surface`, `internal/app`, `internal/httpx`)

**Routes.** The family `{fam}` is `/documentintelligence` and `/formrecognizer`, as today.

| Route | Real pod | Gateway |
|---|---|---|
| `POST {fam}/documentModels/{modelId}:analyze` | 202 + `Operation-Location` | **owned**: 202 after the input is durable; the result comes from the sync upstream call |
| `GET`/`HEAD {fam}/documentModels/{modelId}/analyzeResults/{resultId}` | operation status | **owned**: served from the PVC; `HEAD` behaves as recorded (below) |
| `DELETE` same | 204 | **owned**: 204 |
| `POST {fam}/documentModels/{modelId}:syncAnalyze` | 200 + result | **passthrough**, one attempt |
| `GET …/analyzeResults/{resultId}/pdf`, `…/figures/{figureId}` | the file | **owned**: always 404 in the DI error shape the container uses for a missing result file, because no job ever has these files |
| `GET {fam}/info`, `/documentModels`, `/documentModels/{modelId}` | metadata | **passthrough**, as today |
| `GET {fam}/operations`, `/operations/{operationId}` | model-lifecycle operations, empty for layout | **passthrough** (new) |
| `POST /vision/v3.2/read/analyze` | 202 + `Operation-Location` | **owned** |
| `GET /vision/v3.2/read/analyzeResults/{operationId}` | operation status | **owned** |
| `POST /vision/v3.2/read/syncAnalyze` | 200 + result | **passthrough**, one attempt |
| `GET /ready`, `/status` | container health | **owned**: container-shaped bodies, aggregated over the configured upstreams, as today |
| `GET /`, `/swagger`, `/swagger/{rest...}`, `/records/usage-logs`, `/records/usage-logs/{month}/{year}` | home page, swagger, usage records | **passthrough** (new) to the configured container, DI when both are configured. Usage records come from whichever pod HAProxy picks. |
| `POST {fam}/documentModels/{modelId}:analyzeBatch`, `analyzeBatchResults…`, model build/compose/copy, `documentClassifiers…`, `:syncAnalyze/upstreamResult` | Azure-Blob-bound, n/a to layout, or container-internal | **not served**: documented exception |
| `/_gw/*` | — | gateway admin, extra |
| anything else | Kestrel: bodyless 404, or 405 + `Allow` for a known path with the wrong method | same, as today |

**Passthrough routes.**
- They are GETs relayed verbatim through the existing `proxyGET` / `relaySync` path, bounded by
  the 10-second metadata timeout.
- They are registered only for configured surfaces. An unconfigured surface's routes answer the
  bodyless 404, as a pod of the other kind would.

**Headers on Azure routes.**
- `Server: Kestrel` on every response, as the containers' web server sends.
- **No `apim-request-id`.** The containers are not known to send it (Q-A7). The request id stays
  in the gateway's logs.
- `Operation-Location` keeps the exact shapes in `docs/api-surface.md` §4, built from
  `PUBLIC_BASE_URL`, or else from the request's `Host` header, as the container does.
- `Retry-After` keeps today's rules and values: **1** on the 202 and on in-progress polls, **5**
  on the gateway's 429, and only ever on retriable statuses. These are now constants. Where the
  recorded container responses (below) show a different value, or no header, the gateway follows
  the recording.
- `x-ms-client-request-id` is echoed, as today.

**Bodies.**
- Errors always use the shapes the real containers produce (today's `observed` mode). The
  `documented` mode is deleted.
- Poll envelopes keep their member order and states.
- The `analyzeResult` is the container's own bytes from the sync answer, copied verbatim.
- `createdDateTime` / `lastUpdatedDateTime` use the container's recorded format.

**Before any recording exists,** `HEAD`, the timestamp format and `Retry-After` keep today's
behaviour. Each changes only when a recorded golden response shows the container doing something
else.

**Load shedding.** When `*_MAX_INFLIGHT + QUEUE_DEPTH` jobs are already accepted, the gateway
answers 429 + `Retry-After: 5` in each surface's own error shape, as today (`ServiceUnavailable`
on DI, `InvalidRequest` on Read). Real pods never send
429; this is the one deliberate behavioural difference under overload. Every SDK retries it, and a
large `QUEUE_DEPTH` makes it rare.

**Recorded reference ("golden" responses).**
- `gateway probe --record <dir>` runs a fixed case list against the real containers and writes one
  JSON file per case, holding:
  - the request: method, path template, query, `Content-Type`;
  - the response status;
  - every response header except `Date`;
  - the body.
- Volatile values are masked: GUIDs become `<guid>`, and timestamps become `<datetime>` with their
  format kept.
- **The case list:**
  - `:analyze` + poll to completion and `:syncAnalyze` on the same real document;
  - an unknown result id, a missing or invalid `api-version`, an unsupported `Content-Type`, a
    wrong verb and an unknown path;
  - `HEAD` and `DELETE` on a result;
  - the metadata and passthrough GETs;
  - the per-parameter comparisons of §2;
  - the same set for Read.
- The files are committed under `test/conformance/testdata/golden/<surface>/<image-tag>/`. The
  operator records them, because only their network reaches the containers.
- A **parity test** replays every golden case against the gateway, with `mockazure` serving the
  recorded sync bodies. It compares:
  - the status;
  - the header names, plus the shapes of `Content-Type`, `Operation-Location`, `Retry-After`,
    `Content-Length` and `Server`;
  - the body's keys, order and JSON types, with volatile values masked.
- A **sync-equals-async check** asserts that the container's sync `analyzeResult` equals the
  `analyzeResult` from its own async poll for the same document. That equality is what makes a
  sync-backed poll indistinguishable from a real pod's.
- Until goldens exist, the suite keeps checking the documented reference. Go-live requires them.

### 4. Storage and schema migrations (`internal/store`)

**One store.**
- All job state lives in the `jobs` table of `${DATA_DIR}/gateway.db`, SQLite on the PVC.
- Uploaded documents and results stay as files under `${DATA_DIR}/blobs/` on the same PVC, as
  today. Results can reach 500 MB; as files they stream to clients and free their space on delete.
- Nothing else holds state. The in-memory queue is rebuilt from the table on every start.

**Network filesystems.** `DATA_DIR` on NFS, SMB/CIFS, CephFS, FUSE, GFS2 or OCFS2 is always
refused at startup; the override is removed.

**Versioned migrations.**
- Migrations are numbered SQL files embedded in the binary: `internal/store/migrations/NNNN_name.sql`.
  The highest number is the schema version this binary expects.
- The version actually on disk lives in `PRAGMA user_version`.
- `store.Open` runs this sequence before the HTTP listener starts:

  1. Read `user_version`. If it equals the expected version, do nothing more. This is the normal
     startup and costs one read.
  2. If `user_version` is 0 and a `jobs` table exists, this is a database from before versioning:
     treat it as version **1**. If `user_version` is 0 and there is no table, this is a fresh
     volume: start from 0 and take no backup.
  3. If the version is **greater** than expected (an image rollback after a migration), refuse to
     start: *"database schema vN is newer than this gateway (vM); deploy the newer image, or
     restore gateway.db.vM.bak"*.
  4. Otherwise, back up the database with `VACUUM INTO '${DATA_DIR}/gateway.db.v<N>.bak'`,
     replacing any older backup file. Then apply every pending migration and set `user_version`
     inside **one** `BEGIN IMMEDIATE` transaction. On any error, roll back and refuse to start; the
     database is unchanged. A pod killed mid-migration leaves the transaction uncommitted, and the
     next start re-runs it.
  5. Log one line: from-version, to-version, duration and backup path.
- **Startup time.** The `jobs` table holds at most `RESULT_TTL` (24h) of rows, so a migration takes
  seconds. OPERATIONS documents the startup-probe allowance anyway.

**Migration 0001** is today's schema exactly, the version 1 baseline.

**Migration 0002** (this change):
- drops the dead columns `upstream_op_url`, `has_pdf`, `figures` and `upstream_mode`, using SQLite
  `ALTER TABLE … DROP COLUMN`; none of them is indexed;
- adds `upstream_attempts INTEGER NOT NULL DEFAULT 0`, written at completion and shown on
  `/_gw/jobs`.

**Rule for future changes.** Every schema change is a new migration file. A test fails if the
schema produced by running every migration from scratch differs from the schema produced by
migrating a version-1 fixture database.

### 5. Removed

**Code.**

| Package | Removed |
|---|---|
| `internal/upstream` | `analyzeAsync` (both clients); `trySync` and its probe timer; the capability latch (`syncCapability`, `capabilityName`, `ErrSyncUnavailable`, `resourceScoped404`); `pollUntilTerminal`, `pollConfig`, `nextBackoff`, `operationIDFrom`, `affinityClient` and its cookie jar; `FetchArtifact`; `Request.RequireOperation`; `SyncOutcome.Resolved`; the `SyncAnalyze` field of `Health` |
| `internal/jobs` | `fetchArtifacts`, `figureIDs`, `wantsArtifacts`, `moveInto` (used only by artifact capture) |
| `internal/surface` | `writeResolvedSync`; `diArtifact` and its file streaming, replaced by one handler for the two artifact routes that always answers the 404 of §3 |
| `internal/azerr` | the `documented` error mode and its switch |
| `internal/httpx` | the forwarded-header trust logic in `BaseURLResolver`; the `apim-request-id` response header |
| `internal/store` | `Options.AllowNetworkFS`; the `ModeDegraded` / `ModeAsyncFallback` constants and the `UpstreamMode` type; the `HasPDF`, `Figures` and `UpstreamOp` job fields |

**Kept on purpose.**
- `inspectOperation`, `downloadBody`, `isFailed` and `retryAfterSeconds` are still used. They move
  out of `poll.go`, which is deleted.
- `RemoveAll` still deletes `KindPDF` and `KindFigure` blobs that older jobs left behind.

**Settings.** Eleven are removed:

| Setting | Why |
|---|---|
| `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT` | the sync route is the only route |
| `DI_BLIND_POLL_BUDGET`, `READ_BLIND_POLL_BUDGET` | no upstream polling |
| `ARTIFACT_FETCH_TIMEOUT` | no artifacts |
| `ERROR_COMPAT` | always the containers' own error shapes |
| `TRUST_FORWARDED_HEADERS`, `TRUSTED_FORWARDED_HOSTS` | `PUBLIC_BASE_URL`, or else the `Host` header, as the container does |
| `ALLOW_NETWORK_FS` | network filesystems are always refused |
| `POLL_RETRY_AFTER`, `BUSY_RETRY_AFTER` | constants (§3) |

If a removed setting is still set, the gateway starts, and `Config.Warnings()` names it as *"no
longer used"*. Wega's current manifests therefore keep working until the follow-up removes them.

### 6. Configuration (`internal/config`)

These 21 settings remain:

| Setting | Default | Note |
|---|---|---|
| `DI_UPSTREAM_URL`, `READ_UPSTREAM_URL` | — | each optional, at least one required; empty disables that surface |
| `DI_UPSTREAM_API_KEY`, `READ_UPSTREAM_API_KEY` | — | |
| `DI_MAX_INFLIGHT`, `READ_MAX_INFLIGHT` | `4` | concurrent sync calls; size = max Azure pods (8) × per-pod concurrency, start at **16** |
| `DI_UPSTREAM_TIMEOUT` | `15m` | per attempt |
| `READ_SYNC_TIMEOUT` | `10m` | per attempt |
| `QUEUE_DEPTH` | `0` | accepted jobs waiting on the PVC; size for bursts, e.g. 200 |
| `PUBLIC_BASE_URL` | — | scheme + host for `Operation-Location`; recommended |
| `RESULT_TTL` | `24h` | |
| `GC_INTERVAL` | `5m` | |
| `DISK_HIGH_WATERMARK` | `0.90` | |
| `MAX_REQUEST_BYTES` | 500 MiB | |
| `MAX_RESULT_BYTES` | 512 MiB | |
| `UPLOAD_TIMEOUT` | `10m` | |
| `SHUTDOWN_GRACE` | `30s` | |
| `DATA_DIR` | `/data` | |
| `GATEWAY_ADDR` | `:8080` | |
| `LOG_LEVEL`, `LOG_FORMAT` | `info`, `json` | |

**A disabled surface** has:
- no runner and no routes;
- no entry in `/_gw/health` `upstreams[]`, `/status` or the metrics;
- no validation of its `*_MAX_INFLIGHT`, API key or timeout, and no empty-API-key warning.

**`/_gw/config`** keeps `diMaxInflight`, `readMaxInflight` and `queueDepth`, because Wega's observer
parses them. A disabled surface reports `0` and an empty URL. Removed settings disappear from
`/_gw/config`.

### 7. Observability (`internal/admin`, logs)

- **`/_gw/metrics`** gains two counters:
  - `gateway_upstream_retries_total{surface,reason}`, where `reason` is one of `connection`,
    `status_408`, `status_429`, `status_500`, `status_502`, `status_503`, `status_504`,
    `degraded_202` or `timeout`;
  - `gateway_upstream_exhausted_total{surface}`.
- **`/_gw/jobs`** shows `upstreamAttempts` per job.
- **Logs.** One `Warn` line per failed attempt (`attempt`, `reason`, `status`, `backoffMs`). The
  final job line carries `attempts`.

### 8. Probe (`internal/probe`, `scripts/probe-containers.sh`)

- Probes only the configured surfaces.
- `PROBE_DOCUMENT=<path>` sends that document instead of the blank PNG. `PROBE_TIMEOUT` (default
  `10m`) replaces the fixed 2-minute client timeout. Both are probe-only inputs, not gateway
  settings.
- `--record <dir>` writes the golden responses of §3.
- **Per-parameter checks of §2.** For each candidate parameter, the probe compares the container's
  `:syncAnalyze` and `:analyze` results on the same document and prints `honoured` or `ignored`.
- **The verdict is rewritten for sync-only:**
  - `:syncAnalyze` answering **200** means *ready*;
  - a timeout, 404, 405, 501 or `500 UnhandledEndpointException` means *this build cannot run
    behind this gateway*.

### 9. Deployment requirements (OPERATIONS.md, HANDOFF.md)

**The HAProxy in front of the Azure pods.** The OpenShift Route annotations are:

- `haproxy.router.openshift.io/timeout` longer than the gateway's per-attempt timeout (for example
  `16m` for a `15m` `DI_UPSTREAM_TIMEOUT`). The 30-second default turns every longer analysis into
  a 504.
- `haproxy.router.openshift.io/pod-concurrent-connections` set to the per-pod concurrency divided by
  the number of router replicas (rounded down, minimum 1). The cap applies per router replica.
  Calls over the cap wait in HAProxy and then get a 503, which the gateway retries.
- `haproxy.router.openshift.io/balance: leastconn`.

**The Azure pods.** Set `terminationGracePeriodSeconds` to at least the slowest analysis, so
scale-in does not cut work short. Work that is cut short is retried.

**TLS.** If the Route is https with an internal CA, mount the CA bundle into the gateway and set
`SSL_CERT_FILE`.

**The gateway.**
- One replica, `strategy: Recreate`, an RWO **block** PVC; unchanged.
- A startup probe that allows for a migration.

**Gate before go-live.**
1. `gateway probe` with `PROBE_DOCUMENT` set to a representative multi-page PDF reports
   `:syncAnalyze` **200**. An earlier probed DI build listed the route in its swagger and never
   answered it.
2. The golden responses are recorded and the parity test passes.

### 10. Invariants that must still hold

- The client gets its 202 only after the input blob is fsynced and the job row is committed.
- Cancellation is classified on the context first. A job interrupted during an attempt or a backoff
  stays `running`, and crash recovery re-runs it from the stored input.
- Admission and upstream slots are released on every return path.
- No upstream-minted operation id is ever stored, followed or relayed.
- No `Retry-After` on a non-retriable status.

### 11. Testing

**`internal/mockazure`.**
- The Read fake returns a real Read v3.2 body (`version`, `modelVersion`, `readResults[]`), which
  closes the shared-`analyzeResult` fidelity gap.
- The fakes can serve a recorded golden body.
- New failure modes, each exposing a counter of the calls it received:

| Mode | Behaviour |
|---|---|
| `SyncBusyFirst(n)` | 503 + `Retry-After` for the first *n* calls |
| `SyncStatusFirst(code, n)` | any status for the first *n* calls (502, 504, 500, 408, 429) |
| `SyncResetMidBody` | closes the connection after writing part of the 200 body |
| `Sync202` | existing; now expected to be retried |
| `SyncDelay(d)` | delays the answer |
| `SyncCapacity(k)` | answers 503 while more than *k* calls are in flight, modelling HAProxy's per-pod cap |

**Unit tests, `internal/upstream`.**
- The outcome table in §1, row by row, as a table-driven test.
- Backoff bounds with injected jitter; `Retry-After` honoured and capped.
- The once-only class and the attempt cap.
- Cancellation during a backoff returns the context error.
- `Document.Open` is called once per attempt.
- A new connection per attempt: the mock sees a distinct remote port each time.

**Unit tests, `internal/store` migrations.**
- A fresh volume and a migrated version-1 fixture end with an identical schema (`sqlite_master`).
- Version-1 rows survive migration 0002.
- The equal-version start performs no writes.
- A newer-version database is refused.
- A failing migration leaves the database and `user_version` unchanged.
- The backup file is written before migrating, and only when there is something to migrate.

**Jobs.**
- A job survives *n* < 5 transient failures and succeeds.
- Exhaustion fails the job with an Azure-shaped error.
- A shutdown during a backoff leaves the job `running`, and recovery completes it.

**Conformance (`test/conformance`).**
- Delete the async-upstream, degraded-202 poll-out and artifact-capture cases.
- Parameter policy (§2): each rejected parameter returns 400 with no job row or blob afterwards,
  and `outputContentFormat=text` and an empty `features` are accepted.
- A DI-only gateway serves DI and answers Read routes with the bodyless 404, and the reverse for a
  Read-only gateway.
- The new passthrough GETs are relayed.
- Header rules: `Server: Kestrel` present, `apim-request-id` absent.
- 50 submits against `SyncCapacity(4)` with `DI_MAX_INFLIGHT=16`: every job succeeds.
- The golden parity test (§3), once fixtures exist.

**SDK tests (`test/sdk`).** The real-SDK flows of both surfaces still pass.

**Gate.** `gofmt -l . && go vet ./... && go test -race ./... && make image`.

## Rollout

1. Implement on branch `feat/sync-only-upstream` in `~/imp/azure-gateway-api`, together with the
   docs:
   - the README configuration table, `ARCHITECTURE.md` §3–5, `OPERATIONS.md`, `TESTING.md` and
     `HANDOFF.md`;
   - the `api-surface.md` notes on parameters, routes and headers;
   - the coupling map: the job schema now changes only through a migration file.

   `CONTAINER-BEHAVIOUR.md` keeps its observations. Merge and push when the user asks, then
   re-import the gateway into `wega-unified-services`.
2. The user runs `gateway probe --record` against the real DI and Read builds; I cannot reach that
   network. The recorded files are committed, the parity test runs, and any parameter proven
   `honoured` moves to the forwarded list.
3. Wega follow-up, as a separate spec:
   - the fleet files go to one gateway with `di.max_inflight` and `queue_depth` from §6, and
     `read.max_inflight = 0` for a DI-only gateway;
   - `ocr-cell` drops the removed settings;
   - vision's KEDA ceilings are re-derived;
   - vision's third attempt stops asking for `markdown` / `ocrHighResolution` until those are
     proven;
   - the orchestrator's system handoff is updated.
