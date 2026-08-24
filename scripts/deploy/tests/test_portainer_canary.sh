#!/usr/bin/env bash
set -Eeuo pipefail

test_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "$test_dir/../../.." && pwd)"
test_tmp="$(mktemp -d)"
mock_pid=''

cleanup() {
  if [[ -n "$mock_pid" ]]; then
    kill "$mock_pid" 2>/dev/null || true
    wait "$mock_pid" 2>/dev/null || true
  fi
  rm -rf -- "$test_tmp"
}
trap cleanup EXIT

new_image="ghcr.io/example/buntzen-pass-bot@sha256:$(printf 'b%.0s' {1..64})"
compose_file="$repo_root/deploy/portainer-canary.yml"

start_mock() {
  local scenario="$1"
  local case_dir="$2"
  mkdir -p "$case_dir"
  python3 "$test_dir/mock_portainer.py" \
    --scenario "$scenario" \
    --port-file "$case_dir/port" \
    --record-file "$case_dir/state.json" &
  mock_pid=$!

  for _attempt in {1..100}; do
    [[ -s "$case_dir/port" ]] && return
    kill -0 "$mock_pid" 2>/dev/null || {
      wait "$mock_pid"
      return 1
    }
    sleep 0.05
  done
  printf 'mock server did not start\n' >&2
  return 1
}

stop_mock() {
  kill "$mock_pid" 2>/dev/null || true
  wait "$mock_pid" 2>/dev/null || true
  mock_pid=''
}

run_compose_refusal() {
  local candidate="$1"
  local expected_log="$2"
  local output="$candidate.log"
  local status=0

  env \
    BUNTZEN_CANARY_HEALTH_URL=http://127.0.0.1:9/healthz \
    BUNTZEN_IMAGE="$new_image" \
    CANARY_CONFIRM_STACK=buntzen-canary \
    PORTAINER_API_KEY=test-api-key \
    PORTAINER_ENDPOINT_ID=1 \
    PORTAINER_STACK_ID=2 \
    PORTAINER_STACK_NAME=buntzen-canary \
    PORTAINER_URL=http://127.0.0.1:9 \
    "$repo_root/scripts/deploy/portainer_canary.sh" "$candidate" \
    >"$output" 2>&1 || status=$?

  [[ "$status" != 0 ]] || {
    printf 'tampered Compose file unexpectedly passed preflight\n' >&2
    return 1
  }
  grep -Fq "$expected_log" "$output" || {
    sed -n '1,160p' "$output" >&2
    printf 'tampered Compose file did not emit the expected refusal\n' >&2
    return 1
  }
}

run_case() {
  local scenario="$1"
  local expected_result="$2"
  local expected_log="$3"
  local case_dir="$test_tmp/$scenario"
  local port
  local status=0

  start_mock "$scenario" "$case_dir"
  port="$(<"$case_dir/port")"

  env \
    BUNTZEN_CANARY_HEALTH_URL="http://127.0.0.1:$port/healthz" \
    BUNTZEN_IMAGE="$new_image" \
    CANARY_CONFIRM_STACK=buntzen-canary \
    CANARY_HEALTH_ATTEMPTS=3 \
    CANARY_HEALTH_INTERVAL_SECONDS=0 \
    PORTAINER_API_KEY=test-api-key \
    PORTAINER_ENDPOINT_ID=1 \
    PORTAINER_STACK_ID=2 \
    PORTAINER_STACK_NAME=buntzen-canary \
    PORTAINER_URL="http://127.0.0.1:$port" \
    "$repo_root/scripts/deploy/portainer_canary.sh" "$compose_file" \
    >"$case_dir/output.log" 2>&1 || status=$?

  if [[ "$expected_result" == "success" ]]; then
    [[ "$status" == 0 ]] || {
      sed -n '1,160p' "$case_dir/output.log" >&2
      printf '%s unexpectedly failed with status %s\n' "$scenario" "$status" >&2
      return 1
    }
  else
    [[ "$status" != 0 ]] || {
      printf '%s unexpectedly succeeded\n' "$scenario" >&2
      return 1
    }
  fi

  grep -Fq "$expected_log" "$case_dir/output.log" || {
    sed -n '1,160p' "$case_dir/output.log" >&2
    printf '%s did not emit the expected result\n' "$scenario" >&2
    return 1
  }
  python3 "$test_dir/assert_portainer_state.py" \
    "$scenario" "$case_dir/state.json" "$compose_file" "$new_image"
  stop_mock
}

run_case success success 'Canary deployment is healthy and schedules remain disabled.'
run_case rollback failure 'rollback was verified healthy'
run_case rollback-failure failure 'rollback did not become healthy'
run_case update-rejected failure 'stack update failed: Portainer API returned HTTP 500; rollback was verified healthy'
run_case status-query-failure failure 'stack status verification failed: Portainer API returned HTTP 500; rollback was verified healthy'
run_case status-query-malformed failure 'stack status verification returned a malformed response; rollback was verified healthy'
run_case identity-mismatch failure 'Portainer stack identity, source, or environment shape did not match'
run_case git-backed failure 'Portainer stack identity, source, or environment shape did not match'
run_case preflight-unhealthy failure 'the selected canary was not healthy before deployment'

schedule_spoof="$test_tmp/schedule-spoof.yml"
sed 's/^      SCHEDULES_ENABLED: "false"$/      SCHEDULES_ENABLED: "true"/' "$compose_file" >"$schedule_spoof"
printf '\n# SCHEDULES_ENABLED: "false"\n' >>"$schedule_spoof"
run_compose_refusal "$schedule_spoof" 'canary Compose file does not hard-disable schedules exactly once'

image_spoof="$test_tmp/image-spoof.yml"
sed 's|^    image:.*$|    image: "ghcr.io/example/buntzen-pass-bot:latest"|' "$compose_file" >"$image_spoof"
printf '\n# image: "${BUNTZEN_IMAGE:?comment spoof}"\n' >>"$image_spoof"
run_compose_refusal "$image_spoof" 'canary Compose file does not require exactly one digest-pinned image variable'

printf 'Portainer canary deployment tests passed.\n'
