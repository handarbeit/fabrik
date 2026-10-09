#!/usr/bin/env bash
# scripts/ci/worker-shaped-test.sh — run the full Go suite in a worker-shaped
# environment (#2027, ADR-2027).
#
# A Fabrik stage worker runs the repo's own test suite with the daemon's
# environment inherited: the App credentials, FABRIK_TOKEN, FABRIK_REVIEWER_TOKEN,
# the webhook secret, GH_TOKEN, and the ADR-1846 GIT_CONFIG_COUNT/KEY_n/VALUE_n
# credential-helper entries. CI has none of these, so a test that reads any of
# them passed in CI and failed — or hung — inside a worker. This script exports
# dummy values for all of them and runs the same `go test -race ./...` shape the
# main CI step uses; the suite must produce the same result as it does without
# them. Both CI and a local run use this script so the two cannot drift.
#
#   bash scripts/ci/worker-shaped-test.sh            # whole module
#   bash scripts/ci/worker-shaped-test.sh ./cmd/...  # extra args replace ./...
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$root"
# shellcheck source=../lib/parallel.sh
source scripts/lib/parallel.sh

scratch="${RUNNER_TEMP:-}"
cleanup=""
if [ -z "$scratch" ]; then
  scratch="$(mktemp -d)"
  cleanup="$scratch"
fi
trap '[ -n "$cleanup" ] && rm -rf "$cleanup"' EXIT

token_file="$scratch/github-app-git-token"
printf 'dummy-installation-token' >"$token_file"
helper="!f() { test \"\$1\" = get && echo username=x-access-token && echo password=\$(cat '$token_file'); }; f"

export FABRIK_GITHUB_APP_ID=123456
export FABRIK_GITHUB_APP_INSTALLATION_ID=654321
export FABRIK_GITHUB_APP_PRIVATE_KEY_PATH="$scratch/does-not-exist.pem"
export FABRIK_GITHUB_WEBHOOK_SECRET=dummy-webhook-secret
export FABRIK_TOKEN=dummy-pat
export FABRIK_REVIEWER_TOKEN=dummy-reviewer-token
export GH_TOKEN=dummy-gh-token
export GITHUB_TOKEN=dummy-gh-token
export GIT_CONFIG_COUNT=2
export GIT_CONFIG_KEY_0=credential.https://github.com.helper
export GIT_CONFIG_VALUE_0=
export GIT_CONFIG_KEY_1=credential.https://github.com.helper
export GIT_CONFIG_VALUE_1="$helper"

pkgs=("$@")
[ ${#pkgs[@]} -gt 0 ] || pkgs=(./...)

set -x
go test -race -timeout "${WORKER_SHAPED_TIMEOUT:-10m}" -parallel "$(default_race_parallel)" "${pkgs[@]}"
