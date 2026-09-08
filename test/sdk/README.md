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

`GATEWAY_URL` defaults to `http://localhost:8080`. The virtualenv is created on first run.

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
