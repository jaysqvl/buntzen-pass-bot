#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

die() {
  printf 'Buntzen deployment refused: %s\n' "$1" >&2
  exit 1
}

require_env() {
  local name="$1"
  [[ -n "${!name:-}" ]] || die "required environment value $name is missing"
}

for command in curl jq; do
  command -v "$command" >/dev/null 2>&1 || die "$command is required on the deployment runner"
done

for name in \
  BUNTZEN_HEALTH_URL \
  BUNTZEN_CONFIRM_STACK \
  BUNTZEN_IMAGE \
  PORTAINER_API_KEY \
  PORTAINER_ENDPOINT_ID \
  PORTAINER_STACK_ID \
  PORTAINER_STACK_NAME \
  PORTAINER_URL; do
  require_env "$name"
done

[[ "$PORTAINER_ENDPOINT_ID" =~ ^[1-9][0-9]*$ ]] || die "PORTAINER_ENDPOINT_ID must be a positive integer"
[[ "$PORTAINER_STACK_ID" =~ ^[1-9][0-9]*$ ]] || die "PORTAINER_STACK_ID must be a positive integer"
[[ "$PORTAINER_STACK_NAME" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "PORTAINER_STACK_NAME has an invalid format"
[[ "$BUNTZEN_CONFIRM_STACK" == "$PORTAINER_STACK_NAME" ]] || die "confirmation does not exactly match the protected stack name"
authority='([A-Za-z0-9.-]+|\[[0-9A-Fa-f:]+\])(:[0-9]{1,5})?'
[[ "$PORTAINER_URL" =~ ^https?://$authority$ ]] || die "PORTAINER_URL must be an exact HTTP(S) origin with no path, credentials, query, or fragment"
[[ "$BUNTZEN_HEALTH_URL" =~ ^https?://$authority/healthz$ ]] || die "BUNTZEN_HEALTH_URL must be an exact HTTP(S) /healthz URL"
[[ "$BUNTZEN_IMAGE" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._-]+@sha256:[0-9a-f]{64}$ ]] || die "BUNTZEN_IMAGE must be a lowercase GHCR image pinned by sha256 digest"

health_attempts="${BUNTZEN_HEALTH_ATTEMPTS:-30}"
health_interval="${BUNTZEN_HEALTH_INTERVAL_SECONDS:-4}"
[[ "$health_attempts" =~ ^[1-9][0-9]*$ ]] && ((health_attempts <= 60)) || die "BUNTZEN_HEALTH_ATTEMPTS must be between 1 and 60"
[[ "$health_interval" =~ ^(0|[1-9][0-9]*)$ ]] && ((health_interval <= 30)) || die "BUNTZEN_HEALTH_INTERVAL_SECONDS must be between 0 and 30"

compose_file="${1:-deploy/portainer.yml}"
[[ -f "$compose_file" ]] || die "deployment Compose file is missing"
schedule_lines="$(grep -Ec '^[[:space:]]+SCHEDULES_ENABLED:[[:space:]]*"false"[[:space:]]*$' "$compose_file" || true)"
schedule_mentions="$(grep -Ev '^[[:space:]]*#' "$compose_file" | grep -Ec 'SCHEDULES_ENABLED' || true)"
[[ "$schedule_lines" == "1" && "$schedule_mentions" == "1" ]] || die "deployment Compose file does not hard-disable schedules exactly once"
image_lines="$(grep -Ec '^[[:space:]]+image:[[:space:]]*"[$][{]BUNTZEN_IMAGE:[?][^}]+}"[[:space:]]*$' "$compose_file" || true)"
image_mentions="$(grep -Ev '^[[:space:]]*#' "$compose_file" | grep -Ec 'BUNTZEN_IMAGE' || true)"
[[ "$image_lines" == "1" && "$image_mentions" == "1" ]] || die "deployment Compose file does not require exactly one digest-pinned image variable"

deploy_tmp="$(mktemp -d)"
cleanup() {
  rm -rf -- "$deploy_tmp"
}
trap cleanup EXIT

portainer_origin="${PORTAINER_URL%/}"

api_request_error=''
api_request_raw() {
  local method="$1"
  local path="$2"
  local output="$3"
  local payload="${4:-}"
  local status
  local -a args=(
    --silent
    --show-error
    --noproxy '*'
    --proto '=http,https'
    --connect-timeout 10
    --max-time 180
    --max-filesize 2097152
    --request "$method"
    --header "X-API-Key: $PORTAINER_API_KEY"
    --output "$output"
    --write-out '%{http_code}'
  )
  if [[ -n "$payload" ]]; then
    args+=(--header 'Content-Type: application/json' --data-binary "@$payload")
  fi
  if ! status="$(curl "${args[@]}" "$portainer_origin$path")"; then
    api_request_error="Portainer API request failed"
    return 1
  fi
  if [[ ! "$status" =~ ^2[0-9][0-9]$ ]]; then
    api_request_error="Portainer API returned HTTP $status"
    return 1
  fi
}

api_request() {
  if ! api_request_raw "$@"; then
    die "$api_request_error"
  fi
}

health_request() {
  local output="$1"
  curl \
    --silent \
    --show-error \
    --noproxy '*' \
    --proto '=http,https' \
    --connect-timeout 3 \
    --max-time 5 \
    --max-filesize 64 \
    --output "$output" \
    --fail \
    "$BUNTZEN_HEALTH_URL" 2>/dev/null &&
    [[ "$(tr -d '\r\n' <"$output")" == "ok" ]]
}

read_stack_status() {
  jq -er 'if (.Status | type) == "number" then .Status else error("invalid stack status") end' "$1"
}

api_request GET "/api/stacks/$PORTAINER_STACK_ID" "$deploy_tmp/stack.json"
jq -e --argjson id "$PORTAINER_STACK_ID" --argjson endpoint "$PORTAINER_ENDPOINT_ID" --arg name "$PORTAINER_STACK_NAME" '
  .Id == $id and .EndpointId == $endpoint and .Name == $name and .Type == 2 and
  .GitConfig == null and .AutoUpdate == null and
  ((.Env // []) | type == "array") and
  all((.Env // [])[]; (.name | type == "string") and (.value | type == "string"))
' "$deploy_tmp/stack.json" >/dev/null || die "Portainer stack identity, source, or environment shape did not match"

initial_status="$(read_stack_status "$deploy_tmp/stack.json")"
[[ "$initial_status" == "1" ]] || die "the selected Buntzen stack is not active"
health_request "$deploy_tmp/pre-deploy-health.txt" || die "the selected Buntzen stack was not healthy before deployment"

schedule_gate="$(jq -r '[.Env[]? | select(.name == "SCHEDULES_ENABLED") | .value] | if length == 1 then .[0] else "" end' "$deploy_tmp/stack.json")"
[[ "$schedule_gate" == "false" ]] || die "the selected stack is not initialized with schedules disabled"

api_request GET "/api/stacks/$PORTAINER_STACK_ID/file" "$deploy_tmp/stack-file.json"
jq -e '.StackFileContent | type == "string" and length > 0' "$deploy_tmp/stack-file.json" >/dev/null || die "Portainer did not return a rollback stack file"
jq -rj '.StackFileContent' "$deploy_tmp/stack-file.json" >"$deploy_tmp/rollback.yml"

jq -n \
  --rawfile content "$compose_file" \
  --arg image "$BUNTZEN_IMAGE" \
  --slurpfile current "$deploy_tmp/stack.json" '
    {
      StackFileContent: $content,
      Env: (
        (($current[0].Env // []) | map(select(.name != "BUNTZEN_IMAGE" and .name != "SCHEDULES_ENABLED"))) +
        [
          {name: "BUNTZEN_IMAGE", value: $image},
          {name: "SCHEDULES_ENABLED", value: "false"}
        ]
      ),
      RepullImageAndRedeploy: true
    }
  ' >"$deploy_tmp/update.json"

jq -n \
  --rawfile content "$deploy_tmp/rollback.yml" \
  --slurpfile current "$deploy_tmp/stack.json" '
    {
      StackFileContent: $content,
      Env: ($current[0].Env // []),
      RepullImageAndRedeploy: true
    }
  ' >"$deploy_tmp/rollback.json"

rollback_and_fail() {
  local cause="$1"
  local rollback_ok=false
  local stack_status status_value health_body

  printf '%s; restoring the previous Portainer stack revision.\n' "$cause" >&2
  if ! api_request_raw PUT "/api/stacks/$PORTAINER_STACK_ID?endpointId=$PORTAINER_ENDPOINT_ID" "$deploy_tmp/rollback-response.json" "$deploy_tmp/rollback.json"; then
    die "$cause and the rollback request failed: $api_request_error"
  fi

  for attempt in $(seq 1 "$health_attempts"); do
    stack_status="$deploy_tmp/rollback-stack-status-$attempt.json"
    if ! api_request_raw GET "/api/stacks/$PORTAINER_STACK_ID" "$stack_status"; then
      die "$cause and rollback verification failed: $api_request_error"
    fi
    if ! status_value="$(read_stack_status "$stack_status")"; then
      die "$cause and rollback verification returned a malformed stack status"
    fi
    if [[ "$status_value" != "1" ]]; then
      break
    fi

    health_body="$deploy_tmp/rollback-health-$attempt.txt"
    if health_request "$health_body"; then
      rollback_ok=true
      break
    fi
    sleep "$health_interval"
  done

  if [[ "$rollback_ok" == "true" ]]; then
    die "$cause; rollback was verified healthy"
  fi
  die "$cause and the rollback did not become healthy"
}

printf 'Updating the protected Buntzen stack in place with an immutable image digest.\n'
if ! api_request_raw PUT "/api/stacks/$PORTAINER_STACK_ID?endpointId=$PORTAINER_ENDPOINT_ID" "$deploy_tmp/update-response.json" "$deploy_tmp/update.json"; then
  rollback_and_fail "stack update failed: $api_request_error"
fi

health_ok=false
for attempt in $(seq 1 "$health_attempts"); do
  stack_status="$deploy_tmp/stack-status-$attempt.json"
  if ! api_request_raw GET "/api/stacks/$PORTAINER_STACK_ID" "$stack_status"; then
    rollback_and_fail "stack status verification failed: $api_request_error"
  fi
  if ! status_value="$(read_stack_status "$stack_status")"; then
    rollback_and_fail "stack status verification returned a malformed response"
  fi
  if [[ "$status_value" != "1" ]]; then
    break
  fi

  health_body="$deploy_tmp/health-$attempt.txt"
  if health_request "$health_body"; then
    health_ok=true
    break
  fi
  sleep "$health_interval"
done

if [[ "$health_ok" != "true" ]]; then
  rollback_and_fail "health check failed after deployment"
fi

printf 'Buntzen deployment is healthy and schedules remain disabled.\n'
