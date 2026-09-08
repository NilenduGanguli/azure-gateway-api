#!/bin/sh
# probe-containers.sh — interrogate the two Azure containers and print a short report.
#
# The gateway has to be indistinguishable from these containers, and the containers do not always
# match their own documentation. This asks yours what they actually do, so the gateway is built
# against observed behaviour rather than inference.
#
# It answers, in order of how much they change the design:
#   - does this build serve POST .../{modelId}:syncAnalyze, and how slow is it cold
#   - what error shape does each surface really emit — wrapped {"error":{...}} or flat {"code":...}
#   - the exact Operation-Location format, and whether Retry-After is present
#   - which routes the swagger actually declares
#
# Only curl is required. It sends a blank 200x120 white PNG, never a document of yours; it prints
# no API key and writes nothing outside a temp directory.
#
# Usage:
#   DI_UPSTREAM_URL=http://layout:5000 READ_UPSTREAM_URL=http://ocr:5000 ./probe-containers.sh
#
# Optional:
#   DI_UPSTREAM_API_KEY, READ_UPSTREAM_API_KEY   sent as Ocp-Apim-Subscription-Key
#   API_VERSION        (default 2024-11-30)
#   MODEL_ID           (default prebuilt-layout)
#   POLL_SECONDS       (default 90)  how long to wait for an async round trip
#   SYNC_MAX_SECONDS   (default 300) per-request ceiling for an analyze
#   REQ_MAX_SECONDS    (default 30)  per-request ceiling for everything else
#
# Cost: roughly ten analyses per container, billed like any other query.

set -u

API_VERSION="${API_VERSION:-2024-11-30}"
MODEL_ID="${MODEL_ID:-prebuilt-layout}"
POLL_SECONDS="${POLL_SECONDS:-90}"
SYNC_MAX_SECONDS="${SYNC_MAX_SECONDS:-300}"
REQ_MAX_SECONDS="${REQ_MAX_SECONDS:-30}"
ZERO_GUID="00000000-0000-0000-0000-000000000000"

DI_UPSTREAM_URL="${DI_UPSTREAM_URL:-}"
READ_UPSTREAM_URL="${READ_UPSTREAM_URL:-}"
DI_UPSTREAM_API_KEY="${DI_UPSTREAM_API_KEY:-}"
READ_UPSTREAM_API_KEY="${READ_UPSTREAM_API_KEY:-}"

if [ -z "$DI_UPSTREAM_URL" ] && [ -z "$READ_UPSTREAM_URL" ]; then
  echo "Set DI_UPSTREAM_URL and/or READ_UPSTREAM_URL. See the header of this script." >&2
  exit 2
fi
if ! command -v curl >/dev/null 2>&1; then
  echo "curl is required." >&2
  exit 2
fi

TMP=$(mktemp -d 2>/dev/null || mktemp -d -t probe)
trap 'rm -rf "$TMP"' EXIT INT TERM

# A blank 200x120 white PNG. Above Azure's 50x50 minimum, and contains nothing of yours.
cat >"$TMP/img.b64" <<'B64'
iVBORw0KGgoAAAANSUhEUgAAAMgAAAB4CAIAAAA48Cq8AAABSUlEQVR4nO3SMQEAIAzAMMC/52Fi/RIB
vXpn5sC2t14EY1ExFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFglj
kTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFglj
kTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFglj
kTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFgljkTAWCWORMBYJY5EwFglj
kTAWCWORMBYJY5EwFgljkTAWp/ABNfcD7YrNEhsAAAAASUVORK5CYII=
B64
if base64 -d <"$TMP/img.b64" >"$TMP/probe.png" 2>/dev/null; then :
elif base64 -D <"$TMP/img.b64" >"$TMP/probe.png" 2>/dev/null; then :
else
  echo "Could not decode the test image; base64 accepts neither -d nor -D here." >&2
  exit 2
fi
printf 'not a document' >"$TMP/junk.txt"

# ---------------------------------------------------------------- output helpers
# Everything is kept inside 78 columns so a screenshot stays readable.

hr() { printf '%s\n' "-----------------------------------------------------------------------------"; }

row() {
  if [ -n "${4:-}" ]; then
    printf '  %-5s %-28s %-4s %s\n' "$1" "$2" "$3" "$(printf '%s' "$4" | cut -c1-36)"
  else
    printf '  %-5s %-28s %s\n' "$1" "$2" "$3"
  fi
}

note() { [ -n "${1:-}" ] && printf '        -> %s\n' "$(printf '%s' "$1" | cut -c1-66)"; }

# body prints a response across up to two lines, so a short error is never cut in half.
body() {
  body_t=$(tr -d '\r\n' <"$1" 2>/dev/null | tr -s ' ')
  [ -n "$body_t" ] || return 0
  printf '        -> %s\n' "$(printf '%s' "$body_t" | cut -c1-66)"
  body_r=$(printf '%s' "$body_t" | cut -c67-132)
  [ -n "$body_r" ] && printf '           %s\n' "$body_r"
  return 0
}

pathonly() { printf '%s' "$1" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://[^/]*||'; }

jget() {
  tr -d '\r\n' <"$1" 2>/dev/null |
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
}

hdr() {
  grep -i "^$2:" "$1" 2>/dev/null | head -1 | cut -d: -f2- |
    sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

# shape classifies an error body the way a client's SDK has to.
#   wrapped  {"error":{"code":...}}    what the Document Intelligence models expect
#   flat     {"code":...,"message":}   what the Computer Vision Read models expect
shape() {
  if grep -q '"error"[[:space:]]*:' "$1" 2>/dev/null; then printf 'wrapped'
  elif grep -q '"code"[[:space:]]*:' "$1" 2>/dev/null; then printf 'flat'
  elif grep -q '"status"[[:space:]]*:' "$1" 2>/dev/null; then printf 'envelope'
  elif [ -s "$1" ]; then printf 'other'
  else printf 'empty'; fi
}

# req <method> <url> <api-key> [content-type] [body-file] [max-seconds]
#
# Writes $TMP/h and $TMP/b and echoes the status. curl's own exit code goes to $TMP/err rather
# than a variable: every caller invokes req inside a command substitution, which runs in a
# subshell, so a variable set here would never be visible to why().
req() {
  req_m=$1; req_u=$2; req_k=$3; req_ct=${4:-}; req_bf=${5:-}; req_max=${6:-$REQ_MAX_SECONDS}
  # curl writes neither file when it cannot connect, and a redirect from a missing file is a shell
  # error the helper cannot suppress. Truncating first keeps an unreachable container quiet.
  : >"$TMP/b"; : >"$TMP/h"
  set -- -s -o "$TMP/b" -D "$TMP/h" -w '%{http_code}' -X "$req_m" --max-time "$req_max"
  [ -n "$req_k" ] && set -- "$@" -H "Ocp-Apim-Subscription-Key: $req_k"
  [ -n "$req_ct" ] && set -- "$@" -H "Content-Type: $req_ct"
  [ -n "$req_bf" ] && set -- "$@" --data-binary "@$req_bf"
  # curl already prints 000 on failure and exits non-zero; adding our own would make it "000000".
  req_code=$(curl "$@" "$req_u" 2>/dev/null)
  echo $? >"$TMP/err"
  [ -n "$req_code" ] || req_code=000
  printf '%s' "$req_code"
}

# why turns a 000 into something actionable. A timeout and a refused connection mean very
# different things when the next call to the same host succeeds.
why() {
  why_err=$(cat "$TMP/err" 2>/dev/null)
  [ -n "$why_err" ] || why_err=0
  case "$why_err" in
    28)    printf 'TIMED OUT after %ss' "$1" ;;
    7)     printf 'connection refused' ;;
    6)     printf 'DNS lookup failed' ;;
    35|60) printf 'TLS failure' ;;
    52)    printf 'empty reply from server' ;;
    *)     printf 'no response (curl exit %s)' "$why_err" ;;
  esac
}

# ---------------------------------------------------------------- findings

DI_SYNC="not probed"; DI_SHAPE="-"; DI_ASYNC="-"; DI_PDF="-"; DI_ERRSHAPE="-"; DI_SWAGSYNC="-"
RD_SYNC="not probed"; RD_ASYNC="-"; RD_ALIAS="-"; RD_ERRSHAPE="-"
WARN_VISION="no"

# errcase runs one deliberately-bad request and reports the shape a client would have to parse.
#
# It sets ERRCASE_SHAPE rather than echoing it: the row and body go to stdout, so returning the
# shape through command substitution would capture the printed report along with it.
ERRCASE_SHAPE=""
errcase() {
  ec_label=$1; ec_m=$2; ec_u=$3; ec_k=$4; ec_ct=${5:-}; ec_bf=${6:-}
  ec_st=$(req "$ec_m" "$ec_u" "$ec_k" "$ec_ct" "$ec_bf")
  ERRCASE_SHAPE=$(shape "$TMP/b")
  printf '  %-5s %-26s %-4s %s\n' "$ec_m" "$ec_label" "$ec_st" "$ERRCASE_SHAPE"
  body "$TMP/b"
}

probe_di() {
  base=$(printf '%s' "$DI_UPSTREAM_URL" | sed 's:/*$::')
  key=$DI_UPSTREAM_API_KEY
  q="api-version=$API_VERSION"
  dm="$base/documentintelligence/documentModels"

  hr; printf 'DOCUMENT INTELLIGENCE   %s\n' "$base"; hr

  st=$(req GET "$base/status" "$key")
  row GET "/status" "$st" "apiStatus=$(jget "$TMP/b" apiStatus)"
  st=$(req GET "$base/ready" "$key")
  row GET "/ready" "$st" "ready=$(jget "$TMP/b" ready)"

  for p in /swagger/v1/swagger.json /swagger/v1.0/swagger.json /swagger/docs/v1; do
    st=$(req GET "$base$p" "$key")
    if [ "$st" = "200" ]; then
      n=$(grep -o '"/[^"]*"' "$TMP/b" 2>/dev/null | sort -u | wc -l | tr -d ' ')
      row GET "$p" "$st" "$n paths declared"
      grep -o '"/[^"]*"' "$TMP/b" 2>/dev/null | sort -u | tr -d '"' |
        while IFS= read -r sp; do
          case "$sp" in
            *[Ss]ync*|*nalyze*|*info*|*ocumentModels*) note "$sp" ;;
          esac
        done
      if grep -q 'syncAnalyze' "$TMP/b" 2>/dev/null; then
        DI_SWAGSYNC="declared"
      else
        DI_SWAGSYNC="not declared"
      fi
      break
    fi
  done
  [ "$st" = "200" ] || row GET "/swagger*" "$st" "not found at the known paths"

  # The decisive question, given its own ceiling. A cold container can spend minutes loading a
  # model on the first request, and a timeout is not the same answer as a 404.
  st=$(req POST "$dm/$MODEL_ID:syncAnalyze?$q" "$key" image/png "$TMP/probe.png" "$SYNC_MAX_SECONDS")
  row POST ":syncAnalyze" "$st" ""
  case "$st" in
    200)
      DI_SYNC="AVAILABLE"
      if grep -q '"analyzeResult"' "$TMP/b" 2>/dev/null; then
        DI_SHAPE="envelope (.analyzeResult present)"
      else
        DI_SHAPE="bare AnalyzeResult (no envelope)"
      fi
      note "$DI_SHAPE"; body "$TMP/b" ;;
    202) DI_SYNC="DEGRADES TO 202"
         note "Operation-Location: $(pathonly "$(hdr "$TMP/h" operation-location)")" ;;
    404) DI_SYNC="ABSENT (404)"; body "$TMP/b" ;;
    500) DI_SYNC="ABSENT (500)"; body "$TMP/b" ;;
    000) DI_SYNC="NO ANSWER: $(why "$SYNC_MAX_SECONDS")"
         note "$DI_SYNC"
         note "if the swagger declares it above, the route exists and is slow" ;;
    *)   DI_SYNC="unexpected $st"; body "$TMP/b" ;;
  esac

  # Async round trip: the real Operation-Location format, and whether Retry-After is present.
  st=$(req POST "$dm/$MODEL_ID:analyze?$q" "$key" image/png "$TMP/probe.png" "$SYNC_MAX_SECONDS")
  loc=$(hdr "$TMP/h" operation-location)
  ra=$(hdr "$TMP/h" retry-after)
  apim=$(hdr "$TMP/h" apim-request-id)
  row POST ":analyze" "$st" ""
  if [ "$st" = "202" ]; then
    DI_ASYNC="202"
    note "Operation-Location: $(pathonly "$loc")"
    note "Retry-After: ${ra:-(absent)}   apim-request-id: ${apim:-(absent)}"
    if [ -n "$loc" ]; then
      waited=0; s=""
      while [ "$waited" -lt "$POLL_SECONDS" ]; do
        st=$(req GET "$loc" "$key")
        s=$(jget "$TMP/b" status)
        case "$s" in succeeded|failed) break ;; esac
        sleep 3; waited=$((waited + 3))
      done
      row GET "analyzeResults/{id}" "$st" "status=${s:-?} after ${waited}s"
      [ "$s" = "failed" ] && body "$TMP/b"
      rid=$(printf '%s' "$loc" | sed 's/?.*//; s:.*/::')
      st=$(req GET "$dm/$MODEL_ID/analyzeResults/$rid/pdf?$q" "$key")
      DI_PDF="$st"
      row GET "analyzeResults/{id}/pdf" "$st" "ct=$(hdr "$TMP/h" content-type)"
    fi
  else
    body "$TMP/b"
  fi

  st=$(req GET "$base/documentintelligence/info?$q" "$key")
  row GET "/info" "$st" ""
  st=$(req GET "$dm?$q" "$key")
  row GET "/documentModels" "$st" ""
  echo

  hr; printf 'DOCUMENT INTELLIGENCE  error shapes\n'; hr
  errcase "analyzeResults/{unknown}" GET "$dm/$MODEL_ID/analyzeResults/$ZERO_GUID?$q" "$key"
  DI_ERRSHAPE=$ERRCASE_SHAPE
  errcase "bad api-version" POST "$dm/$MODEL_ID:analyze?api-version=1999-01-01" \
    "$key" image/png "$TMP/probe.png"
  errcase "unknown model" POST "$dm/no-such-model-xyz:analyze?$q" \
    "$key" image/png "$TMP/probe.png"
  errcase "text body sent as pdf" POST "$dm/$MODEL_ID:analyze?$q" \
    "$key" application/pdf "$TMP/junk.txt"
  errcase "unrouted path" GET "$base/documentintelligence/nope?$q" "$key"
  echo
}

probe_read() {
  base=$(printf '%s' "$READ_UPSTREAM_URL" | sed 's:/*$::')
  key=$READ_UPSTREAM_API_KEY
  rd="$base/vision/v3.2/read"

  hr; printf 'COMPUTER VISION READ    %s\n' "$base"; hr

  host=$(printf '%s' "$base" | sed 's://:_:; s:^[^_]*_::; s:/.*::; s/:.*//')
  case "$host" in *[Vv][Ii][Ss][Ii][Oo][Nn]*) WARN_VISION="yes" ;; esac
  [ "$WARN_VISION" = "yes" ] && \
    note 'host contains "vision": the container corrupts its Operation-Location'

  st=$(req GET "$base/status" "$key")
  row GET "/status" "$st" "apiStatus=$(jget "$TMP/b" apiStatus)"
  st=$(req GET "$base/ready" "$key")
  row GET "/ready" "$st" "ready=$(jget "$TMP/b" ready)"

  for p in /swagger/vision-v3.2-read/swagger.json /swagger/v1/swagger.json; do
    st=$(req GET "$base$p" "$key")
    if [ "$st" = "200" ]; then
      n=$(grep -o '"/[^"]*"' "$TMP/b" 2>/dev/null | sort -u | wc -l | tr -d ' ')
      row GET "$p" "$st" "$n paths declared"
      grep -o '"/[^"]*"' "$TMP/b" 2>/dev/null | sort -u | tr -d '"' |
        while IFS= read -r sp; do
          case "$sp" in *[Ss]ync*|*nalyze*|*perations*) note "$sp" ;; esac
        done
      break
    fi
  done
  [ "$st" = "200" ] || row GET "/swagger*" "$st" "not found at the known paths"

  st=$(req POST "$rd/syncAnalyze" "$key" image/png "$TMP/probe.png" "$SYNC_MAX_SECONDS")
  row POST "/read/syncAnalyze" "$st" ""
  case "$st" in
    200) RD_SYNC="AVAILABLE"
         rs=$(jget "$TMP/b" status)
         [ -n "$rs" ] && note "envelope status=$rs (a blank page may legitimately fail)"
         body "$TMP/b" ;;
    202) RD_SYNC="DEGRADES TO 202" ;;
    000) RD_SYNC="NO ANSWER: $(why "$SYNC_MAX_SECONDS")"; note "$RD_SYNC" ;;
    *)   RD_SYNC="unexpected $st"; body "$TMP/b" ;;
  esac

  st=$(req POST "$rd/analyze" "$key" image/png "$TMP/probe.png" "$SYNC_MAX_SECONDS")
  loc=$(hdr "$TMP/h" operation-location)
  row POST "/read/analyze" "$st" ""
  if [ "$st" = "202" ]; then
    RD_ASYNC="202"
    note "Operation-Location: $(pathonly "$loc")"
    note "op-loc host: $(printf '%s' "$loc" | sed 's|^\(.*//[^/]*\).*|\1|')"
    note "Retry-After: $(hdr "$TMP/h" retry-after)"
    if [ -n "$loc" ]; then
      waited=0; s=""
      while [ "$waited" -lt "$POLL_SECONDS" ]; do
        st=$(req GET "$loc" "$key")
        s=$(jget "$TMP/b" status)
        case "$s" in succeeded|failed) break ;; esac
        sleep 3; waited=$((waited + 3))
      done
      row GET "analyzeResults/{id}" "$st" "status=${s:-?} after ${waited}s"
      body "$TMP/b"
    fi
  fi

  st=$(req GET "$rd/operations/$ZERO_GUID" "$key")
  RD_ALIAS="$st"
  row GET "operations/{unknown}" "$st" ""
  note "stale-doc alias; 404 here means only analyzeResults is served"
  echo

  hr; printf 'COMPUTER VISION READ  error shapes\n'; hr
  errcase "analyzeResults/{unknown}" GET "$rd/analyzeResults/$ZERO_GUID" "$key"
  RD_ERRSHAPE=$ERRCASE_SHAPE
  errcase "malformed operation id" GET "$rd/analyzeResults/not-a-guid" "$key"
  errcase "bad readingOrder" POST "$rd/analyze?readingOrder=sideways" \
    "$key" image/png "$TMP/probe.png"
  errcase "text body sent as png" POST "$rd/analyze" "$key" image/png "$TMP/junk.txt"
  errcase "unrouted path" GET "$base/vision/v3.2/nope" "$key"
  echo
}

# ---------------------------------------------------------------- run

printf '\nazure-gateway-api  container probe   %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"
printf 'api-version=%s  model=%s  sync-ceiling=%ss\n\n' \
  "$API_VERSION" "$MODEL_ID" "$SYNC_MAX_SECONDS"

[ -n "$DI_UPSTREAM_URL" ] && probe_di
[ -n "$READ_UPSTREAM_URL" ] && probe_read

hr; printf 'VERDICT\n'; hr
printf '  DI   :syncAnalyze     %s\n' "$DI_SYNC"
printf '  DI   in swagger       %s\n' "$DI_SWAGSYNC"
[ "$DI_SHAPE" = "-" ] || printf '  DI   200 body shape   %s\n' "$DI_SHAPE"
printf '  DI   :analyze         %s\n' "$DI_ASYNC"
[ "$DI_PDF" = "-" ] || printf '  DI   result-file /pdf  %s\n' "$DI_PDF"
printf '  DI   error shape      %s\n' "$DI_ERRSHAPE"
printf '  READ syncAnalyze      %s\n' "$RD_SYNC"
printf '  READ /read/analyze    %s\n' "$RD_ASYNC"
printf '  READ error shape      %s\n' "$RD_ERRSHAPE"
if [ "$RD_ALIAS" = "404" ]; then
  printf '  READ operations alias 404   (not served, as expected)\n'
else
  printf '  READ operations alias %s\n' "$RD_ALIAS"
fi
printf '  READ host has "vision" %s\n' "$WARN_VISION"
echo
case "$DI_SYNC" in
  AVAILABLE)         printf '  -> keep DI_SYNC_ANALYZE=auto. The fast path works on this build.\n' ;;
  "DEGRADES TO 202") printf '  -> keep DI_SYNC_ANALYZE=auto. The gateway polls the 202 out.\n' ;;
  ABSENT*)           printf '  -> set DI_SYNC_ANALYZE=off to skip a wasted call per job.\n' ;;
  "NO ANSWER"*)      printf '  -> inconclusive. If the swagger declares it, the route exists and is\n'
                     printf '     slow; re-run with SYNC_MAX_SECONDS=600 before deciding.\n' ;;
esac
if [ "$DI_ERRSHAPE" != "-" ] && [ "$DI_ERRSHAPE" != "wrapped" ]; then
  printf '  -> DI errors are %s, not the wrapped shape its SDK models expect.\n' "$DI_ERRSHAPE"
fi
if [ "$RD_ERRSHAPE" != "-" ] && [ "$RD_ERRSHAPE" != "flat" ]; then
  printf '  -> READ errors are %s, not the flat shape its SDK models expect.\n' "$RD_ERRSHAPE"
fi
if [ "$WARN_VISION" = "yes" ]; then
  printf '  -> rename the Read service. "vision" in the host makes the container\n'
  printf '     strip the port and the /vision segment from its Operation-Location.\n'
fi
echo
