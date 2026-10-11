#!/usr/bin/env bash
# e2e-local.sh — the built gateway against cmd/fakeazure on real ports, end to end.
#
# The conformance suite runs the gateway in-process. This runs the binary a release ships, and the
# fake a laptop has instead of the containers, over real sockets, and drives them four ways:
#
#   1. curl, once per route of the conformance endpoint table (plan T14), checking the status,
#      the headers clients depend on and the body's shape;
#   2. the official Azure SDKs (test/sdk/run.sh), including 23-page PDFs that are split and merged;
#   3. split jobs by curl: multi-page PDFs through both surfaces, merged page-exact, with the parts
#      visible in /_gw/jobs and /_gw/metrics;
#   4. gateway probe --compare-split on generated PDFs: no page-level difference, split or whole.
#
# Then it stops the gateway with SIGTERM, checks it shut down cleanly, and prints a summary.
#
# Usage:
#   scripts/e2e-local.sh
#
# Needs Go, curl and Python 3. Step 2 creates test/sdk/.venv and pip-installs the SDKs on first
# use. Nothing else is touched outside two temp directories: no Docker, no compose stack. The
# temp directories (logs, the gateway's DATA_DIR) are removed after a clean run and kept, with
# their path printed, after a failure; E2E_KEEP=1 keeps them always.
#
# Addresses (defaults in brackets): E2E_DI_ADDR [127.0.0.1:5001], E2E_READ_ADDR [127.0.0.1:5002],
# E2E_GATEWAY_ADDR [127.0.0.1:18080]. The script refuses to start if anything already answers on
# one of them, rather than testing whatever that is.
#
# Exit status: 0 when every check passed, 1 otherwise.

set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
DI_ADDR=${E2E_DI_ADDR:-127.0.0.1:5001}
READ_ADDR=${E2E_READ_ADDR:-127.0.0.1:5002}
GW_ADDR=${E2E_GATEWAY_ADDR:-127.0.0.1:18080}
GW="http://$GW_ADDR"
DI_UP="http://$DI_ADDR"
READ_UP="http://$READ_ADDR"

# A version only this run's binary reports, so /_gw/version proves the gateway answering is the
# one just built and not a stale process left on the port.
VERSION="e2e-$$-$(date +%s)"
# Every Azure route must echo this back.
CRID="e2e-$(date +%s)-client-request-id"
DIQ="api-version=2024-11-30"
DIM="/documentintelligence/documentModels/prebuilt-layout"
ZERO="00000000-0000-4000-8000-000000000000"
GUID='[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}'

# The fake delays every synchronous call by this much. Without it a job can finish between the
# 202 and the first poll, and "Retry-After only while running" could not be checked reliably.
FAKE_LATENCY=1s

TMPROOT=${TMPDIR:-/tmp}
TMPROOT=${TMPROOT%/}
WORK=$(mktemp -d "$TMPROOT/agw-e2e.XXXXXX") || exit 1
DATA_DIR=$(mktemp -d "$TMPROOT/agw-e2e-data.XXXXXX") || exit 1
BIN="$WORK/bin"
H="$WORK/headers"
B="$WORK/body"
PNG="$WORK/blank.png"
mkdir -p "$BIN"

PASSED=0
FAILED=0
SKIPPED=0
FAILURES=()
NOTES=()
FAKE_PID=""
GW_PID=""

# ------------------------------------------------------------------------------- lifecycle

# stop sends SIGTERM to a process and waits up to 10s before SIGKILL. It reaps the process, so its
# exit status is left in STOP_STATUS.
STOP_STATUS=0
stop() {
  local pid=$1 i
  STOP_STATUS=0
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null || { wait "$pid" 2>/dev/null; STOP_STATUS=$?; return 0; }
  for i in $(seq 1 50); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  kill -KILL "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  STOP_STATUS=$?
}

cleanup() {
  stop "$GW_PID"
  stop "$FAKE_PID"
  if [ "$FAILED" -eq 0 ] && [ "${E2E_KEEP:-0}" != 1 ]; then
    rm -rf "$WORK" "$DATA_DIR"
  else
    echo "Logs and data kept: $WORK (gateway.log, fakeazure.log, sdk.log) and $DATA_DIR"
  fi
}
trap cleanup EXIT
trap 'exit 130' INT TERM

die() {
  FAILED=$((FAILED + 1))
  echo "e2e-local: $*" >&2
  for log in "$WORK/fakeazure.log" "$WORK/gateway.log"; do
    if [ -s "$log" ]; then
      echo "--- last lines of $(basename "$log")" >&2
      tail -n 15 "$log" >&2
    fi
  done
  exit 1
}

# wait_for URL PID NAME polls URL until it answers 2xx, failing early if the process has exited.
wait_for() {
  local i
  for i in $(seq 1 100); do
    kill -0 "$2" 2>/dev/null || die "$3 exited during startup"
    curl -fsS -o /dev/null --max-time 2 "$1" 2>/dev/null && return 0
    sleep 0.2
  done
  die "$3 did not answer $1 within 20s"
}

# ------------------------------------------------------------------------------- checks

CASE=""
WHY=""
AZURE=0
section() { printf '\n%s\n' "$1"; }
begin() { CASE=$1; WHY=""; AZURE=0; }
fail() { WHY="${WHY:+$WHY; }$*"; }

# end records the case. When the case's last request went to an Azure route, its response also
# gets the header rules clients rely on: the container's Server header, no apim-request-id (the
# containers send none), and the client's x-ms-client-request-id echoed. The gateway's own /_gw
# routes must not pose as Kestrel.
end() {
  if [ "$AZURE" = 1 ]; then
    header Server '^Kestrel$'
    no_header apim-request-id
    header x-ms-client-request-id "^$CRID\$"
  elif [ -n "$(hdr Server)" ]; then
    fail "an admin route sends Server: $(hdr Server)"
  fi
  if [ -z "$WHY" ]; then
    PASSED=$((PASSED + 1))
    printf '  PASS  %s\n' "$CASE"
  else
    FAILED=$((FAILED + 1))
    FAILURES+=("$CASE: $WHY")
    printf '  FAIL  %s\n        %s\n' "$CASE" "$WHY"
  fi
}

skip() {
  SKIPPED=$((SKIPPED + 1))
  NOTES+=("SKIPPED  $1")
  printf '  SKIPPED  %s\n' "$1"
}

# req METHOD PATH-OR-URL [curl args...] sends one request to the gateway. The status is left in
# STATUS, the headers in $H and the body in $B. A path outside /_gw/ is an Azure route: it carries
# the client request id, and AZURE tells end() to apply the Azure header rules to the response.
req() {
  local method=$1 target=$2
  shift 2
  case "$target" in http*) ;; *) target="$GW$target" ;; esac
  AZURE=1
  case "${target#"$GW"}" in /_gw/*) AZURE=0 ;; esac
  : >"$H"
  : >"$B"
  # Expect: is emptied so curl never sends 100-continue and -D captures exactly one response.
  local args=(-sS -D "$H" -w '%{http_code}' --max-time 120 -H 'Expect:')
  [ "$AZURE" = 1 ] && args+=(-H "x-ms-client-request-id: $CRID")
  if [ "$method" = HEAD ]; then
    args+=(-I -o /dev/null)
  else
    args+=(-X "$method" -o "$B")
  fi
  STATUS=$(curl "${args[@]}" "$@" "$target" 2>"$WORK/curl.err") || STATUS="000"
  [ "$STATUS" = 000 ] && fail "no response: $(cat "$WORK/curl.err")"
  return 0
}

# hdr NAME prints the last response's NAME header, without the CR.
hdr() { grep -i "^$1:" "$H" | head -n 1 | cut -d: -f2- | sed 's/^[[:space:]]*//' | tr -d '\r'; }

status() { [ "$STATUS" = "$1" ] || fail "status $STATUS, want $1 ($(head -c 160 "$B" | tr -d '\n'))"; }
header() {
  local v
  v=$(hdr "$1")
  printf '%s' "$v" | grep -Eq -- "$2" || fail "$1 is '$v', want /$2/"
}
no_header() { grep -qi "^$1:" "$H" && fail "unexpected header $(grep -i "^$1:" "$H" | head -n 1 | tr -d '\r')"; return 0; }
no_body() { [ -s "$B" ] && fail "want no body, got $(head -c 120 "$B" | tr -d '\n')"; return 0; }

# json EXPR evaluates a Python expression over the parsed body, bound to d (re is imported).
# eval is safe here: every EXPR is a literal in this script; the gateway's response is only ever
# parsed as JSON data, never evaluated.
json() {
  python3 -I -c 'import json, re, sys
d = json.load(open(sys.argv[2], encoding="utf-8"))
sys.exit(0 if eval(sys.argv[1]) else 1)' "$1" "$B" 2>/dev/null ||
    fail "body fails $1: $(head -c 200 "$B" | tr -d '\n')"
}

# field EXPR prints a Python expression over the parsed body, or nothing.
field() {
  python3 -I -c 'import json, sys
d = json.load(open(sys.argv[2], encoding="utf-8"))
print(eval(sys.argv[1]))' "$1" "$B" 2>/dev/null
}

# poll URL GETs an operation until it leaves notStarted/running, for up to 60s. The final
# response stays in $H and $B.
poll() {
  local i st
  for i in $(seq 1 120); do
    req GET "$1"
    st=$(field 'd.get("status")')
    case "$st" in notStarted|running) sleep 0.5 ;; *) return 0 ;; esac
  done
  fail "$1 was still running after 60s"
}

# relayed PATH UPSTREAM: the gateway must answer exactly as the container does.
relayed() {
  begin "GET $1 is relayed"
  req GET "$1"
  local want
  want=$(curl -sS -o "$WORK/direct" -w '%{http_code}' --max-time 30 "$2$1" 2>/dev/null)
  [ "$STATUS" = "$want" ] || fail "status $STATUS, the container answers $want"
  cmp -s "$B" "$WORK/direct" || fail "body differs from the container's: $(head -c 120 "$B" | tr -d '\n')"
  end
}

# ------------------------------------------------------------------------------- start

echo "e2e-local: building into $BIN"
(cd "$ROOT" && go build -trimpath -ldflags "-X main.version=$VERSION" -o "$BIN/gateway" ./cmd/gateway) ||
  die "go build ./cmd/gateway failed"
(cd "$ROOT" && go build -trimpath -o "$BIN/fakeazure" ./cmd/fakeazure) ||
  die "go build ./cmd/fakeazure failed"

for addr in "$DI_ADDR" "$READ_ADDR" "$GW_ADDR"; do
  if curl -s -o /dev/null --max-time 2 "http://$addr/" 2>/dev/null; then
    die "something already answers on $addr; stop it or set E2E_DI_ADDR, E2E_READ_ADDR, E2E_GATEWAY_ADDR"
  fi
done

# A blank 200x120 white PNG, above Azure's 50x50 minimum, built with the standard library so the
# route checks need nothing installed.
python3 -I -c 'import struct, sys, zlib
w, h = 200, 120
def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
rows = b"".join(b"\x00" + b"\xff" * (w * 3) for _ in range(h))
open(sys.argv[1], "wb").write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0))
    + chunk(b"IDAT", zlib.compress(rows)) + chunk(b"IEND", b""))' "$PNG" ||
  die "could not write the test PNG"

echo "e2e-local: starting fakeazure on $DI_ADDR (DI) and $READ_ADDR (Read)"
"$BIN/fakeazure" -di "$DI_ADDR" -read "$READ_ADDR" -latency "$FAKE_LATENCY" >"$WORK/fakeazure.log" 2>&1 &
FAKE_PID=$!
wait_for "$DI_UP/status" "$FAKE_PID" fakeazure
wait_for "$READ_UP/status" "$FAKE_PID" fakeazure

# env -i: only the settings below reach the gateway. A setting exported in the caller's shell,
# such as one of the removed ones the gateway refuses to start with, must not change the run.
echo "e2e-local: starting the gateway on $GW_ADDR (SPLIT_PAGES=5, DATA_DIR=$DATA_DIR)"
env -i PATH="$PATH" HOME="${HOME:-/tmp}" TMPDIR="${TMPDIR:-/tmp}" \
  DATA_DIR="$DATA_DIR" DI_UPSTREAM_URL="$DI_UP" READ_UPSTREAM_URL="$READ_UP" \
  SPLIT_PAGES=5 PUBLIC_BASE_URL="$GW" GATEWAY_ADDR="$GW_ADDR" \
  "$BIN/gateway" serve >"$WORK/gateway.log" 2>&1 &
GW_PID=$!
wait_for "$GW/_gw/live" "$GW_PID" gateway

# ------------------------------------------------------------------------------- 1. every route

section "1. Document Intelligence routes"

begin "POST :analyze (documentintelligence) -> 202"
req POST "$DIM:analyze?$DIQ" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 202
header Operation-Location "^$GW$DIM/analyzeResults/$GUID\\?$DIQ\$"
header Retry-After '^1$'
header Content-Length '^0$'
no_header Location
no_body
end
DI_OP=$(hdr Operation-Location)
DI_ID=$(printf '%s' "$DI_OP" | sed 's/?.*//; s:.*/::')

begin "GET the poll URL while running -> 200 with Retry-After"
req GET "$DI_OP"
status 200
header Retry-After '^1$'
json 'd["status"] in ("notStarted", "running") and "analyzeResult" not in d'
end

begin "HEAD the poll URL while running -> 200 with Retry-After, no body"
req HEAD "$DI_OP"
status 200
header Retry-After '^1$'
end

begin "GET the poll URL after success -> terminal envelope, keys in order, no Retry-After"
poll "$DI_OP"
status 200
no_header Retry-After
json 'list(d) == ["status", "createdDateTime", "lastUpdatedDateTime", "analyzeResult"] and d["status"] == "succeeded"'
end

begin "HEAD the poll URL after success -> 200, no Retry-After"
req HEAD "$DI_OP"
status 200
no_header Retry-After
end

for id in "$DI_ID" "$ZERO"; do
  for artifact in pdf figures/1.1; do
    label=known
    [ "$id" = "$ZERO" ] && label=unknown
    begin "GET .../analyzeResults/{$label}/$artifact -> 404 with a body"
    req GET "$DIM/analyzeResults/$id/$artifact?$DIQ"
    status 404
    json 'isinstance(d, dict) and d'
    end
  done
done

begin "DELETE the result -> 204"
req DELETE "$DI_OP"
status 204
no_body
end

begin "GET the deleted result -> flat DI 404, no Retry-After"
req GET "$DI_OP"
status 404
no_header Retry-After
json 'd.get("code") == "NotFound" and "message" in d and "error" not in d'
end

begin "POST :analyze (formrecognizer, 2023-07-31) -> 202 with a /formrecognizer/ Operation-Location"
req POST "/formrecognizer/documentModels/prebuilt-layout:analyze?api-version=2023-07-31" \
  -H 'Content-Type: image/png' --data-binary "@$PNG"
status 202
header Operation-Location "^$GW/formrecognizer/documentModels/prebuilt-layout/analyzeResults/$GUID\\?api-version=2023-07-31\$"
header Retry-After '^1$'
end
FR_OP=$(hdr Operation-Location)

begin "GET the formrecognizer poll URL after success -> succeeded"
poll "$FR_OP"
status 200
# Status only: the paged fake answers every API family with apiVersion 2024-11-30 (docgen), so the
# result's apiVersion says nothing about the gateway here.
json 'd["status"] == "succeeded" and d["analyzeResult"]["modelId"] == "prebuilt-layout"'
end

begin "POST :syncAnalyze -> 200 JSON, no Operation-Location"
req POST "$DIM:syncAnalyze?$DIQ" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 200
no_header Operation-Location
header Content-Type '^application/json'
json 'd["status"] == "succeeded" and d["analyzeResult"]["modelId"] == "prebuilt-layout"'
end

for path in "/documentintelligence/info?$DIQ" "/documentintelligence/documentModels?$DIQ" \
  "$DIM?$DIQ" "/documentintelligence/operations?$DIQ" "/documentintelligence/operations/$ZERO?$DIQ"; do
  relayed "$path" "$DI_UP"
done

begin "POST :analyzeBatch -> 400"
req POST "$DIM:analyzeBatch?$DIQ" -H 'Content-Type: application/json' --data-binary '{}'
status 400
json 'd["error"]["code"]'
end

begin "POST :bogus -> 404"
req POST "$DIM:bogus?$DIQ" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 404
end

begin "PUT on the analyze path -> 405 with Allow"
req PUT "$DIM:analyze?$DIQ"
status 405
header Allow 'POST'
end

begin "POST :analyze without api-version -> 400 InvalidArgument/ParameterMissing"
req POST "$DIM:analyze" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 400
json 'd["error"]["code"] == "InvalidArgument" and d["error"]["innererror"]["code"] == "ParameterMissing"'
end

begin "GET an unknown result id -> flat DI 404, no Retry-After"
req GET "$DIM/analyzeResults/$ZERO?$DIQ"
status 404
no_header Retry-After
json 'd.get("code") == "NotFound" and "error" not in d'
end

section "1. Computer Vision Read routes"

begin "POST /vision/v3.2/read/analyze -> 202 with a bare-GUID Operation-Location"
req POST "/vision/v3.2/read/analyze" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 202
header Operation-Location "^$GW/vision/v3.2/read/analyzeResults/$GUID\$"
no_body
end
READ_OP=$(hdr Operation-Location)

begin "GET /vision/v3.2/read/analyzeResults/{id} -> 200 with readResults"
poll "$READ_OP"
status 200
json 'd["status"] == "succeeded" and isinstance(d["analyzeResult"]["readResults"], list)'
end

begin "POST /vision/v3.2/read/syncAnalyze -> 200"
req POST "/vision/v3.2/read/syncAnalyze" -H 'Content-Type: image/png' --data-binary "@$PNG"
status 200
no_header Operation-Location
json 'd["status"] == "succeeded" and isinstance(d["analyzeResult"]["readResults"], list)'
end

begin "GET /vision/v3.2/read/operations/{guid} -> bodyless 404"
req GET "/vision/v3.2/read/operations/$ZERO"
status 404
no_body
end

begin "GET an unknown Read result id -> wrapped 404, no Retry-After"
req GET "/vision/v3.2/read/analyzeResults/$ZERO"
status 404
no_header Retry-After
json 'isinstance(d.get("error"), dict) and d["error"].get("code")'
end

section "1. Container-level routes"

for path in / /swagger /swagger/v1/swagger.json /records/usage-logs /records/usage-logs/10/2026; do
  relayed "$path" "$DI_UP"
done

begin "GET /records/usage-logs/xx/yy -> bodyless 404"
req GET /records/usage-logs/xx/yy
status 404
no_body
end

begin "GET /ready -> {\"ready\":\"ready\"}"
req GET /ready
status 200
json 'd == {"ready": "ready"}'
end

begin "GET /status -> apiStatus"
req GET /status
status 200
json '"apiStatus" in d'
end

begin "GET /nope (unrouted) -> bodyless 404"
req GET /nope
status 404
no_body
end

section "1. Gateway admin routes"

admin() {
  begin "GET /_gw/$1 -> 200 with its documented keys"
  req GET "/_gw/$1"
  status 200
  json "$2"
  end
}
admin live 'd["status"] == "ok"'
admin ready 'd["status"] == "ready"'
admin health '{"status", "uptime", "build", "store", "capacity", "upstreams", "jobs"} <= set(d) and len(d["upstreams"]) == 2 and all(u["ok"] for u in d["upstreams"])'
admin version "d[\"version\"] == \"$VERSION\" and {\"commit\", \"built\", \"go\"} <= set(d)"
admin config 'd["diEnabled"] and d["readEnabled"] and d["splitPages"] == 5 and d["diMaxInflight"] > 0'
# The formrecognizer and Read jobs: the deleted DI job is gone, and synchronous calls are no jobs.
admin jobs 'd["count"] >= 2 and {"id", "surface", "status", "upstreamParts"} <= set(d["jobs"][0])'
READ_ID=${READ_OP##*/}
admin "jobs/$READ_ID" "d[\"id\"] == \"$READ_ID\" and d[\"surface\"] == \"read\" and d[\"status\"] == \"succeeded\""

begin "GET /_gw/metrics -> 200 Prometheus text with the engine families"
req GET /_gw/metrics
status 200
header Content-Type '^text/plain'
for family in gateway_jobs_total gateway_parts_total gateway_split_fallback_total \
  gateway_upstream_retries_total gateway_upstream_exhausted_total; do
  grep -q "^# TYPE $family " "$B" || fail "no $family family"
done
end

# ------------------------------------------------------------------------------- 2. real SDKs

section "2. Official Azure SDKs (test/sdk/run.sh, strict)"
GATEWAY_URL="$GW" SDK_STRICT=1 "$ROOT/test/sdk/run.sh" 2>&1 | tee "$WORK/sdk.log"
SDK_STATUS=${PIPESTATUS[0]}
SDK_LINE=$(grep -E '^[0-9]+ passed, [0-9]+ failed$' "$WORK/sdk.log" | tail -n 1)
begin "test/sdk/run.sh: ${SDK_LINE:-no summary line}"
[ "$SDK_STATUS" = 0 ] || fail "exit status $SDK_STATUS"
[ -n "$SDK_LINE" ] || fail "the suite printed no summary"
end
VENV_PY="$ROOT/test/sdk/.venv/bin/python"

# ------------------------------------------------------------------------------- 3. split jobs

section "3. Split jobs (SPLIT_PAGES=5)"

# Written by the SDK harness's own builder, so these are the PDFs the SDK checks send: Pillow
# pages sized the way the paged fake identifies them (see sample_pdf in test/sdk/sdk_test.py).
PDFS="$WORK/pdfs"
mkdir -p "$PDFS"
begin "write 7, 12 and 23-page PDFs with test/sdk/sdk_test.py write-pdf"
for n in 7 12 23; do
  "$VENV_PY" "$ROOT/test/sdk/sdk_test.py" write-pdf "$n" "$PDFS/pages-$n.pdf" ||
    fail "could not write a $n-page PDF"
done
: >"$H"
end

# The paged fake answers page i with "Heading P<i>", so the merged content names every page once,
# in order, only when each part landed where it belongs.
IN_ORDER='re.findall(r"Heading P(\d+)", d["analyzeResult"]["content"]) == [str(i) for i in range(1, 13)]'

begin "DI :analyze, 12-page PDF -> 12 pages in order, merged from 3 parts"
req POST "$DIM:analyze?$DIQ" -H 'Content-Type: application/pdf' --data-binary "@$PDFS/pages-12.pdf"
status 202
op=$(hdr Operation-Location)
poll "$op"
json '[p["pageNumber"] for p in d["analyzeResult"]["pages"]] == list(range(1, 13))'
json "$IN_ORDER"
id=$(printf '%s' "$op" | sed 's/?.*//; s:.*/::')
req GET "/_gw/jobs/$id"
# 12 pages at SPLIT_PAGES=5 is three parts of four (pdfsplit.Plan balances them), one call each
# when nothing fails.
json 'd["status"] == "succeeded" and d["upstreamParts"] == 3'
end

begin "Read /read/analyze, 12-page PDF -> 12 readResults in order, merged from 3 parts"
req POST "/vision/v3.2/read/analyze" -H 'Content-Type: application/pdf' --data-binary "@$PDFS/pages-12.pdf"
status 202
op=$(hdr Operation-Location)
poll "$op"
json '[r["page"] for r in d["analyzeResult"]["readResults"]] == list(range(1, 13))'
req GET "/_gw/jobs/${op##*/}"
json 'd["status"] == "succeeded" and d["upstreamParts"] == 3'
end

begin "DI :syncAnalyze with pages=3-9 on a 12-page PDF -> exactly pages 3..9"
req POST "$DIM:syncAnalyze?$DIQ&pages=3-9" -H 'Content-Type: application/pdf' --data-binary "@$PDFS/pages-12.pdf"
status 200
json '[p["pageNumber"] for p in d["analyzeResult"]["pages"]] == list(range(3, 10))'
end

begin "GET /_gw/metrics after the split jobs -> parts counted, no split fell back"
req GET /_gw/metrics
status 200
python3 -I -c 'import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
def total(family, **labels):
    n = 0.0
    for line in text.splitlines():
        m = re.match(family + r"\{([^}]*)\} (\S+)$", line)
        if m and all(f"{k}=\"{v}\"" in m.group(1) for k, v in labels.items()):
            n += float(m.group(2))
    return n
ok = (total("gateway_parts_total", surface="di", outcome="succeeded") > 0
      and total("gateway_parts_total", surface="read", outcome="succeeded") > 0
      and total("gateway_split_fallback_total") == 0)
sys.exit(0 if ok else 1)' "$B" ||
  fail "want succeeded parts on both surfaces and no split fallback: $(grep -E '^gateway_(parts_total|split_fallback_total)' "$B" | grep -v ' 0$' | tr '\n' ' ')"
end

# ------------------------------------------------------------------------------- 4. compare-split

section "4. gateway probe --compare-split"

# The splitting go-live measurement (splitting spec §6): every PDF analysed whole and split, the
# results diffed, and any page-level difference fails the probe.
begin "gateway probe --compare-split on 7, 12 and 23-page PDFs -> no page-level difference"
mkdir -p "$WORK/probe-data"
env -i PATH="$PATH" HOME="${HOME:-/tmp}" TMPDIR="${TMPDIR:-/tmp}" \
  DATA_DIR="$WORK/probe-data" DI_UPSTREAM_URL="$DI_UP" READ_UPSTREAM_URL="$READ_UP" \
  SPLIT_PAGES=5 PROBE_TIMEOUT=2m \
  "$BIN/gateway" probe --compare-split "$PDFS" >"$WORK/compare.log" 2>&1
rc=$?
sed 's/^/        /' "$WORK/compare.log" | tail -n 25
[ "$rc" = 0 ] || fail "exit status $rc"
[ -s "$PDFS/compare-report.json" ] || fail "no compare-report.json written"
AZURE=0
: >"$H"
end

# ------------------------------------------------------------------------------- shutdown

section "Shutdown"
begin "SIGTERM -> the gateway drains and exits 0"
stop "$GW_PID"
[ "$STOP_STATUS" = 0 ] || fail "exit status $STOP_STATUS"
grep -q '"shutdown complete"' "$WORK/gateway.log" || fail "no \"shutdown complete\" in the log"
GW_PID=""
: >"$H"
end

# ------------------------------------------------------------------------------- summary

echo
echo "============================================================"
echo "e2e-local: $PASSED passed, $FAILED failed, $SKIPPED skipped"
[ -n "${SDK_LINE:-}" ] && echo "  real SDKs: $SDK_LINE"
for note in ${NOTES[@]+"${NOTES[@]}"}; do
  echo "  $note"
done
if [ "$FAILED" -gt 0 ]; then
  echo
  echo "Failures:"
  for f in "${FAILURES[@]}"; do
    echo "  - $f"
  done
  exit 1
fi
exit 0
