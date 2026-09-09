# azure-gateway-api

A wire-identical gateway for two on-prem Azure AI containers, adding the durable asynchronous
lifecycle they cannot provide outside the Azure ecosystem.

| Upstream container | Surface served |
|---|---|
| Document Intelligence `prebuilt-layout` | `/documentintelligence/**` and `/formrecognizer/**`, `api-version=2024-11-30` |
| Computer Vision Read v3.2 | `/vision/v3.2/read/**`, `model-version=2022-04-30` |

Point an unmodified Azure SDK client at it and change nothing else.

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

So the gateway owns the asynchronous lifecycle itself:

```
client ──POST :analyze────► gateway ──► 202 + Operation-Location   (committed to the PVC first)
                              │
                              └─ calls the container SYNCHRONOUSLY, stores the result on the PVC
client ──GET  poll────────► gateway ──► served from the PVC, unaffected by pod churn
client ──POST syncAnalyze─► gateway ──► streamed through; a degraded 202 is resolved, not relayed
```

The client-facing contract stays `202 + Operation-Location + poll`, because that is what every
Azure SDK is generated against. The gateway owns the fiction; the call underneath is synchronous
wherever the container allows it.

---

## Quick start

```bash
export DI_UPSTREAM_URL=http://layout.my-ns.svc.cluster.local:5000
export READ_UPSTREAM_URL=http://ocr.my-ns.svc.cluster.local:5000
export DI_UPSTREAM_API_KEY=...    # optional; the gateway needs no credential of its own
export READ_UPSTREAM_API_KEY=...
export DATA_DIR=./data

make run
```

Then point any Azure SDK at `http://localhost:8080` with any credential — the gateway requires
none of its own.

### Probe your containers first

```bash
DI_UPSTREAM_URL=... READ_UPSTREAM_URL=... ./scripts/probe-containers.sh
```

Plain `sh` and `curl`, nothing else, so it runs on a jump host or in a debug pod. This matters more
than it sounds: **these containers contradict their own documentation in load-bearing ways**, and
the gateway is built against what they actually do. The probe ends in a verdict you can act on:

```
VERDICT
  DI   :syncAnalyze     NO ANSWER: TIMED OUT after 300s
  DI   in swagger       declared
  DI   error shape      flat
  READ syncAnalyze      AVAILABLE
  READ error shape      wrapped

  -> DI errors are flat, not the wrapped shape its SDK models expect.
  -> READ errors are wrapped, not the flat shape its SDK models expect.
```

[`docs/CONTAINER-BEHAVIOUR.md`](docs/CONTAINER-BEHAVIOUR.md) explains what each line means and what
to set because of it.

---

## Two deployment constraints that will bite you

**1. Raise the OpenShift Route timeout.** Routes default to 30 seconds. A synchronous call on a
large PDF holds the connection for minutes.

```yaml
metadata:
  annotations:
    haproxy.router.openshift.io/timeout: 15m
```

**2. Do not put `vision` in the Read container's Service name.** When the inbound authority contains
that substring, the container corrupts its own `Operation-Location`, stripping the port and the
`/vision` path segment:

```
POST http://azure-vision:5000/vision/v3.2/read/analyze
  → Operation-Location: http://azure-vision/v3.2/read/analyzeResults/...
```

The gateway mints its own header and is immune. Anything calling the container directly is not.

---

## Configuration

Everything comes from the environment. Secrets belong in a Kubernetes Secret; nothing is read from
disk and no secret is ever logged. The defaults below are the ones in
[`internal/config/config.go`](internal/config/config.go).

### Required

| Variable | Purpose |
|---|---|
| `DI_UPSTREAM_URL` | Document Intelligence container root |
| `READ_UPSTREAM_URL` | Read container root. **No `vision` in the hostname** |

### Upstream

| Variable | Default | Purpose |
|---|---|---|
| `DI_UPSTREAM_API_KEY` / `READ_UPSTREAM_API_KEY` | | Sent upstream as `Ocp-Apim-Subscription-Key` |
| `DI_MAX_INFLIGHT` / `READ_MAX_INFLIGHT` | `4` | Concurrent jobs, and therefore concurrent upstream calls |
| `DI_UPSTREAM_TIMEOUT` | `15m` | Ceiling for one complete Document Intelligence analysis |
| `READ_SYNC_TIMEOUT` | `10m` | Ceiling for one `syncAnalyze`. Your Route timeout must cover this |
| `DI_SYNC_ANALYZE` | `auto` | `auto` probes and falls back, `force` requires the route, `off` never uses it |
| `DI_SYNC_PROBE_TIMEOUT` | `60s` | Ceiling on the `:syncAnalyze` attempt alone. A build can declare that route and never answer it; without this, every job would burn the full upstream timeout before falling back |
| `DI_BLIND_POLL_BUDGET` / `READ_BLIND_POLL_BUDGET` | `60` | Consecutive poll 404s tolerated before giving up on a degraded operation |

### Capacity and backpressure

| Variable | Default | Purpose |
|---|---|---|
| `QUEUE_DEPTH` | `0` | Admitted jobs allowed to wait. `0` means 429 as soon as every worker is busy |
| `MAX_REQUEST_BYTES` | `524288000` | 500 MB, the S0 ceiling, on the **upload** |
| `MAX_RESULT_BYTES` | `536870912` | Ceiling on an upstream **response**. Separate, because they bound opposite directions |
| `UPLOAD_TIMEOUT` | `10m` | How long a client may take to stream its body while holding a slot |
| `POLL_RETRY_AFTER` | `1` | Integer seconds on the 202 and in-progress polls |
| `BUSY_RETRY_AFTER` | `5` | Integer seconds on a 429 |

### Storage

| Variable | Default | Purpose |
|---|---|---|
| `DATA_DIR` | `/data` | The PVC mount |
| `ALLOW_NETWORK_FS` | `false` | Permit a network filesystem, where SQLite's WAL mode is unsafe |
| `RESULT_TTL` | `24h` | How long a result stays fetchable, matching Azure |
| `GC_INTERVAL` | `5m` | Sweeper period |
| `DISK_HIGH_WATERMARK` | `0.90` | Fraction of the volume above which completed results are evicted |
| `ARTIFACT_FETCH_TIMEOUT` | `2m` | Aggregate budget for one job's result files |

### Addressing and compatibility

| Variable | Default | Purpose |
|---|---|---|
| `PUBLIC_BASE_URL` | *(derived)* | Scheme and authority for `Operation-Location`, no path. **The recommended setting** — correct behind any proxy topology |
| `TRUST_FORWARDED_HEADERS` | `false` | Honour `X-Forwarded-Proto` / `X-Forwarded-Host`. Off by default: with it on and no allowlist, a caller can name its own poll host |
| `TRUSTED_FORWARDED_HOSTS` | | Comma-separated authorities a forwarded header may name |
| `ERROR_COMPAT` | `observed` | `observed` reproduces the error shapes these containers really emit; `documented` follows the swagger and SDK models. They differ on **both** surfaces |

### Process

| Variable | Default | Purpose |
|---|---|---|
| `GATEWAY_ADDR` | `:8080` | Listen address |
| `SHUTDOWN_GRACE` | `30s` | Time allowed for in-flight work at shutdown |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | Logging |

---

## Endpoints

### Document Intelligence — `/documentintelligence` and `/formrecognizer`

Both prefixes are served, because the containers serve both. The prefix a caller arrives on is
echoed back in `Operation-Location`.

| Method | Path |
|---|---|
| POST | `/documentModels/{modelId}:analyze` |
| POST | `/documentModels/{modelId}:syncAnalyze` *(passthrough)* |
| GET · HEAD | `/documentModels/{modelId}/analyzeResults/{resultId}` |
| DELETE | `/documentModels/{modelId}/analyzeResults/{resultId}` |
| GET | `.../analyzeResults/{resultId}/pdf` · `.../figures/{figureId}` |
| GET | `/info` · `/documentModels` · `/documentModels/{modelId}` |

### Computer Vision Read — `/vision/v3.2/read`

| Method | Path |
|---|---|
| POST | `/analyze` |
| POST | `/syncAnalyze` *(passthrough)* |
| GET · HEAD | `/analyzeResults/{operationId}` |

### Operational

`/_gw/health` · `/_gw/ready` · `/_gw/live` · `/_gw/jobs` · `/_gw/jobs/{id}` · `/_gw/metrics` ·
`/_gw/version` · `/_gw/config`, plus container-shaped `/status` and `/ready`.

`/_gw/health` is where you see whether `:syncAnalyze` works on your image: the `syncAnalyze` field
reads `unknown` until the first job runs, then settles on `available` or `unavailable`.

---

## Running it

```bash
make image          # linux/amd64, distroless, non-root, ~5.7 MB
```

One replica, `Recreate` strategy, a PVC at `/data`, `fsGroup: 65532`. Operation ids are pod-local
by construction — which is exactly what makes a PVC the right store and a second gateway replica
wrong.

Put `DATA_DIR` on a **block-backed** volume (RWO ext4/xfs). The gateway refuses to start on NFS,
CIFS or CephFS, where SQLite's WAL mode is unsafe; `ALLOW_NETWORK_FS=true` overrides that, at your
own risk.

Full manifests and runbooks: [`docs/OPERATIONS.md`](docs/OPERATIONS.md).

---

## Development

```bash
make check          # gofmt, go vet, staticcheck, go test -race
make test           # the suite on its own
make probe          # interrogate real containers
make sdk-test       # drive a running gateway with the real Azure SDKs
```

`internal/mockazure` fakes both containers **including their failure modes** — a synchronous route
that is absent, one that answers `500 UnhandledEndpointException`, one that degrades to `202`, one
that accepts and never answers, polls that 404 because they reached the wrong replica, and the
error shapes real containers were observed emitting. The whole system is therefore provable on a
laptop.

To drive the probe script without a cluster:

```bash
MOCKAZURE_LIVE=1 go test ./internal/mockazure -run TestLiveMocks &
set -a; . /tmp/mockurls.env; set +a && ./scripts/probe-containers.sh
```

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

---

## Documentation

| Document | For |
|---|---|
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | How it works internally, before you change it |
| [`docs/OPERATIONS.md`](docs/OPERATIONS.md) | Deploying, configuring, monitoring, troubleshooting |
| [`docs/CONTAINER-BEHAVIOUR.md`](docs/CONTAINER-BEHAVIOUR.md) | What the containers really do vs what they document |
| [`docs/TESTING.md`](docs/TESTING.md) | Test strategy, and how to verify a change |
| [`docs/HANDOFF.md`](docs/HANDOFF.md) | Current state, what is unfinished, how to pick this up |
| [`docs/api-surface.md`](docs/api-surface.md) | The container contract reference, with citations |
| [design spec](docs/superpowers/specs/2026-09-08-azure-gateway-api-design.md) | The approved design and every review finding |
| [`test/sdk/README.md`](test/sdk/README.md) | Proving it against the real Azure SDKs |

---

## Status

Build green: `go vet`, `gofmt`, `go test -race` across all ten packages, and a `linux/amd64` image
whose build runs the suite inside it.

**Not yet done**, stated plainly:

- it has never run against real containers end to end;
- `make sdk-test` has never been executed, and that is the acceptance test that matters;
- coverage is ~62%, below the 80% bar — the shortfall is `cmd/gateway` and `internal/probe`, which
  drive real I/O.

[`docs/HANDOFF.md`](docs/HANDOFF.md) has the full picture.
