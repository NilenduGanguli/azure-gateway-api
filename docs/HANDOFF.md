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
PVC, and calls the containers only synchronously — retrying from the stored input, and splitting
large PDFs across the autoscaled pods — so clients change nothing and operation ids survive pod
churn.

---

## Current state

> **The figures in this section predate branch `feat/sync-only-upstream`.** That branch adds the
> sync-only upstream, splitting, migrations and about a dozen packages; its final gate (plan T17:
> `go test -race ./...`, `make cover`, `scripts/e2e-local.sh`, `make image`) re-measures every row
> and replaces this table. Until then, read the rows as the state of `main` at `0f9c794`.

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

- Both client surfaces, wire-compatible, including the legacy `/formrecognizer` family; either
  surface can be served alone.
- **Sync-only upstream**: every analysis is one `:syncAnalyze` attempt through HAProxy on a fresh
  connection, classified into an outcome and retried from the stored input (5 attempts per whole
  document, backoff with jitter, `Retry-After` honoured). No container operation id is ever stored,
  followed or relayed; the async fallback, the capability latch and affinity polling are deleted.
- **Large-document splitting**: PDFs over `SPLIT_PAGES` pages are cut into 3–5 page parts,
  analysed in parallel under a fair round-robin scheduler, with per-part timeouts, hedges, halving
  and re-checks against the original, then merged page-exactly in one streaming pass. Any doubt
  sends the document whole.
- The client `:syncAnalyze` route runs through the same engine, so sync clients get retries and
  splitting too.
- The parameter policy on DI analyze routes; `Server: Kestrel`; no `apim-request-id`; the container
  GET relays (`operations`, `/`, `swagger`, `records/usage-logs`).
- Durable job store: SQLite (WAL, `synchronous=FULL`, single writer) plus atomic blob storage,
  with **versioned schema migrations** (backup, atomic apply, refusal of a newer schema).
- Crash recovery, TTL expiry, disk-pressure eviction, orphan and work-folder reclamation.
- Settings down from 32 to 22; removed settings warn instead of failing.
- `gateway probe` with `PROBE_DOCUMENT`, `--record` and `--compare-split`; `cmd/fakeazure` and
  `scripts/e2e-local.sh` for live runs on a laptop.
- Faithful container fakes, including HAProxy's failure modes, and a paged fake for splitting.
- 32 defects found by the original adversarial review, all fixed with regression tests.

---

## What is NOT done

Ranked by how much it should worry you.

**1. It has never run against real containers end to end.** Everything is proven against fakes,
including the merge. This is the single largest unknown.

**2. No golden responses exist from the real containers.** `gateway probe --record` must be run by
the operator, whose network reaches the containers, and the files committed; until then the parity
test replays only a set generated from the paged fake (go-live gate 2).

**3. `--compare-split` has not been run on a real corpus.** Page-level content is merged exactly by
construction, but Azure's document-level judgements across a split point are unmeasured. The gate
is at least 30 representative PDFs, zero page-level differences, and every document-level pattern
either given a merge rule with a regression test or accepted by the user (go-live gate 3).

**4. The parameters `features`, `queryFields` and `outputContentFormat=markdown` are rejected** until
a probe proves the DI sync route honours them. Wega's vision asks for `markdown` and
`ocrHighResolution` on its third attempt; see [Known gaps](#known-gaps-for-wega).

**5. The error-shape decision rests on one probe run against one deployment.** There is no longer
an escape hatch (`ERROR_COMPAT` is removed); the golden recordings per image tag are the check.

**6. `:analyzeBatch`, custom models and classifiers are unimplemented**, deliberately — batch is
Azure-Blob-bound and unusable air-gapped.

**7. No Kubernetes manifests ship in the repo**, by request. [`OPERATIONS.md`](OPERATIONS.md)
contains a working set to copy.

---

## Settings, probe and work folder

The gateway has **22 settings**. The README has the full table with purposes; the defaults are in
`internal/config/config.go`.

| Setting | Default | | Setting | Default |
|---|---|---|---|---|
| `DI_UPSTREAM_URL` | — (optional) | | `PUBLIC_BASE_URL` | — (recommended) |
| `READ_UPSTREAM_URL` | — (optional) | | `RESULT_TTL` | `24h` |
| `DI_UPSTREAM_API_KEY` | — | | `GC_INTERVAL` | `5m` |
| `READ_UPSTREAM_API_KEY` | — | | `DISK_HIGH_WATERMARK` | `0.90` |
| `DI_MAX_INFLIGHT` | `4` (size 16) | | `MAX_REQUEST_BYTES` | 500 MiB |
| `READ_MAX_INFLIGHT` | `4` (size 16) | | `MAX_RESULT_BYTES` | 512 MiB |
| `DI_UPSTREAM_TIMEOUT` | `15m` per attempt | | `UPLOAD_TIMEOUT` | `10m` |
| `READ_SYNC_TIMEOUT` | `10m` per attempt | | `SHUTDOWN_GRACE` | `30s` |
| `SPLIT_PAGES` | `5` (`0` = off) | | `DATA_DIR` | `/data` |
| `QUEUE_DEPTH` | `0` (size for bursts) | | `GATEWAY_ADDR` | `:8080` |
| `LOG_LEVEL` | `info` | | `LOG_FORMAT` | `json` |

- At least one upstream URL must be set; an empty one disables that surface, whose own settings are
  then neither validated nor warned about.
- A number or duration that does not parse is fatal, never silently defaulted.
- **Removed settings warn, they do not fail.** `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT`,
  `DI_BLIND_POLL_BUDGET`, `READ_BLIND_POLL_BUDGET`, `ARTIFACT_FETCH_TIMEOUT`, `ERROR_COMPAT`,
  `TRUST_FORWARDED_HEADERS`, `TRUSTED_FORWARDED_HOSTS`, `ALLOW_NETWORK_FS`, `POLL_RETRY_AFTER` and
  `BUSY_RETRY_AFTER` are recognised, ignored, and named in a `<NAME> is no longer used` warning in
  the startup log and `/_gw/health` `warnings[]`. Wega's current `gateway.env` sets two of them
  (`DI_SYNC_ANALYZE`, `ERROR_COMPAT`) and keeps starting.

**Probe usage.** Run it wherever the Azure Routes are reachable — `oc exec deploy/azure-gateway-api
-- /usr/local/bin/gateway probe` uses the pod's own settings:

| Command | Purpose |
|---|---|
| `PROBE_DOCUMENT=<pdf> gateway probe` | The readiness verdict per configured surface: `:syncAnalyze` **200** = ready; timeout, 404, 405, 501 or `UnhandledEndpointException` = this build cannot run behind the gateway. Also reports each unproven parameter as `honoured` or `ignored`. `PROBE_TIMEOUT` (default `10m`) bounds each call. Exits `0` only when every configured surface is ready, `1` otherwise, `2` on a usage error |
| `gateway probe --record <dir>` | Writes the golden responses for the parity test (`<dir>/<surface>/<case>.json`, to commit as `test/conformance/testdata/golden/<surface>/<image-tag>/`) and `<dir>/<surface>-comparisons.json` (sync-equals-async and per-parameter verdicts) |
| `gateway probe --compare-split <dir>` | Whole versus split for every PDF in `<dir>`; writes `compare-report.json`; exits non-zero on any page-level difference |

**The work folder is on the PVC**, at `${DATA_DIR}/work/`: one folder per running job (`<id>/`:
part PDFs, part results, the merged result) and per client `:syncAnalyze` in flight (`sync-<id>/`).
It lives next to `blobs/` so nothing is copied across filesystems and nothing is written outside
`DATA_DIR` (the root filesystem stays read-only). Folders are deleted when their job ends; the
sweeper removes any a crash left behind once they are an hour old. Size the PVC for it: a running
split job holds roughly twice its input plus its result there, briefly.

**The schema backup is on the PVC too**: `${DATA_DIR}/gateway.db.v<N>.bak`, written before each
migration. See [Rollback](#rollback).

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

> **This release changes several facts below** — settings, the calls made to the upstreams,
> health fields, the schema and the rollback. The rows marked for this release were updated on
> 2026-10-11 against branch `feat/sync-only-upstream`; the orchestrator, vision and DES facts were
> not re-checked then. The Wega side of the change is listed under
> [Known gaps](#known-gaps-for-wega).

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
| Real upstreams | The Azure Document Intelligence Layout and Read deployments, autoscaled to at most 8 pods each, reached through their HAProxy Routes with the annotations of [OPERATIONS §3](OPERATIONS.md#3-the-haproxy-route-in-front-of-the-azure-pods). Probe them before the first deploy (`gateway probe` with `PROBE_DOCUMENT`; see [OPERATIONS §1](OPERATIONS.md#1-before-you-deploy-probe-the-containers)): `:syncAnalyze` must answer 200, because there is no fallback. |
| Staging and prod | No `ocr-cell`. The orchestrator's committed staging and prod fleets list several cells, `gw-NN` at `http://azure-gateway-api-NN.gateway-<env>.svc.cluster.local:8080`. These are real gateways against real Azure, deployed outside the Wega overlays: open input `I-GATEWAY-CELLS` in the system handoff. Vision's staging and prod egress reaches them; see the network items [below](#probes-network-and-security-context-in-ocr-cell). `ocr-cell` is the template for one cell, with three changes. The Secret below must exist. The ingress policy admits callers from the cell's own namespace only, so it must admit `wega.io/deployment` `vision` and `orch-control` from namespace `wega-<env>`. The egress policy admits only the two mocks, so it must name the real upstreams instead, and must allow DNS unless a policy in `gateway-<env>` already does, as `wega-allow-dns` does in `wega-<env>`. Each of these fleet entries gives `di` 8/4 and `read` 4/2; see [Known gaps](#known-gaps-for-wega). |
| Callers | `wega-vision`, whose pods are labelled `wega.io/deployment: vision`, submits and polls on both surfaces, and reads `/_gw/health`, `/_gw/jobs/{id}` and `/_gw/version`. The orchestrator observer runs in `wega-orch-control`, labelled `wega.io/deployment: orch-control`, and reads `/_gw/health` and `/_gw/config`. User-workload monitoring scrapes `/_gw/metrics` through PodMonitor `azure-gateway-api` every 30 s. The ingress policy admits nobody else. |

### Configuration Wega sets

The values live in ConfigMap `azure-gateway-api`. `ocr-cell` generates it from `gateway.env`, and
the container reads it with `envFrom`. Each overlay's `patch-gateway.yaml` adds
`PUBLIC_BASE_URL`. This table gives the value Wega sets in dev and perf (orchestrator `44016ec`),
and the rule each value must satisfy against this release. The full catalogue is in
[OPERATIONS §4](OPERATIONS.md#4-configuration).

| Variable | dev and perf (`ocr-cell`) | Against real containers | Why |
|---|---|---|---|
| `DI_UPSTREAM_URL` | `http://azure-di-mock:5000` | The HAProxy Route in front of the Layout pods | Optional since this release, but Wega uses both surfaces. 5000 is the `infra-mocks` Service port. |
| `READ_UPSTREAM_URL` | `http://azure-read-mock:5000` | The Route in front of the Read pods | The hostname must not contain `vision` ([why](OPERATIONS.md#the-vision-substring-bug-in-the-read-container)). |
| `DI_SYNC_ANALYZE` | `off` | **Remove** | Removed setting: the gateway starts and warns `DI_SYNC_ANALYZE is no longer used`. Every DI job now calls `:syncAnalyze`, so the Layout mock **must** serve it; the DES Layout mock on DES `main` (`bf57bbb`) does, since `d1b8ec4`. |
| `ERROR_COMPAT` | `observed` | **Remove** | Removed setting; warns. The observed shapes are now the only ones. |
| `DI_MAX_INFLIGHT` | `4`, which is `gw-01` `di.max_inflight` | max Azure pods × per-pod concurrency, e.g. `16` | Now bounds every Azure call, parts and hedges included. Every 300 s the orchestrator observer reads `/_gw/config`; when `diMaxInflight` differs from the fleet value, it raises `WegaGatewayFleetDrift`. Change the fleet file and the ConfigMap together. |
| `QUEUE_DEPTH` | `2`, which is `gw-01` `di.queue_depth` | sized for bursts, e.g. `200` | Also drift-checked, as `queueDepth`. One `QUEUE_DEPTH` for both surfaces. |
| `READ_MAX_INFLIGHT` | `4`, which is `gw-01` `read.max_inflight` | as `DI_MAX_INFLIGHT`; `0` in the fleet for a DI-only gateway | The observer does not drift-check it. A disabled surface reports `0` on `/_gw/config`. |
| `SPLIT_PAGES` | unset, so `5` | `5` | New. `0` turns splitting off. Vision's own split-and-merge stays as it is; a vision slice over 5 pages is split again by the gateway, and both merges are page-exact. |
| `PUBLIC_BASE_URL` | the fleet `base_url`, from `overlays/<env>/patch-gateway.yaml` | Same rule | Scheme and authority only. Vision rebuilds every poll URL from the fleet `base_url`, so this is not load-bearing for Wega. |
| `DATA_DIR` | `/data` | Same | Must be the PVC mount: the database, its migration backup, the blobs and the work folders. |
| `SHUTDOWN_GRACE` | `30s`, under `terminationGracePeriodSeconds: 60` | Less than `terminationGracePeriodSeconds` | The default is `30s`. |

The orchestrator's `tests/parity/test_deploy_components.py` fails when `DI_MAX_INFLIGHT`,
`READ_MAX_INFLIGHT`, `QUEUE_DEPTH` or `PUBLIC_BASE_URL` disagrees with the `gw-01` entry of that
overlay's fleet file. The fleet file is the source: derive the values from it, not from this
table.

`gateway.env` also sets `GATEWAY_ADDR=:8080`, `DI_UPSTREAM_TIMEOUT=15m`,
`READ_SYNC_TIMEOUT=10m`, `RESULT_TTL=24h`, `GC_INTERVAL=5m`, `DISK_HIGH_WATERMARK=0.90`,
`LOG_LEVEL=info` and `LOG_FORMAT=json`. Each equals the gateway's own default. Note that
`DI_UPSTREAM_TIMEOUT` and `READ_SYNC_TIMEOUT` now bound **one attempt** at a whole document, not a
job.

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

**How `upstreams[].ok` is decided.** `/_gw/health` lists one upstream per configured surface:
`document-intelligence` for Layout and `computer-vision-read` for Read. On every request, for each upstream, the gateway
sends `GET <upstream URL>/status` with that upstream's API key and a 5 s timeout; the whole
handler has 6 s. `ok` is true exactly when that call returns HTTP `200`. The body only feeds
`detail`, which copies `apiStatus` and `apiStatusMessage` when they are present. Neither field
affects `ok`, so `apiStatus: Invalid` at HTTP `200` still counts as ok. The logic is `Client.Health` in [`internal/upstream`](../internal/upstream). There is no
`syncAnalyze` field any more.

**Why this matters to Wega.** An upstream reads `ok: false` when its `/status` answers 404, times
out, or cannot be reached. After two consecutive `ok: false` reads, vision opens that surface's
breaker and stops sending it work, and the observer drops the surface from the OCR window. **So
every upstream this gateway points at, mocks included, must answer `GET /status` with HTTP 200
within 5 s.** The recommended body is `{"apiStatus":"Valid"}`. The DES mocks on DES `main`
(`bf57bbb`) serve that, with an `apiStatusMessage` beside it. The route arrived in `d1b8ec4` and
reached `main` with merge `d28de8d`; a mock image built from an older `main` serves only
`/health`. `infra-mocks` makes `/status` the startup and readiness probe of both mocks, so a mock
image without it never becomes Ready.

The same response carries `capacity.di` and `capacity.read` for the configured surfaces, each with
`inFlight`, `limit`, `queued`, `upstreamInUse`, `upstreamWaiting` and `upstreamLimit`. `limit` is
`MAX_INFLIGHT + QUEUE_DEPTH`; the observer uses it as that surface's admission slots. The
`upstream*` keys describe the scheduler: Azure calls in progress, calls waiting for a slot, and
`MAX_INFLIGHT`.

### What an upstream must serve

These are the calls the gateway makes, so this is the contract the DES mocks must meet in dev and
perf. Since this release the gateway makes **no** `:analyze` call and **no** poll.

| Call | When | Required answer |
|---|---|---|
| `GET /status` | On every `/_gw/health` request and every root `/status` request, for each configured surface | `200`, within 5 s |
| `POST /documentintelligence/documentModels/{modelId}:syncAnalyze?api-version=…` (or `/formrecognizer/…`) | Every DI job and client `:syncAnalyze`: once for a whole document, or once per part, hedge and re-check | `200` with the operation envelope or a bare `AnalyzeResult`. A part is a standalone PDF of 3–5 pages; a re-check is the original with `pages=`. A bodyless `404`, `405`, `501` or `500 UnhandledEndpointException` fails every job as a deployment fault |
| `POST /vision/v3.2/read/syncAnalyze` | Every Read job, likewise | `200` with the result |
| `GET {fam}/info`, `/documentModels`, `/documentModels/{id}`, `{fam}/operations`, `/operations/{id}`, `/`, `/swagger…`, `/records/usage-logs…` | Only when a client calls the same route on the gateway | Anything: relayed as the container answers |

A `503`, `502`, `504`, `500`, `408`, `429`, a `202`, a reset or a timeout is retried, so a mock
that fails occasionally costs time, not correctness. Mocks may run more than one replica now: the
gateway never polls, so nothing needs to land on the replica that answered first.

### Probes, network and security context in `ocr-cell`

- Probes, all on port `http` (8080):
  - startup on `/_gw/live`, every 2 s, failure threshold 30, so the gateway has 60 s to start —
    including the schema migration and boot recovery, which run before the listener opens (see
    [Known gaps](#known-gaps-for-wega), item 7);
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
| Required env | At least one of `DI_UPSTREAM_URL` and `READ_UPSTREAM_URL`, each `http` or `https` with a host. With an invalid configuration, startup exits and lists every problem | `internal/config/config.go` (`Load`) |
| Other env | Everything else, all optional with defaults — 22 settings in all; see the [README](../README.md#configuration). Removed settings warn | `internal/config/config.go` |
| Secret | `azure-gateway-api`, with keys `DI_UPSTREAM_API_KEY` and `READ_UPSTREAM_API_KEY`, read from the environment only | `internal/config/config.go` |
| Probes | Liveness `/_gw/live`, readiness `/_gw/ready`, startup `/_gw/live` | `internal/admin/admin.go:48-61` |
| Metrics | `GET /_gw/metrics`, in Prometheus text format, unauthenticated | `internal/admin/admin.go:56` |
| Make targets | `help`, `build`, `test`, `test-short`, `cover`, `lint`, `check`, `image`, `probe`, `probe-go`, `run`, `sdk-test`, `clean`. There is no push target and no manifest target | `Makefile` |
| Pre-build gate | `make check`: gofmt, `go vet`, staticcheck when installed, and `go test -race ./...` | `Makefile:31-42` |
| Toolchain | Go 1.26 (`go 1.26.0`), with three direct dependencies, all pure Go: `modernc.org/sqlite` v1.58.0, `github.com/pdfcpu/pdfcpu` v0.16.1 and `github.com/rivo/uniseg` v0.4.7 | `go.mod` |
| Image size | About 5.7 MB before pdfcpu and uniseg were added; re-measure at the branch's final gate | `make image`, 2026-09-09 |
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
2. The gateway log has `job store schema migrated` (first start of this release, `from` 1, `to` 2,
   with the `backup` path) or `job store schema is current`.
3. `/_gw/ready` answers `{"status":"ready"}`.
4. `/_gw/health` shows `status: ok`, and `ok: true` on each configured upstream. There is no
   `syncAnalyze` field any more. `warnings` holds at most the two empty-key lines and one
   `… is no longer used` line per removed setting the ConfigMap still sets.
5. `/_gw/config` shows `diMaxInflight`, `readMaxInflight` and `queueDepth` equal to the `gw-01`
   fleet entry, `splitPages`, and `diEnabled`/`readEnabled` both `true`.
6. Send one document through Wega end to end, as the system handoff describes. Then
   `/_gw/jobs?limit=5` shows `status: succeeded`, `upstreamAttempts` of at least 1, and
   `upstreamParts` of 1 for a document of 5 pages or fewer, more for a larger PDF.

### Rollback

Re-deploy the previous image digest — but read this first, because **this release migrates the
job store** (schema version 1 → 2) on its first start, after copying the database to
`/data/gateway.db.v1.bak`.

- **Rolling back to `0f9c794` or any build before versioning:** that image does not check the
  schema version. On the migrated database it would start and then fail every query naming a
  dropped column. **Restore the backup first**: scale the gateway to 0; in a debug pod that mounts
  the PVC, delete `gateway.db-wal` and `gateway.db-shm`, then copy `gateway.db.v1.bak` over
  `gateway.db`; then deploy the old digest and scale back to 1. The full procedure is
  [OPERATIONS §9](OPERATIONS.md#9-schema-migrations-and-rollback).
- **Rolling back to a later build that knows migrations:** it refuses to start on a newer schema
  and names the backup; restore it the same way, or roll forward.
- **What a restore costs:** jobs accepted after the upgrade are not in the backup, so their ids
  404. Prefer rolling forward when the reason allows.

Ids and results otherwise live on the PVC, and `Recreate` keeps the gateway at one pod.

### Known gaps for Wega

As of 2026-10-11, against orchestrator `44016ec`, vision `5f93223` and DES `bf57bbb`:

1. **The Wega follow-up for this release is not done** (a separate spec, sync-only design
   "Rollout" item 3):
   - `ocr-cell`'s `gateway.env` still sets `DI_SYNC_ANALYZE=off` and `ERROR_COMPAT=observed`. Both
     only warn now; remove them.
   - The fleet files still give `di.max_inflight: 4`, `queue_depth: 2`. Re-derive them as max
     Azure pods × per-pod concurrency (16 for 8 × 2) and a burst-sized queue depth, with
     `read.max_inflight: 0` for a DI-only gateway — in both repositories, since vision carries the
     same file.
   - Vision's KEDA ceilings are derived from the old slot counts and need re-deriving.
   - **Vision's third attempt asks for `outputContentFormat=markdown` and `features=ocrHighResolution`.**
     The gateway now rejects both with `400 InvalidArgument` until a probe proves the sync route
     honours them, so that attempt fails fast instead of silently getting a result without them.
     Vision must stop asking, or the parameters must be proven and allowlisted first.
   - Vision's own split-and-merge is **kept as is**; the gateway's splitting composes with it.
   - The orchestrator's system handoff needs the new settings, the migration and the rollback.
2. **It has never run against the DES mocks in a cluster.** DES `8c01dc9` adds an opt-in test that
   drives both mocks through a gateway binary built from `0f9c794`, outside a cluster; it predates
   this release. The mocks serve `GET /status`, Layout `:syncAnalyze` and Read `syncAnalyze`, which
   is now everything the gateway calls on them.
3. **The staging and prod fleets give each cell `di.queue_depth: 4` but `read.queue_depth: 2`**.
   The gateway has one `QUEUE_DEPTH`, so no cell can match both; the observer drift-checks
   `queueDepth` against `di.queue_depth` only. The fix is not in this repository.
4. **The `clientRequestId` lookup (GW-CR-1) is not implemented.** `/_gw/jobs` ignores a
   `clientRequestId` parameter. With `apim-request-id` gone, `x-ms-client-request-id` is the only
   correlation handle a client has, so this matters a little more than it did.
5. **The OpenShift SCC behaviour on `/data` is unverified**; see the security-context item above.
6. **The base images are pinned by tag, not by digest.** Wega pins the built image by digest.
7. **The startup probe in `ocr-cell` allows 60 s.** The first start of this release migrates the
   database before the listener opens. That takes seconds for a day of rows, but give it more room
   (OPERATIONS §2 uses 5 s × 60).

### Build and deploy change log

| Date | Commit | Change |
|---|---|---|
| 2026-10-04 | `ec4da83` | The `OPERATIONS.md` sample labels its pods and selectors `app.kubernetes.io/name: azure-gateway-api`, the label Wega's NetworkPolicies select. |
| 2026-10-04 | `656fa6b` | This section: how Wega deploys the gateway, the configuration rules, the health semantics, the build facts and the update rule. |
| 2026-10-04 | `7743bc3` | This section checked against orchestrator `c8ce603`: the upstream port, ConfigMap and Secret wiring, PVC, probes, resources, security context, network policies, the mocks' `/status` and `syncAnalyze`, and the staging and prod cells. |
| 2026-10-04 | `e688dbb` | Re-checked against the `main` branches of the orchestrator (`44016ec`), vision (`5f93223`) and DES (`bf57bbb`). |
| 2026-10-04 | after `e688dbb` | [Build and publish](#build-and-publish): `<registry>` means the registry host plus the namespace (`P-REGISTRY`); digest capture and the full-name release-overlay key. |
| 2026-10-11 | `feat/sync-only-upstream` | **Sync-only upstream and splitting.** 22 settings; eleven removed settings warn. The gateway calls only `:syncAnalyze`, `/status` and the GET relays. Migration 0002 runs on first start and writes `gateway.db.v1.bak`; rolling back to an older image needs that backup restored. New `/_gw/config` keys `splitPages`, `diEnabled`, `readEnabled`; `/_gw/health` loses `syncAnalyze` and gains `upstreamWaiting`/`upstreamLimit`; `/_gw/jobs` gains `upstreamAttempts` and `upstreamParts` and loses `upstreamMode`, `hasPdf`, `figures`. The DI parameter policy rejects `markdown`, `features` and `queryFields`. `Server: Kestrel`, no `apim-request-id`. Memory sizing for two concurrent splits. The Wega follow-up is listed under Known gaps. |

### Read next

- [`OPERATIONS.md` §2](OPERATIONS.md#2-deployment): why the volume must be block-backed, why
  there is one replica, the probe advice, and a full sample manifest.
- [`OPERATIONS.md` §9–§10](OPERATIONS.md#9-schema-migrations-and-rollback): migrations, rollback,
  and the go-live gates.
- The [README configuration table](../README.md#configuration): every variable and its default.
- [`TESTING.md`](TESTING.md) and [`CONTAINER-BEHAVIOUR.md`](CONTAINER-BEHAVIOUR.md).
- `document-embedding-orchestrator/docs/deploy/SYSTEM-HANDOFF.md`: the whole system.

---

## Traps — things that look wrong and are not

Do not "fix" these without reading the reason.

| Looks wrong | Why it is that way |
|---|---|
| A container's `202` is drained and retried, never polled | With autoscaled pods behind HAProxy, the operation lives only on the pod that answered, and a poll would land anywhere. The stored input makes a retry cheap and correct |
| Every attempt opens a new connection | So HAProxy balances each attempt on its own and a retry can reach a different pod. Keep-alive would pin retries to the pod that just failed |
| A job gives its scheduler slot back while it backs off | A slot bounds calls into the pods, and a job that is waiting out a backoff is not calling. Holding the slot would idle capacity that another job's parts could use; the backing-off job is still slowed by its own backoff and `Retry-After`. The sync-only spec §1 asked for the opposite; the splitting design's per-call scheduler replaced it |
| Parts time out after 60 s while whole documents get 15 minutes | A 3–5 page part normally takes 2 s; a hung pod should cost a minute. A whole document is unknown work |
| A terminal error on a part is re-checked against the original instead of failing the job | A splitting artefact must never fail a good document; Azure's answer about the original is the one that counts |
| A merge check failure re-analyses the whole document instead of failing | The client asked for an analysis, not a merge. Whole is always correct |
| `sched.Acquire` returns a nil `release` with an error | Deferring it unchecked would panic. Check the error first |
| `pdfsplit.ErrPanic` wraps `ErrUnparseable` | Both send the document whole; check `ErrPanic` first so the metric says `panic` |
| pdfcpu runs with `NewStatelessConfiguration` | The default configuration writes a config file in the home directory and panics when it cannot — which is always, on a read-only root filesystem |
| An owner-password-only encrypted PDF is sent whole although pdfcpu could open it | The design splits only unencrypted documents; whole, Azure handles the encryption as it always did |
| Parts have no named destinations | pdfcpu rewrites the `/Dests` tree in place while extracting, which gave later parts destinations pointing at pages they do not hold |
| The merge refuses when the end of a part's page spans disagrees with the gateway's count of its content | uniseg and .NET can implement different Unicode versions. Where they disagree every shifted offset would be wrong. The page spans, not the root section, are the measure: real roots leave out page furniture and blank pages |
| Old migration files can never be edited, even for a typo | They have already run on volumes in service. `TestReleasedMigrationsAreUnchanged` pins them by sha256 |
| The unrouted 404 has **no body** | Both containers answer that way. Every *routed* endpoint still carries a body |
| DI's unknown-result-id error is **flat** while every other DI error is wrapped | The container does exactly this |
| `jsonx` hand-rolls a JSON scanner and rewriter instead of using `encoding/json` | `Decoder.Token()` materialises every scalar: 309 MB of heap for a 107 MB `content` string. The rewriter must also copy every byte it does not change |
| Boot recovery ignores leases; the periodic pass respects them | A freshly booted process holds no leases, so any `running` row is abandoned |
| Request contexts do not derive from the signal context | They used to, and in-flight synchronous calls died with a 500 at SIGTERM |
| `Store.Delete` removes the row *before* the blobs | Blob-first left a row pointing at a missing result, the shape that crashes Java clients |
| One replica only | Operation ids are pod-local by construction |

---

## Coupling map — if you change X, you must also change Y

- **`Operation-Location` format** → `internal/surface/di.go` *and* `read.go`, the conformance tests
  that assert the shape, and `test/sdk/sdk_test.py`. Read [`api-surface.md`](api-surface.md) §4
  first.
- **Error shapes** → `internal/azerr`, `internal/mockazure` (the fakes must keep matching real
  containers), the conformance tests, and the golden recordings.
- **The job schema** → **only through a new migration file** in `internal/store/migrations/`
  (`NNNN_name.sql`, the next number), with its sha256 added to `TestReleasedMigrationsAreUnchanged`
  — never by editing a released file — plus `jobColumns`, `scanJob`, `Create`, `Succeed`/`Fail` in
  `internal/store/db.go`, the job view in `internal/admin`, and the rollback notes in
  `OPERATIONS.md` §9 and [Rollback](#rollback).
- **A setting** → `internal/config/config.go` (field, parse, validate, `Redacted`,
  `TestRedactedCoversEveryField`), the README table, `OPERATIONS.md` §4 and the table above. A
  **removed** setting goes into `removedSettings`, so manifests that still set it keep starting.
  Settings Wega parses from `/_gw/config` (`diMaxInflight`, `readMaxInflight`, `queueDepth`) keep
  their names.
- **The parameter policy** → `internal/surface/params.go`, `test/conformance/params_test.go`,
  `api-surface.md` §0, and the probe's per-parameter check that justifies the change.
- **An attempt outcome or retry rule** → `internal/upstream/classify.go`, its row-by-row test, the
  engine's handling, the `reason` labels in `gateway_upstream_retries_total`, and ARCHITECTURE
  §4.1.
- **A merge rule** → `internal/merge`, `internal/docgen` (so the round trip still holds),
  `test/conformance/split_test.go`, a regression test from the document `--compare-split` found
  it on, and ARCHITECTURE §4.7.
- **Anything touching admission or slots** → every path that calls `Admit`, `AdmitMetadata`,
  `sched.Acquire`, `TryAcquireIdle` or `Splitter.Open` releases on *every* return path, including
  panics and cancellation.
- **A metric** → `internal/engine/metrics.go` or `internal/admin`, `admin_test.go`, and the alerts
  in `OPERATIONS.md` §5.

---

## How to work on this

1. **Read the design specs.** The original spec's review section lists 32 defects that were found
   and fixed; the sync-only and splitting specs say what changed and why. Re-introducing a defect
   is the most likely failure mode.
2. **Change the fakes when the containers surprise you.** If a probe or a golden recording finds
   something new, `internal/mockazure` gets it first, then the code, then a regression test.
3. **Every fix gets a regression test that fails before it.**
4. **Verify before claiming done:**
   ```bash
   gofmt -l . && go vet ./... && go test -race ./... && scripts/e2e-local.sh && make image
   ```
   The image build runs the suite inside it and has caught two bugs the local run missed. Do not
   skip it.
5. **Re-probe after any container upgrade**: `gateway probe` with `PROBE_DOCUMENT`, then
   `--record` into a new image-tag directory, and `--compare-split` on the corpus.

### Conventions

- Commits are authored `NilenduGanguli <nilendu.ganguli@gmail.com>`, conventional-commit prefixes,
  **no co-author trailers**.
- Comments explain *why*, and name the specific client or observation that forces the behaviour.
- `internal/` only; three external dependencies, all pure Go so the image stays static:
  `modernc.org/sqlite`, `github.com/pdfcpu/pdfcpu` (Apache-2.0) and `github.com/rivo/uniseg` (MIT).

### A warning about agent-assisted work

Subagents writing scratch files into the working tree got swept into commits by `git add -A`, and
the history had to be rewound. **Stage explicit paths when agents can write to the repo.**

---

## Open questions

| Question | Why it matters | How to settle it |
|---|---|---|
| Does the DI sync route honour `features`, `queryFields` and `outputContentFormat=markdown`? | They are rejected until it is proven; Wega's vision asks for two of them | `gateway probe` per-parameter checks against the real container |
| Do the real containers' sync and async `analyzeResult`s match byte for byte? | A sync-backed poll is indistinguishable from a real pod's only if they do | The sync-equals-async case in `gateway probe --record` |
| Which document-level judgements differ between whole and split analyses? | Each one needs a merge rule or an explicit acceptance | `gateway probe --compare-split` on 30+ representative PDFs |
| Does the .NET runtime in the container count graphemes like uniseg? | A disagreement makes the merge refuse (and analyse whole) for affected scripts | Watch `gateway_split_fallback_total{reason="merge_check"}` on Devanagari-heavy documents |
| Do other environments' containers agree on error shapes and headers? | The gateway reproduces one deployment's behaviour | Golden recordings per environment and image tag |
| Does a real container send `Cache-Control`? | The gateway's NoCache middleware adds `Cache-Control: no-store` to every answer; the parity test compares header names, so a container without it fails the first real recording on every gateway-answered case | The first `gateway probe --record` against the real containers, replayed by `parity_test.go` |

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
