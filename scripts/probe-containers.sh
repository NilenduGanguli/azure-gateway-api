#!/bin/sh
# probe-containers.sh — interrogate the two Azure containers and print a short report.
#
# The gateway's fast path rests on a route Microsoft does not document:
# POST /documentintelligence/documentModels/{modelId}:syncAnalyze. It is present in the container
# image's own routing table, absent from every published doc and from azure-rest-api-specs, and
# older builds answer it with 500 UnhandledEndpointException. This script asks your containers
# what they actually serve, so the answer comes from your build rather than from inference.
#
# Only curl is required. Nothing is written anywhere; no document of yours is uploaded — the probe
# sends a blank 200x120 white PNG that clears Azure's 50x50 minimum. API keys are never printed.
#
# Usage:
#   DI_UPSTREAM_URL=http://layout:5000 READ_UPSTREAM_URL=http://ocr:5000 ./probe-containers.sh
#
# Optional:
#   DI_UPSTREAM_API_KEY, READ_UPSTREAM_API_KEY   sent as Ocp-Apim-Subscription-Key
#   API_VERSION   (default 2024-11-30)
#   MODEL_ID      (default prebuilt-layout)
#   POLL_SECONDS  (default 40) how long to wait for the async round trip
#
# Cost: about four analyses per container, billed like any other query.

set -u

API_VERSION="${API_VERSION:-2024-11-30}"
MODEL_ID="${MODEL_ID:-prebuilt-layout}"
POLL_SECONDS="${POLL_SECONDS:-40}"
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

# A blank 200x120 white PNG. Above Azure's 50x50 minimum, and contains no text of yours.
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

# ---------------------------------------------------------------- output helpers

# Output is kept inside 78 columns so it screenshots cleanly.
hr()  { printf '%s\n' "-----------------------------------------------------------------------------"; }

row() {
  if [ -n "${4:-}" ]; then
    printf '  %-5s %-28s %-4s %s\n' "$1" "$2" "$3" "$(printf '%s' "$4" | cut -c1-36)"
  else
    printf '  %-5s %-28s %s\n' "$1" "$2" "$3"
  fi
}

note() { [ -n "${1:-}" ] && printf '        -> %s\n' "$(printf '%s' "$1" | cut -c1-66)"; }

# short trims a body to one readable line.
short() {
  tr -d '\r\n' <"$1" 2>/dev/null | tr -s ' ' | cut -c1-66
}

# pathonly strips scheme and authority from a URL, so a long Operation-Location still fits.
# Written for POSIX sed: BSD sed rejects \? in a basic regular expression.
pathonly() {
  printf '%s' "$1" | sed -e 's|^[a-zA-Z][a-zA-Z0-9+.-]*://[^/]*||'
}

# jget pulls a top-level string field out of a JSON body without needing jq.
jget() {
  tr -d '\r\n' <"$1" 2>/dev/null |
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
}

hdr() {
  # hdr <headers-file> <name>  -> value, trimmed
  grep -i "^$2:" "$1" 2>/dev/null | head -1 | cut -d: -f2- |
    sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

# req <method> <url> <api-key> [content-type] [body-file]
# Writes $TMP/h (headers) and $TMP/b (body); echoes the status code, or 000 if unreachable.
req() {
  req_m=$1; req_u=$2; req_k=$3; req_ct=${4:-}; req_bf=${5:-}
  # curl writes neither file when it cannot connect, and a later redirect from a missing file is a
  # shell error that 2>/dev/null inside the helper cannot suppress. Truncating them first keeps an
  # unreachable container quiet.
  : >"$TMP/b"; : >"$TMP/h"
  set -- -s -o "$TMP/b" -D "$TMP/h" -w '%{http_code}' -X "$req_m" --max-time 120
  [ -n "$req_k" ] && set -- "$@" -H "Ocp-Apim-Subscription-Key: $req_k"
  [ -n "$req_ct" ] && set -- "$@" -H "Content-Type: $req_ct"
  [ -n "$req_bf" ] && set -- "$@" --data-binary "@$req_bf"
  # curl already writes 000 to stdout when it cannot connect, and then exits non-zero. Appending
  # our own 000 on failure produced "000000", which no branch below matched, so an unreachable
  # container was reported as an unexpected status instead of an unreachable one.
  req_code=$(curl "$@" "$req_u" 2>/dev/null)
  [ -n "$req_code" ] || req_code=000
  printf '%s' "$req_code"
}

# ---------------------------------------------------------------- findings

DI_SYNC="not probed"; DI_SHAPE="-"; DI_ASYNC="-"; DI_PDF="-"
RD_SYNC="not probed"; RD_ASYNC="-"; RD_ALIAS="-"
WARN_VISION="no"

probe_di() {
  base=$(printf '%s' "$DI_UPSTREAM_URL" | sed 's:/*$::')
  key=$DI_UPSTREAM_API_KEY
  q="api-version=$API_VERSION"

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
      s=$(grep -o '"/[^"]*[Ss]ync[^"]*"' "$TMP/b" 2>/dev/null | sort -u | tr '\n' ' ' | cut -c1-60)
      note "sync routes in swagger: ${s:-(none declared)}"
      break
    fi
  done
  [ "$st" = "200" ] || row GET "/swagger*" "$st" "not found at the known paths"

  # The decisive question.
  st=$(req POST "$base/documentintelligence/documentModels/$MODEL_ID:syncAnalyze?$q" \
        "$key" image/png "$TMP/probe.png")
  row POST ":syncAnalyze" "$st" ""
  case "$st" in
    200)
      DI_SYNC="AVAILABLE"
      if grep -q '"analyzeResult"' "$TMP/b" 2>/dev/null; then
        DI_SHAPE="envelope (.analyzeResult present)"
      else
        DI_SHAPE="bare AnalyzeResult (no envelope)"
      fi
      note "$DI_SHAPE"
      note "$(short "$TMP/b")"
      ;;
    202)
      DI_SYNC="DEGRADES TO 202"
      note "Operation-Location: $(pathonly "$(hdr "$TMP/h" operation-location)")"
      ;;
    404) DI_SYNC="ABSENT (404)";        note "$(short "$TMP/b")" ;;
    500) DI_SYNC="ABSENT (500)";        note "$(short "$TMP/b")" ;;
    000) DI_SYNC="UNREACHABLE" ;;
    *)   DI_SYNC="unexpected $st";      note "$(short "$TMP/b")" ;;
  esac

  # Async round trip: shows the Operation-Location format and whether Retry-After is emitted.
  st=$(req POST "$base/documentintelligence/documentModels/$MODEL_ID:analyze?$q" \
        "$key" image/png "$TMP/probe.png")
  loc=$(hdr "$TMP/h" operation-location)
  ra=$(hdr "$TMP/h" retry-after)
  apim=$(hdr "$TMP/h" apim-request-id)
  row POST ":analyze" "$st" ""
  if [ "$st" = "202" ]; then
    DI_ASYNC="202"
    note "Operation-Location: $(pathonly "$loc")"
    note "Retry-After: ${ra:-(absent)}   apim-request-id: ${apim:-(absent)}"
    if [ -n "$loc" ]; then
      waited=0
      while [ "$waited" -lt "$POLL_SECONDS" ]; do
        st=$(req GET "$loc" "$key")
        s=$(jget "$TMP/b" status)
        case "$s" in succeeded|failed) break ;; esac
        sleep 2; waited=$((waited + 2))
      done
      row GET "analyzeResults/{id}" "$st" "status=${s:-?} after ${waited}s"
      rid=$(printf '%s' "$loc" | sed 's/?.*//; s:.*/::')
      st=$(req GET "$base/documentintelligence/documentModels/$MODEL_ID/analyzeResults/$rid/pdf?$q" "$key")
      DI_PDF="$st"
      row GET "analyzeResults/{id}/pdf" "$st" ""
      note "output=pdf was not requested, so a 4xx here is expected"
    fi
  else
    note "$(short "$TMP/b")"
  fi

  st=$(req GET "$base/documentintelligence/documentModels/$MODEL_ID/analyzeResults/$ZERO_GUID?$q" "$key")
  row GET "analyzeResults/{unknown}" "$st" ""
  note "$(short "$TMP/b")"

  st=$(req GET "$base/documentintelligence/info?$q" "$key")
  row GET "/info" "$st" "$(short "$TMP/b")"
  st=$(req GET "$base/documentintelligence/documentModels?$q" "$key")
  row GET "/documentModels" "$st" ""
  echo
}

probe_read() {
  base=$(printf '%s' "$READ_UPSTREAM_URL" | sed 's:/*$::')
  key=$READ_UPSTREAM_API_KEY

  hr; printf 'COMPUTER VISION READ    %s\n' "$base"; hr

  host=$(printf '%s' "$base" | sed 's://:_:; s:^[^_]*_::; s:/.*::; s/:.*//')
  case "$host" in
    *vision*|*VISION*)
      WARN_VISION="yes"
      note "host contains \"vision\": the container corrupts its own Operation-Location"
      ;;
  esac

  st=$(req GET "$base/status" "$key")
  row GET "/status" "$st" "apiStatus=$(jget "$TMP/b" apiStatus)"
  st=$(req GET "$base/ready" "$key")
  row GET "/ready" "$st" "ready=$(jget "$TMP/b" ready)"

  for p in /swagger/vision-v3.2-read/swagger.json /swagger/v1/swagger.json; do
    st=$(req GET "$base$p" "$key")
    if [ "$st" = "200" ]; then
      n=$(grep -o '"/[^"]*"' "$TMP/b" 2>/dev/null | sort -u | wc -l | tr -d ' ')
      row GET "$p" "$st" "$n paths declared"
      break
    fi
  done
  [ "$st" = "200" ] || row GET "/swagger*" "$st" "not found at the known paths"

  st=$(req POST "$base/vision/v3.2/read/syncAnalyze" "$key" image/png "$TMP/probe.png")
  row POST "/read/syncAnalyze" "$st" ""
  case "$st" in
    200) RD_SYNC="AVAILABLE"; note "$(short "$TMP/b")" ;;
    202) RD_SYNC="DEGRADES TO 202" ;;
    000) RD_SYNC="UNREACHABLE" ;;
    *)   RD_SYNC="unexpected $st"; note "$(short "$TMP/b")" ;;
  esac

  st=$(req POST "$base/vision/v3.2/read/analyze" "$key" image/png "$TMP/probe.png")
  loc=$(hdr "$TMP/h" operation-location)
  row POST "/read/analyze" "$st" ""
  if [ "$st" = "202" ]; then
    RD_ASYNC="202"
    note "Operation-Location: $(pathonly "$loc")"
    note "Retry-After: $(hdr "$TMP/h" retry-after)"
  fi

  st=$(req GET "$base/vision/v3.2/read/analyzeResults/$ZERO_GUID" "$key")
  row GET "analyzeResults/{unknown}" "$st" ""
  note "$(short "$TMP/b")"

  # The install doc shows /read/operations/{id}; every other source says analyzeResults.
  st=$(req GET "$base/vision/v3.2/read/operations/$ZERO_GUID" "$key")
  RD_ALIAS="$st"
  row GET "operations/{unknown}" "$st" ""
  note "stale-doc alias; 404 here means only analyzeResults is served"
  echo
}

# ---------------------------------------------------------------- run

printf '\nazure-gateway-api  container probe   %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null)"
printf 'api-version=%s  model=%s\n\n' "$API_VERSION" "$MODEL_ID"

[ -n "$DI_UPSTREAM_URL" ] && probe_di
[ -n "$READ_UPSTREAM_URL" ] && probe_read

hr; printf 'VERDICT\n'; hr
printf '  DI   :syncAnalyze     %s\n' "$DI_SYNC"
[ "$DI_SHAPE" = "-" ] || printf '  DI   200 body shape   %s\n' "$DI_SHAPE"
printf '  DI   :analyze         %s\n' "$DI_ASYNC"
[ "$DI_PDF" = "-" ] || printf '  DI   result-file /pdf  %s   (4xx expected without output=pdf)\n' "$DI_PDF"
printf '  READ syncAnalyze      %s\n' "$RD_SYNC"
printf '  READ /read/analyze    %s\n' "$RD_ASYNC"
if [ "$RD_ALIAS" = "404" ]; then
  printf '  READ operations alias 404   (not served, as expected)\n'
else
  printf '  READ operations alias %s\n' "$RD_ALIAS"
fi
printf '  READ host has "vision" %s\n' "$WARN_VISION"
echo
case "$DI_SYNC" in
  AVAILABLE)        printf '  -> keep DI_SYNC_ANALYZE=auto. The fast path works on this build.\n' ;;
  "DEGRADES TO 202") printf '  -> keep DI_SYNC_ANALYZE=auto. The gateway polls the 202 out; consider more container RAM.\n' ;;
  ABSENT*)          printf '  -> set DI_SYNC_ANALYZE=off to skip a wasted call per job. The gateway\n'
                    printf '     falls back to :analyze with affinity polling either way.\n' ;;
  UNREACHABLE)      printf '  -> the container did not answer. Check DI_UPSTREAM_URL and the key.\n' ;;
esac
if [ "$WARN_VISION" = "yes" ]; then
  printf '  -> rename the Read service. "vision" in the host makes the container\n'
  printf '     strip the port and the /vision segment from its own\n'
  printf '     Operation-Location. The gateway is immune, direct callers are not.\n'
fi
echo
