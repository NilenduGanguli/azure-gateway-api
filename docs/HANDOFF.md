# Handoff

For whoever picks this up next, human or agent. It says what state the work is in, what is
deliberately unfinished, the things that look wrong but are not, and how to continue without
breaking the guarantees.

Read this before [`ARCHITECTURE.md`](ARCHITECTURE.md). This tells you *why the repo is shaped like
this*; that tells you how it works.

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
| Tests | `go test -race ./...` | 10/10 packages pass |
| Image | `make image` | exit 0, ~5.7 MB, suite runs *inside* the build |
| Git | `git status` | clean, pushed to `origin/main` |
| Publishable | `git clone <url> /tmp/x && cd /tmp/x && go build ./...` | pass |

> **Do not substitute `git status` for that last row.** A `.gitignore` entry of `gateway` — meant
> for the build output — also matched the `cmd/gateway` **directory**, so `cmd/gateway/main.go` was
> never committed. `git status` called the tree clean the whole time, because an ignored file is
> not untracked. Every local check above passed against a published repository that had no `main`
> function in it. The only check that catches this class of error is building a fresh clone.
> `git check-ignore -v <path>` tells you which rule is responsible.

102 test functions: 45 conformance, 57 unit. Coverage ~62% overall — the paths that carry the
guarantees are covered (`ids` 98%, `jsonx` 86%, `azerr` 80%, `store`/`jobs`/`surface` ~70%); the
shortfall is `cmd/gateway` and `internal/probe`, which drive real I/O.

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

**3. Coverage is ~62%, below the 80% bar.** `internal/surface` and `internal/admin` have no direct
unit tests — they are covered only through the conformance suite. `cmd/gateway` and
`internal/probe` are near zero.

**4. The error-shape decision rests on one probe run against one deployment.** If other
environments run different image builds they may not agree. `ERROR_COMPAT=documented` is the
escape hatch. Re-probe per environment.

**5. `:analyzeBatch`, custom models and classifiers are unimplemented**, deliberately — batch is
Azure-Blob-bound and unusable air-gapped. If a client needs them this is new work, not a bug.

**6. No Kubernetes manifests ship in the repo**, by request. [`OPERATIONS.md`](OPERATIONS.md)
contains a working set to copy.

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
