#!/usr/bin/env bash
# Drive a running gateway with the real Azure SDKs.
#
# This is the definitive check: unmodified, officially published client libraries against the
# gateway. Everything else in the test suite asserts what the gateway should emit; this asserts
# that the clients actually accept it.
set -euo pipefail

cd "$(dirname "$0")"

: "${GATEWAY_URL:=http://localhost:8080}"
export GATEWAY_URL

if ! curl -fsS "${GATEWAY_URL}/_gw/live" >/dev/null 2>&1; then
  echo "No gateway at ${GATEWAY_URL}. Start one first, e.g.:" >&2
  echo "  DI_UPSTREAM_URL=... READ_UPSTREAM_URL=... DATA_DIR=./data make run" >&2
  exit 1
fi

# The install is stamped with a copy of the requirements it installed. Checking only for the
# directory let a venv whose pip install was interrupted, or that predates a new requirement, be
# reused forever: the missing SDK's checks then SKIP and the run still reads as a pass.
if [ ! -x .venv/bin/python ] || ! cmp -s requirements.txt .venv/requirements.installed; then
  echo "Creating a virtualenv for the Azure SDKs..."
  python3 -m venv .venv
  ./.venv/bin/pip install --quiet --upgrade pip
  ./.venv/bin/pip install --quiet -r requirements.txt
  cp requirements.txt .venv/requirements.installed
fi

# SDK_STRICT=1 makes a skipped check a failure; scripts/e2e-local.sh sets it.
exec ./.venv/bin/python sdk_test.py "$@"
