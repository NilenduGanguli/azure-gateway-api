# Operations

This is the runbook for deploying and running `azure-gateway-api`: what to check before you
deploy, how to configure and size it, how to upgrade and roll back across a schema migration, what
to watch, what to do when something breaks, and the gates that must pass before go-live. Every
default described here comes from the source — settings from
[`internal/config/config.go`](../internal/config/config.go), metric names from
[`internal/admin/admin.go`](../internal/admin/admin.go) and the engine — so where this document and
a blog post disagree, this document is describing the binary you are running. For what the gateway
*is* and why, start at [`../README.md`](../README.md); for how it works inside, see
[`ARCHITECTURE.md`](ARCHITECTURE.md).

The deployment this targets: **one gateway pod** per cluster, in front of an Azure Document
Intelligence `prebuilt-layout` deployment, a Computer Vision Read 3.2 deployment, or both. The
Azure deployments **autoscale to at most 8 pods** each, and the gateway reaches them **only through
their HAProxy Route**, with synchronous calls only.

---

## 1. Before you deploy: probe the containers

Every analysis the gateway makes is one `:syncAnalyze` call. For Document Intelligence that route
is undocumented: some builds serve it, some 404 it, and one probed build declared it in its own
swagger and then never answered it. A build that cannot answer it **cannot run behind this
gateway** — there is no fallback any more — so probe first.

```sh
DI_UPSTREAM_URL=https://layout.apps.example.com \
READ_UPSTREAM_URL=https://ocr-read.apps.example.com \
DI_UPSTREAM_API_KEY=… READ_UPSTREAM_API_KEY=… \
PROBE_DOCUMENT=./representative-20-pages.pdf \
gateway probe
```

- It probes only the surfaces whose upstream URL is set.
- `PROBE_DOCUMENT` sends that document instead of the default blank PNG. Use a representative
  multi-page PDF: a blank image proves the route answers, not that it answers your documents in
  time. `PROBE_TIMEOUT` (default `10m`) bounds each call. Both are probe inputs, not gateway
  settings.
- It runs from anywhere that reaches the Routes: a jump host or a debug pod with the `bin/gateway`
  binary from `make build`, or `oc exec deploy/azure-gateway-api -- /usr/local/bin/gateway probe`
  inside the running gateway pod, which already has the settings in its environment.

**The verdict, per surface:**

| `:syncAnalyze` answered | Meaning | Do |
|---|---|---|
| **200** | ready | Deploy. |
| a timeout | the route exists and hangs (or the pods are starved) | Give the pods their resources (§8) and re-probe with a longer `PROBE_TIMEOUT`. If it still hangs, this build cannot run behind the gateway. |
| 404, 405, 501, or `500 UnhandledEndpointException` | the build does not serve the route | Use a build that does. The gateway would fail every job with *"The … container does not serve the synchronous analyze route."* |

**Exit status:** `0` when every configured surface is ready, `1` when any is not (or none is
configured), `2` for a usage error such as an unknown argument. `scripts/probe-containers.sh`
exits `1` when a surface is not ready and `3` when nothing answered at all. Script the gate on the
status, not on the text.

It also reports, for each parameter the gateway rejects until proven (`features`, `queryFields`,
`outputContentFormat=markdown`), whether the container's `:syncAnalyze` result equals its
`:analyze` result with that parameter set: `honoured` or `ignored`. A parameter shown `honoured`
can be moved to the forwarded list in `internal/surface/params.go`, citing the probe output.

`scripts/probe-containers.sh` (`make probe`) does the same interrogation in plain `sh` and `curl`,
for a host with nothing else on it. Its useful knobs are `SYNC_MAX_SECONDS`, `MODEL_ID` (default
`prebuilt-layout`) and `API_VERSION` (default `2024-11-30`). Neither tool prints an API key, and an
analysis costs what any other analysis costs.

The probe's two other modes, `--record` and `--compare-split`, are go-live gates (§10).
[`CONTAINER-BEHAVIOUR.md`](CONTAINER-BEHAVIOUR.md) records what the probed containers did;
[`api-surface.md`](api-surface.md) carries the citations.

---

## 2. Deployment

### The image

`make image` builds `linux/amd64` explicitly, because an Apple Silicon build defaults to `arm64`
and will not run on the cluster. The [`Dockerfile`](../Dockerfile) is multi-stage:
`golang:1.26-bookworm` builds with `CGO_ENABLED=0` and runs `go test ./...` inside the build stage,
then the binary is copied into `gcr.io/distroless/static-debian12:nonroot`. `CGO_ENABLED=0` works
because every dependency is pure Go: SQLite (`modernc.org/sqlite`), pdfcpu and uniseg.

The runtime image runs as `nonroot`, **uid 65532**, declares `VOLUME ["/data"]`, exposes `8080`,
and presets `DATA_DIR=/data`, `GATEWAY_ADDR=:8080`, `LOG_FORMAT=json`. Its entrypoint is
`gateway serve`; `gateway probe` and `gateway version` are the other subcommands. `cmd/fakeazure`
is a development tool and is not in the image.

### The volume, and why it must be block-backed

`DATA_DIR` holds a SQLite database in WAL mode, the blob store and the engine's work folders. They
make demands a network filesystem does not reliably meet:

- **WAL needs shared memory and POSIX advisory locks.** `store.Open` calls `networkFS()` and
  refuses to start with a message naming the filesystem it found:
  `nfs`, `smb`, `cifs`, `ceph`, `fuse`, `gfs2`, `ocfs2` (by `statfs` magic on Linux). **There is
  no override**; `ALLOW_NETWORK_FS` was removed.
- **Durability rests on `fsync` of the file and then of the directory after a rename.** The
  client-facing `202` is a promise that rests on it.

A block volume — RBD, iSCSI, EBS, a cloud PD — formatted `ext4` or `xfs` passes; CephFS, NFS and a
FUSE-presenting CSI driver do not. `fsGroup: 65532` in the pod spec makes the volume writable by
the image's non-root uid.

### One replica, `Recreate`

Two replicas are actively wrong, for two independent reasons.

1. **Operation ids are pod-local by construction.** The gateway mints its own GUID, commits a row
   for it, and serves every later poll from its own store. A second replica with its own volume
   404s every id the first one minted. With an RWO volume it cannot even start.
2. **Boot recovery ignores leases.** `store.Abandoned` deliberately selects every `notStarted` and
   `running` row without checking `lease_until`, because a process that has just booted holds no
   leases. That is only correct for a single pod: a second replica would re-run the first one's
   *active* jobs against the pods.

So: `replicas: 1`, `strategy.type: Recreate`. A `RollingUpdate` would violate both for the length
of the rollout, and would also let two processes migrate the same database.

### A complete manifest

The repository ships no `k8s/` directory, by design. What follows is documentation to copy into
whatever manages your cluster state. The Wega pipeline deploys from its own manifests in the
orchestrator repository; see [`HANDOFF.md` § Wega](HANDOFF.md#wega-build-and-deploy-facts).

The pods are labelled `app.kubernetes.io/name: azure-gateway-api`, the label Wega's
NetworkPolicies select. A Deployment's `spec.selector` is immutable, so a Deployment created with
another label has to be deleted and re-created to change it.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: azure-gateway-api
type: Opaque
stringData:
  # The only two secrets. Empty is legal — the gateway warns and calls the pods unauthenticated.
  # Keep credentials here rather than in the upstream URLs.
  DI_UPSTREAM_API_KEY: "replace-me"
  READ_UPSTREAM_API_KEY: "replace-me"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: azure-gateway-api
data:
  GATEWAY_ADDR: ":8080"
  DATA_DIR: "/data"

  # Recommended. Scheme and authority only — a path shifts the segment positions
  # Azure.AI.FormRecognizer counts backwards from, and startup rejects it. Unset, the request's
  # Host header is used, as the container does.
  PUBLIC_BASE_URL: "https://gateway.apps.example.com"

  # The HAProxy Route in front of the Layout pods (§3). Leave unset to serve Read only.
  DI_UPSTREAM_URL: "https://layout.apps.example.com"
  DI_MAX_INFLIGHT: "16"          # 8 pods x 2 concurrent analyses per pod (§8)
  DI_UPSTREAM_TIMEOUT: "15m"     # one whole-document attempt; the Route timeout must exceed it

  # No "vision" anywhere in this hostname (§3). Leave unset to serve DI only.
  READ_UPSTREAM_URL: "https://ocr-read.apps.example.com"
  READ_MAX_INFLIGHT: "16"
  READ_SYNC_TIMEOUT: "10m"

  SPLIT_PAGES: "5"               # 0 turns splitting off
  QUEUE_DEPTH: "200"             # accepted jobs waiting on the PVC, per surface
  RESULT_TTL: "24h"
  GC_INTERVAL: "5m"
  DISK_HIGH_WATERMARK: "0.90"
  SHUTDOWN_GRACE: "30s"
  LOG_LEVEL: "info"
  LOG_FORMAT: "json"
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: azure-gateway-api-data
spec:
  accessModes: ["ReadWriteOnce"]
  # Must be block-backed: ext4 or xfs on RBD/iSCSI/EBS/PD. Not NFS, not CephFS, not a FUSE CSI
  # driver — the gateway refuses to start on those.
  storageClassName: "ocs-storagecluster-ceph-rbd"
  resources:
    requests:
      storage: 100Gi
---
apiVersion: v1
kind: Service
metadata:
  name: azure-gateway-api
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: azure-gateway-api
  ports:
    - name: http
      port: 8080
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: azure-gateway-api
spec:
  replicas: 1              # see "One replica, Recreate" — 2 is a correctness bug
  strategy:
    type: Recreate         # never RollingUpdate: two pods must not share this volume
  selector:
    matchLabels:
      app.kubernetes.io/name: azure-gateway-api
  template:
    metadata:
      labels:
        app.kubernetes.io/name: azure-gateway-api
    spec:
      # Must exceed SHUTDOWN_GRACE, which bounds connection draining *and* job draining.
      terminationGracePeriodSeconds: 60
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532     # distroless "nonroot"
        fsGroup: 65532       # makes the PVC writable by that uid
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: gateway
          image: your-registry/azure-gateway-api:2.0.0
          imagePullPolicy: IfNotPresent
          args: ["serve"]
          ports:
            - name: http
              containerPort: 8080
          envFrom:
            - configMapRef:
                name: azure-gateway-api
            - secretRef:
                name: azure-gateway-api
          # Only if the Azure Routes are https with an internal CA:
          # env:
          #   - name: SSL_CERT_FILE
          #     value: /etc/pki/internal-ca/ca-bundle.crt
          livenessProbe:
            httpGet: {path: /_gw/live, port: http}
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
          readinessProbe:
            httpGet: {path: /_gw/ready, port: http}
            periodSeconds: 10
            timeoutSeconds: 5
            failureThreshold: 3
          # The listener opens only after the schema migration and boot recovery. A migration
          # takes seconds, but allow minutes: a startup probe that kills the pod mid-migration
          # just makes it start again from the same place (§9).
          startupProbe:
            httpGet: {path: /_gw/live, port: http}
            periodSeconds: 5
            failureThreshold: 60
          resources:
            requests:
              cpu: "500m"
              memory: "512Mi"
            limits:
              cpu: "2"
              memory: "2Gi"        # two concurrent splits of your largest PDF (§8)
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          volumeMounts:
            - {name: data, mountPath: /data}
            - {name: tmp, mountPath: /tmp}
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: azure-gateway-api-data
        - name: tmp
          emptyDir: {sizeLimit: 64Mi}
```

Things about this manifest that are easy to get wrong:

- **Do not point liveness at `/_gw/health`.** It answers `503` when the store is unreachable, and
  it calls each configured container's `/status` on every request with a six-second budget. As a
  liveness probe it turns a slow container into a restart loop.
- **`readOnlyRootFilesystem: true` works** because every write the gateway makes — the database,
  its backups, the blobs, the work folders, the upstream temp files — lands under `DATA_DIR`.
  pdfcpu runs with its stateless configuration precisely so it never tries to create a config file
  in the home directory. The `/tmp` `emptyDir` is insurance only.
- **An SCC may assign its own uid and `fsGroup`** on OpenShift and override `runAsUser`/`fsGroup`
  above. That is a cluster property and is **not verified in this repository**; a permission error
  on `/data` at startup points there first.

### First five minutes after it starts

```sh
oc logs deploy/azure-gateway-api | grep -m1 schema        # the migration line (§9)
oc exec deploy/azure-gateway-api -- /usr/local/bin/gateway version
curl -s http://gateway:8080/_gw/ready                     # {"status":"ready"}
curl -s http://gateway:8080/_gw/health | jq '.warnings, .upstreams, .capacity'
curl -s http://gateway:8080/_gw/config | jq '{diEnabled, readEnabled, diMaxInflight, readMaxInflight, queueDepth, splitPages}'
```

Read the `warnings` array. It carries what is legal but probably wrong: a **removed setting** that
is still set (`<NAME> is no longer used`), a `vision` substring in the Read hostname, an empty API
key on a configured upstream, and `PUBLIC_BASE_URL` unset. The same strings are logged at `WARN` at
startup. `upstreams[]` lists only the configured surfaces.

---

## 3. The HAProxy Route in front of the Azure pods

The gateway cannot address or count the Azure pods; every call goes through the Route, so the
Route's settings are part of the gateway's behaviour.

```yaml
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: layout
  annotations:
    # Longer than the gateway's per-attempt timeout (DI_UPSTREAM_TIMEOUT 15m). The 30-second
    # default turns every longer analysis into a 504.
    haproxy.router.openshift.io/timeout: 16m
    # Per-pod concurrency divided by the number of router replicas, rounded down, minimum 1.
    # The cap applies per router replica.
    haproxy.router.openshift.io/pod-concurrent-connections: "1"
    haproxy.router.openshift.io/balance: leastconn
spec:
  to:
    kind: Service
    name: layout
```

- **`timeout`** must exceed the per-attempt timeout of whole documents and re-checks:
  `DI_UPSTREAM_TIMEOUT` (`15m` → `16m`) for Layout, `READ_SYNC_TIMEOUT` (`10m` → `11m`) for Read.
  Part attempts are 60 seconds and fit either way. Too short, and every analysis longer than it
  becomes a 504 that the gateway retries once (`status_504`) and then fails.
- **`pod-concurrent-connections`** is enforced by each router replica on its own. With pods that
  take 2 analyses at a time and 2 router replicas, set `1`: each replica sends a pod at most one
  call, so the pod sees at most two. Calls over the cap wait in HAProxy's queue and then get a
  503, which the gateway retries with backoff (`status_503`), honouring any `Retry-After`, so
  each job backs away from an overloaded deployment. The backing-off job gives its scheduler slot
  back meanwhile, so the gateway keeps up to `MAX_INFLIGHT` calls open; size that against what the
  pods can take, not higher.
- **`balance: leastconn`** sends each call to the pod with the fewest open calls. Analyses vary from
  2 seconds to many minutes, so round-robin would stack calls behind one slow document.
- **The Azure pods** need `terminationGracePeriodSeconds` at least as long as the slowest
  analysis, so scale-in does not cut work short. Work that is cut short anyway is retried from
  the stored input (`connection` or `status_502`).
- **TLS.** If the Route is `https` with an internal CA, mount the CA bundle into the gateway and
  set `SSL_CERT_FILE` (commented out in the manifest above).

### The gateway's own Route, if clients reach it through one

The same 30-second default bites on the client side. Raise
`haproxy.router.openshift.io/timeout` on the gateway's Route to cover the longest of:

- a client `:syncAnalyze`, which now runs through the engine with retries and splitting and can
  legitimately take up to the **60-minute job deadline**;
- a large upload, bounded by `UPLOAD_TIMEOUT` (10m);
- streaming a large result back out on a poll.

If clients only use `:analyze` + poll, `15m` is plenty; if they use `:syncAnalyze`, use `61m` or
move them to `:analyze`, which returns its 202 immediately. In-cluster clients calling the Service
directly (as Wega does) pass through no router at all.

### The `vision` substring bug in the Read container

When the inbound authority contains the substring `vision`, the Read container rebuilds its own
callback URL with a naive string operation and strips both the port and the `/vision` path
segment:

```
POST http://azure-vision:5000/vision/v3.2/read/analyze
  → Operation-Location: http://azure-vision/v3.2/read/analyzeResults/…
```

**No `vision` anywhere in the hostname of `READ_UPSTREAM_URL`.** The gateway itself is immune — it
only ever calls `syncAnalyze` and never follows a container's `Operation-Location` — but anything
calling the container directly is not. `Config.Warnings()` flags it at startup.

---

## 4. Configuration

Everything is an environment variable; nothing is read from disk. There are **22 settings**.

Parsing rules, before the tables:

- An **empty value counts as unset** and the default applies.
- A malformed **number or duration is fatal**. `SPLIT_PAGES=off` fails startup with
  `SPLIT_PAGES is not a valid integer`, rather than silently leaving splitting on at its default of
  5; `RESULT_TTL=24` (no unit) fails with `RESULT_TTL is not a valid duration`. Byte sizes are plain
  integers, so `500Mi` is rejected. Durations are Go syntax: `30s`, `10m`, `24h`.
- **At least one upstream URL must be set.** A surface whose URL is empty is disabled: no routes,
  no worker, no scheduler, no entry in `/_gw/health` `upstreams[]`, `/status` or the metrics. Its
  own settings — `*_MAX_INFLIGHT`, its timeout, its API key — are neither validated nor warned
  about, so a stale value in the manifest of a single-surface gateway cannot stop it starting.
- Validation reports **every** problem at once, because a gateway that starts with a bad upstream
  URL accepts work it can never complete after having already promised a `202`.

### Upstreams

| Variable | Default | Notes |
|---|---|---|
| `DI_UPSTREAM_URL` | — | The HAProxy Route in front of the Layout pods, e.g. `https://layout.apps.example.com`. Must be `http` or `https` with a host; a trailing `/` is stripped. Empty disables DI. |
| `READ_UPSTREAM_URL` | — | The Route in front of the Read pods. **No `vision` in the hostname.** Empty disables Read. |
| `DI_UPSTREAM_API_KEY` / `READ_UPSTREAM_API_KEY` | — | Sent as `Ocp-Apim-Subscription-Key`. Empty means no header at all, and a startup warning for a configured surface. The gateway requires no credential *from* clients. |

### Capacity

| Variable | Default | What it does |
|---|---|---|
| `DI_MAX_INFLIGHT` / `READ_MAX_INFLIGHT` | `4` | Slots in that surface's scheduler: concurrent calls to its Azure pods of **any** kind — whole documents, parts, hedges, re-checks — and the number of jobs running at once. Must be ≥ 1 for a configured surface. Size it as max pods × per-pod concurrency: **16** for 8 × 2 (§8). |
| `QUEUE_DEPTH` | `0` | Accepted jobs allowed to wait on the PVC, per surface, on top of `*_MAX_INFLIGHT`. `0` means a submit is refused with `429` the moment every worker is busy. Size for bursts, e.g. `200`. |
| `SPLIT_PAGES` | `5` | The most pages one part may hold. A PDF with more pages to analyse is split into balanced parts of `ceil(SPLIT_PAGES/2)`–`SPLIT_PAGES` pages. `0` turns splitting off: every document goes whole. Must be ≥ 0. |
| `MAX_REQUEST_BYTES` | `524288000` (500 MiB) | Ceiling on an **upload**. Exceeding it yields `400` with `InvalidRequest`/`InvalidContentLength` on DI, `InvalidImageSize` on Read — not `413`, which is unmodelled on both surfaces. |
| `MAX_RESULT_BYTES` | `536870912` (512 MiB) | Ceiling on a **result**: each part's, each whole document's, and the merged one. A merged result over it fails the job with the same error the document gets whole. Separate from the upload cap because they bound opposite directions. |

### Timeouts

| Variable | Default | What it bounds |
|---|---|---|
| `DI_UPSTREAM_TIMEOUT` | `15m` | **One attempt** at a whole DI document, or one `pages=` re-check. Not the job: a job may make up to 5 attempts, within the 60-minute job deadline. |
| `READ_SYNC_TIMEOUT` | `10m` | The same for Read. |
| `UPLOAD_TIMEOUT` | `10m` | How long a client may take to stream its request body while holding an admission slot. |
| `SHUTDOWN_GRACE` | `30s` | Connection draining **and then** job draining share this single budget. `terminationGracePeriodSeconds` must exceed it. |

### Storage, addressing and process

| Variable | Default | What it does |
|---|---|---|
| `DATA_DIR` | `/data` | The PVC mount: `gateway.db`, its backups, `blobs/`, `work/`. |
| `RESULT_TTL` | `24h` | How long an operation id resolves. After it, a poll `404`s exactly like Azure. |
| `GC_INTERVAL` | `5m` | Sweeper period: TTL deletion, temp files, orphan blobs, stale work folders, lease reclaim, disk-pressure eviction. |
| `DISK_HIGH_WATERMARK` | `0.90` | Fraction of the volume above which completed results are evicted, expired ones first. Must be in `(0,1]`. Submits are refused with `503` at the watermark plus half the remaining headroom — `0.95` at the default. |
| `PUBLIC_BASE_URL` | *(the request's `Host`)* | Scheme and authority for `Operation-Location`. **Recommended.** Rejected at startup if it carries a path, query or fragment. Forwarded headers are never consulted. |
| `GATEWAY_ADDR` | `:8080` | Listen address. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`/`warning`, `error`. Anything else is `info`. |
| `LOG_FORMAT` | `json` | `text` selects `slog`'s text handler; anything else is JSON, on stdout. |

### Removed settings

Eleven settings were removed by the sync-only change. A removed setting that is still set does
**not** stop the gateway: it starts, and `Config.Warnings()` names each one as
`<NAME> is no longer used` in the startup log and in `/_gw/health` `warnings[]`. Remove them from
your manifests at your own pace; none of them does anything.

| Setting | Replaced by |
|---|---|
| `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT` | nothing: `:syncAnalyze` is the only upstream route, bounded by the per-attempt timeout |
| `DI_BLIND_POLL_BUDGET`, `READ_BLIND_POLL_BUDGET` | nothing: the gateway never polls a container |
| `ARTIFACT_FETCH_TIMEOUT` | nothing: `output=pdf` / `output=figures` are rejected with 400 |
| `ERROR_COMPAT` | the containers' observed error shapes, always |
| `TRUST_FORWARDED_HEADERS`, `TRUSTED_FORWARDED_HOSTS` | `PUBLIC_BASE_URL`, or else the `Host` header |
| `ALLOW_NETWORK_FS` | nothing: network filesystems are always refused |
| `POLL_RETRY_AFTER`, `BUSY_RETRY_AFTER` | the constants 1 and 5 below |

`/_gw/config` no longer shows them. It shows `splitPages`, `diEnabled` and `readEnabled`, and keeps
`diMaxInflight`, `readMaxInflight` and `queueDepth` — Wega's observer parses those — with `0` and an
empty URL for a disabled surface.

### Constants

Not configurable, by design: each is either a contract with the clients or a bound whose right
value does not depend on the deployment.

| Constant | Value | Why |
|---|---|---|
| `Retry-After` on the 202 and in-progress polls | `1` s | without the header azure-core waits its 30-second default between polls |
| `Retry-After` on the gateway's 429 and its disk-pressure 503 | `5` s | the JS SDK retries a 429 only when the header is present |
| Attempts per whole document | `5` | at most 2 of them in the once-only class (attempt timeout, 500, 504): a slow analysis is retried once, not five times |
| Backoff between attempts | 50–100% of `min(60s, 2s × 2^(n−1))` | a parseable integer `Retry-After` from HAProxy or the pod replaces it, capped at 60s |
| Part attempt timeout | `60s` | a 3–5 page part normally takes about 2 s; a hung pod costs a minute, not fifteen |
| Attempts per part size | `3` | then the part is halved, down to single pages; a single page that still fails is re-checked against the original under the whole-document rules (`pages=<page>`, the whole timeout, 5 attempts), and only that answer can fail the job |
| Hedge delay | `clamp(4 × median, 5s, 30s)` | the median of the surface's last 200 successful parts; `30s` until there are 20 samples. One hedge per part attempt, idle slots only |
| Job deadline | `60m` | the containers' own maximum task time; covers all attempts, parts and backoffs |
| Concurrent splits | `2` | shared by both surfaces; bounds parsing memory |
| Split budget | `60s` | from when the split slot is held; over budget, the document goes whole |
| Dial timeout | `10s` | per attempt; a fresh connection every attempt |
| Metadata relay lane | `4` slots, `10s` per call | `/info`, `/documentModels`, `/operations`, the container root |
| Worker lease / heartbeat | `2m` / `30s` | a long analysis is never reclaimed under its worker |
| Worker runs per job | `3` | recovery fails a job that keeps dying with its worker |
| Stale work folders, temp files, orphan blobs | older than `1h` | swept every `GC_INTERVAL` |

---

## 5. Observability

### The endpoints

The eight `/_gw/*` endpoints live under that prefix so they can never collide with an Azure route,
and carry no `Server: Kestrel` because they are the gateway's own. `/status` and `/ready` are served
at the root on purpose, mirroring the containers' own paths. **None is authenticated.** Do not
expose them through a public Route.

| Endpoint | Returns |
|---|---|
| `GET /_gw/live` | `200 {"status":"ok"}`, always, without touching a dependency. Use it for liveness and startup. |
| `GET /_gw/ready` | `200 {"status":"ready"}`, or `503` when either SQLite pool fails a 3-second ping. Upstream reachability is deliberately excluded. |
| `GET /_gw/health` | `status` (`ok`/`degraded`), `uptime`, `build`, `store`, `capacity` per configured surface (`inFlight`, `limit`, `queued`, `upstreamInUse`, `upstreamWaiting`, `upstreamLimit`), `upstreams[]` for the configured surfaces only, job counts, and `warnings[]`. HTTP `503` only when the store is unreachable; an unreachable container yields `degraded` at `200`. There is no `syncAnalyze` field any more. |
| `GET /_gw/jobs?limit=N` | `{"jobs":[…],"count":N}`, most recently updated first; `limit` 1..1000, else 50. |
| `GET /_gw/jobs/{id}` | One job view, or `404 {"error":"no such job"}`. |
| `GET /_gw/metrics` | Prometheus text format, hand-written. |
| `GET /_gw/version` | `{"version","commit","built","go"}`. |
| `GET /_gw/config` | The effective configuration. Both API keys render as `***`, and a password embedded in an upstream URL as `user:xxxxx`. |
| `GET /status` | Container-shaped `{"apiStatus":"Valid"…}` or `Invalid`, always at HTTP 200, over the configured upstreams. |
| `GET /ready` | Container-shaped `{"ready":"ready"}` at 200, or `{"ready":"notReady"}` at 503. |

A job view carries `id`, `surface`, `modelId`, `status`, `createdAt`, `updatedAt`, `expiresAt`,
`inputBytes`, `resultBytes`, `upstreamMs`, `attempts` (worker runs), `upstreamAttempts` (calls to
Azure), `upstreamParts` (1 = whole; more = split and merged; 0 = not completed) and `error`, the
stored Azure-shaped error. `upstreamMode`, `hasPdf` and `figures` are gone. Client `:syncAnalyze`
calls are not jobs and do not appear.

**Relayed routes carry the container's own headers.** The metadata and passthrough GETs (`/info`,
`/documentModels…`, `/operations…`, `/`, `/swagger…`, `/records/usage-logs…`) pass the response
headers of whichever pod HAProxy picked straight through to the client, minus hop-by-hop headers,
`Operation-Location`, `Location` and `Set-Cookie`. So on those routes `Server` (Kestrel), and any
`apim-request-id` a container build does send, come from the pod; every Azure route the gateway
answers itself sends `Server: Kestrel` and never an `apim-request-id`. When comparing a relay with
an owned route, expect that difference. Error bodies on a relay are normalised to the surface's
shape at the container's own status. See
[`api-surface.md` §0](api-surface.md#0-what-the-gateway-serves).

### The metrics

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `gateway_uptime_seconds` | gauge | — | Seconds since process start. A drop means a restart. |
| `gateway_jobs_in_flight` | gauge | `surface` | Admission slots held: jobs not yet terminal, plus client `:syncAnalyze` calls in progress. Occupancy, not a job count. |
| `gateway_jobs_capacity` | gauge | `surface` | `MAX_INFLIGHT + QUEUE_DEPTH`. |
| `gateway_jobs_queued` | gauge | `surface` | Admitted jobs waiting for a worker. |
| `gateway_jobs_total` | gauge | `surface`, `status` | Rows in the database by status. Falls as the TTL sweeper runs; not cumulative. |
| `gateway_volume_used_ratio` | gauge | — | Fraction of the volume in use. Omitted when it cannot be determined; alert on absence too. |
| `gateway_upstream_slots` | gauge | `surface`, `state` (`inUse`, `waiting`, `limit`) | The scheduler. `waiting > 0` means every slot is busy and calls are queued in the gateway. |
| `gateway_upstream_retries_total` | counter | `surface`, `reason` | Failed attempts that were retried. `reason`: `connection`, `status_408`, `status_429`, `status_500`, `status_502`, `status_503`, `status_504`, `degraded_202`, `timeout`. |
| `gateway_upstream_exhausted_total` | counter | `surface` | Jobs failed because their attempts ran out. |
| `gateway_part_seconds` | histogram | `surface` | Duration of successful part calls. |
| `gateway_job_parts` | histogram | `surface` | Parts per split job. |
| `gateway_merge_seconds` | histogram | `surface` | Merge duration. |
| `gateway_parts_total` | counter | `surface`, `outcome` | `succeeded`, `retried`, `halved`, `hedged`, `hedge_won`, `rechecked`. |
| `gateway_split_fallback_total` | counter | `surface`, `reason` | Documents that qualified for splitting but went whole: `unparseable`, `encrypted`, `budget`, `panic`, `pages_param` (a `pages` the gateway cannot use, or given more than once under any spelling), `string_index_type` (a `stringIndexType` the merge cannot count in, or given more than once), `merge_check`. |

Histogram buckets are 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120 and 300. There is still no request-latency
histogram; per-request timing is in the access log's `durationMs`, per-job timing in `upstreamMs`.

### The log fields that matter

Logs are `log/slog` on stdout, JSON by default.

| Field | Where it comes from |
|---|---|
| `requestId` | Minted per request, in the logs only. It is **not** sent to the client: the `apim-request-id` header is gone, because the containers are not known to send it. |
| `clientRequestId` | The client's own `x-ms-client-request-id`, echoed back in the response. **This is now the correlation handle a client can quote back to you.** |
| `method`, `path`, `status`, `bytes`, `durationMs` | The `"request"` access-log line: `ERROR` for 5xx, `WARN` for 4xx, `INFO` otherwise. |
| `job`, `surface` | Every job and engine line. A client `:syncAnalyze` runs as job `sync-<id>`. |
| `pages`, `attempt` | Engine lines about one part, e.g. `pages=12-15 attempt=2`. |
| `attempt`, `reason`, `status`, `backoffMs` | The one `WARN` line per failed attempt. |
| `attempts`, `parts`, `halvings`, `hedges`, `fallback` | A job's final line. |
| `from`, `to`, `backup`, `legacy`, `took` | The `job store schema migrated` line at startup (§9). |

The bridge between a client and a job is the `accepted operation` line, which is request-scoped
*and* carries the job id:

```
client's x-ms-client-request-id → grep clientRequestId=… → "accepted operation" job=<operation id>
                                                         → grep job=<operation id> for everything after
```

### What to alert on

| Alert | Condition | Why it matters |
|---|---|---|
| Clients being refused | `gateway_jobs_in_flight / gateway_jobs_capacity == 1` for minutes, per surface | The next submit gets a `429`. Raise `QUEUE_DEPTH`, or scale the Azure side. |
| Jobs failing after all attempts | `increase(gateway_upstream_exhausted_total[15m]) > 0` | The pods were unreachable, overloaded or failing for longer than five attempts. Read the `reason` mix below. |
| Retry storm | `rate(gateway_upstream_retries_total[5m])` by `reason` well above its baseline | `status_503`: HAProxy has no ready pod or the per-pod cap is too tight. `connection`/`status_502`: pod churn or scale-in cutting work. `timeout`/`status_504`: hung pods, or a Route timeout below the attempt timeout. `degraded_202`: pods under memory pressure. |
| Gateway-side queueing | `gateway_upstream_slots{state="waiting"} > 0` for long stretches | Every slot is busy. Expected during bursts; sustained, it means `*_MAX_INFLIGHT` is below what the Azure side can take — or the Azure side is slow. |
| Hung parts | `increase(gateway_parts_total{outcome="halved"}[15m]) > 0`, or part p95 near 60 s | Parts are timing out repeatedly: a pod is hanging. |
| Merge checks failing | `increase(gateway_split_fallback_total{reason="merge_check"}[1h]) > 0` | Part results disagreed or did not add up. The document was analysed whole, so clients are fine, but something changed: read the log line's check message. |
| pdfcpu panics | `increase(gateway_split_fallback_total{reason="panic"}[1d]) > 0` | A PDF crashed the splitter (recovered; sent whole). Keep the document and report it. |
| Volume filling | `gateway_volume_used_ratio > 0.90` | Eviction has started; at 0.95 submits fail with `503`. |
| Restarts | `gateway_uptime_seconds` resetting | Every restart re-runs in-flight jobs from their stored input. |
| Degraded upstream | `/_gw/health` `.status == "degraded"` | A configured container's `/status` is not answering. |

---

## 6. Troubleshooting playbooks

### Clients get 429s

**Check.** `curl -s :8080/_gw/health | jq .capacity` — compare `inFlight` against `limit` per
surface. In metrics, `gateway_jobs_in_flight` versus `gateway_jobs_capacity`.

**What it means.** A `429` is always the gateway's own backpressure — the pods never send one. It
comes from the job admission gate (`MAX_INFLIGHT + QUEUE_DEPTH`, held from acceptance to a terminal
state, shared by `:analyze` and client `:syncAnalyze`) or from the metadata lane (4 slots, for the
GET relays). Polls are never admission-controlled. Waiting for a *scheduler* slot never produces a
429: that wait happens inside a job that has already been accepted.

**Do.** Raise `QUEUE_DEPTH` so callers wait on the PVC instead of being refused — the cost is disk
for the queued inputs. Raise `*_MAX_INFLIGHT` only up to what the Azure side can absorb (§8). If
`upstreamMs` on recent jobs is large, the pods are slow and that is the real problem.

### A job sits at "running" for a long time

**Check.** `curl -s :8080/_gw/jobs/<id> | jq`, then `grep job=<id>` in the logs: every failed
attempt has its own `WARN` line with `reason`, `status` and `backoffMs`.

**What it means.**
- **No attempt lines:** the job is inside its first call. A whole document's attempt is bounded by
  `DI_UPSTREAM_TIMEOUT`/`READ_SYNC_TIMEOUT`; a part's by 60 s. Or it is waiting for a scheduler
  slot — check `gateway_upstream_slots{state="waiting"}`.
- **Attempt lines with backoffs:** the pods are failing it transiently. It will either succeed or
  exhaust its attempts and fail with the last error.
- **`attempts` (worker runs) climbing:** the worker keeps dying — a restart loop or an OOM. At 3 it
  is failed with "The analysis was interrupted repeatedly and could not be completed."

Nothing runs forever: every job has a **60-minute deadline**, after which it fails with a timeout
error in the surface's shape.

### Polls 404 unexpectedly

The gateway returns `404` for exactly four reasons, never for a live operation:

1. **The id is not a well-formed GUID** (`8-4-4-4-12`, either hex case on input).
2. **The id belongs to the other surface**, or that surface is not configured on this gateway.
3. **The TTL passed** (`RESULT_TTL`, 24h) — the poll 404s even before the sweeper deletes the row.
4. **The row is gone**: a client `DELETE`, the TTL sweep, or disk-pressure eviction.

A restart never causes this; ids and results live on the PVC. **A rollback restore does** (§9):
jobs accepted after the backup was taken are not in it.

### The volume fills

**Check.** `gateway_volume_used_ratio`, `/_gw/health` `.store`, `/_gw/jobs?limit=1000` for
`resultBytes`. The image has no shell: inspect from a debug pod that mounts the PVC, with the
gateway scaled to 0 (the volume is RWO).

| Usage | Behaviour |
|---|---|
| below `DISK_HIGH_WATERMARK` | Normal. TTL sweep every `GC_INTERVAL`. |
| at or above it | Every sweep evicts completed jobs, expired ones first, then results a client could still fetch — each logged as `evicted a result that had not yet expired`. |
| at watermark + half the headroom (0.95) | New submits get `503` + `Retry-After: 5` before any bytes are written. |

**Do.** Grow the PVC. Lower `RESULT_TTL` if clients fetch promptly. Account for the queue: with
`QUEUE_DEPTH=200`, up to 200 inputs per surface wait on the PVC. Split jobs also hold their parts
and part results in `work/` while they run — roughly twice the input plus its result per job,
briefly. A `work/` that keeps growing means folders outlive their jobs, which the sweeper removes
after an hour.

### The pod is OOMKilled

**Check.** `lastState.terminated.reason`; the sizes of recent inputs (`inputBytes`); the
`gateway_split_fallback_total` and `gateway_job_parts` rates around the kill.

**What it means.** Results never land on the heap: uploads stream to the PVC, responses stream to
temp files, the merge is a streaming rewrite. The one large consumer is **pdfcpu parsing a PDF**,
about 2–3× the file's size, at most two at once. An OOM therefore almost always means two large
PDFs were being split at the same time against a limit sized for small ones. A crash loop is
possible: after an OOM, boot recovery re-runs the same jobs and splits the same PDFs again, until
the worker-run limit (3) fails them.

**Do.** Size the memory limit for two of your largest PDFs (§8), or lower `MAX_REQUEST_BYTES` to
what you actually need. `SPLIT_PAGES=0` stops splitting entirely as an emergency measure; documents
then go whole, as before.

### Analyses fail at about 30 seconds, or at the per-attempt timeout

**Check.** `grep reason=` in the attempt lines, and `gateway_upstream_retries_total` by reason.

**What it means.**
- `status_504` at almost exactly 30 s: the **Azure Route** still has HAProxy's default timeout.
  Set `haproxy.router.openshift.io/timeout` above the attempt timeout (§3).
- A client reports failure at 30 s while the gateway logs the request finishing much later: the
  **gateway's own Route** timed out. Raise it, or move the client to `:analyze` (§3).
- `timeout` at exactly `DI_UPSTREAM_TIMEOUT`: the analysis itself takes longer. Raise the timeout
  (and the Route's with it) or let splitting cut it into small parts.

### Every analysis fails but the containers look healthy

`/_gw/health` reporting `ok` only means each container's `/status` answered.

**Check.** `curl -s :8080/_gw/jobs?limit=5 | jq '.jobs[] | {id,status,upstreamAttempts,upstreamParts,error}'`.

| Signal in `error` / logs | Cause |
|---|---|
| `The … container does not serve the synchronous analyze route.` | This build does not answer `:syncAnalyze` (404/405/501 or `UnhandledEndpointException`). Re-run the probe (§1); the build cannot run behind this gateway. |
| `engine: attempts exhausted` in the log, the job's `upstreamAttempts` at 5 in `/_gw/jobs` | Every attempt failed transiently, and the client got the last attempt's error verbatim. The attempt lines' `reason` says how: 503s from HAProxy, resets, timeouts. |
| `Unauthorized` / an upstream 401 | Wrong or missing `*_UPSTREAM_API_KEY`. |
| `400 InvalidArgument` naming `features`, `queryFields`, `outputContentFormat` or `output` | The parameter policy (README, api-surface §0). Not a container failure. |
| `upstream: response exceeds the configured maximum` | `MAX_RESULT_BYTES` is smaller than the real output. |
| `ModelNotFound` | `modelId` in the request. |
| a content error with a plausible message | The container is rejecting the input. If the document is split, the gateway re-checked the part's pages against the original before failing, so this is Azure's own answer about the original document. |

### Large documents come back different from a whole analysis

Page-level content (words, lines, tables and figures on a page, offsets, page numbers) is merged
exactly. Document-level judgements — which heading Azure marks `title`, section nesting across a
split point, page headers and footers, a table Azure joins across a page break — can differ. Run
`gateway probe --compare-split` on the documents in question (§10) and see which pattern it is.
`SPLIT_PAGES=0` restores whole-document analysis while a merge rule is written.

### The gateway will not start

Startup failures print every problem found, not just the first.

| Message | Cause |
|---|---|
| `at least one of DI_UPSTREAM_URL and READ_UPSTREAM_URL must be set` | Both empty. |
| `…must use http or https` / `…must include a host` | A bare hostname without a scheme. |
| `<VAR> is not a valid integer` / `is not a valid duration` | Including `SPLIT_PAGES=off` and `RESULT_TTL=24`. Numbers are never silently defaulted. |
| `SPLIT_PAGES must not be negative`, `DISK_HIGH_WATERMARK must be in (0,1]` | It is a fraction, not a percentage. |
| `PUBLIC_BASE_URL must be scheme and host only…` | You included a path. |
| `store: DATA_DIR … is on a <fs> filesystem, where SQLite WAL mode is unsafe` | Use a block-backed volume. There is no override. |
| `store: database schema vN is newer than this gateway (vM); deploy the newer image, or restore gateway.db.vM.bak` | An image rollback after a migration. See §9. |
| `store: … migration …` | A migration failed and was rolled back; the database is unchanged. The message names the statement. |
| `store: create data dir: … permission denied` | `fsGroup` is missing, or an SCC assigned a different uid. |

A removed setting is **not** in this table: it only warns.

---

## 7. Data

### What is on the PVC

```
${DATA_DIR}/
  gateway.db              SQLite, one table `jobs`, three indexes; schema version in PRAGMA user_version
  gateway.db-wal          WAL — present while the database is open
  gateway.db-shm          shared-memory index for the WAL
  gateway.db.v<N>.bak     the copy taken before the last migration from version N (§9)
  blobs/
    *.tmp-*               upstream responses being downloaded; swept once older than an hour
    <aa>/<bb>/            two-level shard, from the first four characters of the job id
      <id>.in             the submitted document, deleted the moment the job succeeds or fails
      <id>.res            the exact response envelope a successful poll will stream
      <id>.pdf, .fig-*    legacy only: files older builds captured; removed with their job
  work/
    <id>/                 a running job: decoded input, part PDFs, part results, merged.json
    sync-<id>/            a client :syncAnalyze in flight: its saved body
```

Every blob write is atomic: temp file in the destination directory, `fsync`, `rename`, `fsync` of
the directory. The database runs `journal_mode=WAL` with `synchronous=FULL` on its single writer.
A work folder is deleted when its job finishes; the sweeper removes any left by a crash once they
are an hour old.

### How big it gets

> (submissions per TTL window × average result size)
> \+ (queued and running jobs × average input size)
> \+ (running split jobs × roughly twice their input, briefly)

The input is deleted as soon as the job is terminal; results live for `RESULT_TTL`. Azure permits a
Document Intelligence result up to 500 MB, and the gateway's ceiling is `MAX_RESULT_BYTES`
(512 MiB). Size the PVC in tens of gigabytes and watch `gateway_volume_used_ratio`.

### What happens at TTL expiry

- **The poll checks first.** Any GET on an operation past `expires_at` returns `404` immediately.
- **The sweeper deletes.** Every `GC_INTERVAL`, up to 512 expired jobs are removed — the row first,
  then the blobs, so a partial failure leaks a file rather than stranding a row that points at a
  missing result. An expired `running` job is not swept while its lease is live.

### What a restart does to in-flight work

The graceful path, on `SIGTERM`:

1. The signal only *triggers* the drain; requests and workers run under a context that outlives it.
2. `srv.Shutdown` stops accepting connections and lets in-flight handlers finish — client
   `:syncAnalyze` calls matter most, because a client is holding the connection.
3. `manager.Drain` waits for background jobs. Steps 2 and 3 **share one `SHUTDOWN_GRACE`**.
4. Workers are cancelled. Every part attempt and backoff in progress is cancelled with them. A job
   interrupted this way is **not** marked failed — it is left `running` for the next process.

On the next boot, after the schema check and before the listener opens:

- unfinished rows are scanned **ignoring leases** and re-run **from their stored input**. Part
  progress is not kept: parts take seconds, so a split job simply splits again;
- rows past their TTL are excluded;
- a job whose input is gone, or which has used 3 worker runs, fails with an Azure-shaped error;
- everything else **waits** for an admission slot rather than being 429'd — a 429 is a refusal to
  make a promise, and these promises were already made.

A client polling across all of this sees `running`, then a terminal state. A client in the middle
of a `:syncAnalyze` sees its connection close, exactly as it would with a real pod, and retries.

---

## 8. Capacity and sizing

### What the limits bound

| Pool | Capacity | Held for |
|---|---|---|
| Admission | `MAX_INFLIGHT + QUEUE_DEPTH` per surface | From accepting a submit (or a client `:syncAnalyze`) until it is terminal. Exhausting it produces a `429`. |
| Workers | `MAX_INFLIGHT` per surface | Jobs actually running. |
| Scheduler slots | `MAX_INFLIGHT` per surface | One call to the Azure pods — whole document, part, hedge or re-check — for that call alone: the backoff before a retry holds none. Shared fairly between jobs: waiting jobs take turns, round-robin. |
| Split slots | 2, both surfaces together | Parsing and cutting one PDF. |
| Metadata lane | 4 per surface | One GET relay, 10 s at most. |

### Sizing `MAX_INFLIGHT` and `QUEUE_DEPTH`

**`*_MAX_INFLIGHT = max Azure pods × per-pod concurrency`**: with the deployment autoscaling to 8
pods that each take 2 analyses at once, **16**. That keeps every pod busy at full scale. When the
deployment is scaled in, the extra calls queue in HAProxy at the per-pod cap and come back as 503s,
which the gateway retries while holding the slot — so a gateway sized for 8 pods slows itself down
to what 2 pods can take, without configuration.

Do not size it far above that: calls beyond what the pods can take only wait in HAProxy, then
fail and back off. Do not size it far below either: with splitting, one job can use every slot,
and the hedges that rescue hung parts only ever use idle slots.

**`QUEUE_DEPTH`** absorbs bursts. Each queued job is an input file on the PVC and nothing else, so
a large value (e.g. `200`) is cheap; it is what makes the gateway's 429 — the one place it behaves
unlike a real pod — rare.

### Memory for two concurrent splits

The gateway's own heap is small and does not grow with result size. Splitting adds one consumer:
pdfcpu holds about **2–3× a PDF's size** while parsing it, and up to **2** PDFs are parsed at once.
So:

> memory limit ≈ 512 MiB + 2 × 3 × (largest PDF you expect to split)

| Largest PDF | Limit |
|---|---|
| 50 MB | 1 GiB |
| 200 MB | 2 GiB |
| 500 MB (`MAX_REQUEST_BYTES` default) | 4 GiB |

If that is more than you can give, lower `MAX_REQUEST_BYTES` to what your documents need. The
60-second split budget bounds the time a parse may take, not its memory.

### The Azure pods

The containers are the expensive part. From Microsoft's documentation
([`api-surface.md` §9.4](api-surface.md)):

| Container | Resources |
|---|---|
| DI Layout | minimum **8 cores / 16 GB**, recommended **8 cores / 24 GB**, x64 Linux only |
| CV Read 3.2 | minimum 4 cores / 8 GB, recommended 8 cores / 16 GB; the host **must support AVX2** |

A 3–5 page part takes about 2 seconds on a healthy Layout pod; a 100-page PDF as one call takes
minutes. That difference is the point of splitting: 8 pods each analysing a few pages beat one pod
analysing the whole document while seven sit idle.

Scale the Azure deployment on demand the gateway can see: `gateway_upstream_slots{state="waiting"}`
and `gateway_jobs_queued`. CPU is a poor signal, because one multi-page document pins all of a
Layout pod's cores regardless of how much is waiting.

The gateway itself does not scale horizontally. See [One replica, `Recreate`](#one-replica-recreate).

---

## 9. Schema migrations and rollback

The job store is versioned. Each binary carries numbered migrations
(`internal/store/migrations/NNNN_name.sql`); the database records its version in
`PRAGMA user_version`. The sync-only release ships **migration 0002**, which drops four dead
columns and adds `upstream_attempts` and `upstream_parts`. ARCHITECTURE §5 has the mechanism.

### Upgrading

Nothing to do but deploy. On the first start of a new image, before the listener opens:

1. If the database is already at the image's version: nothing, one read.
2. Otherwise the gateway copies the database to `${DATA_DIR}/gateway.db.v<N>.bak` (`N` = the
   version it found; a database from before versioning counts as version 1), replacing any older
   backup.
3. It applies every pending migration in one transaction. Any failure rolls back and stops the
   gateway with the database unchanged; a pod killed mid-migration leaves it unchanged too, and the
   next start tries again.
4. It logs one line, `job store schema migrated`, with `from`, `to`, `backup`, `legacy` (a
   database from before versioning) and `took`:

   ```
   {"level":"INFO","msg":"job store schema migrated","from":1,"to":2,"backup":"/data/gateway.db.v1.bak","legacy":true,"took":"180ms"}
   ```

   An ordinary restart logs `job store schema is current` with the `version` instead.

The migration takes seconds (the table holds at most a day of rows), but the startup probe should
allow minutes. Leave enough free space on the PVC for one copy of `gateway.db`.

### Rolling back

**To an image that knows migrations** (this release or later): it refuses to start on a newer
schema, with a message naming the backup to restore —
`database schema v2 is newer than this gateway (v1); deploy the newer image, or restore gateway.db.v1.bak`.
Either roll forward again, or restore (below).

**To an image from before versioning** (anything older than this release, including Wega's
`0f9c794`): it does **not** check the schema version. On a version-2 database it would start, and
then every query naming a dropped column would fail. **Always restore the backup before deploying
such an image.**

### Restoring the backup

The image has no shell, and the volume is RWO, so the restore runs in a one-off pod while the
gateway is stopped:

1. **Stop the gateway:** `oc scale deploy/azure-gateway-api --replicas=0`, and wait until its pod
   is gone.
2. **Mount the PVC in a debug pod** with a shell (any UBI or busybox image, running as a uid that
   can write the volume) and, in `/data`:
   1. **delete `gateway.db-wal` and `gateway.db-shm`**: they belong to the newer database, and
      SQLite would otherwise try to apply that WAL to the restored file;
   2. **copy `gateway.db.v<N>.bak` over `gateway.db`** (`cp gateway.db.v1.bak gateway.db`). Keep the
      `.bak` until the old image is confirmed running.
3. Delete the debug pod, set the older image, and scale back to 1.

What a restore costs: every job accepted **after** the backup was taken is gone from the database.
Clients polling those ids get `404`; their blobs become orphans the sweeper removes after an hour.
Jobs in the backup that were unfinished are re-run on boot. Prefer rolling forward when the reason
for the rollback allows it.

**Rule for future changes:** every schema change is a new migration file, pinned by sha256 in
`TestReleasedMigrationsAreUnchanged`. A released migration file is never edited.

---

## 10. Go-live gates

All of these must pass on the real deployment, in this order, before production traffic.

1. **The sync route answers.** `gateway probe` with `PROBE_DOCUMENT` set to a representative
   multi-page PDF reports `:syncAnalyze` **200** on every configured surface (§1). An earlier
   probed DI build listed the route in its swagger and never answered it.
2. **Golden responses are recorded and the parity test passes.**
   `gateway probe --record <dir>` runs a fixed case list against the real containers —
   `:analyze` + poll and `:syncAnalyze` on the same real document; an unknown result id; a missing
   or invalid `api-version`; an unsupported `Content-Type`; a wrong verb and an unknown path;
   `HEAD` and `DELETE` on a result; the metadata and passthrough GETs; the per-parameter
   comparisons; the same for Read — and writes one JSON file per case: the request (method, path
   template, query, `Content-Type`), the status, every response header except `Date`, and the
   body, with GUIDs masked as `<guid>` and timestamps as `<datetime:…>` keeping their format.
   The probed base URL is masked as `<base>`. It writes `<dir>/<surface>/<case>.json`, plus
   `<dir>/<surface>-comparisons.json` holding the sync-equals-async verdict and the per-parameter
   `honoured`/`ignored` verdicts — evidence for a person, not a case to replay. It exits non-zero
   when any case got no answer. Commit each `<dir>/<surface>/` folder as
   `test/conformance/testdata/golden/<surface>/<image-tag>/`; the parity test replays every case
   against the gateway and compares status, header names and shapes, and body keys, order and
   types. Only the operator's network reaches the containers, so the operator records them.
3. **Split results match whole results.** `gateway probe --compare-split <folder>` on a
   representative corpus — **at least 30 PDFs** across your document types. For each PDF it
   analyses the document whole through the container's `:syncAnalyze`, then split, through the
   gateway's own planner, splitter, part calls and merge code (linked into the probe), and diffs
   the two JSONs field by field. It writes `<folder>/compare-report.json` and prints a summary
   grouped by path pattern (e.g. `paragraphs[*].role: 3 differences`), each labelled
   **page-level** (anything under `pages[]`, page-local bounding regions, figures, table cells) or
   **document-level** (paragraph roles, `sections`, the table count, `styles`, `content` at part
   boundaries). It exits non-zero on any page-level difference. The gate:
   - **zero page-level differences**;
   - every **document-level** pattern either has a merge rule that reproduces Azure's
     whole-document answer, with a regression test built from that document, or is explicitly
     accepted by the user.
4. **The Route annotations are in place** on every Azure Route (§3), and the Azure pods'
   `terminationGracePeriodSeconds` covers the slowest analysis.
5. **The real SDKs pass.** `make sdk-test` against the deployed gateway, including the large-PDF
   flows.

Rerun gates 1–3 after any Azure image upgrade. A new image tag gets its own golden directory.

---

## Related documents

| Document | For |
|---|---|
| [`../README.md`](../README.md) | What this is, and the shortest path to running it |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | How it works internally, before you change it |
| [`CONTAINER-BEHAVIOUR.md`](CONTAINER-BEHAVIOUR.md) | What the containers really do versus what they document |
| [`TESTING.md`](TESTING.md) | Test strategy, the local end-to-end script, and how to verify a change |
| [`HANDOFF.md`](HANDOFF.md) | Current state, what is unfinished, how to pick this up |
| [`api-surface.md`](api-surface.md) | What the gateway serves, and the container contract reference |
| [sync-only upstream design](superpowers/specs/2026-10-10-sync-only-upstream-design.md) · [splitting design](superpowers/specs/2026-10-10-large-document-splitting-design.md) | The approved designs |
