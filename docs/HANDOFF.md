# Handoff

For whoever picks this up next, human or agent. It says what state the work is in, what is
deliberately unfinished, the things that look wrong but are not, and how to continue without
breaking the guarantees.

Read this before [`ARCHITECTURE.md`](ARCHITECTURE.md). This tells you *why the repo is shaped like
this*; that tells you how it works.

**Building or deploying this for the Wega pipeline?** Go straight to
[Wega: build and deploy facts](#wega-build-and-deploy-facts).

**Update rule: update this file in the same change as anything that alters build or deploy
facts.** That covers the Dockerfile, the Makefile, a configuration variable or its default, an
endpoint under `/_gw`, and the probe or health semantics.

---

## What this is, in three sentences

Two Azure AI containers run inside an OpenShift cluster, outside Azure. Their synchronous calls are
fine, but their asynchronous ones break under autoscaling because the result store is
container-local and the only supported fix — shared Azure Blob + Queue — does not exist outside
Azure. This gateway is byte-compatible with both containers, owns the asynchronous lifecycle on a
PVC, and calls the containers synchronously wherever they allow it, so clients change nothing and
operation ids survive pod churn.

---

## Current state

Verified, not remembered — re-run the commands if you doubt any of it.

| Check | Command | State |
|---|---|---|
| Build | `go build ./...` | pass |
| Cross-compile | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/gateway` | pass |
| Format | `gofmt -l .` | clean |
| Vet | `go vet ./...` | clean |
| Tests | `go test -race ./...` | 11/11 packages with tests pass |
| Image | `make image` | exit 0, ~5.7 MB, suite runs *inside* the build |
| Git | `git status` | clean. `origin/main` stops at `0f9c794`; the Wega handoff commits after it were not pushed as of 2026-10-04 |
| Publishable | `git clone <url> /tmp/x && cd /tmp/x && go build ./...` | pass |

> **Do not substitute `git status` for that last row.** A `.gitignore` entry of `gateway` — meant
> for the build output — also matched the `cmd/gateway` **directory**, so `cmd/gateway/main.go` was
> never committed. `git status` called the tree clean the whole time, because an ignored file is
> not untracked. Every local check above passed against a published repository that had no `main`
> function in it. The only check that catches this class of error is building a fresh clone.
> `git check-ignore -v <path>` tells you which rule is responsible.

109 test functions: 46 conformance, 63 unit. Coverage 67.7% overall — the paths that carry the
guarantees are covered (`ids` 97%, `app` 93%, `httpx` 89%, `azerr` 83%, `jsonx` 77%, `store` 75%,
`surface` 71%, `jobs` 69%). The shortfall is `internal/probe` at 0% and `internal/admin` at 10%,
both of which drive real I/O. `cmd/gateway` contributes nothing either way: the Makefile measures
with `-coverpkg=./internal/...`, so it is not in the denominator. Figures from `make cover`;
`docs/TESTING.md` carries the full table and is the one to update.

Re-verified on 2026-10-04 on `main` after merging `wega/handoff`; the code is unchanged since
`0f9c794`, and only documentation moved. `go build ./...`, the linux/amd64 cross-compile,
`make lint` (gofmt, `go vet`, staticcheck), `go test ./...` and `go test -race ./...` all pass.
The race run passes 109 tests in 11 packages and skips 1 (`TestLiveMocks`, the harness gated on
`MOCKAZURE_LIVE=1`). Coverage measured 68.0%; it moves by a few tenths between runs. A fresh
clone of `main` builds and vets clean. `make image` was not re-run that day, because Docker was
unavailable on that machine.

---

## The three things that make this codebase make sense

Almost every odd-looking decision traces to one of these. If a piece of code looks arbitrary, check
here before "fixing" it.

**1. The containers contradict their own documentation.** Not in trivia — in the error shapes every
client parses, and in whether a route answers at all. A probe against a live deployment found
Document Intelligence returning an unknown result id *flat* while wrapping every other error, and
Computer Vision Read wrapping *every* error including the ones its own `Ocr.json` models as flat.
The gateway reproduces observed behaviour, not documented behaviour.
See [`CONTAINER-BEHAVIOUR.md`](CONTAINER-BEHAVIOUR.md).

**2. Azure SDK pollers are brittle in specific, documented-nowhere ways.** An HTTP-date
`Retry-After` hard-fails the Python client. A `Location` header on the 202 makes Python and JS
issue a bogus extra GET and parse *that* as the result. A 404 on a live operation kills Java
clients with an opaque `NullPointerException`. Roughly twenty such rules are encoded in the code
and each has a conformance test naming the client it protects.
See [`api-surface.md`](api-surface.md) §10.

**3. The 202 is a durability promise.** Once a client holds an operation id, it must resolve for
its whole TTL across any crash short of losing the volume. That single constraint dictates the
submit ordering, `synchronous=FULL`, the lease and claim machinery, and the whole recovery path.

---

## What is done

- Both client surfaces, wire-compatible, including the legacy `/formrecognizer` family.
- Synchronous passthrough that resolves a degraded 202 instead of relaying it.
- Durable job store: SQLite (WAL, `synchronous=FULL`, single writer) plus atomic blob storage.
- Crash recovery, TTL expiry, disk-pressure eviction, orphan reclamation.
- Admission control with 429, separate upstream and metadata concurrency lanes.
- Upstream decision tree with capability latching and affinity polling.
- Byte-range result composition so a 500 MB result never lands on the heap.
- Faithful container fakes, including their failure modes.
- A standalone `sh` probe for interrogating real containers.
- 32 defects found by adversarial review, all fixed with regression tests.

---

## What is NOT done

Ranked by how much it should worry you.

**1. It has never run against real containers end to end.** Everything is proven against fakes. The
fakes model observed behaviour, but a fake is still a fake. This is the single largest unknown.

**2. `make sdk-test` has never been executed.** It drives a running gateway with the real Azure
SDKs and is the only check that proves an unmodified client cannot tell the difference. It needs a
running gateway plus a Python venv. **Treat this as the acceptance test.** Note its assertions were
written before the error shapes changed — verify it still asserts the right shapes before trusting
a pass. See [`TESTING.md`](TESTING.md).

**3. Coverage is 67.7%, below the 80% bar.** `internal/admin` has no direct unit tests and is
covered only incidentally through the conformance suite; `internal/probe` is at zero. `cmd/gateway`
is outside the profile entirely (`-coverpkg=./internal/...`), so it is neither a contributor nor a
shortfall.

**4. The error-shape decision rests on one probe run against one deployment.** If other
environments run different image builds they may not agree. `ERROR_COMPAT=documented` is the
escape hatch. Re-probe per environment.

**5. `:analyzeBatch`, custom models and classifiers are unimplemented**, deliberately — batch is
Azure-Blob-bound and unusable air-gapped. If a client needs them this is new work, not a bug.

**6. No Kubernetes manifests ship in the repo**, by request. [`OPERATIONS.md`](OPERATIONS.md)
contains a working set to copy. Wega's manifests for this gateway live in the orchestrator
repository; see [Wega: build and deploy facts](#wega-build-and-deploy-facts).

---

## Wega: build and deploy facts

Last verified on 2026-10-04: the code at `0f9c794`, and the manifests that deploy it at
`document-embedding-orchestrator` `44016ec` (`deploy/openshift/components/ocr-cell/`,
`deploy/openshift/components/infra-mocks/` and the dev and perf overlays), together with the
fleet files of all four overlays there. The facts quoted here from the other repositories were
checked on their `main` branches: `document-vision-service` `5f93223` and
`document-enrichment-services` (DES) `bf57bbb`. The system-level
handoff for every Wega repository is `docs/deploy/SYSTEM-HANDOFF.md` in
`document-embedding-orchestrator`, with the machine-readable `docs/deploy/system.yaml` beside it.
This section covers only what is true of the gateway.

**Update rule: update this file in the same change as anything that alters build or deploy
facts.** Some facts here are also relied on by the orchestrator's `ocr-cell` component or by the
shared gateway fleet file: a port, a label, an endpoint, a variable name, or a health rule. When
one of those changes, change the consumers in the same release. The reverse holds too: when
`ocr-cell` or `infra-mocks` changes, re-check the tables below against it.

### How Wega deploys it

The Wega pipeline uses this gateway as its OCR cell: vision sends all of Wega's Layout and Read
work through it. The gateway is a real component in every environment. In dev and perf, only its
two upstreams are mocks.

| Aspect | Fact |
|---|---|
| Manifests | Not in this repository. They are the orchestrator's kustomize component `deploy/openshift/components/ocr-cell/`: `gateway.yaml` (the PVC, Service, Deployment and PodMonitor), `gateway.env` (the ConfigMap), `networkpolicies.yaml` and `serviceaccount.yaml`. The dev and perf overlays include it. Staging and prod do not; see the staging and prod row. |
| Namespace | `wega-<env>`, shared with every other Wega workload of that environment. |
| Workload | Deployment `azure-gateway-api` with `replicas: 1` and `strategy.type: Recreate`. It runs one container, `gateway`, with `args: [serve]`, under ServiceAccount `azure-gateway-api` with `automountServiceAccountToken: false`. Never scale it out, and never use `RollingUpdate`; see [One replica, `Recreate`](OPERATIONS.md#one-replica-recreate). |
| Storage | PVC `azure-gateway-api-data`: `ReadWriteOnce`, 50 Gi, mounted at `/data` (`DATA_DIR`). The class must be **block**: ext4 or xfs on RBD or similar. `ocr-cell` commits `ocs-storagecluster-ceph-rbd`. The class is the system parameter `P-STORAGE-BLOCK`, which a release overlay patches per cluster. The gateway refuses to start on CephFS, NFS or FUSE. |
| Service | `azure-gateway-api`, ClusterIP, port `8080` named `http`, with `targetPort: http`. Wega needs no Route: every Wega client is in-cluster. Do not add one either, because no `/_gw` endpoint is authenticated. |
| Labels | The Deployment, its pods, the Service, the PVC and the ServiceAccount carry `app.kubernetes.io/name: azure-gateway-api` and `app.kubernetes.io/part-of: wega`. The Service, the PodMonitor and both gateway NetworkPolicies select the name label. So do the mocks' ingress policy, the orchestrator's `wega-control-egress` and vision's egress policy. The pods carry no `wega.io/deployment` label, so the namespace's `wega-scrape` policy does not cover them; the gateway's own ingress policy admits the scrape instead. |
| Fleet entry | `gw-01`, with `base_url` `http://azure-gateway-api.wega-<env>.svc.cluster.local:8080`, and `max_inflight: 4` and `queue_depth: 2` on both `di` and `read`. The file is the orchestrator's `deploy/openshift/overlays/<env>/gateway-fleet.json`, published as ConfigMap `wega-gateway-fleet`. Vision's overlays carry the same file byte for byte: all four environments match from `document-vision-service` `4e9d40e`, and `WEGA_ORCHESTRATOR_REPO=<orchestrator checkout> make test-parity` in vision checks it. A fleet change therefore lands in both repositories. |
| Upstreams in dev and perf | The DES Azure mocks, from the orchestrator component `infra-mocks` (`azure-mocks.yaml`). Layout is Deployment and ClusterIP Service `azure-di-mock`; Read is `azure-read-mock`. Each Service listens on port `5000`, named `http`, which is also the container port. The images are `wega-azure-di-mock` and `wega-azure-read-mock`, built from `document-enrichment-services` (`mock_azure_di/`, `mock_azure_ocr/`). Each mock runs `replicas: 1` with `Recreate`. Its startup and readiness probes are `GET /status`, and its liveness probe is `GET /health`. The pods are labelled `app.kubernetes.io/name: azure-di-mock` or `azure-read-mock`, plus `wega.io/mock: "true"`. Their ingress policy, `wega-azure-mocks-ingress`, admits only pods labelled `app.kubernetes.io/name: azure-gateway-api`, on TCP 5000. |
| Real upstreams | The Azure Document Intelligence Layout and Read containers. Probe them before the first deploy (`make probe`; see [OPERATIONS §1](OPERATIONS.md#1-before-you-deploy-probe-the-containers)), and set `DI_SYNC_ANALYZE` from the verdict. |
| Staging and prod | No `ocr-cell`. The orchestrator's committed staging and prod fleets list several cells, `gw-NN` at `http://azure-gateway-api-NN.gateway-<env>.svc.cluster.local:8080`. These are real gateways against real Azure, deployed outside the Wega overlays: open input `I-GATEWAY-CELLS` in the system handoff. Vision's staging and prod egress reaches them; see the network items [below](#probes-network-and-security-context-in-ocr-cell). `ocr-cell` is the template for one cell, with three changes. The Secret below must exist. The ingress policy admits callers from the cell's own namespace only, so it must admit `wega.io/deployment` `vision` and `orch-control` from namespace `wega-<env>`. The egress policy admits only the two mocks, so it must name the real upstreams instead, and must allow DNS unless a policy in `gateway-<env>` already does, as `wega-allow-dns` does in `wega-<env>`. Each of these fleet entries gives `di` 8/4 and `read` 4/2; see [Known gaps](#known-gaps-for-wega-as-of-2026-10-04). |
| Callers | `wega-vision`, whose pods are labelled `wega.io/deployment: vision`, submits and polls on both surfaces, and reads `/_gw/health`, `/_gw/jobs/{id}` and `/_gw/version`. The orchestrator observer runs in `wega-orch-control`, labelled `wega.io/deployment: orch-control`, and reads `/_gw/health` and `/_gw/config`. User-workload monitoring scrapes `/_gw/metrics` through PodMonitor `azure-gateway-api` every 30 s. The ingress policy admits nobody else. |

### Configuration Wega sets

The values live in ConfigMap `azure-gateway-api`. `ocr-cell` generates it from `gateway.env`, and
the container reads it with `envFrom`. Each overlay's `patch-gateway.yaml` adds
`PUBLIC_BASE_URL`. This table gives the value Wega sets in dev and perf, and the rule each value
must satisfy. The full catalogue is in [OPERATIONS §4](OPERATIONS.md#4-configuration-recipes).

| Variable | dev and perf (`ocr-cell`) | Against real containers | Why |
|---|---|---|---|
| `DI_UPSTREAM_URL` | `http://azure-di-mock:5000` | The Layout container's Service URL | Required; startup fails without it. 5000 is the `infra-mocks` Service port, and the mock's container port too. The gateway's egress policy admits TCP 5000 to the two mocks and nothing else. |
| `READ_UPSTREAM_URL` | `http://azure-read-mock:5000` | The Read container's Service URL | Required. The hostname must not contain `vision` ([why](OPERATIONS.md#the-vision-substring-bug-in-the-read-container)). |
| `DI_SYNC_ANALYZE` | `off` | From the probe verdict | With `off`, the gateway never tries `:syncAnalyze`. `/_gw/health` reports `syncAnalyze: unavailable` for Layout from boot, and DI jobs run as `:analyze` plus polling and show `upstreamMode: async-fallback`. `gateway.env` keeps `off` as a choice, not a necessity: the DES Layout mock on DES `main` (`bf57bbb`) serves `:syncAnalyze`, a route it gained in `d1b8ec4`, so `auto` would work against it too. `off` costs only the fast path. Changing it is an `ocr-cell` change, and this row changes with it. |
| `DI_MAX_INFLIGHT` | `4`, which is `gw-01` `di.max_inflight` | Same rule | Every 300 s the orchestrator observer reads `/_gw/config`. When `diMaxInflight` differs from the fleet value, it raises `WegaGatewayFleetDrift`. |
| `QUEUE_DEPTH` | `2`, which is `gw-01` `di.queue_depth` | Same rule | Also drift-checked, as `queueDepth`. The gateway has one `QUEUE_DEPTH` for both surfaces, so the fleet's `read.queue_depth` must equal its `di.queue_depth`. |
| `READ_MAX_INFLIGHT` | `4`, which is `gw-01` `read.max_inflight` | Same rule | The observer does not drift-check it. Keep it equal anyway, so that the fleet file stays a true description of the gateway. |
| `PUBLIC_BASE_URL` | `http://azure-gateway-api.wega-<env>.svc.cluster.local:8080`, the fleet `base_url`, from `overlays/<env>/patch-gateway.yaml` | Same rule | Give scheme and authority only; the gateway rejects a path. Vision takes only the operation id from `Operation-Location` and rebuilds every poll URL from the fleet `base_url`, so this is not load-bearing for Wega. Setting it silences the `PUBLIC_BASE_URL is unset` warning and fixes the advertised authority. |
| `DATA_DIR` | `/data`, set explicitly, and also the image default | Same | Must be the PVC mount. |
| `SHUTDOWN_GRACE` | `30s`, under `terminationGracePeriodSeconds: 60` | Less than `terminationGracePeriodSeconds` | The default is `30s`. |

The orchestrator's `tests/parity/test_deploy_components.py` fails when `DI_MAX_INFLIGHT`,
`READ_MAX_INFLIGHT`, `QUEUE_DEPTH` or `PUBLIC_BASE_URL` disagrees with the `gw-01` entry of that
overlay's fleet file. The fleet file is the source: derive the values from it, not from this
table.

`gateway.env` also sets `GATEWAY_ADDR=:8080`, `DI_UPSTREAM_TIMEOUT=15m`,
`READ_SYNC_TIMEOUT=10m`, `RESULT_TTL=24h`, `GC_INTERVAL=5m`, `DISK_HIGH_WATERMARK=0.90`,
`ERROR_COMPAT=observed`, `LOG_LEVEL=info` and `LOG_FORMAT=json`. Each equals the gateway's own
default in `internal/config/config.go`. Changing one of those defaults therefore does not change
Wega, because the ConfigMap still sets the old value. Every variable not named in this section
keeps its default.

**Secret (name only).** The Secret is `azure-gateway-api`, with keys `DI_UPSTREAM_API_KEY` and
`READ_UPSTREAM_API_KEY`. The container reads each key into the variable of the same name through
`env[].valueFrom.secretKeyRef` with `optional: true`, not through `envFrom`. The orchestrator's
`policy/secrets.rego` admits exactly these two keys of this Secret, and only in this container.
Everywhere else, Wega mounts secrets as files. The gateway reads nothing from disk, so it is the
documented exception. In dev and perf the Secret need not exist, because the DES mocks check no
key: they would only check one with `MOCK_API_KEY` set, and `infra-mocks` does not set it. Both
variables are then unset, and `/_gw/health` lists two
`…_API_KEY is empty; upstream calls will be unauthenticated` lines under `warnings`. In those
environments they are expected, not faults. Against real containers the Secret is required, and
the keys are the Azure resource's billing keys.

### Health: the endpoints, and which one Wega reads

| Endpoint | What it answers | Who uses it |
|---|---|---|
| `GET /_gw/live` | `200 {"status":"ok"}`, always, touching no dependency | The liveness and startup probes |
| `GET /_gw/ready` | `200` when both SQLite pools answer a ping within 3 s, else `503`. Upstreams are deliberately excluded | The readiness probe, and nothing else. Wega never treats it as health, because it answers `ready` with both upstreams down. |
| `GET /_gw/health` | The full diagnostic. `503` only when the store is unreachable. Otherwise `200`, with `status` set to `ok`, or to `degraded` when any upstream is not ok | Wega's only health signal, for vision's per-surface breakers and the observer's OCR window. Never a probe. |
| `GET /_gw/config` | The effective configuration, with keys redacted | The observer's fleet-drift check, through `diMaxInflight`, `readMaxInflight` and `queueDepth` |
| `GET /_gw/metrics` | Prometheus text format | PodMonitor `azure-gateway-api`, every 30 s |
| `GET /status`, at the root | Container-shaped `apiStatus`, `Valid` or `Invalid`, always at HTTP `200` | Nothing in Wega. It cannot fail, so it is useless as a probe. |

**How `upstreams[].ok` is decided.** `/_gw/health` lists two upstreams: `document-intelligence`
for Layout and `computer-vision-read` for Read. On every request, for each upstream, the gateway
sends `GET <upstream URL>/status` with that upstream's API key and a 5 s timeout; the whole
handler has 6 s. `ok` is true exactly when that call returns HTTP `200`. The body only feeds
`detail`, which copies `apiStatus` and `apiStatusMessage` when they are present. Neither field
affects `ok`, so `apiStatus: Invalid` at HTTP `200` still counts as ok. The logic is
`checkStatus` in [`internal/upstream/client.go`](../internal/upstream/client.go).

**Why this matters to Wega.** An upstream reads `ok: false` when its `/status` answers 404, times
out, or cannot be reached. After two consecutive `ok: false` reads, vision opens that surface's
breaker and stops sending it work, and the observer drops the surface from the OCR window. **So
every upstream this gateway points at, mocks included, must answer `GET /status` with HTTP 200
within 5 s.** The recommended body is `{"apiStatus":"Valid"}`. The DES mocks on DES `main`
(`bf57bbb`) serve that, with an `apiStatusMessage` beside it. The route arrived in `d1b8ec4` and
reached `main` with merge `d28de8d`; a mock image built from an older `main` serves only
`/health`. `infra-mocks` makes `/status` the startup and readiness probe of both mocks, so a mock
image without it never becomes Ready.

The same response carries `capacity.di` and `capacity.read`, each with `inFlight`, `limit`,
`queued` and `upstreamInUse`. `limit` is `MAX_INFLIGHT + QUEUE_DEPTH`; the observer uses it as
that surface's admission slots.

### What an upstream must serve

These are the calls the gateway makes, so this is the contract the DES mocks must meet in dev and
perf.

| Call | When | Required answer |
|---|---|---|
| `GET /status` | On every `/_gw/health` request and every root `/status` request | `200`, within 5 s |
| `POST /documentintelligence/documentModels/{modelId}:analyze?api-version=…` | Every Layout job, with `DI_SYNC_ANALYZE=off` | `202`, with an `Operation-Location` whose last path segment is the result id |
| `GET /documentintelligence/documentModels/{modelId}/analyzeResults/{id}?api-version=…` | Polling, until terminal | `200` with `status`, plus `analyzeResult` once `succeeded` |
| `POST /vision/v3.2/read/syncAnalyze` | First, on every Read job | `200` with the result. A `404` or `405` makes the gateway fall back for that job. |
| `POST /vision/v3.2/read/analyze` | The fallback | `202`, with an `Operation-Location` whose last path segment is the operation id |
| `GET /vision/v3.2/read/analyzeResults/{id}` | Polling, until terminal | `200` with `status`, plus `analyzeResult` once `succeeded` |

The Read fallback has no latch. Against a mock without `syncAnalyze`, every Read job uploads its
document twice, once to the `404` and once to `analyze`, and shows `upstreamMode: async-fallback`.
That costs time, not correctness. The DES Read mock on DES `main` serves `syncAnalyze`, so Read
jobs against it run `sync`. Keep each mock at one replica, as `infra-mocks` does, because their
job stores are per process. A poll that reaches the other replica gets a 404, and 60 of those in
a row (the default blind-poll budget) fail the job.

### Probes, network and security context in `ocr-cell`

- Probes, all on port `http` (8080):
  - startup on `/_gw/live`, every 2 s, failure threshold 30, so the gateway has 60 s to start;
  - readiness on `/_gw/ready`, every 10 s, timeout 5 s, failure threshold 3;
  - liveness on `/_gw/live`, every 10 s, timeout 3 s, failure threshold 3.

  Never put liveness or readiness on `/_gw/health`, because it calls both upstreams, and never
  use `/status` as a probe.
- `terminationGracePeriodSeconds: 60`, which must exceed `SHUTDOWN_GRACE` (`30s`).
- Resources: requests of 250m CPU and 256Mi, limits of 2 CPU and 1Gi.
- The container sets `readOnlyRootFilesystem: true`, `allowPrivilegeEscalation: false`, and drops
  all capabilities. The pod sets `runAsNonRoot: true`, the `RuntimeDefault` seccomp profile and
  `enableServiceLinks: false`. The read-only root works because every write lands under
  `DATA_DIR`; a 64Mi `emptyDir` is mounted at `/tmp` anyway.
- Ingress, policy `azure-gateway-api-ingress`: TCP 8080 from pods in the namespace labelled
  `wega.io/deployment` `vision` or `orch-control`, and from the namespace
  `openshift-user-workload-monitoring` for metrics. No other pod can connect, including the
  orchestrator's API and workers.
- Egress, policy `azure-gateway-api-egress`: TCP 5000 to pods labelled `app.kubernetes.io/name`
  `azure-di-mock` or `azure-read-mock`. DNS comes from the namespace-wide `wega-allow-dns`
  policy of the orchestrator's `network-policies` component. Under the namespace default-deny,
  that is everything the gateway can reach.
- Vision's egress policy `wega-vision-egress`, at `document-vision-service` `5f93223`, reaches
  the gateway on TCP 8080 by the pod label `app.kubernetes.io/name: azure-gateway-api`. In dev
  and perf the peer is a bare `podSelector`, which matches the gateway in vision's own
  namespace, `wega-<env>`, where `ocr-cell` puts it. In staging and prod,
  `overlays/<env>/patch-gateway-cells-egress.yaml` replaces that peer with the same label plus a
  `namespaceSelector` on `kubernetes.io/metadata.name: gateway-<env>`, so vision reaches the
  cells there and no gateway in its own namespace. The orchestrator's `wega-control-egress`
  selects the same label with an empty `namespaceSelector`, which matches it in any namespace.
  The gateway guarantees only the pod label.
- The image's `USER` is `nonroot:nonroot`, uid and gid 65532, given by name. `ocr-cell` sets
  neither `runAsUser` nor `fsGroup`: OpenShift's `restricted-v2` SCC assigns both from the
  namespace's range, and its `fsGroup` makes `/data` writable. The binary is world-readable.
  **This is not verified on a cluster.** A permission error on `/data` at startup points here
  first. The uid matters in two other places:
  - The OPERATIONS sample pins `runAsUser: 65532` and `fsGroup: 65532`. `restricted-v2` rejects
    a pinned uid outside the namespace range unless the ServiceAccount is granted an SCC that
    allows it, such as `nonroot-v2`.
  - Outside OpenShift, nothing assigns a uid. The kubelet then refuses `runAsNonRoot: true`,
    because the image's user is a name, not a number. Such a cluster needs `runAsUser: 65532`
    and `fsGroup: 65532`.

### Build facts

| Fact | Value | Source of truth |
|---|---|---|
| Image name | `azure-gateway-api`, set with `IMAGE ?=` so it can be overridden | `Makefile:6` |
| Recipe | `make image`, which runs `docker build --platform linux/amd64` with the build args `VERSION`, `COMMIT` and `BUILT` | `Makefile:44-48` |
| Default tags | `:$(VERSION)` and `:latest`. `VERSION` defaults to `git describe --tags --always --dirty`. The repository has no tags, so that is the short SHA, with `-dirty` on an unclean tree | `Makefile:3` |
| Platform | `linux/amd64` only. The Dockerfile pins it as well as the Makefile | `Dockerfile:19,36` |
| Build stage | `golang:1.26-bookworm`, `CGO_ENABLED=0`, `-trimpath` | `Dockerfile:5,21-27` |
| In-build test | `go test ./...` runs inside the build stage, without `-race`. A failing test fails the image build | `Dockerfile:29` |
| Runtime base | `gcr.io/distroless/static-debian12:nonroot`, with no shell and no curl | `Dockerfile:36` |
| User | `nonroot:nonroot`, uid and gid 65532, given by name | `Dockerfile:48` |
| Port | `8080`/TCP. One listener serves both Azure surfaces, `/_gw/*`, `/status` and `/ready` | `Dockerfile:44,47` |
| Entrypoint | `/usr/local/bin/gateway`, with default command `serve`; the other commands are `probe` and `version` | `Dockerfile:50-51` |
| Volume | `VOLUME /data` | `Dockerfile:42` |
| Image env | `DATA_DIR=/data`, `GATEWAY_ADDR=:8080`, `LOG_FORMAT=json` | `Dockerfile:43-45` |
| Required env | `DI_UPSTREAM_URL` and `READ_UPSTREAM_URL`, each `http` or `https` with a host. With an invalid configuration, startup exits and lists every problem | `internal/config/config.go:198-199,267-288` |
| Other env | Everything else, all optional with defaults; see the [README](../README.md#configuration) | `internal/config/config.go:133-196` |
| Secret | `azure-gateway-api`, with keys `DI_UPSTREAM_API_KEY` and `READ_UPSTREAM_API_KEY`, read from the environment only | `internal/config/config.go:155,160` |
| Probes | Liveness `/_gw/live`, readiness `/_gw/ready`, startup `/_gw/live` | `internal/admin/admin.go:48-61` |
| Metrics | `GET /_gw/metrics`, in Prometheus text format, unauthenticated | `internal/admin/admin.go:56` |
| Make targets | `help`, `build`, `test`, `test-short`, `cover`, `lint`, `check`, `image`, `probe`, `probe-go`, `run`, `sdk-test`, `clean`. There is no push target and no manifest target | `Makefile` |
| Pre-build gate | `make check`: gofmt, `go vet`, staticcheck when installed, and `go test -race ./...` | `Makefile:31-42` |
| Toolchain | Go 1.26 (`go 1.26.0`), with one direct dependency: `modernc.org/sqlite` v1.58.0, which is pure Go | `go.mod` |
| Image size | About 5.7 MB | `make image`, 2026-09-09 |
| Manifests | None here. For Wega, the orchestrator's `deploy/openshift/components/ocr-cell/` | orchestrator `44016ec` |
| Image in Wega | `gateway.yaml` names the bare image `azure-gateway-api`. Each overlay's `images:` maps it to `image-registry.openshift-image-registry.svc:5000/wega-<env>/azure-gateway-api` by digest, with a zero digest committed as the placeholder | orchestrator `deploy/openshift/overlays/{dev,perf}/kustomization.yaml` |
| Verified at | Code `0f9c794` and orchestrator manifests `44016ec`, on 2026-10-04 | — |

### Build and publish

Run `make check` first, then `make image`. There is no push target: publishing is the deploy
agent's job.

`<registry>` means what it means in the orchestrator's `docs/deploy/SYSTEM-HANDOFF.md` §5 and
`docs/deploy/system.yaml`: the system parameter `P-REGISTRY`, which is the registry host **plus**
the namespace. Its default is `image-registry.openshift-image-registry.svc:5000/wega-<env>`, so
the image is always `<registry>/azure-gateway-api`. Never write `wega-<env>` after `<registry>`:
that doubles the namespace (`…:5000/wega-dev/wega-dev/azure-gateway-api`), which is not where
the overlays pull from. For Wega, tag with semver plus the git SHA:

```sh
make image IMAGE=<registry>/azure-gateway-api VERSION=<semver>
docker tag <registry>/azure-gateway-api:<semver> \
  <registry>/azure-gateway-api:$(git rev-parse --short HEAD)
```

With the default registry in dev, that is
`image-registry.openshift-image-registry.svc:5000/wega-dev/azure-gateway-api:<semver>`, plus
`:<git-sha>` and `:latest`. The default is the in-cluster pull address. A push from outside the
cluster goes through the registry's exposed route: the route host replaces
`image-registry.openshift-image-registry.svc:5000`, and `/wega-<env>/azure-gateway-api` stays.

`VERSION` also stamps the binary, so `/_gw/version` reports it. `COMMIT` defaults to the short
SHA, and `make image` always adds `:latest` as well. Wega deploys by digest only, so no committed
manifest carries a real one. The deploy agent captures the digest at push. `skopeo` must use the
address the push used, so from outside the cluster it names the route host. With the default,
integrated registry, `oc` asks the OpenShift API for the same value:

```sh
skopeo inspect --format '{{.Digest}}' docker://<registry>/azure-gateway-api:<git-sha>
oc -n wega-<env> get istag azure-gateway-api:<git-sha> -o jsonpath='{.image.metadata.name}'
```

It injects the digest in the orchestrator's untracked release overlay for dev or perf, never in a
committed overlay. The committed overlay has already renamed `azure-gateway-api` to its full
in-cluster name with the all-zero placeholder digest, so the release overlay's key must be that
full name. A bare `azure-gateway-api=` key matches nothing, and the render keeps `@sha256:0000…`:

```sh
FULL=image-registry.openshift-image-registry.svc:5000/wega-<env>/azure-gateway-api
kustomize edit set image "$FULL=<registry>/azure-gateway-api@sha256:<digest>"
```

The orchestrator's `docs/deploy/release-example/` writes the same entry as an `images:` item
(`name:` the full name, `digest:`), and its `make manifests-release RELEASE=<release dir>` must
then pass with no warning.

### Verify after deploy

The gateway image has no shell and no curl, though
`oc exec deploy/azure-gateway-api -- /usr/local/bin/gateway version` works. Run the HTTP checks
from a Wega pod that is allowed to reach the gateway and has Python, such as `wega-orch-control`:

```sh
oc -n wega-<env> exec deploy/wega-orch-control -- python -c \
  'import sys,urllib.request; print(urllib.request.urlopen(sys.argv[1]).read().decode())' \
  http://azure-gateway-api:8080/_gw/health
```

1. In dev and perf, wait for the mocks before the gateway, one rollout at a time:

   ```sh
   oc -n wega-<env> rollout status deploy/azure-di-mock --timeout=10m
   oc -n wega-<env> rollout status deploy/azure-read-mock --timeout=10m
   oc -n wega-<env> rollout status deploy/azure-gateway-api --timeout=10m
   ```

   With `Recreate`, the old gateway pod stops before the new one starts, so a short gap is
   expected.
2. `/_gw/ready` answers `{"status":"ready"}`.
3. `/_gw/health` shows `status: ok`, and `ok: true` on both upstreams. In dev and perf:
   - `document-intelligence` shows `syncAnalyze: unavailable`.
   - `computer-vision-read` shows `syncAnalyze: available`, which is always reported for Read
     and never probed.
   - `warnings` holds at most the two empty-key lines.
4. `/_gw/config` shows `diMaxInflight`, `readMaxInflight` and `queueDepth` equal to the `gw-01`
   fleet entry. In dev and perf it also shows `diSyncAnalyze: off`, `diUpstreamUrl`
   `http://azure-di-mock:5000` and `readUpstreamUrl` `http://azure-read-mock:5000`.
5. Send one document through Wega end to end, as the system handoff describes. Then
   `/_gw/jobs?limit=5` shows `status: succeeded`. Against the DES mocks, DI jobs show
   `upstreamMode: async-fallback`, because of `DI_SYNC_ANALYZE=off`. Read jobs show `sync`,
   because the Read mock serves `syncAnalyze`.

### Rollback

Re-deploy the previous image digest. `Recreate` keeps the gateway at one pod, and the restart
loses no accepted operation: ids and results live on the PVC, and jobs that were in flight re-run
on boot. The job store has no migration framework (see the
[coupling map](#coupling-map--if-you-change-x-you-must-also-change-y)). Before rolling across
commits that change `schema` or `jobColumns` in `internal/store/db.go`, read what changed: a
column that a newer build adds is not applied to an existing volume.

### Known gaps for Wega, as of 2026-10-04

1. **It has never run against the DES mocks in a cluster.** DES `8c01dc9`, on DES `main`, adds
   an opt-in test that drives both mocks through a gateway binary built from `0f9c794`, outside
   a cluster. The mocks on DES `main` (`bf57bbb`) serve `GET /status`, Layout `:syncAnalyze` and
   Read `syncAnalyze`, so an image built from it can pass the `infra-mocks` probes.
2. **The staging and prod fleets give each cell `di.queue_depth: 4` but `read.queue_depth: 2`**
   (`max_inflight` 8 and 4), at orchestrator `44016ec` and identically in vision. The gateway
   has one `QUEUE_DEPTH`, so no cell can match both. The observer drift-checks `queueDepth`
   against `di.queue_depth` only, so a cell set from the fleet takes `QUEUE_DEPTH=4`; its Read
   admission limit is then 8, where the fleet declares 6. The alternative is to align the file
   in both repositories. The cells themselves are open input `I-GATEWAY-CELLS`. dev and perf are
   consistent: 4/2 on both surfaces, and `QUEUE_DEPTH=2`. The fix is not in this repository.

3. **The `clientRequestId` lookup (GW-CR-1) is not implemented.** `/_gw/jobs` ignores a
   `clientRequestId` parameter, and `/_gw/version` advertises no `clientRequestIdLookup`. Vision
   therefore treats the lookup as unsupported, which costs at most one extra resubmit after an
   ambiguous submit.
4. **The OpenShift SCC behaviour on `/data` is unverified**; see the security-context item above.
5. **The base images are pinned by tag, not by digest**: `golang:1.26-bookworm` and
   `gcr.io/distroless/static-debian12:nonroot`. Wega pins the built gateway image by digest
   regardless.
6. **`make image` last ran on 2026-09-09.** Docker was unavailable for the 2026-10-04
   verification.
7. Items 1 and 2 of [What is NOT done](#what-is-not-done) still stand: there has been no
   end-to-end run against real containers, and `make sdk-test` has never been executed.

### Build and deploy change log

| Date | Commit | Change |
|---|---|---|
| 2026-10-04 | `ec4da83` | The `OPERATIONS.md` sample labels its pods and selectors `app.kubernetes.io/name: azure-gateway-api`, the label Wega's NetworkPolicies select. |
| 2026-10-04 | `656fa6b` | This section: how Wega deploys the gateway, the configuration rules, the health semantics, the build facts and the update rule. |
| 2026-10-04 | `7743bc3` | This section checked against orchestrator `c8ce603`: the upstream port is filled in as 5000. The ConfigMap and Secret wiring, the PVC, probes, resources, security context, network policies, the mocks' `/status` and `syncAnalyze`, and the staging and prod cells now match `ocr-cell` and `infra-mocks`. The fleet mismatch with vision is recorded. |
| 2026-10-04 | `e688dbb` | This section re-checked against the `main` branches of the orchestrator (`44016ec`), vision (`5f93223`) and DES (`bf57bbb`). The fleet gap is closed: vision's four fleet files are byte-identical to the orchestrator's from vision `4e9d40e`, so vision's perf ceiling is 7 against `di` 4/2, and its staging and prod egress reaches `gateway-<env>`. The DES mocks that serve `/status` and `syncAnalyze` are on DES `main`, so `527c6c0` and branch `wega/azure-mocks` are no longer cited. A staging or prod cell also needs ingress from `wega-<env>` and DNS egress. The staging and prod `queue_depth` mismatch is recorded. The code, the image and the values `ocr-cell` sets are unchanged. |
| 2026-10-04 | The first commit after `e688dbb` | [Build and publish](#build-and-publish): `<registry>` now means the registry host plus the namespace (`P-REGISTRY`), as in the orchestrator's `SYSTEM-HANDOFF.md` §5 at `6cdaec6`, so the image is `<registry>/azure-gateway-api`. The old recipe wrote `wega-<env>` after `<registry>`, which doubled the namespace with the default registry this file named. The digest capture and the full-name release-overlay key are added; `kustomize build` over orchestrator `6cdaec6`'s dev overlay renders the given digest with the full-name key and the zero digest with a bare key. Between `44016ec` and `6cdaec6`, `ocr-cell`, the overlays and their fleet files are unchanged, and `infra-mocks` changed only `oracle-init.sh`; the DES mocks are unchanged between `bf57bbb` and `fec8bd7`. The code, the image and the values `ocr-cell` sets are unchanged. |

### Read next

- [`OPERATIONS.md` §2](OPERATIONS.md#2-deployment): why the volume must be block-backed, why
  there is one replica, the probe advice, and a full sample manifest.
- The [README configuration tables](../README.md#configuration): every variable and its default.
- [`TESTING.md`](TESTING.md) and [`CONTAINER-BEHAVIOUR.md`](CONTAINER-BEHAVIOUR.md).
- `document-embedding-orchestrator/docs/deploy/SYSTEM-HANDOFF.md`: the whole system.
- `document-embedding-orchestrator/deploy/openshift/components/ocr-cell/` and
  `components/infra-mocks/`: the manifests this section describes.

---

## Traps — things that look wrong and are not

Do not "fix" these without reading the reason.

| Looks wrong | Why it is that way |
|---|---|
| `TRUST_FORWARDED_HEADERS` defaults **off** | With it on and no allowlist, a caller sets `X-Forwarded-Host` and the gateway mints an `Operation-Location` pointing anywhere. Behind an OpenShift Route the request's own `Host` is already correct |
| The unrouted 404 has **no body** | Both containers answer that way. Every *routed* endpoint still carries a body, which is what the Java-NPE rule requires |
| DI's unknown-result-id error is **flat** while every other DI error is wrapped | The container does exactly this. It is not a bug in `azerr` |
| `jsonx` hand-rolls a JSON scanner instead of using `encoding/json` | `Decoder.Token()` materialises every scalar. Measured at 309 MB of heap for a 107 MB `content` string — enough to OOM the pod with four workers |
| The sync attempt has its own short timeout, separate from the upstream timeout | A build can *declare* `:syncAnalyze` in its swagger and never answer it. Without the separate bound, every job burned the full 15-minute timeout before falling back |
| Boot recovery ignores leases; the periodic pass respects them | A freshly booted process holds no leases, so any `running` row is abandoned. Filtering on lease expiry at boot skipped exactly the jobs that needed rescuing |
| Request contexts do not derive from the signal context | They used to, and `Shutdown` returned in ~1.5 ms while in-flight synchronous calls died with a 500. The grace period existed and nothing used it |
| `Store.Delete` removes the row *before* the blobs | Blob-first meant a partial failure left a row pointing at a missing result, and the poll on it produced a body with no `status` — the shape that crashes Java clients |
| Admission slots are held from admission to *completion* | That is what bounds upstream concurrency. Releasing at submit would let the container see unbounded load |
| One replica only | Operation ids are pod-local by construction. A second replica cannot serve the first's polls |

---

## Coupling map — if you change X, you must also change Y

- **`Operation-Location` format** → `internal/surface/di.go` *and* `read.go`, the conformance tests
  that assert the shape, and `test/sdk/sdk_test.py`. Four SDKs parse this header with hard-coded
  regexes; read [`api-surface.md`](api-surface.md) §4 first.
- **Error shapes** → `internal/azerr`, the `Compat` switch, `internal/mockazure` (the fakes must
  keep matching real containers), and the conformance tests.
- **The job schema** → `internal/store/db.go` (`schema`, `jobColumns`, `scanJob`, `Create`). All
  four must move together; there is no migration framework, and the schema uses
  `CREATE TABLE IF NOT EXISTS`, so **adding a column will not apply to an existing volume**.
- **Adding a config knob** → `internal/config/config.go` (field, parse, validate, `Redacted`), the
  README table, and `OPERATIONS.md`.
- **Anything touching admission** → check every path that calls `Admit`, `AdmitUpstream` or
  `AdmitMetadata` releases its slot on *every* return path, including panics.

---

## How to work on this

1. **Read the design spec's review section first.** 32 defects were found and fixed; the table says
   what each one was. Re-introducing one is the most likely failure mode.
2. **Change the fakes when the containers surprise you.** `internal/mockazure` is meant to model
   real behaviour including failure modes. If a probe finds something new, the fake gets it first,
   then the code, then a regression test.
3. **Every fix gets a regression test that fails before it.** All 32 review fixes have one.
4. **Verify before claiming done:**
   ```bash
   gofmt -l . && go vet ./... && go test -race ./... && make image
   ```
   The image build runs the suite inside it and has caught two bugs the local run missed — different
   scheduling exposes different races. Do not skip it.
5. **Re-probe after any container upgrade.** `./scripts/probe-containers.sh` — the verdict block
   tells you which knobs to change.

### Conventions

- Commits are authored `NilenduGanguli <nilendu.ganguli@gmail.com>`, conventional-commit prefixes,
  **no co-author trailers**.
- Comments explain *why*, and name the specific client or observation that forces the behaviour.
- `internal/` only; one external dependency (`modernc.org/sqlite`, pure Go, keeps the image static).

### A warning about agent-assisted work

Subagents writing scratch files into the working tree got swept into commits by `git add -A`, and
the history had to be rewound. **Stage explicit paths when agents can write to the repo.**

---

## Open questions

| Question | Why it matters | How to settle it |
|---|---|---|
| Is `:syncAnalyze` broken on that build, or is the container starved? | Decides whether the fast path is ever usable | Give the Layout container its 8 cores / 16–24 GB and re-probe |
| Do other environments' containers agree on error shapes? | The gateway reproduces one deployment's behaviour | Run the probe per environment |
| Does `analyzeResults/{id}/pdf` ever return a real PDF? | It answered `200 application/json` on the probed build | Submit with `output=pdf` and re-check |
| Do any clients actually use `/formrecognizer`? | Determines whether that surface needs its own tests | Ask, or log by prefix |
| Is 90s for a blank PNG normal for this deployment? | Sets every timeout default | Compare against a resourced container |

---

## How this was built

Useful context for judging how much to trust any given piece.

1. **Research** — 21 agents mapped both container contracts with adversarial verification. Output:
   [`api-surface.md`](api-surface.md), 94k with citations. It overturned my initial conclusion that
   DI had no synchronous route.
2. **Design** — approved before implementation, in the
   [design spec](superpowers/specs/2026-09-08-azure-gateway-api-design.md).
3. **Build** — Go, one external dependency.
4. **Adversarial review** — 147 agents, six lenses, three verifiers per finding. 32 confirmed
   defects out of 47 candidates, all fixed. Two of the fixes were themselves wrong and were caught
   by the in-image test run.
5. **Probe** — a real deployment contradicted the documentation, and the gateway changed twice as a
   result.

The lesson worth carrying forward: **every stage found something the previous stage got wrong.**
Assume this document is also incomplete, and probe rather than infer.
