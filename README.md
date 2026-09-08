# azure-gateway-api

A wire-identical gateway for two on-prem Azure AI containers, adding the durable asynchronous
lifecycle they cannot provide outside the Azure ecosystem.

| Upstream container | Surface served by the gateway |
|---|---|
| Document Intelligence `prebuilt-layout` | `/documentintelligence/**`, `api-version=2024-11-30` |
| Computer Vision Read v3.2 | `/vision/v3.2/read/**`, `model-version=2022-04-30` |

Unmodified Azure SDK clients cannot tell the gateway from the container. Point them at it and
change nothing else.

---

## The problem it solves

The containers' synchronous behaviour is fine. Their **asynchronous** behaviour is not, because by
default the result store is container-local:

- a `resultId` minted by replica A is invisible to replica B, so a poll routed elsewhere either
  404s or reports `running` forever;
- a restart destroys in-flight work with **no failure signal** and invalidates every operation id
  already handed out;
- `HealthCheck:MemoryUpperboundInMB` makes a container self-report unhealthy under memory
  pressure, so the kubelet restarts pods *mid-analysis*.

Microsoft's only supported fix is a shared **Azure Blob + Azure Queue** backend — *"Currently only
Azure Storage and Azure Queue are supported"*, with Redis and RabbitMQ removed in v3.x. Outside
Azure that fix does not exist.

So the gateway owns the async lifecycle itself:

```
client ──POST :analyze──► gateway ──► 202 + Operation-Location   (committed to the PVC first)
                             │
                             └─ calls the container SYNCHRONOUSLY, stores the result on the PVC
client ──GET  poll────────► gateway ──► served from the PVC, unaffected by pod churn
client ──POST syncAnalyze─► gateway ──► streamed straight through, nothing stored
```

The client-facing contract stays `202 + Operation-Location + poll`, because that is what every
Azure SDK is generated against. The gateway owns the fiction; the call underneath is synchronous.

---

## Quick start

```bash
export DI_UPSTREAM_URL=http://layout.my-ns.svc.cluster.local:5000
export DI_UPSTREAM_API_KEY=...
export READ_UPSTREAM_URL=http://ocr.my-ns.svc.cluster.local:5000
export READ_UPSTREAM_API_KEY=...
export DATA_DIR=./data

make run
```

Then point any Azure SDK at `http://localhost:8080` with any credential — the gateway requires
none of its own.

### Check what your containers actually serve, first

```bash
make probe
```

This matters more than it sounds. The gateway's happy path depends on
`POST /documentintelligence/documentModels/{modelId}:syncAnalyze`, a route that appears in the
container image's own routing table and in **no Microsoft documentation and no public REST spec**.
Some builds do not serve it; some degrade to `202` under load. `probe` tells you which you have,
and pulls each container's own `/swagger` document, which is authoritative for your exact image
and supersedes every published article where they disagree.

The gateway handles all three outcomes on its own — the probe just tells you which one you are
living with, and lets you skip a wasted call per job by setting `DI_SYNC_ANALYZE=off`.

---

## Two deployment constraints that will bite you

**1. Raise the OpenShift Route timeout.** Routes default to a 30-second HAProxy timeout. A
synchronous call on a large PDF holds the connection for minutes.

```yaml
metadata:
  annotations:
    haproxy.router.openshift.io/timeout: 15m
```

**2. Do not put `vision` in the Read container's Service name.** When the inbound authority
contains that substring, the Read container corrupts its own `Operation-Location`, stripping both
the port and the `/vision` path segment:

```
POST http://azure-vision:5000/vision/v3.2/read/analyze
  → Operation-Location: http://azure-vision/v3.2/read/analyzeResults/...
```

The gateway mints its own header and never follows the container's, so it is immune — but anything
calling the container directly is not. The gateway logs a warning at startup if it sees this.

---

## Configuration

Everything comes from the environment. Secrets belong in a Kubernetes Secret; nothing is read from
disk and no secret is ever logged.

| Variable | Default | Purpose |
|---|---|---|
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `PUBLIC_BASE_URL` | *(derived)* | Scheme and authority for `Operation-Location`, no path. **The recommended setting** — it is the only option that is correct behind any proxy topology, and it removes the question of which headers to trust |
| `TRUST_FORWARDED_HEADERS` | `false` | Honour `X-Forwarded-Proto` / `X-Forwarded-Host`. Off by default: with it on and no allowlist, a caller can set `X-Forwarded-Host` and have its `Operation-Location` point anywhere. Behind an OpenShift Route the request's own `Host` is already correct |
| `TRUSTED_FORWARDED_HOSTS` | | Comma-separated authorities a forwarded header may name. An entry without a port matches that host on any port |
| `DATA_DIR` | `/data` | The PVC mount |
| `ALLOW_NETWORK_FS` | `false` | Permit a network filesystem, where SQLite's WAL mode is unsafe |
| `DI_UPSTREAM_URL` | *(required)* | Document Intelligence container root |
| `DI_UPSTREAM_API_KEY` | | Sent upstream as `Ocp-Apim-Subscription-Key` |
| `DI_MAX_INFLIGHT` | `4` | Concurrent DI jobs, and therefore concurrent upstream calls |
| `DI_UPSTREAM_TIMEOUT` | `15m` | Ceiling for one complete DI analysis |
| `DI_SYNC_ANALYZE` | `auto` | `auto` probes and falls back, `force` requires the route, `off` never uses it |
| `DI_BLIND_POLL_BUDGET` | `60` | Consecutive poll 404s tolerated before giving up on a degraded operation |
| `READ_UPSTREAM_URL` | *(required)* | Read container root. **No `vision` in the hostname** |
| `READ_UPSTREAM_API_KEY` | | Sent upstream |
| `READ_MAX_INFLIGHT` | `4` | Concurrent Read jobs |
| `READ_SYNC_TIMEOUT` | `10m` | Ceiling for one `syncAnalyze`. Your Route timeout must cover this |
| `QUEUE_DEPTH` | `0` | Admitted jobs allowed to wait. `0` means 429 as soon as every worker is busy |
| `RESULT_TTL` | `24h` | How long a result stays fetchable, matching Azure |
| `GC_INTERVAL` | `5m` | Sweeper period |
| `ARTIFACT_FETCH_TIMEOUT` | `2m` | Aggregate budget for one job's result files. Bounds a wedged container so it cannot outlast the shutdown grace |
| `UPLOAD_TIMEOUT` | `10m` | How long a client may take to stream its request body while holding an admission slot |
| `DISK_HIGH_WATERMARK` | `0.90` | Fraction of the volume above which completed results are evicted |
| `MAX_REQUEST_BYTES` | `524288000` | 500 MB, the Document Intelligence S0 ceiling, on the **upload** |
| `MAX_RESULT_BYTES` | `536870912` | Ceiling on an upstream **response**. Separate from the upload cap so lowering one does not silently break the other |
| `POLL_RETRY_AFTER` | `1` | Integer seconds on the 202 and in-progress polls |
| `BUSY_RETRY_AFTER` | `5` | Integer seconds on a 429 |
| `SHUTDOWN_GRACE` | `30s` | Time allowed for in-flight work at shutdown |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Logging |

### Capacity

`DI_MAX_INFLIGHT` and `READ_MAX_INFLIGHT` are the only concurrency numbers that matter. A slot is
taken when a request is admitted and held until the job is terminal, so they bound accepted work
and upstream concurrency at once. With `QUEUE_DEPTH=0` a submit past that limit gets an immediate,
well-formed `429` with `Retry-After` rather than an unbounded wait.

Size them against the containers, not the gateway: a Layout container wants 8 cores and 16–24 GB
and saturates its own cores on a single multi-page document, so CPU-based autoscaling on the
container is close to useless. Scale on the gateway's queue depth instead.

---

## Endpoints

### Document Intelligence

| Method | Path |
|---|---|
| POST | `/documentintelligence/documentModels/{modelId}:analyze` |
| POST | `/documentintelligence/documentModels/{modelId}:syncAnalyze` *(passthrough)* |
| GET | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}` |
| DELETE | `/documentintelligence/documentModels/{modelId}/analyzeResults/{resultId}` |
| GET | `.../analyzeResults/{resultId}/pdf` · `.../figures/{figureId}` |
| GET | `/documentintelligence/info` · `/documentModels` · `/documentModels/{modelId}` |

### Computer Vision Read

| Method | Path |
|---|---|
| POST | `/vision/v3.2/read/analyze` |
| POST | `/vision/v3.2/read/syncAnalyze` *(passthrough)* |
| GET | `/vision/v3.2/read/analyzeResults/{operationId}` |
| DELETE | `/vision/v3.2/read/analyzeResults/{operationId}` |

### Operational

`/_gw/health` · `/_gw/ready` · `/_gw/live` · `/_gw/jobs` · `/_gw/jobs/{id}` · `/_gw/metrics` ·
`/_gw/version` · `/_gw/config`, plus container-shaped `/status` and `/ready`.

`/_gw/health` is where you see whether `:syncAnalyze` works on your image: the `syncAnalyze` field
reads `unknown` until the first job runs, then settles on `available` or `unavailable`.

Use `/_gw/live` for the liveness probe and `/_gw/ready` for readiness. Readiness deliberately
ignores upstream reachability — a container that is briefly down should fail individual jobs, not
take the gateway out of service and strand every stored result.

### Result files

`output=pdf` and `output=figures` force the asynchronous upstream path, because result files are
addressed by the container's own operation id and a synchronous call never mints one. They are
fetched and stored the moment the analysis completes, since that id may vanish with its replica.

---

## Running it

```bash
make image                      # linux/amd64, distroless, non-root
```

The pod needs a PVC at `/data`, `fsGroup: 65532` so the non-root user can write it, and
`strategy: Recreate` with **one replica**. Operation ids are pod-local by construction — that is
exactly the property that makes a PVC the right store, and a second gateway replica wrong.

Put `DATA_DIR` on a **block-backed** volume (RWO ext4/xfs). The gateway refuses to start on NFS,
CIFS, CephFS and similar, where SQLite's WAL mode is unsafe; `ALLOW_NETWORK_FS=true` overrides
that, at your own risk.

---

## Development

```bash
make check       # gofmt, go vet, staticcheck, go test -race
make test        # the suite on its own
make probe       # interrogate real containers
make sdk-test    # drive a running gateway with the real Azure SDKs
```

`internal/mockazure` fakes both containers **including their failure modes** — a synchronous route
that is absent, one that answers `500 UnhandledEndpointException`, one that degrades to `202`,
polls that 404 because they reached the wrong replica, and the bare `{"status":"Failed"}` body that
CV `syncAnalyze` returns. So the whole thing is provable on a laptop.

---

## Why the code looks the way it does

A surprising amount of this gateway exists to satisfy behaviour buried in SDK poller internals
rather than in any published contract. The comments name the specific client that breaks in each
case; the short version:

- **`Operation-Location` differs per surface.** Document Intelligence must be absolute, must
  contain the literal `/documentintelligence/` segment (four SDKs hard-code
  `[^:]+://[^/]+/documentintelligence/.+/([^?/]+)` to derive the operation id), must place
  `modelId` four segments from the end and `resultId` two, and must carry `?api-version=`.
  Computer Vision Read must be **bare** — no query, no trailing slash — because its SDK has no
  poller and the canonical samples do `split("/")[-1]` and `Substring(len-36)` + `Guid.Parse`.
- **`Retry-After` is integer seconds, always.** An HTTP-date makes the Python DI client raise
  `DeserializationError`. Omitting it makes azure-core wait 30 seconds between polls.
- **Never attach `Retry-After` to a non-retriable error** — azure-core retries *any* response ≥400
  that carries it, ten times, bypassing its method allowlist.
- **No `Location` header on the 202, no `resourceLocation` in a poll body.** Either makes Python
  and JS issue a bogus extra GET and parse *that* response as the result.
- **A 404 is only ever for an unknown or expired id.** A 404 on a live operation kills Java clients
  with an opaque `NullPointerException` and marks the operation terminally failed in .NET and JS.
- **A failed analysis is HTTP 200** with `"status":"failed"`. This is the dominant failure mode on
  both surfaces; code that switches on HTTP status alone mis-maps every one of them.
- **The two surfaces use different error shapes.** DI wraps in `{"error":{…}}`; Read is flat.
  Read is the odd one out even within Computer Vision, because its routes live in `Ocr.json`.

The full derivation, with citations, is in [`docs/api-surface.md`](docs/api-surface.md).

## Documentation

- [Design](docs/superpowers/specs/2026-09-08-azure-gateway-api-design.md) — architecture and the
  decisions behind it
- [API surface reference](docs/api-surface.md) — the authoritative contract for both containers,
  including the open questions `gateway probe` settles
- [Real-SDK conformance](test/sdk/README.md)
