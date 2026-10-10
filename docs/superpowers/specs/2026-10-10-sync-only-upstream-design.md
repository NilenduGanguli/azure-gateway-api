# Sync-only upstream, single gateway pod — design

**Date:** 2026-10-10
**Status:** approved in conversation, spec awaiting review
**Changes:** the upstream strategy of the [2026-09-08 design](2026-09-08-azure-gateway-api-design.md)
(its "Upstream decision tree"). The client-facing contract is unchanged.

## Problem

The target deployment has changed:

- **One gateway pod** per OpenShift cluster, in front of an Azure Document Intelligence
  `prebuilt-layout` deployment, a Computer Vision Read v3.2 deployment, or both.
- The Azure deployment **autoscales on load, to at most 8 pods**.
- The gateway reaches those pods **only through the HAProxy load balancer**. It cannot address or
  count pods, and it cannot use Kubernetes APIs.
- The gateway talks to the Azure pods **only through the synchronous routes**:
  `POST {prefix}/documentModels/{modelId}:syncAnalyze` (DI, `prefix` = `/documentintelligence` or
  `/formrecognizer`) and `POST /vision/v3.2/read/syncAnalyze` (Read).

Today's gateway does not fit:

1. **It still makes async calls to the containers.**
   - `output=pdf` or `output=figures` forces `:analyze` + polling, because the artifact endpoints
     are addressed by the container's operation id.
   - Read falls back to `/read/analyze` + polling when `syncAnalyze` answers 404 or 405.
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

## Goal

- Every upstream analysis is one synchronous call through HAProxy. Apart from the existing small
  synchronous GETs (`/status` for health, the model-metadata proxy), nothing else is sent to the
  Azure pods.
- The client-facing contract is unchanged. `:analyze` returns 202 + `Operation-Location`, polls
  are served from the PVC, and a client's own `:syncAnalyze` still passes through.
- Jobs survive pod churn: transient failures are retried from the stored input.
- A gateway can front a single surface.

## Non-goals

- Discovering, counting or addressing Azure pods; adaptive concurrency.
- The searchable PDF and cropped figures (`output=pdf`, `output=figures`).
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

**Limits.** `UPSTREAM_MAX_ATTEMPTS` (default 5, minimum 1) caps the attempts per job. The
"retriable once" class allows at most two attempts of that class in a job, and they count toward
the cap.

**Exhaustion.** When attempts run out, the job fails with the **last** attempt's error, shaped for
the surface. The message says how many attempts were made, e.g. *"The document-intelligence
service was unavailable after 5 attempts."*

**Slots.** A job keeps its upstream slot while it backs off. A deployment that answers 503
therefore lowers the gateway's request rate with no extra mechanism.

**Result.** `Result` gains `Attempts int`. `Mode` is always `store.ModeSync`.

### 2. Removed

| Package | Removed |
|---|---|
| `internal/upstream` | `analyzeAsync` (both clients); `trySync` and its probe timer; the capability latch (`syncCapability`, `capabilityName`, `ErrSyncUnavailable`, `resourceScoped404`); `pollUntilTerminal`, `pollConfig`, `nextBackoff`, `operationIDFrom`, `affinityClient` and its cookie jar; `FetchArtifact`; `Request.RequireOperation`; `SyncOutcome.Resolved` |
| `internal/jobs` | `fetchArtifacts`, `figureIDs`, `wantsArtifacts`, `moveInto` (used only by artifact capture) |
| `internal/surface` | `writeResolvedSync`; the `GET …/analyzeResults/{id}/pdf` and `…/figures/{figureId}` routes on both families, which now fall through to the bodyless unrouted 404 |
| `internal/config` | `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT`, `DI_BLIND_POLL_BUDGET`, `READ_BLIND_POLL_BUDGET`, `ARTIFACT_FETCH_TIMEOUT`, and their `Config` fields and `/_gw/config` keys |
| `internal/admin` | the `syncAnalyze` field of `/_gw/health` `upstreams[]` (no Wega consumer reads it) |

**Kept on purpose:**

- `inspectOperation`, `downloadBody`, `isFailed` and `retryAfterSeconds` are still used. They move
  out of `poll.go`, which is deleted, into the files that use them.
- The store's `ModeDegraded` and `ModeAsyncFallback` constants stay, because older rows still carry
  them.
- The `has_pdf`, `figures` and `upstream_op_url` columns stay and are no longer written; there are
  no migrations (HANDOFF trap).
- `RemoveAll` still deletes `KindPDF` and `KindFigure` blobs left by older jobs.

**Old settings still present.** If any removed setting is set, the gateway still starts, and
`Config.Warnings()` names it as *"no longer used; the gateway always calls :syncAnalyze"*. Starting
cleanly avoids breaking a deployment that still sets `DI_SYNC_ANALYZE=off`.

### 3. Client-facing changes (`internal/surface`)

- **`output=pdf` / `output=figures`.** On both DI analyze routes (`:analyze` and `:syncAnalyze`), an
  `output` value containing `pdf` or `figures` is rejected with **400 `InvalidArgument`** (inner
  `InvalidParameter`): *"output=pdf and output=figures are not supported by this gateway."* The
  check runs before admission, so nothing is stored. Read has no `output` parameter. Wega's vision
  never sends `output`.
- **Client `:syncAnalyze` passthrough.**
  - It always calls the sync route, makes **one** attempt, and never retries: the client's body is
    streamed, not stored.
  - A 202 from upstream becomes **503** with `Retry-After: BUSY_RETRY_AFTER`.
  - Every other upstream answer follows today's `relaySync` rules: the container's status is kept,
    and `Retry-After` is added for retriable statuses. Azure SDKs and vision already retry those.
- **Unconfigured surface.** Its routes are not registered and answer the bodyless unrouted 404,
  like any unknown path.

### 4. Configuration (`internal/config`)

- **Optional upstreams.**
  - `DI_UPSTREAM_URL` and `READ_UPSTREAM_URL` are each optional; **at least one is required**.
  - An empty URL disables that surface: no runner, no routes, and no entry in `/_gw/health`
    `upstreams[]`, `/status` or the metrics.
  - That surface's `*_MAX_INFLIGHT`, API key and timeout are not validated, and the empty-API-key
    warning is raised only for configured surfaces.
- **`/_gw/config`** keeps the keys `diMaxInflight`, `readMaxInflight` and `queueDepth` (Wega's
  observer parses them). A disabled surface reports `0` and an empty URL.
- **New:** `UPSTREAM_MAX_ATTEMPTS` (default `5`, minimum `1`), shown on `/_gw/config` as
  `upstreamMaxAttempts`.
- **Sizing guidance** (README and OPERATIONS):
  - `*_MAX_INFLIGHT` = maximum Azure pods (8) × documents one pod can analyze at once. Start with
    2 per pod, which gives **16**, and tune with a load test.
  - `QUEUE_DEPTH` is sized for bursts (for example 200), so clients get a 202 and wait on the PVC
    rather than receiving 429.
  - `DI_UPSTREAM_TIMEOUT` / `READ_SYNC_TIMEOUT` must be at least the slowest expected analysis.

### 5. Observability (`internal/admin`, logs)

- **`/_gw/metrics`** gains two counters:
  - `gateway_upstream_retries_total{surface,reason}`, where `reason` is one of `connection`,
    `status_408`, `status_429`, `status_500`, `status_502`, `status_503`, `status_504`,
    `degraded_202` or `timeout`;
  - `gateway_upstream_exhausted_total{surface}`.
- **Logs.** One `Warn` line per failed attempt (`attempt`, `reason`, `status`, `backoffMs`). The
  final job line carries `attempts`.
- **Not added to the job record.** Attempt counts are not stored: the job table has no migrations,
  and the existing `attempts` column counts crash-recovery runs, a different thing.

### 6. Probe (`internal/probe`, `scripts/probe-containers.sh`)

- Probes only the configured surfaces.
- `PROBE_DOCUMENT=<path>` sends that document instead of the blank PNG. `PROBE_TIMEOUT` (default
  `10m`) replaces the fixed 2-minute client timeout.
- The verdict is rewritten for sync-only:
  - DI `:syncAnalyze` and Read `syncAnalyze` answering **200** means *ready*.
  - A timeout, 404, 405, 501 or `500 UnhandledEndpointException` means *this build cannot run
    behind this gateway*.
- The async-route rows stay, labelled informational. They still record the container's error
  shapes.

### 7. Deployment requirements (OPERATIONS.md, HANDOFF.md)

**The HAProxy in front of the Azure pods.** The OpenShift Route annotations are:

- `haproxy.router.openshift.io/timeout` longer than the gateway's per-attempt timeout (for example
  `16m` for a `15m` `DI_UPSTREAM_TIMEOUT`). The 30-second default turns every longer analysis into
  a 504.
- `haproxy.router.openshift.io/pod-concurrent-connections` set to the per-pod concurrency divided by
  the number of router replicas (rounded down, minimum 1). The cap applies per router replica.
  Calls over the cap wait in HAProxy and then get a 503, which the gateway retries. This cap is what
  protects a pod after scale-in, when the gateway's full concurrency would otherwise land on one or
  two pods.
- `haproxy.router.openshift.io/balance: leastconn`.

**The Azure pods.** Set `terminationGracePeriodSeconds` to at least the slowest analysis, so
scale-in does not cut work short. Work that is cut short is retried.

**TLS.** If the Route is https with an internal CA, mount the CA bundle into the gateway and set
`SSL_CERT_FILE`.

**The gateway.** One replica, `strategy: Recreate`, RWO PVC; unchanged.

**Gate before go-live.** `gateway probe` with `PROBE_DOCUMENT` set to a representative multi-page
PDF must report DI `:syncAnalyze` **200**. An earlier probed DI build listed the route in its
swagger and never answered it.

### 8. Invariants that must still hold

- The client gets its 202 only after the input blob is fsynced and the job row is committed.
- Cancellation is classified on the context first. A job interrupted during an attempt or a backoff
  stays `running`, and crash recovery re-runs it from the stored input.
- Admission and upstream slots are released on every return path.
- No upstream-minted operation id is ever stored, followed or relayed.

### 9. Testing

**`internal/mockazure` failure modes.** Each mode exposes a counter of the calls it received.

| Mode | Behaviour |
|---|---|
| `SyncBusyFirst(n)` | 503 + `Retry-After` for the first *n* calls |
| `SyncStatusFirst(code, n)` | any status for the first *n* calls (502, 504, 500, 408, 429) |
| `SyncResetMidBody` | closes the connection after writing part of the 200 body |
| `Sync202` | existing; now expected to be retried |
| `SyncDelay(d)` | delays the answer |
| `SyncCapacity(k)` | answers 503 while more than *k* calls are in flight, modelling HAProxy's per-pod cap |

**Unit tests (`internal/upstream`).**
- The outcome table in §1, row by row, as a table-driven test.
- Backoff bounds with injected jitter; `Retry-After` honoured and capped.
- The once-only class and the attempt cap.
- Cancellation during a backoff returns the context error.
- `Document.Open` is called once per attempt.
- A new connection per attempt: the mock sees a distinct remote port each time.

**Jobs.**
- A job survives *n* < cap transient failures and succeeds.
- Exhaustion fails the job with an Azure-shaped error.
- A shutdown during a backoff leaves the job `running`, and recovery completes it.

**Conformance (`test/conformance`).**
- Delete the async-upstream, degraded-202 poll-out and artifact cases.
- `output=pdf` returns 400, and no job row or blob exists afterwards.
- A DI-only gateway serves DI and answers Read routes with the unrouted 404, and the reverse for a
  Read-only gateway.
- 50 submits against `SyncCapacity(4)` with `DI_MAX_INFLIGHT=16`: every job succeeds.

**SDK tests (`test/sdk`).** The real-SDK flows still pass.

**Gate.** `gofmt -l . && go vet ./... && go test -race ./... && make image`.

## Rollout

1. Implement on branch `feat/sync-only-upstream` in `~/imp/azure-gateway-api`, together with the
   docs: README configuration table, `ARCHITECTURE.md` §3–5, `OPERATIONS.md`, `TESTING.md`,
   `HANDOFF.md` and the `api-surface.md` notes on `output`. `CONTAINER-BEHAVIOUR.md` keeps its
   observations. Merge and push when the user asks, then re-import the gateway into
   `wega-unified-services`.
2. The user runs the probe against the real DI and Read builds; I cannot reach that network.
3. Wega follow-up, as a separate spec:
   - the fleet files go to one gateway with `di.max_inflight` and `queue_depth` from §4, and
     `read.max_inflight = 0` for a DI-only gateway;
   - `ocr-cell` drops `DI_SYNC_ANALYZE=off` and the other removed settings;
   - vision's KEDA ceilings are re-derived;
   - the orchestrator's system handoff is updated.
