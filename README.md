# azure-gateway-api

A wire-identical gateway for two on-prem Azure AI containers. It adds the durable asynchronous
lifecycle they cannot provide outside the Azure ecosystem, retries transient failures from the
stored input, and splits large PDFs into small parts that a fleet of pods analyses in parallel.

| Upstream container | Surface served |
|---|---|
| Document Intelligence `prebuilt-layout` | `/documentintelligence/**` and `/formrecognizer/**`, `api-version=2024-11-30` |
| Computer Vision Read v3.2 | `/vision/v3.2/read/**`, `model-version=2022-04-30` |

Point an unmodified Azure SDK client at it and change nothing else. A gateway may front either
container or both: a surface whose upstream URL is unset is simply not served.

---

## The problem it solves

The containers' synchronous behaviour is fine. Their **asynchronous** behaviour is not, because by
default the result store is container-local:

- a `resultId` minted by replica A is invisible to replica B, so a poll routed elsewhere either
  404s or reports `running` forever;
- a restart destroys in-flight work with **no failure signal**, and invalidates every operation id
  already handed out;
- `HealthCheck:MemoryUpperboundInMB` makes a container self-report unhealthy under memory pressure,
  so the kubelet restarts pods *mid-analysis*.

Microsoft's only supported fix is a shared **Azure Blob + Azure Queue** backend — *"Currently only
Azure Storage and Azure Queue are supported"*, with Redis and RabbitMQ removed in v3.x. Outside
Azure that fix does not exist.

So the gateway owns the asynchronous lifecycle itself, and talks to the containers **only
synchronously**, through the HAProxy load balancer in front of their autoscaled pods:

```
client ──POST :analyze────► gateway ──► 202 + Operation-Location   (input fsynced, row committed first)
                              │
                              ├─ small PDF or not a PDF: one :syncAnalyze call, retried on
                              │                          transient failures
                              └─ large PDF: split into 3–5 page parts, analysed in parallel,
                                            merged page-exactly into one result on the PVC
client ──GET  poll────────► gateway ──► served from the PVC, unaffected by pod churn
client ──POST :syncAnalyze► gateway ──► the same engine: retries and splitting, then one 200
```

The client-facing contract stays `202 + Operation-Location + poll`, because that is what every
Azure SDK is generated against. Underneath, every call to Azure is one synchronous
`:syncAnalyze` on a fresh connection, so HAProxy balances each one on its own and a retry is free
to land on a different pod. No container operation id is ever stored, followed or relayed.

---

## Quick start

```bash
export DI_UPSTREAM_URL=http://layout.my-ns.svc.cluster.local:5000
export READ_UPSTREAM_URL=http://ocr.my-ns.svc.cluster.local:5000   # optional: either one is enough
export DI_UPSTREAM_API_KEY=...    # optional; the gateway needs no credential of its own
export READ_UPSTREAM_API_KEY=...
export DATA_DIR=./data

make run
```

Then point any Azure SDK at `http://localhost:8080` with any credential — the gateway requires
none of its own.

No cluster? `scripts/e2e-local.sh` builds the gateway and `cmd/fakeazure` (paged fakes of both
containers), drives every endpoint and the real-SDK suite against them, and prints a pass/fail
summary. See [`docs/TESTING.md`](docs/TESTING.md).

### Probe your containers first

```bash
PROBE_DOCUMENT=./sample-20-pages.pdf gateway probe
```

`gateway probe` interrogates the configured containers only. With `PROBE_DOCUMENT` set it sends
that document instead of a blank PNG; `PROBE_TIMEOUT` (default `10m`) bounds each call. The
verdict is written for a sync-only gateway:

- `:syncAnalyze` answering **200** means the build is **ready**;
- a timeout, `404`, `405`, `501` or `500 UnhandledEndpointException` means **this build cannot run
  behind this gateway**. One probed DI build declared the route in its own swagger and never
  answered it.

Two more modes feed the go-live gates in [`docs/OPERATIONS.md`](docs/OPERATIONS.md#10-go-live-gates):

| Command | What it does |
|---|---|
| `gateway probe --record <dir>` | Records the containers' exact responses ("golden" files) for the parity test |
| `gateway probe --compare-split <dir>` | For every PDF in `<dir>`, analyses it whole and split, diffs the two results, and writes `compare-report.json`. Exits non-zero on any page-level difference |

`scripts/probe-containers.sh` is the same interrogation in plain `sh` and `curl`, for a jump host
or a debug pod with nothing else on it. [`docs/CONTAINER-BEHAVIOUR.md`](docs/CONTAINER-BEHAVIOUR.md)
records what the containers were observed doing.

---

## Deployment constraints that will bite you

**1. Configure the HAProxy Route in front of the Azure pods.** The gateway reaches the pods only
through it, and its defaults break long analyses:

```yaml
metadata:
  annotations:
    haproxy.router.openshift.io/timeout: 16m                       # > DI_UPSTREAM_TIMEOUT
    haproxy.router.openshift.io/pod-concurrent-connections: "2"    # per-pod concurrency / router replicas
    haproxy.router.openshift.io/balance: leastconn
```

The 30-second default timeout turns every longer analysis into a 504.
`pod-concurrent-connections` applies **per router replica**: divide the per-pod concurrency by the
number of router replicas, rounded down, minimum 1. Calls over the cap wait in HAProxy and then
get a 503, which the gateway retries. Details in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md#3-the-haproxy-route-in-front-of-the-azure-pods).

**2. Do not put `vision` in the Read container's hostname.** When the inbound authority contains
that substring, the container corrupts its own `Operation-Location`, stripping the port and the
`/vision` path segment. The gateway never follows that header, so it is immune; anything calling
the container directly is not. The gateway warns at startup.

**3. Memory for splitting.** At most two PDFs are parsed at once, and pdfcpu holds about 2–3× a
PDF's size while parsing it. Size the gateway's memory limit for two of your largest PDFs.

---

## Configuration

Everything comes from the environment. Secrets belong in a Kubernetes Secret; nothing is read from
disk and no secret is ever logged. The defaults below are the ones in
[`internal/config/config.go`](internal/config/config.go). There are **22 settings**.

| Variable | Default | Purpose |
|---|---|---|
| `DI_UPSTREAM_URL` | — | Document Intelligence container root, i.e. the HAProxy Route in front of the pods. Optional, but at least one of the two upstream URLs must be set. Empty disables the DI surface |
| `READ_UPSTREAM_URL` | — | Read container root. Optional, as above. **No `vision` in the hostname** |
| `DI_UPSTREAM_API_KEY` / `READ_UPSTREAM_API_KEY` | — | Sent upstream as `Ocp-Apim-Subscription-Key`. Empty is legal and warns |
| `DI_MAX_INFLIGHT` / `READ_MAX_INFLIGHT` | `4` | Concurrent calls to that surface's Azure pods, of any kind: whole documents, parts, hedges, re-checks. Also the number of jobs running at once. Size as max Azure pods × per-pod concurrency: **16** for 8 pods × 2 |
| `DI_UPSTREAM_TIMEOUT` | `15m` | One attempt at a whole DI document, or a `pages=` re-check |
| `READ_SYNC_TIMEOUT` | `10m` | One attempt at a whole Read document, or a re-check |
| `SPLIT_PAGES` | `5` | Most pages one part may hold. A PDF with more pages to analyse is split into parts of `ceil(SPLIT_PAGES/2)`–`SPLIT_PAGES` pages. `0` turns splitting off |
| `QUEUE_DEPTH` | `0` | Accepted jobs allowed to wait on the PVC per surface, on top of `*_MAX_INFLIGHT`. Size for bursts, e.g. `200`. `0` means 429 as soon as every worker is busy |
| `PUBLIC_BASE_URL` | — | Scheme and authority for `Operation-Location`, no path. **Recommended.** Unset, the request's `Host` header is used, as the container does |
| `RESULT_TTL` | `24h` | How long a result stays fetchable, matching Azure |
| `GC_INTERVAL` | `5m` | Sweeper period: TTL, temp files, orphans, stale work folders, eviction |
| `DISK_HIGH_WATERMARK` | `0.90` | Fraction of the volume above which completed results are evicted |
| `MAX_REQUEST_BYTES` | `524288000` | 500 MiB, the S0 ceiling, on the **upload** |
| `MAX_RESULT_BYTES` | `536870912` | 512 MiB ceiling on a **result**, part or merged. Separate, because they bound opposite directions |
| `UPLOAD_TIMEOUT` | `10m` | How long a client may take to stream its body while holding a slot |
| `SHUTDOWN_GRACE` | `30s` | Connection draining and job draining share this budget |
| `DATA_DIR` | `/data` | The PVC mount. Must be block-backed |
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Logging |

Parsing rules:
- An empty value counts as unset.
- A value that does not parse is fatal, for numbers as well as durations: `SPLIT_PAGES=off` fails
  startup rather than silently leaving splitting on at 5. Startup lists every problem at once.
- A disabled surface's own settings (`*_MAX_INFLIGHT`, its timeout, its API key) are not validated
  and do not warn.

Not settings, but fixed: `Retry-After` is **1** on the 202 and on in-progress polls and **5** on
the gateway's own 429; 5 attempts per whole document; 3 attempts per part size with a 60-second
part timeout; a 60-minute job deadline; 2 concurrent splits with a 60-second split budget. The
full list, and why each value is what it is, is in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md#constants).

### Removed settings

These eleven settings no longer exist. If one is still set, the gateway **starts anyway** and
names it in a warning (`<NAME> is no longer used`) in the startup log and in `/_gw/health`
`warnings[]`, so an old manifest keeps working until it is cleaned up.

| Setting | Why it went |
|---|---|
| `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT` | `:syncAnalyze` is the only upstream route |
| `DI_BLIND_POLL_BUDGET`, `READ_BLIND_POLL_BUDGET` | the gateway never polls a container |
| `ARTIFACT_FETCH_TIMEOUT` | no result files: `output=pdf` / `output=figures` are rejected |
| `ERROR_COMPAT` | errors always use the containers' own observed shapes |
| `TRUST_FORWARDED_HEADERS`, `TRUSTED_FORWARDED_HOSTS` | `PUBLIC_BASE_URL`, or else the `Host` header, as the container does |
| `ALLOW_NETWORK_FS` | a network filesystem under `DATA_DIR` is always refused |
| `POLL_RETRY_AFTER`, `BUSY_RETRY_AFTER` | now the constants 1 and 5 |

---

## Endpoints

### Document Intelligence — `/documentintelligence` and `/formrecognizer`

Both prefixes are served, because the containers serve both. The prefix a caller arrives on is
echoed back in `Operation-Location`.

| Method | Path | Served by |
|---|---|---|
| POST | `/documentModels/{modelId}:analyze` | the gateway: 202 once the input is durable |
| POST | `/documentModels/{modelId}:syncAnalyze` | the gateway: the body is saved and run through the engine (retries, splitting), then one 200 |
| GET · HEAD | `/documentModels/{modelId}/analyzeResults/{resultId}` | the gateway, from the PVC |
| DELETE | `/documentModels/{modelId}/analyzeResults/{resultId}` | the gateway: 204 |
| GET | `.../analyzeResults/{resultId}/pdf` · `.../figures/{figureId}` | the gateway: always 404, because no job ever has these files |
| GET | `/info` · `/documentModels` · `/documentModels/{modelId}` · `/operations` · `/operations/{operationId}` | relayed to the container |

**Parameter policy** on both analyze routes, checked before anything is stored:
`api-version`, `pages`, `locale`, `stringIndexType` and unknown parameters are forwarded;
`outputContentFormat=text` and an empty `features` are accepted; `outputContentFormat=markdown`,
any `features` value and `queryFields` are rejected with 400 until a probe proves the sync route
honours them; `output=pdf|figures` is always rejected. See
[`docs/api-surface.md`](docs/api-surface.md#0-what-the-gateway-serves).

### Computer Vision Read — `/vision/v3.2/read`

| Method | Path | Served by |
|---|---|---|
| POST | `/analyze` | the gateway |
| POST | `/syncAnalyze` | the gateway, through the engine |
| GET · HEAD | `/analyzeResults/{operationId}` | the gateway, from the PVC |

### Container root

`GET /`, `/swagger`, `/swagger/{rest...}`, `/records/usage-logs` and
`/records/usage-logs/{month}/{year}` are relayed to the configured container (DI when both are
configured). `GET /status` and `/ready` are answered by the gateway, container-shaped, aggregated
over the configured upstreams.

### Operational

`/_gw/health` · `/_gw/ready` · `/_gw/live` · `/_gw/jobs` · `/_gw/jobs/{id}` · `/_gw/metrics` ·
`/_gw/version` · `/_gw/config`. None is authenticated; keep them off any public Route.
`/_gw/jobs` shows `upstreamAttempts` and `upstreamParts` per job (1 = analysed whole).

---

## Running it

```bash
make image          # linux/amd64, distroless, non-root
```

One replica, `Recreate` strategy, an RWO **block** PVC at `/data`, `fsGroup: 65532`. Operation ids
are pod-local by construction — which is exactly what makes a PVC the right store and a second
gateway replica wrong. The gateway refuses to start on NFS, CIFS, CephFS, FUSE, GFS2 or OCFS2,
where SQLite's WAL mode is unsafe; there is no override.

The job store has **versioned schema migrations**. A new image migrates the database at startup,
after copying it to `gateway.db.v<N>.bak`; an image that knows migrations refuses a newer schema
rather than running on it. Upgrade, rollback and restore procedures are in
[`docs/OPERATIONS.md`](docs/OPERATIONS.md#9-schema-migrations-and-rollback).

---

## Development

```bash
make check            # gofmt, go vet, staticcheck, go test -race
make test             # the suite on its own
make probe            # interrogate real containers (shell script)
make sdk-test         # drive a running gateway with the real Azure SDKs
scripts/e2e-local.sh  # gateway + fakeazure + every endpoint + the SDK suite, live on a laptop
```

`internal/mockazure` fakes both containers **including their failure modes** — HAProxy's 503 when
every pod is at its cap, a connection reset mid-body, a 202 under memory pressure, a sync route
that is absent or hangs — and a **paged fake** that answers from the original page numbers of
whatever part PDF it is sent, with per-page hangs, failures and bad content. The whole system,
splitting included, is therefore provable on a laptop.

---

## Why the code looks the way it does

A surprising amount of this exists to satisfy behaviour buried in SDK poller internals, or to
absorb containers contradicting their own specifications. The comments name the specific client or
observation in each case; the short version:

- **`Operation-Location` differs per surface.** Document Intelligence must be absolute, must carry
  the caller's own family prefix (four SDKs hard-code
  `[^:]+://[^/]+/documentintelligence/.+/([^?/]+)` to derive the operation id), must place
  `modelId` four segments from the end and `resultId` two, and must carry `?api-version=`. Read
  must be **bare** — no query, no trailing slash — because its SDK has no poller and the canonical
  samples do `split("/")[-1]` and `Substring(len-36)`.
- **`Retry-After` is integer seconds, always.** An HTTP-date makes the Python client raise
  `DeserializationError`; omitting it makes azure-core wait 30 seconds between polls.
- **Never attach `Retry-After` to a non-retriable error** — azure-core retries *any* response ≥400
  that carries it, ten times.
- **No `Location` header on the 202, no `resourceLocation` in a poll body.** Either makes Python
  and JS issue a bogus extra GET and parse *that* as the result.
- **A 404 is only ever for an unknown or expired id.** A 404 on a live operation kills Java clients
  with an opaque `NullPointerException`.
- **A failed analysis is HTTP 200** with `"status":"failed"`. This is the dominant failure mode on
  both surfaces; code that switches on HTTP status alone mis-maps every one.
- **The error shapes are not what the specifications say.** Read wraps *every* error, including the
  ones its own `Ocr.json` models as flat; Document Intelligence wraps everything *except* an
  unknown result id. Both are reproduced — see
  [`docs/CONTAINER-BEHAVIOUR.md`](docs/CONTAINER-BEHAVIOUR.md).
- **`Server: Kestrel` on every Azure route, and no `apim-request-id`.** That is what the containers'
  own web server sends, and the containers are not known to send `apim-request-id`.
- **A merged result is page-exact or it is not written.** Offsets, page numbers, figure ids and
  every `/paragraphs/N`-style pointer are rewritten in one streaming pass that copies everything
  else byte for byte. If any safety check fails, the document is analysed whole instead.

---

## Documentation

| Document | For |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How it works internally, before you change it |
| [`docs/OPERATIONS.md`](docs/OPERATIONS.md) | Deploying, sizing, migrating, monitoring, troubleshooting, go-live gates |
| [`docs/CONTAINER-BEHAVIOUR.md`](docs/CONTAINER-BEHAVIOUR.md) | What the containers really do vs what they document |
| [`docs/TESTING.md`](docs/TESTING.md) | Test strategy, and how to verify a change |
| [`docs/HANDOFF.md`](docs/HANDOFF.md) | Current state, what is unfinished, how to pick this up |
| [`docs/api-surface.md`](docs/api-surface.md) | What the gateway serves, and the container contract reference with citations |
| [sync-only upstream design](docs/superpowers/specs/2026-10-10-sync-only-upstream-design.md) | Sync-only upstream, parameter policy, migrations, settings |
| [large-document splitting design](docs/superpowers/specs/2026-10-10-large-document-splitting-design.md) | Splitting, scheduling, part rules, merge |
| [original design spec](docs/superpowers/specs/2026-09-08-azure-gateway-api-design.md) | The first design and every review finding |
| [`test/sdk/README.md`](test/sdk/README.md) | Proving it against the real Azure SDKs |

---

## Status

The sync-only upstream and large-document splitting are implemented on branch
`feat/sync-only-upstream`. [`docs/HANDOFF.md`](docs/HANDOFF.md) has the current gate results.

**Not yet done**, stated plainly:

- it has never run against real containers end to end;
- no golden responses have been recorded from the real containers, so the parity test checks only
  the documented reference (go-live gate 2);
- `gateway probe --compare-split` has not been run on a real corpus, so document-level merge rules
  (which heading Azure marks `title`, sections across a split point, a table joined across a page
  break) are unmeasured (go-live gate 3).
