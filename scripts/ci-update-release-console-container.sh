#!/usr/bin/env bash
set -euo pipefail
# Keep verified TLS usable through gateways that reset Go's hybrid-MLKEM
# handshake. This affects the CI Docker clients, not the console containers.
export GODEBUG="${GODEBUG:+$GODEBUG,}tlsmlkem=0"
: "${RUNNER_TEMP:?RUNNER_TEMP is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${GITHUB_RUN_ATTEMPT:?GITHUB_RUN_ATTEMPT is required}"
export COMPOSE_PROJECT_NAME="pinvou-ci-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
export PINVOU_UPDATE_CI_UID PINVOU_UPDATE_CI_GID
PINVOU_UPDATE_CI_UID=$(id -u)
PINVOU_UPDATE_CI_GID=$(id -g)
state="$RUNNER_TEMP/update-console-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}.path"
compose() {
  docker compose --file update-release-console/deploy/compose.yaml \
    --file update-release-console/deploy/compose.ci.yaml "$@"
}
cleanup() {
  if [[ -f "$state" ]]; then
    export PINVOU_UPDATE_CI_PUBLIC_ROOT
    PINVOU_UPDATE_CI_PUBLIC_ROOT=$(cat "$state")
    # This directory is created by mktemp below, within this job's runner temp.
    [[ "$PINVOU_UPDATE_CI_PUBLIC_ROOT" == "$RUNNER_TEMP"/release-console.* ]]
    compose down --volumes --remove-orphans
    python3 - "$PINVOU_UPDATE_CI_PUBLIC_ROOT" "$RUNNER_TEMP" <<'PY'
from pathlib import Path
import shutil, sys
target, parent = (Path(value).resolve() for value in sys.argv[1:])
if target.parent != parent or not target.name.startswith('release-console.'):
    raise SystemExit('Refusing cleanup outside the job temporary directory')
shutil.rmtree(target, ignore_errors=False)
PY
    rm -- "$state"
  fi
}
if [[ "${1:-}" == --cleanup ]]; then
  cleanup
  exit 0
fi
export PINVOU_UPDATE_CI_PUBLIC_ROOT
PINVOU_UPDATE_CI_PUBLIC_ROOT=$(mktemp -d "$RUNNER_TEMP/release-console.XXXXXX")
printf '%s\n' "$PINVOU_UPDATE_CI_PUBLIC_ROOT" > "$state"
trap cleanup EXIT
mkdir -p "$PINVOU_UPDATE_CI_PUBLIC_ROOT/public" "$PINVOU_UPDATE_CI_PUBLIC_ROOT/state"
printf '%s\n' '{"platforms":{"linux-x86_64":{"version":"0.10.4"},"macos-universal":{"version":"0.10.4"}}}' \
  > "$PINVOU_UPDATE_CI_PUBLIC_ROOT/public/latest.json"
chmod 0755 "$PINVOU_UPDATE_CI_PUBLIC_ROOT"
docker build --build-arg NODE_IMAGE="$PINVOU_UPDATE_NODE_IMAGE" --file update-release-console/Dockerfile update-release-console
compose config --quiet
docker run --rm \
  -v "$PWD/update-release-console/deploy/nginx-test.conf:/etc/nginx/nginx.conf:ro" \
  -v "$PWD/update-release-console/deploy/nginx-location.conf:/etc/nginx/pinvou3-release-location.conf:ro" \
  mirror.gcr.io/library/nginx@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10 nginx -t
compose up --detach --build
address=$(compose port console 8792)
port=${address##*:}
[[ "$port" =~ ^[0-9]+$ ]]
base="http://127.0.0.1:$port/pinvou3/release-console"
for attempt in {1..30}; do
  if curl --fail --silent --show-error "$base/healthz"; then break; fi
  if [[ "$attempt" == 30 ]]; then compose logs; exit 1; fi
  sleep 2
done
curl --fail --silent --show-error \
  --header "Authorization: Bearer $PINVOU_UPDATE_ADMIN_TOKEN" "$base/api/status"
