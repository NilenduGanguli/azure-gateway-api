# Real-SDK conformance

Points the official Azure client libraries at a running gateway and checks they work unmodified.

The Go conformance suite asserts what the gateway *emits*. This asserts that the SDKs *accept*
it — which is a different question, because the parts that break proxies live inside SDK poller
internals rather than in any published contract.

```bash
DI_UPSTREAM_URL=http://layout:5000 READ_UPSTREAM_URL=http://read:5000 DATA_DIR=./data make run
# in another shell
make sdk-test
```

`GATEWAY_URL` defaults to `http://localhost:8080`. The virtualenv (`.venv`, git-ignored) is
created on first run and rebuilt whenever `requirements.txt` changes or a previous install did not
finish. `SDK_STRICT=1` turns a skipped check into a failure, so a broken virtualenv cannot pass by
skipping everything.

`scripts/e2e-local.sh` runs this suite strictly against the built gateway and `cmd/fakeazure`,
with `SPLIT_PAGES=5`, so the large-PDF checks below go through splitting and the merge. It also
uses `./sdk_test.py write-pdf PAGES PATH` to write the same PDFs for its own split checks.

## What it checks

| Check | Breaks if |
|---|---|
| `begin_analyze_document` accepts the 202 | the submit returns anything but exactly 202 |
| `poller.details` yields an operation id | `Operation-Location` loses the literal `/documentintelligence/` segment |
| `poller.result()` completes | the terminal body does not nest `analyzeResult`, or a `Location` header triggers a bogus final GET |
| the operation finishes in under 25s | `Retry-After` is missing or unparseable, so azure-core falls back to its 30s default |
| `Operation-Location.split("/")[-1]` is a GUID | the Read header carries a query string or trailing slash |
| the Read status is inside its closed enum | a status string outside the four canonical values leaks out, and the canonical sample loop never terminates |
| unknown ids 404 with the right body per surface | the two error shapes are swapped, or the 404 carries `Retry-After` |
| a 23-page PDF through `begin_analyze_document` has pages 1..23 | a split part is dropped, repeated or misplaced, or the merged result no longer deserialises into the SDK's models |
| its page spans are ascending and inside the merged content | the merge did not shift a later part's offsets |
| the same PDF through `read_in_stream` has `readResults` pages 1..23 | the Read merge loses or misnumbers a part |
| the same PDF by raw `urllib` POST to DI `:syncAnalyze` and Read `syncAnalyze` gives 200 and pages 1..23 | the synchronous routes do not run through the splitting engine, or answer with anything but the succeeded envelope |

## The test PDF

`sample_pdf` builds it with Pillow. Page *i* is (600+*i*) × 800 points with "PAGE *i*" drawn on
it. The sizes follow `internal/pdftest`'s convention, from which the paged fake recovers the
original pages a split part holds, so against `cmd/fakeazure` every part is answered for exactly
its own pages. A real container ignores the sizes and reads the text.
