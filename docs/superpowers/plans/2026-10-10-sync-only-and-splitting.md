# Sync-only upstream and large-document splitting — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task by task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** implement the two approved specs end to end:
[sync-only upstream](../specs/2026-10-10-sync-only-upstream-design.md) and
[large-document splitting](../specs/2026-10-10-large-document-splitting-design.md). Every endpoint
is tested.

**Architecture.**
- Every upstream analysis becomes an `upstream.Client.Do` attempt: one synchronous call through
  HAProxy, classified into an outcome.
- A new `engine` drives attempts for whole documents and for split parts:
  - it schedules them through a fair round-robin `sched.Scheduler`;
  - `pdfsplit` cuts PDFs;
  - `merge` stitches part results with a byte-preserving streaming rewriter (`jsonx.Rewrite`).
- `jobs` runs the engine for `:analyze`, and `surface` runs it for a client's `:syncAnalyze`.
- `store` gains versioned migrations.
- `config`, `azerr` and `httpx` shed the removed settings.

**Tech stack.** Go 1.26; `modernc.org/sqlite`; `github.com/pdfcpu/pdfcpu` v0.16.1 (Apache-2.0);
`github.com/rivo/uniseg` v0.4.7 (MIT); Python 3 + `azure-ai-documentintelligence` +
`azure-cognitiveservices-vision-computervision` for `test/sdk`.

---

## Ground rules for every task

- **Location.** Repository `~/imp/azure-gateway-api`, branch `feat/sync-only-upstream`.
  Wave-1 tasks run in their own worktrees (see Execution); everything else runs on the branch
  itself.
- **Commits.**
  - Author `NilenduGanguli <nilendu.ganguli@gmail.com>`. Check with
    `git config user.email` before the first commit in a worktree.
  - **No `Co-Authored-By` or any other trailer.**
  - Conventional subjects (`feat:`, `fix:`, `test:`, `docs:`, `refactor:`, `chore:`).
- **TDD.** Write the failing test, run it and see it fail, implement, see it pass, commit. Each
  task lists its tests; write them first.
- **Before every commit:** `gofmt -l . && go vet ./... && go test -race ./<touched packages>/...`.
  At the end of each wave: `go test -race ./...`.
- **Code style.** Match the surrounding code:
  - comments explain *why*, as the existing ones do;
  - table-driven tests;
  - `t.Helper()` in helpers;
  - no new dependency beyond pdfcpu and uniseg.
- **Hard rules from the repo** (`docs/HANDOFF.md`, "Traps"):
  - the client's 202 is written only after the input blob is fsynced and the row committed;
  - cancellation is classified on the context first;
  - slots are released on every return path;
  - no `Retry-After` on a non-retriable status;
  - one gateway replica.

## File map

| Path | Status | Responsibility |
|---|---|---|
| `internal/pdftest/pdftest.go` | new | Builds small valid PDFs whose page *i* has MediaBox width `600+i`, so fakes can tell which original pages a part holds |
| `internal/textunit/textunit.go` | new | `stringIndexType` units and lengths (graphemes, code points, UTF-16) |
| `internal/store/migrate.go`, `internal/store/migrations/*.sql` | new | Versioned migrations: `PRAGMA user_version`, backup, refuse newer schema |
| `internal/store/db.go` | modify | `Open` migrates; job fields follow migration 0002 |
| `internal/pdfsplit/{detect,pages,plan,split}.go` | new | PDF detection, Azure `pages` grammar, balanced planner, pdfcpu split with semaphore, budget and panic recovery |
| `internal/sched/sched.go` | new | Fair round-robin slot scheduler with `TryAcquireIdle` |
| `internal/jsonx/rewrite.go` | new | Streaming path-aware JSON rewriter that copies bytes verbatim |
| `internal/jsonx/object.go` | new | Top-level member ranges of an object, and array item iteration over byte ranges |
| `internal/merge/{di,read,check,sections,separator}.go` | new | Part merging per spec §5 |
| `internal/docgen/docgen.go` | new | Deterministic DI and Read results for any page set; feeds fakes and round-trip tests |
| `internal/mockazure/mockazure.go` | modify | Real Read v3.2 body; failure modes `BusyFirst`, `StatusFirst`, `ResetMidBody`, `Capacity` |
| `internal/mockazure/paged.go` | new | Paged fake: reads part PDFs' MediaBoxes, answers from `docgen`, with per-page hangs and failures |
| `cmd/fakeazure/main.go` | new | Runs paged fakes on ports for live end-to-end tests (dev only, not in the image) |
| `internal/config/config.go` | modify | 21 + 1 settings, optional upstreams, warnings for removed settings, retry-after constants |
| `internal/azerr/azerr.go` | modify | Drop the `documented` mode |
| `internal/httpx/httpx.go` | modify | `BaseURLResolver` = `PUBLIC_BASE_URL` or `Host`; no `apim-request-id` header; `ServerHeader` middleware |
| `internal/upstream/{client,attempt,classify,result,retry,health,proxy}.go` | new or modify | One sync attempt, outcome classes, backoff; `poll.go`, `sync.go`, the affinity code and artifacts are deleted at the end |
| `internal/engine/{engine,whole,parts,hedge,workdir,metrics}.go` | new | Orchestration per splitting spec §3–§4 |
| `internal/jobs/manager.go`, `recover.go` | modify | Run the engine; scheduler replaces the `upstream` channel; runners only for configured surfaces; work-folder sweep |
| `internal/surface/{surface,di,read,params,passthrough,sync}.go` | modify or new | Parameter policy, engine-backed sync route, artifact 404s, GET passthroughs, configured surfaces only |
| `internal/app/app.go` | modify | New `BaseURLResolver`, `ServerHeader` middleware |
| `internal/admin/admin.go` | modify | Health without `syncAnalyze`, config view, jobs view (`upstreamAttempts`, `upstreamParts`), engine metrics |
| `internal/probe/{probe,record,compare,diff}.go` | modify or new | Configured surfaces, `PROBE_DOCUMENT`/`PROBE_TIMEOUT`, `--record`, `--compare-split`, sync-only verdict |
| `cmd/gateway/main.go` | modify | Wiring, migration log line, probe flags |
| `test/conformance/*` | modify or new | Harness on the new config; every endpoint; splitting; parity scaffold |
| `test/sdk/sdk_test.py`, `run.sh` | modify | Large-PDF flows through the real SDKs |
| `scripts/e2e-local.sh` | new | Builds the gateway and fakes, runs every endpoint plus the SDK tests live |
| `README.md`, `docs/*.md` | modify | Settings table, architecture, operations, testing, handoff, API-surface notes |

## Execution

| Wave | Tasks | How |
|---|---|---|
| 0 | T0 | On the branch, by the controller |
| 1 | T1 store migrations, T2 pdfsplit, T3 sched, T4 jsonx rewriter + textunit + docgen + merge, T5a mockazure Read body + failure modes | In parallel, one git worktree each (`git worktree add ../agw-<task> -b wt/<task>`), merged back in that order |
| 1.5 | T5b paged fake + `cmd/fakeazure` | Needs T2's helpers and T4's `docgen` |
| 2 | T6 config/azerr/httpx → T7 upstream → T8 engine → T9 jobs + migration 0002 → T10 surface + app → T11 admin → T12 delete dead code | Sequential, on the branch. Every commit builds and passes. |
| 3 | T13 probe, T14 conformance (every endpoint), T15 SDK + live end-to-end, T16 docs | In parallel worktrees, merged back |
| 4 | T17 full gate, review, re-import into `wega-unified-services` | Controller |

---

### T0: dependencies and the test-PDF helper

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/pdftest/pdftest.go`, `internal/pdftest/pdftest_test.go`

- [ ] **Step 1. Write the failing test.**

```go
package pdftest

import (
	"bytes"
	"context"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
)

func TestMakeParsesWithDistinctWidths(t *testing.T) {
	data := Make(7)
	conf := model.NewDefaultConfiguration()
	conf.ValidationMode = model.ValidationRelaxed
	ctx, err := api.ReadAndValidate(context.Background(), bytes.NewReader(data), conf)
	if err != nil {
		t.Fatalf("pdfcpu cannot read the generated PDF: %v", err)
	}
	if ctx.PageCount != 7 {
		t.Fatalf("PageCount = %d, want 7", ctx.PageCount)
	}
	dims, err := ctx.PageDims(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range dims {
		if got := PageOf(d.Width); got != i+1 {
			t.Fatalf("page %d width %v maps to %d", i+1, d.Width, got)
		}
	}
}
```

- [ ] **Step 2.** Run `go get github.com/pdfcpu/pdfcpu@v0.16.1 github.com/rivo/uniseg@v0.4.7`,
  then `go test ./internal/pdftest/`. Expected: FAIL (`Make` undefined).
- [ ] **Step 3. Implement `pdftest.go`.**
  - Each page has its own MediaBox `[0 0 600+i 800]` and a content stream `BT /F1 24 Tf 72 700 Td
    (PAGE i) Tj ET`.
  - One shared Helvetica font.
  - The xref is built from byte offsets.

```go
// Package pdftest builds small, valid PDFs for tests.
//
// Page i (1-based) has a MediaBox width of BaseWidth+i points. A split part carries the original
// page objects, so a fake container can recover which original pages a part holds from the
// widths alone, without trusting anything the gateway sends alongside the document.
package pdftest

import (
	"bytes"
	"fmt"
)

// BaseWidth is the width of a notional page 0.
const BaseWidth = 600

// PageOf maps a MediaBox width back to its original page number.
func PageOf(width float64) int { return int(width+0.5) - BaseWidth }

// Make returns a PDF with n pages.
func Make(n int) []byte {
	var b bytes.Buffer
	offsets := make([]int, 0, 3+2*n)
	obj := func(body string) {
		offsets = append(offsets, b.Len())
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", len(offsets), body)
	}
	b.WriteString("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n")
	// 1: catalog, 2: pages, 3: font, then page/content pairs.
	kids := new(bytes.Buffer)
	for i := 0; i < n; i++ {
		fmt.Fprintf(kids, "%d 0 R ", 4+2*i)
	}
	obj("<< /Type /Catalog /Pages 2 0 R >>")
	obj(fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids.String(), n))
	obj("<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i := 1; i <= n; i++ {
		stream := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (PAGE %d) Tj ET", i)
		obj(fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d 800] "+
			"/Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", BaseWidth+i, 5+2*(i-1)))
		obj(fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes()
}
```

- [ ] **Step 4.** `go test ./internal/pdftest/ && go mod tidy`. Expected: PASS. pdfcpu and uniseg
  are direct requires once T2 and T4 import them; until then `tidy` keeps pdfcpu only.
- [ ] **Step 5.** Commit: `chore: add pdfcpu and the test-PDF helper`.

---

### T1: versioned schema migrations (wave 1)

**Files:**
- Create: `internal/store/migrate.go`, `internal/store/migrations/0001_initial.sql`,
  `internal/store/migrate_test.go`
- Modify: `internal/store/db.go`. `Open` calls `migrate`, the `schema` const is deleted, and
  `Store` gains `Migration MigrationReport`.

**Interface:**

```go
// MigrationReport describes what Open did to the schema, for the startup log line.
type MigrationReport struct {
	From, To int           // schema versions before and after; equal when nothing ran
	Backup   string        // path of the VACUUM INTO copy, "" when none was taken
	Took     time.Duration
}

//go:embed migrations/*.sql
var migrationFS embed.FS

// SchemaVersion is the highest migration this binary carries.
func SchemaVersion() int

// migrate implements spec §4 steps 1–5 on the writer connection.
func migrate(w *sql.DB, dataDir string) (MigrationReport, error)
```

**Rules** (sync-only spec §4):
- `0001_initial.sql` is the current `schema` const verbatim: `CREATE TABLE IF NOT EXISTS jobs …`
  plus the 3 indexes.
- Files are named `NNNN_name.sql`. Loading fails at startup on a gap or duplicate number.
- `user_version == SchemaVersion()` → return immediately, no writes.
- `user_version == 0` and the `jobs` table exists → treat as version 1. Set `user_version = 1`
  inside the migration transaction; no backup when 1 is already the target.
- `user_version == 0` and no table → fresh volume: run everything, no backup.
- `user_version > SchemaVersion()` → error `store: database schema v%d is newer than this gateway
  (v%d); deploy the newer image, or restore %s`, naming `gateway.db.v<M>.bak`.
- Otherwise:
  1. `VACUUM INTO '<dataDir>/gateway.db.v<from>.bak'`, after removing any `gateway.db.v*.bak`;
  2. `BEGIN IMMEDIATE`;
  3. apply each pending file (split on `;` at line end; no triggers, so this is safe);
  4. `PRAGMA user_version = N`;
  5. `COMMIT`.

  On error, `ROLLBACK` and return the error.

**Tests** (`migrate_test.go`, each with `t.TempDir()`):
1. `TestFreshVolumeReachesLatest`: `Open` on an empty dir gives `user_version == SchemaVersion()`,
   no `.bak`, and `Report.From == 0`.
2. `TestLegacyDatabaseIsVersionOne`: create `gateway.db` with the old `schema` text and no
   `user_version`, insert a row, `Open`. Expect version = latest, the row is still readable, and
   `From == 1`. With only 0001 present, no backup is taken.
3. `TestEqualVersionDoesNoWrites`: `Open`, close, record the file mtime and size, `Open` again.
   Both are unchanged and `Report.From == Report.To`.
4. `TestNewerSchemaIsRefused`: set `PRAGMA user_version = 99`, then `Open` returns an error
   containing `newer than this gateway`.
5. `TestFailedMigrationRollsBack`: inject an extra in-memory migration with a broken statement
   through an unexported `migrations` variable the test can append to. `Open` errors, and
   `user_version` and the table are unchanged.
6. `TestBackupTakenBeforeMigrating`: same injection with a valid statement. The `.bak` exists
   and opens as SQLite with the old `user_version`.
7. `TestSchemaFromScratchEqualsMigrated`: the fresh-volume `sqlite_master` and the
   legacy-migrated `sqlite_master` (excluding `sqlite_sequence`) are identical. This test stays
   to guard every future migration.

- [ ] Steps: write tests 1–7 → fail → implement → pass → `go test -race ./internal/store/` →
  commit `feat(store): versioned schema migrations with backup and rollback refusal`.

---

### T2: `pdfsplit` (wave 1)

**Files:** create `internal/pdfsplit/{detect,pages,plan,split}.go` and `*_test.go`.

**Interface:**

```go
package pdfsplit

// IsPDF reports whether head (the first bytes of a body) contains "%PDF-" within 1024 bytes.
func IsPDF(head []byte) bool

// PageRange is one term of Azure's pages grammar.
type PageRange struct{ First, Last int }

// ParsePages parses "1-3,5,7-9" (whitespace around terms tolerated). ok is false for anything
// else, including open ranges, zero, or First > Last.
func ParsePages(spec string) (ranges []PageRange, ok bool)

// Select expands ranges into ascending, de-duplicated page numbers, all within 1..count.
func Select(ranges []PageRange, count int) (pages []int, ok bool)

// FormatPages renders pages in the grammar, collapsing runs: [3 4 5 9] -> "3-5,9".
func FormatPages(pages []int) string

// Part is a run of original pages analysed together, ascending.
type Part struct{ Pages []int }

// Label is FormatPages(p.Pages), used in logs and file names.
func (p Part) Label() string

// Plan cuts pages into k = ceil(n/max) parts; the first n mod k get one page more.
// With n <= max it returns one part. max < 1 panics (a programming error).
func Plan(pages []int, max int) []Part

// Halve splits a part of n >= 2 pages into ceil(n/2) and the rest.
func (p Part) Halve() (Part, Part)

var (
	ErrEncrypted   = errors.New("pdfsplit: document is encrypted")
	ErrUnparseable = errors.New("pdfsplit: document cannot be parsed")
	ErrBudget      = errors.New("pdfsplit: split budget exceeded")
)

// Splitter bounds concurrent parses and the time each may take.
type Splitter struct{ /* sem chan struct{}; budget time.Duration */ }

func NewSplitter(concurrency int, budget time.Duration) *Splitter

// Document is a parsed PDF holding one concurrency slot until Close.
type Document struct{ PageCount int /* ctx *model.Context; deadline context */ }

// Open parses the file at path within the budget. Encrypted PDFs give ErrEncrypted; parse
// failures and recovered pdfcpu panics give ErrUnparseable; a budget overrun gives ErrBudget.
// It waits for a slot (honouring ctx).
func (s *Splitter) Open(ctx context.Context, path string) (*Document, error)

// WritePart writes the given original pages (1-based) as a standalone PDF to w, within the
// document's remaining budget.
func (d *Document) WritePart(ctx context.Context, pages []int, w io.Writer) error

// Close releases the parsed model and the slot. Safe to call twice.
func (d *Document) Close()

// Recut writes pages (1-based within src) of an existing part file to w; used for halving.
func (s *Splitter) Recut(ctx context.Context, srcPath string, local []int, w io.Writer) error
```

**Implementation notes.**
- Configuration: `conf := model.NewDefaultConfiguration(); conf.ValidationMode =
  model.ValidationRelaxed`.
- Parse with `api.ReadAndValidate(budgetCtx, f, conf)`. Plain `ReadContext` leaves `PageCount`
  at 0; the T0 helper test learned this. Page sizes come from `ctx.PageDims(c)`, which takes a
  context in v0.16.
- Encrypted: check `ctx.E != nil` (or the pdfcpu encryption error) and return `ErrEncrypted`.
- Cut with `pdfcpu.ExtractPages(budgetCtx, d.ctx, pages, true)`, then
  `api.WriteContext(budgetCtx, part, w)`.
- Wrap every pdfcpu call in a `recover()` that maps a panic to `ErrUnparseable`.
- Map `context.DeadlineExceeded` from the budget context to `ErrBudget`, but return the caller's
  `ctx.Err()` unchanged.

**Tests.**
1. `IsPDF`: header at 0, header after 100 junk bytes, header after 1100 bytes (false), empty.
2. `ParsePages`, table-driven:
   - accepted: `"1"`, `"1-3"`, `"1-3,5,7-9"`, `" 2 - 4 , 6 "`;
   - rejected: `"0"`, `"3-1"`, `"1-"`, `"a"`, `""`, `"1,,2"`.
3. `Select`: overlaps are de-duplicated, out-of-range gives ok=false, order is ascending.
4. `FormatPages`: `[1]→"1"`, `[1 2 3]→"1-3"`, `[3 4 5 9]→"3-5,9"`, `[2 4]→"2,4"`.
5. `Plan`, table-driven:
   - `n=1..5` → one part;
   - `6→[3,3]`, `7→[4,3]`, `11→[4,4,3]`, `16→[4,4,4,4]`, `21→[5,4,4,4,4]`;
   - `n=2000, max=5` → 400 parts, all of size 5;
   - every part between `ceil(max/2)` and `max` for `n>max`;
   - parts concatenated equal the input;
   - a non-contiguous selection `[1 2 3 10 11 12 13]` → `[1 2 3 10] [11 12 13]`.
6. `Halve`: `5→3+2`, `2→1+1`, `3→2+1`.
7. Splitting:
   - `pdftest.Make(12)`, `Open` → `PageCount 12`;
   - `WritePart([4,5,6])` → reparse gives 3 pages with widths 604, 605, 606 (via
     `pdftest.PageOf`);
   - `Recut` of that part with `[2]` → 1 page, width 605.
8. Garbage bytes → `ErrUnparseable`.
9. An encrypted copy, made with `api.Encrypt` and a user password, → `ErrEncrypted`.
10. Budget: `NewSplitter(1, time.Nanosecond)` on `pdftest.Make(200)` → `ErrBudget`.
11. Concurrency: with `NewSplitter(2, time.Minute)`, a third `Open` blocks until one `Close`.
    Assert with channels and a 2-second timeout; no sleeps longer than 50 ms.
12. Cancellation: a waiting `Open` returns `ctx.Err()` when its context is cancelled.

- [ ] Steps: tests → fail → implement → pass → commit `feat(pdfsplit): PDF detection, pages
  grammar, balanced planner and bounded pdfcpu splitting`.

---

### T3: `sched` (wave 1)

**Files:** create `internal/sched/sched.go` and `sched_test.go`.

**Interface:**

```go
package sched

// Scheduler hands out a fixed number of slots. Waiting callers are grouped by job; a freed slot
// goes to the next job, in round-robin order, that has a waiter (FIFO within a job).
type Scheduler struct{ /* mu; limit, inUse int; ring []string; next int; waiters map[string][]*waiter */ }

type Stats struct{ InUse, Waiting, Limit int }

func New(limit int) *Scheduler // limit < 1 is treated as 1

// Acquire blocks until a slot is granted for job or ctx is done. release is idempotent.
func (s *Scheduler) Acquire(ctx context.Context, job string) (release func(), err error)

// TryAcquireIdle grants a slot only when one is free and nobody is waiting. Hedges use it.
func (s *Scheduler) TryAcquireIdle() (release func(), ok bool)

func (s *Scheduler) Stats() Stats
```

**Implementation notes.**
- A grant to a waiter is handed over under the mutex: `waiter.ch <- struct{}{}` on a buffered
  channel of 1, then `inUse++`.
- A waiter whose context ends removes itself under the mutex. If it was already granted, it
  releases the slot it got.
- The round-robin ring holds job ids with at least one waiter, in the order they first waited.
  `next` advances past the last granted job.

**Tests.**
1. `TestLimit`: with limit 2, a third `Acquire` blocks, and a release unblocks it.
2. `TestRoundRobin`: limit 1, slot held.
   - Job A enqueues 3 waiters, then job B enqueues 1. Releasing in turn grants A1, B1, A2, A3.
   - Record grant order through channels. B is served before A's remaining waiters.
3. `TestFIFOWithinJob`: one job's waiters are granted in arrival order.
4. `TestCancelledWaiterLeavesQueue`: cancel a waiter's context, release the holder. The next
   waiter gets the slot, and `Stats().Waiting == 0` afterwards.
5. `TestCancelAfterGrantReturnsSlot`: race a grant against a cancellation. Run 1,000 iterations
   under `-race`; `InUse` always returns to 0.
6. `TestIdleOnlyWhenNoWaiters`: `TryAcquireIdle` fails while someone waits, even if a slot is
   about to free, and succeeds when free with no waiters.
7. `TestReleaseIdempotent`: double release does not over-free (`InUse` never negative, the limit
   holds).
8. `TestStress`: 50 goroutines × 20 acquires across 5 jobs. `InUse` never exceeds the limit
   (checked inside the critical section with an atomic counter) and everything completes.

- [ ] Steps: tests → fail → implement → pass (`-race -count=3`) → commit `feat(sched): fair
  round-robin slot scheduler with idle-only hedging`.

---

### T4: `jsonx` rewriter, `textunit`, `docgen`, `merge` (wave 1)

**Files:**
- `internal/jsonx/rewrite.go`, `object.go` and tests
- `internal/textunit/textunit.go` and test
- `internal/docgen/docgen.go` and test
- `internal/merge/{di,read,check,sections,separator}.go` and tests (`merge_test.go`,
  `roundtrip_test.go`)

#### T4a: `textunit`

```go
package textunit

type Unit int

const (
	TextElements Unit = iota // extended grapheme clusters, UAX #29 (Azure's default)
	UnicodeCodePoint
	UTF16CodeUnit
)

// Parse maps a stringIndexType value; "" is TextElements. Unknown values give ok=false.
func Parse(v string) (Unit, bool)

// Len returns the length of s in u.
func Len(s string, u Unit) int // uniseg.GraphemeClusterCount / utf8.RuneCountInString / UTF-16 units
```

**Tests:** `"abc"` (3/3/3), `"é"` decomposed `é` (1/2/2), `"👩‍👩‍👧"` (1/5/8), `"🇮🇳"` (1/2/4),
`"\n"` (1/1/1).

#### T4b: `jsonx` rewriter and object helpers

```go
// PathElem is one step into a JSON value: an object key, or an array index.
type PathElem struct {
	Key     string
	Index   int
	IsIndex bool
}

// Kind is a scalar's JSON type.
type Kind int

const (
	String Kind = iota
	Number
	Bool
	Null
)

// Visitor sees every scalar. It returns replacement raw JSON bytes and true to replace, or
// false to copy the original bytes. Strings are passed raw, with quotes and escapes intact.
// Strings longer than MaxVisitString are copied without a visit.
type Visitor func(path []PathElem, kind Kind, raw []byte) ([]byte, bool)

const MaxVisitString = 4096

// Rewrite copies one JSON value from r to w byte for byte, applying visit. The path given to
// the visitor starts with prefix.
func Rewrite(w io.Writer, r io.Reader, prefix []PathElem, visit Visitor) error

// ArrayItems copies the items of the JSON array read from r to w, rewritten, separated by commas
// and without the enclosing brackets. It returns the number of items written. Paths start at
// prefix + the item index (offset by base, so a caller can number items across parts).
func ArrayItems(w io.Writer, r io.Reader, prefix []PathElem, base int, visit Visitor) (int, error)

// Member is one top-level member of an object: its name and the byte range of its value.
type Member struct {
	Name       string
	Start, End int64
}

// Members lists the top-level members of the object occupying ra[start:end], in order.
func Members(ra io.ReaderAt, start, end int64) ([]Member, error)

// CountItems counts the items of the array at ra[start:end] without materialising them.
func CountItems(ra io.ReaderAt, start, end int64) (int, error)
```

**Tests:**
1. **Identity:** for a corpus of JSON (escapes `\" \\ é 😀`, nested arrays, empty
   objects and arrays, numbers like `-0.0`, `1e-07`, `12345678901234567890`), `Rewrite` with a
   nil-replacing visitor gives output identical to the input.
2. **Targeted:** replace every `offset` under `span` → only those bytes change.
3. **Paths:** the visitor sees `[pages 0 words 2 span offset]` for that number.
4. **Long strings:** a 10 KB string is copied unvisited.
5. **Errors:** truncated input and trailing garbage give an error.
6. **`ArrayItems`:** `[]` → 0 items, no bytes; `[1,2]` → `1,2`; `base` shifts the visited index.
7. **`Members` / `CountItems`:** byte ranges round-trip through `ReadAt`.
8. **Benchmark** `BenchmarkRewrite100MB` must stay under 64 MiB of allocations
   (`b.ReportAllocs`; assert in a non-benchmark test with `testing.AllocsPerRun` on a 10 MB
   input).

#### T4c: `docgen`

Deterministic results so that merging the parts of a document reproduces the whole document
byte for byte. This verifies the merge mechanics. Fidelity to real Azure comes from the probe
(T13).

```go
package docgen

// Numbering selects how the result numbers its pages.
type Numbering int

const (
	Original Numbering = iota // whole document, or a pages= request on the original
	Local                     // a physically split part: pages numbered 1..k
)

type Options struct {
	Unit textunit.Unit
	// TitleOnFirstPage gives the first analysed page's heading role "title" instead of
	// "sectionHeading", imitating a document-level judgement so compare-split has a difference
	// to report.
	TitleOnFirstPage bool
}

// DI returns the bare AnalyzeResult JSON for the given original pages analysed together.
func DI(pages []int, n Numbering, o Options) []byte

// Read returns the bare Read v3.2 analyzeResult JSON for the given pages.
func Read(pages []int, n Numbering) []byte
```

**DI content for original page `p`.** Write the JSON by hand with `strconv` so the bytes are
stable.
- **Member order:** `apiVersion`, `modelId`, `stringIndexType`, `content`, `pages`, `paragraphs`,
  `tables`, `figures`, `sections`, `contentFormat`.
- **Lines:**
  - `Heading P<p>`, a paragraph with role `sectionHeading` (or `title`, see Options);
  - `Line <p>.1 naïve 👩‍👩‍👧 text`;
  - `Line <p>.2 plain`.
- **Words:** one per space-separated token, each with `span`, `polygon` (8 floats derived from
  `p` and the index, formatted with `strconv.FormatFloat(v, 'f', -1, 64)`) and `confidence`.
- **Lines array** with `spans`.
- **Page fields:** `pageNumber` (as numbered), `angle` 0, `width` 8.5, `height` 11, `unit "inch"`,
  page `spans`.
- **A table when `p%3 == 0`:**
  - 1×2 with cells `A<p>` and `B<p>`, each a paragraph;
  - cells carry `elements: ["/paragraphs/<i>"]`;
  - `boundingRegions` with `pageNumber`.
- **A figure when `p%4 == 0`:**
  - `id "<n>.1"`, where `n` is the result's page number;
  - `boundingRegions`, `spans`;
  - `elements` pointing at a caption paragraph `Figure <p>`.
- **Content:** each page's lines joined with `\n`, pages joined with `\n`. Offsets are in
  `o.Unit`.
- **Sections:**
  - section 0 is the root, `spans [{0, total}]`, `elements ["/sections/1".."/sections/k"]`;
  - section *i* covers page *i* of the result, with its paragraph, table and figure pointers and
    the page's span.

**Read for `p`.** `readResults[i] = {page, angle 0, width 600+p, height 800, unit "pixel", lines:
[{boundingBox, text "Read line <p>", words: [...]}]}`. Top-level member order: `version
"3.2.0"`, `modelVersion "2022-04-30"`, `readResults`.

**Tests:**
- valid JSON;
- spans inside content bounds, measured in the unit;
- pointers resolve;
- the same input gives the same bytes.

#### T4d: `merge`

```go
package merge

// ErrCheck wraps every safety-check failure; the engine falls back to a whole-document analysis.
var ErrCheck = errors.New("merge: safety check failed")

// Part is one part's analyzeResult on disk.
type Part struct {
	Path       string
	Start, End int64 // byte range of the bare AnalyzeResult object
	Pages      []int // original pages this part covered, ascending
}

type Stats struct {
	Pages  int
	Bytes  int64
	Counts map[string]int
}

// DI writes the merged AnalyzeResult object to w.
func DI(w io.Writer, parts []Part, unit textunit.Unit) (Stats, error)

// Read writes the merged Read analyzeResult object to w.
func Read(w io.Writer, parts []Part) (Stats, error)
```

**Algorithm** (splitting spec §5):
1. **Measure each part** (`check.go`):
   - top-level members (`jsonx.Members`);
   - the raw scalars (`apiVersion`, `modelId`, `stringIndexType`, `contentFormat`) must equal
     part 0's, else `ErrCheck`;
   - `content`: decode with `encoding/json` into a string and take `textunit.Len`;
   - `CountItems` for `paragraphs`, `tables`, `figures`, `sections`, `keyValuePairs`;
   - the page numbers in `pages[*].pageNumber`, read through `Rewrite` with a collecting visitor;
   - any non-array, non-string member must equal part 0's bytes, else `ErrCheck`.
2. **Page map per part** (`check.go`):
   - returned numbers ⊆ `1..len(Pages)` → `local i → Pages[i-1]`;
   - else ⊆ the `Pages` set → identity;
   - else `ErrCheck`.

   The union across parts must equal the planned pages, each exactly once and ascending, else
   `ErrCheck`.
3. **Shifts.**
   - `char[k] = Σ_{j<k} (len_j + sepLen)`.
   - `count[k][member] = Σ_{j<k} count_j[member]`.
   - Sections: `secBase[k] = 1 + Σ_{j<k} (sections_j − 1)`.
4. **Write** `{` and the members in part 0's order, appending names first seen in later parts:
   - **scalars:** part 0's raw bytes;
   - **`content`:** `"` + each part's raw string body (between its quotes) + the separator raw
     between parts (`separator.go`, see below) + `"`;
   - **`sections`:** `sections.go`, see below;
   - **arrays:** `[` + `jsonx.ArrayItems` of each part with that part's visitor + `]`;
   - **other members:** part 0's raw bytes.
5. **Visitor for part k:**
   - `Number` at a path ending `…, "span", "offset"` or `…, "spans", <i>, "offset"` → `+char[k]`;
   - `Number` at `pages, <i>, "pageNumber"` or `…, "boundingRegions", <i>, "pageNumber"` → page map;
   - `String` inside an `elements` array matching `^"/(paragraphs|tables|figures|sections|keyValuePairs)/(\d+)"$`
     → `+count[k][member]`; `sections` uses `sectionIndex(k, i)`;
   - `String` at `figures, <i>, "id"` matching `^"(\d+)\.(.+)"$` → page map on the first group.
6. **`sections.go`.**
   - Read every part's `sections` item 0 with `encoding/json` into an ordered key list. A small
     decoder walks tokens and keeps `json.RawMessage` values in key order.
   - Merged root, keys in part 0's root order:
     - `elements` = all parts' root elements rewritten with their visitor;
     - `spans` = `[{"offset":<part 0 root offset>,"length":<end of last part's root span after
       shift − part 0 root offset>}]`;
     - other keys from part 0.
   - Then the items 1.. of each part through `ArrayItems` with the visitor.
   - `sectionIndex(k, 0) = 0`; `sectionIndex(k, i≥1) = secBase[k] + i − 1`.
7. **`separator.go`.**
   - The decoded separator is `"\n"` for `text` or an absent `contentFormat`, and
     `"\n<!-- PageBreak -->\n"` for `markdown`.
   - The raw form is the JSON escape of that. When part 0's raw content contains `<`, `<`
     and `>` are written as `<` and `>` to match the container's escaping style.
   - `sepLen` is `textunit.Len(decoded, unit)`.

**Read merge:**
- `version` and `modelVersion` must agree;
- `readResults` concatenated;
- a visitor maps `readResults, <i>, "page"`;
- the same page-coverage check.

**Tests:**
1. **Round-trip, DI** (`roundtrip_test.go`): for `n ∈ {1, 2, 6, 7, 11, 23}`, each unit, and
   `TitleOnFirstPage=false`:
   - `whole := docgen.DI(1..n, Original)`;
   - parts from a test-local `plan(n, 5)` helper, so this wave-1 task does not depend on T2:
     `k = ceil(n/5)`, the first `n%k` parts get one page more. Each part is
     `docgen.DI(part, Local)` written to a temp file;
   - `merge.DI` → bytes equal `whole`.

   Repeat with `Original` numbering for the parts (the `pages=` re-check case), and with a mix:
   one part Original, the rest Local.
2. **Round-trip, Read:** the same with `docgen.Read`.
3. **Checks:**
   - mismatched `stringIndexType` → `ErrCheck`;
   - a missing page → `ErrCheck`;
   - a duplicated page → `ErrCheck`;
   - parts out of order → `ErrCheck`;
   - an unknown object member that differs → `ErrCheck`.
4. **Separator:** markdown parts give `\n<!-- PageBreak -->\n`, and the escaped style is followed.
5. **Unknown array member** (`warnings` with a `spans` element) is concatenated and shifted.
6. **Memory:** a 60 MB merged result allocates under 32 MiB (`testing.AllocsPerRun`, or
   `runtime.MemStats` peak sampling).

- [ ] Steps: T4a → T4b → T4c → T4d, each test-first, one commit each: `feat(textunit)…`,
  `feat(jsonx): streaming byte-preserving rewriter`, `test(docgen): deterministic DI and Read
  results`, `feat(merge): page-exact DI and Read merges`.

---

### T5a: `mockazure` fidelity and failure modes (wave 1)

**Files:** modify `internal/mockazure/mockazure.go` and its tests.

**Changes:**
1. The Read fake's `analyzeResult` becomes a real v3.2 body: `version`, `modelVersion`,
   `readResults[]` with one page (`page 1`, `lines` with `boundingBox`, `text`, `words`). The DI
   fake keeps its Layout body.
2. New `Options` fields:

```go
	// BusyFirst answers the first n synchronous calls with 503 + Retry-After: 1, as HAProxy does
	// when every pod is at its connection cap.
	BusyFirst int
	// StatusFirst / StatusFirstCode answer the first n synchronous calls with that status and a
	// container-shaped error body (502, 504, 500, 408, 429 are the interesting ones).
	StatusFirst     int
	StatusFirstCode int
	// ResetMidBody writes half of a 200 body and closes the connection (http.Hijacker).
	ResetMidBody bool
	// Capacity answers 503 while more than this many synchronous calls are in flight; 0 = unlimited.
	Capacity int
```

3. `Container.SyncCalls() int64`: the count of synchronous analyze requests received.

**Tests:**
- each mode's status sequence;
- the Read body has `readResults`;
- `Capacity` gives 503 only above the cap. Run concurrent calls with `Latency`.

- [ ] Commit `test(mockazure): real Read v3.2 body and HAProxy-style failure modes`.

### T5b: paged fake and `cmd/fakeazure` (wave 1.5)

**Files:** create `internal/mockazure/paged.go`, `paged_test.go`, `cmd/fakeazure/main.go`.

```go
// PagedOptions configures a fake that answers per page from docgen.
type PagedOptions struct {
	Surface    Surface        // DI or Read
	Doc        docgen.Options // passed to docgen
	Hang       map[int]bool   // any call containing one of these original pages hangs until the client gives up
	FailFirst  map[int]int    // page -> number of calls containing it that answer 503 before succeeding
	BadContent map[int]bool   // calls containing the page answer a terminal InvalidContent...
	BadOnlyWhenSplit bool     // ...only when the body is a split part (no pages= on a full original)
	Latency    time.Duration
	APIKey     string
}

func NewPaged(o PagedOptions) *Container

// Calls returns, for every synchronous analyze served, the original pages it covered.
func (c *Container) Calls() [][]int
```

**Behaviour.**
- Each `:syncAnalyze` (DI) or `read/syncAnalyze` (Read) parses the body with pdfcpu and maps page
  widths with `pdftest.PageOf`. It honours `pages=` by selecting those page numbers of the body.
- Numbering:
  - when the body's first page width maps to 1 and the body has exactly the selected pages, the
    result uses Original numbering (a whole document or a `pages=` request);
  - when the body is a part, it uses Local numbering unless `pages=` was given.
- It returns 200 with the operation envelope (DI) or the Read envelope, `status succeeded`.
- `:analyze` + poll are also served, so the probe's whole-document async comparison works.
- Also served:
  - `/status` `200 {"apiStatus":"Valid"}`, `/ready`;
  - `/` HTML;
  - `/swagger` and `/swagger/v1/swagger.json`;
  - `/records/usage-logs`;
  - `{fam}/info`, `/documentModels`, `/documentModels/{id}`, `{fam}/operations`, `/operations/{id}`
    (empty list).

**`cmd/fakeazure`:** flags `-di :5001 -read :5002 -hang 7 -fail-first 3:2 -title-first`. It
serves until SIGINT.

**Tests:**
- a 12-page PDF → result pages 1..12;
- a part `[4 5 6]` → Local numbering;
- `pages=4-6` on the full PDF → Original numbering;
- a hang page blocks until the request context ends;
- fail-first counts down;
- bad content is answered only when split.

- [ ] Commit `test(mockazure): paged fake backed by docgen, and the fakeazure command`.

---

### T6: config, azerr, httpx cleanup (wave 2)

**Files:** modify `internal/config/config.go` and test, `internal/azerr/azerr.go` and test,
`internal/httpx/httpx.go` and test, `internal/app/app.go`, `cmd/gateway/main.go`,
`test/conformance/harness_test.go`.

**`config` changes:**
- **Remove** the fields and env vars no surviving code needs: `ErrorCompat`,
  `TrustForwardedHeaders`, `TrustedForwardedHosts`, `AllowNetworkFS`, `PollRetryAfter`,
  `BusyRetryAfter`.
- **Keep for now** the five that the old upstream code still reads (`DISyncMode`/`SyncMode`,
  `DISyncProbeTimeout`, `DIBlindPollBudget`, `ReadBlindPollBudget`, `ArtifactFetchTimeout`). T12
  removes them together with that code, so every commit builds.
- **Add:**

```go
	// SplitPages is the most pages one part may hold; 0 turns splitting off.
	SplitPages int
```

  read from `SPLIT_PAGES`, default 5, validated `>= 0`.
- **Add constants:**

```go
// RetryAfterPoll is the integer-seconds Retry-After on a 202 and on in-progress polls.
const RetryAfterPoll = 1

// RetryAfterBusy is the integer-seconds Retry-After on the gateway's own 429.
const RetryAfterBusy = 5

// removedSettings are recognised so an old manifest still starts, with a warning naming each.
// T6 lists six; T12 appends DI_SYNC_ANALYZE, DI_SYNC_PROBE_TIMEOUT, DI_BLIND_POLL_BUDGET,
// READ_BLIND_POLL_BUDGET and ARTIFACT_FETCH_TIMEOUT when it deletes their code.
var removedSettings = []string{"ERROR_COMPAT", "TRUST_FORWARDED_HEADERS",
	"TRUSTED_FORWARDED_HOSTS", "ALLOW_NETWORK_FS", "POLL_RETRY_AFTER", "BUSY_RETRY_AFTER"}
```

- **`Upstream`** gains `func (u Upstream) Enabled() bool { return u.BaseURL != "" }`.
  `validateUpstream` runs only for enabled upstreams. When neither is enabled:
  `"at least one of DI_UPSTREAM_URL and READ_UPSTREAM_URL must be set"`.
- **`Warnings()`:**
  - one line per removed setting present in the environment: `"<NAME> is no longer used"`;
  - empty-API-key warnings only for enabled upstreams;
  - the `PUBLIC_BASE_URL`-unset warning stays.
- **`Redacted()`:**
  - drops the removed keys and adds `splitPages`;
  - keeps `diMaxInflight` and `readMaxInflight` as numbers, `0` for a disabled surface;
  - keeps `queueDepth`;
  - adds `diEnabled` and `readEnabled`.

**`azerr` changes.** Delete `Compat`, `CompatObserved`, `CompatDocumented`, `SetCompat` and
`CompatMode`, and the documented branch of `useFlat`/`Body`. Observed shapes only.

**`httpx` changes:**
- `BaseURLResolver{Configured string}`. `Resolve`:
  - returns `Configured` when set;
  - otherwise `scheme://r.Host`, with scheme `https` when `r.TLS != nil`, after `validAuthority`;
  - otherwise `""`.

  Delete the forwarded-header code and its tests.
- `RequestID` keeps minting the id into the context and logger but **no longer sets the
  `apim-request-id` response header**.
- New middleware `ServerHeader()`: for paths not starting with `/_gw/`, set `Server: Kestrel`
  before the handler runs.

**Callers:**
- `app.NewHandler` builds `BaseURLResolver{Configured: cfg.PublicBaseURL}` and chains
  `ServerHeader()`.
- `main.go` drops `azerr.SetCompat`, and uses `config.RetryAfterPoll` / `RetryAfterBusy`
  wherever `cfg.PollRetryAfter` / `cfg.BusyRetryAfter` were read.
- `grep -rn "PollRetryAfter\|BusyRetryAfter\|TrustForwarded\|AllowNetworkFS\|ErrorCompat" --include=*.go .`
  must be empty at the end of the task, except the new constants.
- `store.Options.AllowNetworkFS` is deleted. `Open` always refuses network filesystems; on a
  platform where detection is unavailable, `networkFS` reports false, as today.

**Tests:**
1. Config:
   - `SPLIT_PAGES` default 5, `0` valid, `-1` invalid;
   - a DI-only config is valid, and so is a Read-only one; neither set is invalid;
   - a removed variable set gives a warning naming it;
   - `Redacted` has no removed keys and has `splitPages`, `diEnabled` and `readEnabled`.
2. azerr: the observed shapes stay as the existing tests assert; delete the documented-mode tests.
3. httpx:
   - `Resolve` with `Configured`;
   - `Resolve` from `Host`, http and https;
   - a forwarded header is ignored;
   - `ServerHeader` sets the header on `/vision/...` and not on `/_gw/health`;
   - `RequestID` no longer sets `apim-request-id`.

- [ ] Commit `refactor(config)!: remove eleven settings, add SPLIT_PAGES and optional upstreams`.
  This commit also updates the conformance harness config so `go test ./...` passes.

---

### T7: upstream attempt, classification, retry (wave 2)

**Files:**
- Create: `internal/upstream/attempt.go`, `classify.go`, `retry.go`, `result.go`, and tests
- Modify: `client.go` (new `Client` type next to the old ones until T12), `health.go`
  (split out of `client.go`), `proxy.go`

**Interface:**

```go
// Client makes synchronous calls to one surface's container.
type Client struct{ /* name, baseURL, apiKey, surface, tempDir, maxBytes */ }

func NewClient(surface azerr.Surface, up config.Upstream, tempDir string, maxResultBytes int64) *Client

func (c *Client) Surface() azerr.Surface
func (c *Client) Name() string // "document-intelligence" or "computer-vision-read"

// Health reports reachability via GET /status (unchanged logic, without the syncAnalyze field).
func (c *Client) Health(ctx context.Context) Health

// Call is one synchronous analyze attempt.
type Call struct {
	ModelID string     // DI
	Prefix  string     // DI family, "/documentintelligence" or "/formrecognizer"
	Query   url.Values // forwarded parameters, already policy-checked
	Body    Document   // re-openable body
	Timeout time.Duration
}

// Outcome is the class of one attempt (sync-only spec §1).
type Outcome int

const (
	Success        Outcome = iota
	Cancelled      // row 1: the caller's context ended
	AnalysisFailed // row 3: 200 with status failed
	RouteMissing   // row 4
	RetryOnce      // row 5: the attempt's deadline, 500, 504
	Retry          // rows 6–8: connection errors, 408, 429, 502, 503, 202
	Terminal       // rows 9–10
)

// Attempt is everything the engine needs to decide what to do next.
type Attempt struct {
	Outcome    Outcome
	Result     *Result         // set on Success; caller owns Result.Cleanup
	Err        *azerr.APIError // set on every failure, shaped for the surface
	Reason     string          // metrics label: connection, status_503, degraded_202, timeout, …
	Status     int             // HTTP status, 0 for transport failures
	RetryAfter time.Duration   // parsed integer Retry-After, 0 when absent
	Took       time.Duration
}

// Do performs one attempt on a fresh connection (new Transport, keep-alive off, no cookie jar).
func (c *Client) Do(ctx context.Context, call Call) Attempt

// Result is a successful analysis on disk.
type Result struct {
	Path       string
	Start, End int64 // byte range of the bare AnalyzeResult inside Path
	Bare       bool  // the container answered a bare AnalyzeResult instead of an envelope
	UpstreamMS int64
}

func (r *Result) Cleanup()

// Backoff returns the wait before the next attempt (spec §1).
func Backoff(attempt int, retryAfter time.Duration, jitter func() float64) time.Duration

// ProxyGET relays a small GET to the container through a short-lived client (10s timeout).
func (c *Client) ProxyGET(ctx context.Context, pathAndQuery string, accept string) (*http.Response, error)
```

**Rules:**
- **Routes:**
  - DI target: `Prefix + "/documentModels/" + escapeSegment(ModelID) + ":syncAnalyze"`.
  - Read target: `/vision/v3.2/read/syncAnalyze`.
  - The query is `Call.Query.Encode()`. The API key header is set when configured.
  - `Content-Type` and `Content-Length` come from `Body`.
- **Per-attempt deadline:** a context of its own (`context.WithTimeout(ctx, call.Timeout)`). The
  classification checks `ctx.Err()` (row 1) before the attempt context (row 5).
- **`classify.go`** implements the table in the sync-only spec §1 in order, reusing
  `looksLikeMissingEndpoint`, `readErrorBody`, `inspectOperation` (moved to `result.go`) and
  `isFailed`.
  - `Reason` values: `connection`, `timeout`, `degraded_202`, `status_<code>`.
  - Error message on `RouteMissing`: `"The <name> container does not serve the synchronous analyze route."`
- **`retry.go` `Backoff`:**
  - `base := min(60s, 2s << (attempt-1))`;
  - `d := time.Duration(float64(base) * (0.5 + 0.5*jitter()))`;
  - when `retryAfter > 0`, `d = min(retryAfter, 60s)`.

**Tests** (`attempt_test.go`, against `mockazure` with T5a's modes and an `httptest.Server`
for edge cases):
1. 200 envelope → `Success`; also a bare body (`SyncBodyBare`), with `Bare` set.
2. 200 `{"status":"Failed"}` (Read) → `AnalysisFailed`, with a synthesised error.
3. A bodyless 404 → `RouteMissing`; 500 `UnhandledEndpointException` → `RouteMissing`.
4. 404 `ModelNotFound` → `Terminal`, with the error relayed.
5. 500 plain → `RetryOnce` `status_500`; 504 → `RetryOnce`; an attempt deadline (`SyncHang`
   with `Timeout` 100ms) → `RetryOnce` `timeout`.
6. Connection refused (closed server), `ResetMidBody` → `Retry` `connection`.
7. 408, 429, 502, 503 → `Retry`; 503 + `Retry-After: 3` → `RetryAfter == 3s`.
8. 202 → `Retry` `degraded_202`, with no poll request made (`Requests()` has no
   `analyzeResults`).
9. The caller's context cancelled mid-call → `Cancelled`, even though the transport error also
   looks like a timeout.
10. A fresh connection per attempt: an `httptest.Server` with `ConnState` counting `StateNew`
    sees 3 new connections for 3 attempts.
11. `Backoff` with jitter 0 and 1 for attempts 1..7; `Retry-After` overrides; caps.

- [ ] Commit `feat(upstream): one synchronous attempt with outcome classes and backoff`.

---

### T8: engine (wave 2)

**Files:** create `internal/engine/{engine,whole,parts,hedge,workdir,metrics}.go` and tests.

**Interface:**

```go
package engine

type Options struct {
	Client       *upstream.Client
	Sched        *sched.Scheduler
	Splitter     *pdfsplit.Splitter
	SplitPages   int
	WorkRoot     string        // ${DATA_DIR}/work
	WholeTimeout time.Duration // cfg.DI.Timeout or cfg.Read.Timeout
	Metrics      *Metrics
	Log          *slog.Logger
	Now          func() time.Time
	Jitter       func() float64 // rand.Float64 in production
}

func New(o Options) *Engine

// Input is one document to analyse.
type Input struct {
	JobID       string
	ContentType string
	Path        string // stored input on the PVC
	Size        int64
	ModelID     string     // DI
	Prefix      string     // DI family
	Query       url.Values // forwarded, policy-checked; may contain pages
}

// Output is the analyzeResult on disk.
type Output struct {
	Path       string
	Start, End int64 // the bare AnalyzeResult object inside Path
	Bare       bool
	Attempts   int
	Parts      int
	Halvings   int
	Hedges     int
	Fallback   string // "" or the split-fallback reason
	UpstreamMS int64
	cleanup    func()
}

func (o *Output) Cleanup()

// Run analyses one document. Errors are *azerr.APIError, or the context's error on shutdown.
func (e *Engine) Run(ctx context.Context, in Input) (*Output, error)

// Constants (splitting spec §7).
const (
	PartTimeout    = 60 * time.Second
	PartAttempts   = 3
	WholeAttempts  = 5
	JobDeadline    = 60 * time.Minute
	HedgeMin       = 5 * time.Second
	HedgeMax       = 30 * time.Second
	HedgeFactor    = 4
	HedgeSamples   = 200
	HedgeMinSample = 20
)
```

**Flow** (`engine.go`):
1. `ctx, cancel := context.WithTimeout(ctx, JobDeadline)`. Create the work folder
   `WorkRoot/<JobID>` (`workdir.go`). It is removed on return unless the output lives in it, in
   which case `Output.Cleanup` removes it.
2. **Decide** (splitting spec §1).
   - `body := in.Path`. When the content type is `application/json`, decode
     `{"base64Source": …}` into `work/input.pdf` when it decodes to a PDF. `urlSource` → whole.
   - Check `IsPDF` on the first 1024 bytes and `SplitPages > 0`.
   - `doc, err := Splitter.Open`. An error records the fallback reason (`encrypted`,
     `unparseable`, `budget`, `panic`) and goes whole.
   - Resolve the client's `pages` with `ParsePages` and `Select(…, doc.PageCount)`. Failure
     (`pages_param`) → whole.
   - `len(pages) <= SplitPages` → whole, with the original body and query unchanged.
3. **Whole** (`whole.go`): up to `WholeAttempts` attempts, with the `RetryOnce` class limited to 2.
   - Each attempt does `Sched.Acquire(ctx, JobID)`, then `Client.Do` with `Timeout: WholeTimeout`,
     then release.
   - `Backoff` between attempts, waiting on `ctx`.
   - Success → `Output{Path, Start, End, Bare, Attempts, Parts: 1}`.
   - Exhaustion returns the last error with `Message` = `"<original message> (after <n>
     attempts)"`. `AnalysisFailed`, `Terminal` and `RouteMissing` return immediately.
4. **Split** (`parts.go`).
   - `Plan(pages, SplitPages)`. Write each part with `doc.WritePart` into
     `work/part-<label>.pdf`, then `doc.Close()`.
   - Run all parts concurrently with an `errgroup`-style loop. A failed part cancels the rest
     through a shared context.
   - The query for each part is the client's query minus `pages`.
   - **Part loop** (splitting spec §4):
     - `attempt := 0`;
     - each attempt does `Sched.Acquire(ctx, JobID)`, then `doHedged` (below), then release;
     - `Retry` and `RetryOnce` up to `PartAttempts` per size, with `Backoff` between;
     - after exhausting at a size: if `len(Pages) > 1`, `Halve` with `Splitter.Recut` from the
       part file into two new files, recurse on both concurrently, `Halvings++`; else return the
       last error;
     - `AnalysisFailed` or `Terminal` → **re-check**: `Client.Do` on the **original** body with
       `pages=FormatPages(part.Pages)` (scheduled, `WholeTimeout`). Success → use it. Failure →
       return the re-check's error;
     - `RouteMissing` → return the error;
     - `Cancelled` → return `ctx.Err()`.
   - **`doHedged`** (`hedge.go`):
     - start the attempt;
     - after `delay()` (`clamp(HedgeFactor × median, HedgeMin, HedgeMax)`, or `HedgeMax` with
       fewer than `HedgeMinSample` samples), if `Sched.TryAcquireIdle()` succeeds, start one
       duplicate;
     - the first `Success` wins and cancels the other;
     - if one fails and the other is still running, wait for it;
     - the attempt's outcome is the winner's, or the last failure when both fail;
     - successful part durations feed a ring of the last `HedgeSamples`.
   - **Merge:** order parts by first page, then `merge.DI` (or `merge.Read`) into
     `work/merged.json`, with the unit from the query's `stringIndexType`. `ErrCheck` → whole,
     with `Fallback = "merge_check"`. The output `Bare` is part 0's `Bare`.
5. **`metrics.go`:**
   - atomic counters for parts by outcome, split fallbacks by reason, retries by reason,
     exhausted jobs;
   - fixed-bucket histograms for part seconds, parts per job and merge seconds, with buckets
     `0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300`;
   - `Snapshot()` for the admin.

**Tests** (`engine_test.go`, against `mockazure.NewPaged` with `pdftest.Make`, a real
`sched.Scheduler(4)`, a real `Splitter(2, time.Minute)`, `t.TempDir()` work roots, and jitter
fixed at 0):
1. **Whole:**
   - a 3-page PDF → 1 call, the output equals `docgen.DI(1..3, Original)`;
   - a JPEG body (`image/jpeg`, arbitrary bytes) → whole, 1 call;
   - `urlSource` JSON → whole, with the body forwarded byte for byte.
2. **Split:**
   - a 23-page PDF → parts `5,5,5,4,4`, with merged bytes equal to `docgen.DI(1..23, Original)`;
   - `pages=3-14` → only those pages, and the merge equals `docgen.DI(3..14, Original)`;
   - a `base64Source` 12-page PDF → split and merged.
3. **Splitting off:** `SplitPages=0` → whole.
4. **Hang:** page 9 hangs, `PartTimeout` overridden to 200ms through an unexported field. The
   part `[9..]` times out, is retried 3 times, then halved down to `[9]`, which keeps hanging →
   the job fails with a timeout error after `PartAttempts` at size 1. Calls are recorded in order.
5. **Hedge:**
   - page 5 hangs only on its first call (`HangFirst` option on the fake);
   - the hedge delay forced to 50ms;
   - the hedge wins, the output is correct, and `Hedges == 1`.
6. **Retries:** `FailFirst{4:2}` → success after retries, with `Attempts` counted.
7. **Re-check:**
   - `BadContent{6}` with `BadOnlyWhenSplit` → the re-check with `pages=` on the original
     succeeds, and the output is correct;
   - without `BadOnlyWhenSplit` → the job fails with the container's `InvalidContent` error.
8. **Unparseable PDF** (`%PDF-` then garbage) → whole, `Fallback == "unparseable"`.
9. **Merge check:** a fake that returns a wrong page count for one part → whole,
   `Fallback == "merge_check"`.
10. **Fairness:** with a scheduler limit of 2, a 30-page job and a 1-page job submitted together
    → the 1-page job finishes before the 30-page job has finished 3 parts.
11. **Cancellation:** cancel mid-split → `Run` returns `context.Canceled`, no goroutine is left
    (`goleak`-style check with `runtime.NumGoroutine` settling), and the work folder is removed.
12. **Read surface:** a 12-page PDF → equals `docgen.Read(1..12, Original)`.

- [ ] Commit `feat(engine): split, schedule, hedge, retry, halve, re-check and merge`.

---

### T9: jobs run the engine; migration 0002 (wave 2)

**Files:**
- Modify: `internal/jobs/manager.go`, `recover.go` and tests; `internal/store/db.go` and test
- Create: `internal/store/migrations/0002_sync_only.sql`

**Migration 0002:**

```sql
ALTER TABLE jobs DROP COLUMN upstream_op_url;
ALTER TABLE jobs DROP COLUMN has_pdf;
ALTER TABLE jobs DROP COLUMN figures;
ALTER TABLE jobs DROP COLUMN upstream_mode;
ALTER TABLE jobs ADD COLUMN upstream_attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN upstream_parts INTEGER NOT NULL DEFAULT 0;
```

**`store` changes:**
- `Job` loses `Mode`, `UpstreamOp`, `HasPDF` and `Figures`, and gains `UpstreamAttempts int` and
  `UpstreamParts int`. `jobColumns`, `scanJob` and `Create` follow.
- `Succeed(ctx, id, resultPath string, resultBytes, upstreamMS int64, attempts, parts int, now time.Time) (bool, error)`.
- `Fail(ctx, id, errorJSON string, upstreamStatus int, upstreamMS int64, attempts, parts int, now time.Time) (bool, error)`.
- Delete `UpstreamMode` and its constants.

**`jobs` changes:**
- `Options` drops `DI`/`Read upstream.Analyzer` and takes `DI, Read *engine.Engine` plus
  `DIClient, ReadClient *upstream.Client`, each nil when that surface is disabled.
  `runners` are created only for non-nil engines.
- `surfaceRunner.upstream` is deleted. `AdmitUpstream` is deleted: the engine's scheduler bounds
  upstream calls.
- `Analyzer(surface)` becomes `Client(surface) *upstream.Client`, for health and proxies, and
  `Engine(surface) *engine.Engine` is added.
- `run()`:
  - after `MarkRunning` and the heartbeat, build an `engine.Input` from the job and call
    `Engine.Run(ctx, in)`;
  - on error, classify as today: context first, then `azerr.AsAPIError`, then `failJob` with the
    attempt and part counts;
  - on success, `finish(ctx, r, job, out)`.
- `finish` writes the envelope from `out.Path[out.Start:out.End]` exactly as today, calls
  `Succeed` with the new signature, and calls `out.Cleanup()`.
- Delete `fetchArtifacts`, `figureIDs`, `wantsArtifacts`, `outputSet`, `moveInto` and `isPDF`
  (if unused).
- `recover.go` `sweepOnce` also removes `${DATA_DIR}/work/<id>` folders whose job is not running
  in this process (`claims`) and that are older than 1 hour, and stale `work/sync-*` folders older
  than 1 hour.
- `Capacity()` drops `upstreamInUse`/`upstreamLimit` and adds `upstreamInUse`, `upstreamWaiting`
  and `upstreamLimit` from `Sched.Stats()`, keeping the key names stable for the admin.

**Wiring:**
- Create `app.NewEngines(cfg *config.Config, st *store.Store, log *slog.Logger) Engines` with
  `type Engines struct{ DI, Read *engine.Engine; DIClient, ReadClient *upstream.Client }`. It
  builds:
  - one `sched.Scheduler` per enabled surface (limit `MaxInflight`);
  - one shared `pdfsplit.Splitter(2, 60s)`;
  - one `upstream.Client` and one `engine.Engine` per enabled surface;
  - `WorkRoot` = `filepath.Join(cfg.DataDir, "work")`.
- `cmd/gateway/main.go` and the conformance harness both call it; never build engines by hand.
- `main` logs `store.Migration` after `Open`.

**Keeping the build green.** In this same task:
- `surface.passthrough` is replaced by the engine-backed `sync.go` described in T10 item 2;
- the artifact handlers become the 404s of T10 item 3;
- admin calls `Jobs.Client(name).Health` instead of `Analyzer(name).Health`.

T10 and T11 then add the remaining surface and admin features.

**Tests:**
- store tests for the new signatures;
- migration test 7 still passes;
- a new store test: 0002 from a v1 database keeps rows, and the new columns default to 0;
- jobs tests that used `upstream.Analyzer` fakes are ported to engines over `mockazure` and keep
  their intent:
  - `recovery_test.go`: crash reclaim, attempts exhausted, a deleted row discards its result;
- a new test: work folders are swept after a simulated crash.

- [ ] Commit `refactor(jobs)!: run every job through the engine; migration 0002`.

---

### T10: surface and app (wave 2)

**Files:**
- Modify: `internal/surface/{surface,di,read}.go`, `internal/app/app.go`, `cmd/gateway/main.go`
- Create: `internal/surface/params.go`, `passthrough.go`, `sync.go`, and tests
- Delete: `cmd/gateway/main_test.go`

**Changes:**
1. **`params.go`:** `checkDIParams(q url.Values) *azerr.APIError` implements the sync-only spec
   §2 table. Each rejection is
   `azerr.New(400, CodeInvalidArgument, "").WithInner(InnerInvalidParameter, "<message naming the parameter>")`:
   - `output` containing `pdf` or `figures` (comma-separated or repeated, case-insensitive) →
     `"output=pdf and output=figures are not supported by this gateway."`;
   - `outputContentFormat` other than empty or `text` (case-insensitive) → `"outputContentFormat=<v> is not supported by this gateway yet."`;
   - `features` non-empty → `"features is not supported by this gateway yet."`;
   - `queryFields` present → `"queryFields is not supported by this gateway yet."`.

   It is called by `diAnalyze` and `diSyncAnalyze` after `requireAPIVersion`. Read routes do not
   call it.
2. **`sync.go`:** `syncAnalyze(w, r, surface, surfaceName, modelID, prefix)` replaces
   `passthrough`.
   - `Admit` (429 when busy), upload deadline, then save the body to
     `${DATA_DIR}/work/sync-<id>/input` with `store`-style limits (`MaxRequestBytes`;
     `ContentTooLarge` on overrun).
   - `Engine.Run` with `JobID "sync-<id>"`.
   - **Success:** 200, `Content-Type: application/json; charset=utf-8`, exact `Content-Length`.
     The body is either `out.Path[out.Start:out.End]` when `out.Bare`, or the envelope
     `{"status":"succeeded","createdDateTime":<received>,"lastUpdatedDateTime":<now>,"analyzeResult":…}`
     in `jobs.TimeFormat`.
   - **Error:** `apiErr.WriteTo(w, surface)`, which applies the existing `Retry-After` rules. A
     context error after a client disconnect writes nothing.
   - Remove the folder and `Cleanup` on return.
3. **Artifacts:** `diPDF` and `diFigure` keep their routes.
   - Unknown or expired id → `azerr.NotFound(DI)`.
   - Known id → `404` with `New(404, CodeNotFound, "The requested result file was not produced
     for this operation.").WithInner(InnerOperationNotFound, "")`.
   - No blob is opened.
4. **`passthrough.go`:** GET relays through `Client.ProxyGET` under `AdmitMetadata`, using
   `relaySync` / `copyResponse` unchanged. Routes, registered only when the owning surface is
   enabled:
   - `GET {fam}/operations`, `GET {fam}/operations/{operationId}` (DI; the id is validated with
     `ids.Normalize` or passed escaped);
   - root routes, owned by DI when enabled, else by Read: `GET /{$}`, `GET /swagger`,
     `GET /swagger/{rest...}` (each segment escaped), `GET /records/usage-logs`,
     `GET /records/usage-logs/{month}/{year}` (digits only, else the unrouted 404).
5. **`Register`** registers DI routes only when `Jobs.Engine(jobs.SurfaceDI) != nil`, and Read
   routes likewise.
6. **`submit`** uses `config.RetryAfterPoll` and `RetryAfterBusy`, and `diAnalyze` calls
   `checkDIParams` before `submit`.
7. **Delete** `writeResolvedSync` and the old `passthrough`.
8. **Must remove:** `requireBothUpstreams` in `cmd/gateway/main.go` and its test,
   `cmd/gateway/main_test.go`. T6 added the guard because the job manager still built a runner for
   both surfaces. With T9's runners and item 5's routes both per configured surface, a gateway with
   one upstream is servable, and leaving the guard in keeps the optional-upstream feature unusable.

**Tests** (`surface` package, `httptest` with fake engines through the real wiring):
- every parameter-policy row, on both DI routes;
- sync success in the envelope shape and the bare shape;
- sync with the container failing → the surface error at the container's status;
- sync busy → 429 + `Retry-After: 5`;
- artifacts for unknown and known ids;
- each passthrough route relays status, headers and body;
- the month/year guard;
- registration with DI only and with Read only.

- [ ] Commit `feat(surface): parameter policy, engine-backed sync route, container GET passthroughs`.

---

### T11: admin (wave 2)

**Files:** modify `internal/admin/admin.go` and tests.

**Changes:**
- **`health`:** upstreams come from `Jobs.Client(name)` for enabled surfaces, and `Health` has no
  `syncAnalyze`. `capacity` comes from `Capacity()`.
- **`config`:** `cfg.Redacted()`, unchanged call.
- **`jobView`** adds `upstreamAttempts` and `upstreamParts` and drops `upstreamMode`, `hasPdf`
  and `figures`.
- **`metrics`:** keep the existing series and append, per surface, from
  `Jobs.Engine(name).Metrics().Snapshot()`:
  - `gateway_part_seconds_bucket{surface,le}`, `_sum`, `_count`;
  - `gateway_job_parts_bucket{surface,le}`, `_sum`, `_count`;
  - `gateway_merge_seconds_bucket{surface,le}`, `_sum`, `_count`;
  - `gateway_parts_total{surface,outcome}`;
  - `gateway_split_fallback_total{surface,reason}`;
  - `gateway_upstream_retries_total{surface,reason}`;
  - `gateway_upstream_exhausted_total{surface}`;
  - `gateway_upstream_slots{surface,state="inUse|waiting|limit"}`.
- **`containerStatus` / `containerReady`:** iterate enabled clients only.

**Tests:**
- health with DI only;
- config keys;
- a jobs view after a split job, with `upstreamParts > 1`;
- metrics text containing each new family with `# HELP`/`# TYPE`, and histogram bucket
  monotonicity.

- [ ] Commit `feat(admin): engine metrics, parts and attempts on jobs, sync-only health`.

---

### T12: delete the dead upstream code (wave 2)

Delete the remaining five settings from `config` (fields, parsing, `Redacted`, tests) and append
them to `removedSettings`: `DI_SYNC_ANALYZE`, `DI_SYNC_PROBE_TIMEOUT`, `DI_BLIND_POLL_BUDGET`,
`READ_BLIND_POLL_BUDGET`, `ARTIFACT_FETCH_TIMEOUT`.

Delete:
- `internal/upstream/poll.go`, after moving the still-used helpers into `result.go`;
- `sync.go`;
- the old `DIClient`/`ReadClient`/`base`/`affinityClient`, `Analyzer`, `SyncAnalyzer`,
  `SyncOutcome`, `FetchArtifact`, `trySync`, `analyzeAsync`, `resourceScoped404` and the
  capability latch;
- tests that only covered them (`cancel_test.go`, `leak_test.go` and `probebound_test.go` are
  ported where they guard a still-relevant invariant, for example no leaked goroutines per
  attempt, otherwise deleted);
- in `internal/mockazure`, the `Sync202` poll machinery stays, because the fake still has to
  answer polls for the probe.

`grep -rn "pollUntilTerminal\|affinity\|FetchArtifact\|SyncOutcome\|DISyncMode" --include=*.go .`
must be empty.

- [ ] `go test -race ./...` green → commit `refactor(upstream)!: delete the async upstream path`.

---

### T13: probe (wave 3)

**Files:**
- Modify: `internal/probe/probe.go`
- Create: `internal/probe/{record,compare,diff,golden}.go` and tests
- Modify: `cmd/gateway/main.go` (flags), `scripts/probe-containers.sh` (configured surfaces,
  verdict text)

**Behaviour:**
- `gateway probe` probes the enabled surfaces only.
  - `PROBE_DOCUMENT` (a path) replaces the blank PNG in the analyze probes.
  - `PROBE_TIMEOUT` (duration, default 10m) is the probe client's timeout.
  - The verdict text follows the sync-only spec §8.
- **`gateway probe --record DIR`** runs the case list of the sync-only spec §3 (constants in
  `record.go`).
  - It writes `DIR/<surface>/<case>.json`:
    `{"request":{method,path,query,contentType},"response":{status,headers,body}}`.
  - `body` is parsed JSON when possible, else a string. `Date` is dropped.
  - Masks (`golden.go`): GUIDs → `<guid>`; values matching RFC 3339 → `<datetime:<layout>>`,
    keeping the detected layout.
- **`gateway probe --compare-split DIR`**, for each `*.pdf`:
  - **whole:** `Client.Do` with `WholeTimeout = PROBE_TIMEOUT`;
  - **split:** a probe-local `engine.Engine` (scheduler 4, splitter 2) with `SplitPages`;
  - **diff** (`diff.go`): a structural walk producing `path pattern → count, examples[≤3]`, with
    indices written `[*]`;
  - **categories:** page-level for paths under `pages`, page-local `boundingRegions`, `figures`,
    and `tables[*].cells`; document-level for everything else;
  - **report:** `DIR/compare-report.json` plus a text summary on stdout. The exit status is
    non-zero when any page-level difference exists.

**Tests:**
- `diff` on hand-built pairs: identical gives no diffs; a role difference is document-level; a
  word offset difference is page-level;
- golden masking;
- `--record` against `mockazure.NewPaged` writes the expected files, then the conformance parity
  test (T14) replays them;
- `--compare-split` against the paged fake:
  - with `TitleOnFirstPage` it reports `paragraphs[*].role` as document-level and exits 0;
  - with a fake that corrupts one part's words it reports a page-level difference and exits 1.

- [ ] Commit `feat(probe): configured surfaces, record, compare-split, sync-only verdict`.

---

### T14: conformance, every endpoint (wave 3)

**Files:**
- Modify: `test/conformance/harness_test.go`; delete obsolete cases in `behaviour_test.go` and
  `sync_test.go`
- Create: `endpoints_test.go`, `split_test.go`, `params_test.go`, `single_surface_test.go`,
  `parity_test.go`, `admin_test.go`

**Harness:**
- `harnessOpts` gains `paged bool` (use `mockazure.NewPaged` for both surfaces), `diOnly`,
  `readOnly`, and `splitPages`.
- Engines come from `app.NewEngines`, introduced in T9, exactly as in `main.go`.

**`endpoints_test.go`: one table-driven test over every route.** Each row has method, path,
headers, body, expected status, required headers, forbidden headers, and a body assertion:

| Route | Assertion |
|---|---|
| `POST /documentintelligence/documentModels/prebuilt-layout:analyze?api-version=2024-11-30` | 202, `Operation-Location` regex (fidelity checklist 11), `Retry-After: 1`, `Content-Length: 0`, no `Location`, `Server: Kestrel`, no `apim-request-id` |
| Same on `/formrecognizer/…?api-version=2023-07-31` | 202 with a `/formrecognizer/` `Operation-Location` |
| `GET` and `HEAD` the poll URL while running and after success | 200; `Retry-After` only while running; terminal envelope keys in order |
| `DELETE` the result | 204, then the poll gives the flat DI 404 |
| `POST …:syncAnalyze?api-version=…` | 200 JSON, no `Operation-Location` |
| `GET …/analyzeResults/{id}/pdf` and `…/figures/1.1` | 404 bodies (known and unknown id) |
| `GET /documentintelligence/info`, `/documentModels`, `/documentModels/prebuilt-layout`, `/operations`, `/operations/<guid>` | relayed |
| `POST …:analyzeBatch` | 400 |
| `POST …:bogus` | 404 |
| `POST /vision/v3.2/read/analyze` | 202, bare GUID `Operation-Location` |
| `GET /vision/v3.2/read/analyzeResults/{id}` | 200, `readResults` |
| `POST /vision/v3.2/read/syncAnalyze` | 200 |
| `GET /vision/v3.2/read/operations/<guid>` | bodyless 404 |
| `GET /`, `/swagger`, `/swagger/v1/swagger.json`, `/records/usage-logs`, `/records/usage-logs/10/2026` | relayed |
| `/records/usage-logs/xx/yy` | bodyless 404 |
| `GET /ready` | `{"ready":"ready"}` |
| `GET /status` | `apiStatus` |
| `/_gw/live`, `/_gw/ready`, `/_gw/health`, `/_gw/version`, `/_gw/config`, `/_gw/jobs`, `/_gw/jobs/{id}`, `/_gw/metrics` | 200, documented keys, no `Server: Kestrel` |
| Unrouted `GET /nope` | bodyless 404 |
| `PUT` on the analyze path | 405 + `Allow` |
| Missing `api-version` | 400 `InvalidArgument` / `ParameterMissing` |
| Unknown result id | DI flat 404, Read wrapped 404, no `Retry-After` |
| `x-ms-client-request-id` | echoed on every Azure route |

**Other files:**
- **`params_test.go`:** each rejected parameter on `:analyze` and `:syncAnalyze` → 400 with no
  job row and no blob; `outputContentFormat=text` and an empty `features` → accepted.
- **`split_test.go`** (paged fake, `SplitPages 5`):
  - a 40-page PDF through `:analyze` → the polled result equals the envelope around
    `docgen.DI(1..40)`;
  - the same through `:syncAnalyze`;
  - Read 40 pages;
  - `pages=10-25`;
  - a hung page with the part timeout overridden → the job still succeeds when the hang affects
    only the first call (hedge or retry);
  - bad content when split → re-check → success;
  - an unparseable PDF → whole;
  - `SplitPages 0` → one call;
  - `Capacity(4)` with `DI_MAX_INFLIGHT 16` and 50 submits → all succeed;
  - `/_gw/jobs` shows `upstreamParts`.
- **`single_surface_test.go`:** a DI-only gateway serves DI and answers every Read route with the
  bodyless 404; Read-only the reverse; `/_gw/health` lists one upstream; `/_gw/config` reports the
  disabled surface's inflight as 0.
- **`parity_test.go`:** loads `testdata/golden/<surface>/<tag>/*.json`, skips with a message when
  none exist, replays each case against a harness whose fakes serve the recorded bodies, and
  compares under the T13 masks. T13 checks in one generated set, from the paged fake, under
  `testdata/golden/fake/` so the replay path itself is exercised.
- **`admin_test.go`:** the metrics families after a split job; health and config shapes.
- **Existing `fidelity_test.go`, `method_test.go`, `legacy_family_test.go` and
  `shutdown_test.go`:** kept and updated to the new config; assertions about `apim-request-id` are
  inverted.

- [ ] Commit `test(conformance): every endpoint, parameter policy, splitting, single surface, parity`.

---

### T15: real SDKs and live end-to-end (wave 3)

**Files:** modify `test/sdk/sdk_test.py`, `run.sh`, `README.md`; create `scripts/e2e-local.sh`.

1. **`sdk_test.py` additions:**
   - `test_document_intelligence_large_pdf`: Pillow builds a 23-page PDF (`save_all=True`), then
     `begin_analyze_document` → `poller.result()`. Expect `len(result.pages) == 23` and page
     numbers 1..23.
   - `test_read_large_pdf`: through the Read client.
   - `test_sync_analyze_large_pdf`: a raw `requests`-free `urllib` POST to `:syncAnalyze` → 200,
     23 pages.
   - The existing tests stay.
2. **`scripts/e2e-local.sh`:**
   1. `go build` the gateway and `cmd/fakeazure`;
   2. start `fakeazure -di 127.0.0.1:5001 -read 127.0.0.1:5002`;
   3. start the gateway with `DATA_DIR=$(mktemp -d)`, both upstream URLs, `SPLIT_PAGES=5` and
      `PUBLIC_BASE_URL=http://127.0.0.1:18080` on `GATEWAY_ADDR=127.0.0.1:18080`;
   4. wait for `/_gw/live`;
   5. curl every route in the T14 table with status checks;
   6. run `test/sdk/run.sh` with `GATEWAY_URL`;
   7. run `gateway probe --compare-split` on a folder of generated PDFs against the fake;
   8. stop both processes and print a pass/fail summary.

   The exit status is non-zero on any failure.

- [ ] Commit `test(e2e): large-PDF SDK flows and a live end-to-end script`.

---

### T16: documentation (wave 3)

**Files:** `README.md`, `docs/ARCHITECTURE.md`, `docs/OPERATIONS.md`, `docs/TESTING.md`,
`docs/HANDOFF.md`, `docs/api-surface.md`, `docs/CONTAINER-BEHAVIOUR.md` (pointer notes only), and
the specs' Status lines.

**Content:**
- **README:** the settings table (22 settings) and the removed-settings list.
- **ARCHITECTURE.md:**
  - §3 lifecycles: the sync route through the engine;
  - §4: replace the decision tree with the attempt/outcome model and the splitting flow;
  - §5: migrations, the work folder, and the new columns;
  - invariants.
- **OPERATIONS.md:**
  - HAProxy annotations;
  - sizing (`MAX_INFLIGHT = 8 × per-pod`, `QUEUE_DEPTH`);
  - memory for 2 concurrent splits (pdfcpu holds about 2–3× the PDF size while parsing);
  - the constants;
  - metrics and alerts (`gateway_split_fallback_total`, `gateway_upstream_exhausted_total`);
  - the migration and rollback procedure with the `.bak`;
  - the go-live gates.
- **TESTING.md:** the new suites, the `e2e-local.sh` script, `--record` and `--compare-split`.
- **HANDOFF.md:**
  - the settings table;
  - removed settings (the gateway warns, doesn't fail);
  - the new probe usage;
  - the work folder on the PVC;
  - the coupling map: the job schema changes **only** through a new migration file, and the
    "no migrations" trap is deleted.
- **api-surface.md:** the parameter policy, the routes table, and the header rules (`Server:
  Kestrel`, no `apim-request-id`).

- [ ] Commit `docs: sync-only upstream, splitting, migrations, operations and handoff`.

---

### T17: full gate and hand-over (wave 4)

- [ ] `gofmt -l . && go vet ./... && go test -race -count=1 ./...`: all green.
- [ ] `make cover`: report the total and keep it at or above today's.
- [ ] `scripts/e2e-local.sh`: all green.
- [ ] `make image`: the image builds and re-runs the suite. Docker must be running; it must not
  touch any compose stack.
- [ ] A code review of the whole branch diff against both specs, with fixes.
- [ ] Re-import `azure-gateway-api` into `~/wega-unified-services` (`git read-tree` flow, history
  kept), committed locally there. **No push** anywhere unless the user asks.
- [ ] Update the memory note and `wega-unified-services/HANDOFF.md`'s gateway pointer.

## Self-review against the specs

**Sync-only spec.**

| Spec section | Covered by |
|---|---|
| §1 upstream call | T7 (attempt, classes, backoff), T8 whole loop (5 attempts, once-class 2) |
| §2 parameter policy | T10 `params.go`, T14 `params_test.go` |
| §3 parity: routes | T10 passthroughs and artifacts |
| §3 parity: headers | T6 (`Server: Kestrel`, no `apim-request-id`) |
| §3 parity: bodies | T6 (observed only) |
| §3 parity: load shedding | unchanged |
| §3 parity: golden | T13, T14 |
| §4 migrations | T1, T9 |
| §4 store and work folder | T8, T9 |
| §4 network filesystem | T6 |
| §5 removed code and settings | T6, T9, T12 |
| §6 configuration | T6 |
| §7 observability | T8, T11 |
| §8 probe | T13 |
| §9 deployment | T16 |
| §10 invariants | T8, T9 tests |
| §11 testing | T1–T15 |

**Splitting spec.**

| Spec section | Covered by |
|---|---|
| §1 decision | T8 |
| §2 splitting | T2 |
| §3 engine and scheduler | T3, T8 |
| §4 part rules | T8 |
| §5 merge | T4 |
| §6 measure | T13 |
| §7 configuration | T6, T8 constants, T9 `upstream_parts` |
| §8 observability | T8, T11 |
| §9 invariants | T8 |
| §10 testing | T2–T5, T8, T14, T15 |
| Rollout docs | T16 |

**Type consistency.**
- `upstream.Result{Path, Start, End, Bare, UpstreamMS}` is used by T8 and the `merge.Part{Path,
  Start, End, Pages}` adapter.
- `engine.Output` is consumed by T9 `finish` and T10 `sync.go`.
- `Jobs.Client`/`Jobs.Engine` are used by T10 and T11.
- `textunit.Unit` is used by `docgen`, `merge` and the engine.
- `pdfsplit.Part{Pages}` is used by `docgen` tests, the engine and `merge`.
