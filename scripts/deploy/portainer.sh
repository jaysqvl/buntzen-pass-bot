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
  local timeout="${5:-180}"
  local -a args=(
    --silent
    --show-error
    --noproxy '*'
    --proto '=http,https'
    --connect-timeout 10
    --max-time "$timeout"
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
  local timeout="${2:-5}"
  curl \
    --silent \
    --show-error \
    --noproxy '*' \
    --proto '=http,https' \
    --connect-timeout 3 \
    --max-time "$timeout" \
    --max-filesize 64 \
    --output "$output" \
    --fail \
    "$BUNTZEN_HEALTH_URL" 2>/dev/null &&
    [[ "$(tr -d '\r\n' <"$output")" == "ok" ]]
}

read_stack_status() {
  jq -er 'if (.Status | type) == "number" then .Status else error("invalid stack status") end' "$1"
}

# The Docker image ID is a configuration digest, not a registry manifest digest.
# Resolve the requested immutable coordinate through Docker and compare its ID
# with the running container, in addition to checking Config.Image and labels.
runtime_failure=''
runtime_get() {
  local path="$1" output="$2" deadline="$3"
  local remaining=$((deadline - SECONDS))
  runtime_failure='runtime verification deadline expired'
  ((remaining > 0)) || return 1
  if ! api_request_raw GET "$path" "$output" "" "$remaining"; then
    runtime_failure="runtime verification failed: $api_request_error"
    return 1
  fi
}

verify_runtime() {
  local expected_image="$1" phase="$2" deadline="$3"
  local proxy="/api/endpoints/$PORTAINER_ENDPOINT_ID/docker"
  local inventory="$deploy_tmp/$phase-containers.json"
  local container="$deploy_tmp/$phase-container.json"
  local resolved_image="$deploy_tmp/$phase-image.json"
  local filters container_id encoded_image
  filters="$(jq -nr --arg project "$PORTAINER_STACK_NAME" \
    '{label:[("com.docker.compose.project=" + $project),"com.docker.compose.service=buntzen-pass-bot"]} | tojson | @uri')"
  runtime_get "$proxy/containers/json?all=1&filters=$filters" "$inventory" "$deadline" || return 1
  runtime_failure='runtime verification did not identify exactly one service container'
  container_id="$(jq -er 'if type == "array" and length == 1 then .[0].Id else null end | select(type == "string") | select(test("^[0-9a-f]{64}$"))' "$inventory")" || return 1
  runtime_get "$proxy/containers/$container_id/json" "$container" "$deadline" || return 1
  runtime_failure='runtime container identity, image, health, or schedules did not match'
  jq -e --arg id "$container_id" --arg project "$PORTAINER_STACK_NAME" --arg image "$expected_image" '
    .Id == $id and
    .Config.Labels["com.docker.compose.project"] == $project and
    .Config.Labels["com.docker.compose.service"] == "buntzen-pass-bot" and
    .Config.Image == $image and
    (.Image | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    .State.Status == "running" and .State.Running == true and
    .State.Paused == false and .State.Restarting == false and .State.Dead == false and
    .State.Health.Status == "healthy" and
    (.Config.Env | type == "array" and all(.[]; type == "string")) and
    ([.Config.Env[] | select(. == "SCHEDULES_ENABLED" or startswith("SCHEDULES_ENABLED="))] == ["SCHEDULES_ENABLED=false"])
  ' "$container" >/dev/null || return 1
  encoded_image="$(jq -nr --arg image "$expected_image" '$image | @uri')"
  runtime_get "$proxy/images/$encoded_image/json" "$resolved_image" "$deadline" || return 1
  runtime_failure='runtime image ID did not match the requested immutable image'
  jq -e --slurpfile container "$container" '
    (.Id | type == "string" and test("^sha256:[0-9a-f]{64}$")) and .Id == $container[0].Image
  ' "$resolved_image" >/dev/null || return 1
  ((SECONDS < deadline)) || { runtime_failure='runtime verification deadline expired'; return 1; }
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
previous_image="$(jq -r '[.Env[]? | select(.name == "BUNTZEN_IMAGE") | .value] | if length == 1 then .[0] else "" end' "$deploy_tmp/stack.json")"
[[ "$previous_image" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._-]+@sha256:[0-9a-f]{64}$ ]] || die "the selected stack must have exactly one immutable BUNTZEN_IMAGE for runtime verification"
verify_runtime "$previous_image" preflight "$((SECONDS + health_attempts * (health_interval > 0 ? health_interval : 1)))" || die "pre-deployment $runtime_failure"

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

# Portainer 2.45 uses Active=1, Inactive=2, Deploying=3, Error=4.
# PUT returns before its asynchronous deployment ends. Poll within one shared
# time budget, including HTTP requests, and never treat an old healthy container
# as proof that a stack still marked Deploying has completed.
wait_failure=''
wait_for_stack() {
  local phase="$1"
  local expected_image="$2"
  local deadline=$((SECONDS + health_attempts * (health_interval > 0 ? health_interval : 1)))
  local remaining status_value='' health_timeout pause
  local stack_status="$deploy_tmp/$phase-stack.json"
  local health_body="$deploy_tmp/$phase-health.txt"

  wait_failure='stack verification deadline expired'
  for attempt in $(seq 1 "$health_attempts"); do
    remaining=$((deadline - SECONDS))
    ((remaining > 0)) || break
    if ! api_request_raw GET "/api/stacks/$PORTAINER_STACK_ID" "$stack_status" "" "$remaining"; then
      wait_failure="stack status verification failed: $api_request_error"
      return 1
    fi
    if ! status_value="$(read_stack_status "$stack_status")"; then
      wait_failure='stack status verification returned a malformed response'
      return 1
    fi
    case "$status_value" in
      3)
        wait_failure='the stack is still deploying after the verification deadline'
        ;;
      1|2|4)
        if [[ "$status_value" != '1' ]]; then
          wait_failure="stack deployment ended with status $status_value"
          return 1
        fi
        if ! verify_runtime "$expected_image" "$phase" "$deadline"; then
          wait_failure="$runtime_failure"
        else
          remaining=$((deadline - SECONDS))
          ((remaining > 0)) || break
          health_timeout="$remaining"
          ((health_timeout <= 5)) || health_timeout=5
          if health_request "$health_body" "$health_timeout" && ((SECONDS < deadline)); then
            return 0
          fi
          wait_failure='health check failed after deployment'
        fi
        ;;
      *)
        wait_failure='stack status verification returned an unsupported status'
        return 1
        ;;
    esac
    remaining=$((deadline - SECONDS))
    ((attempt < health_attempts && remaining > 0)) || break
    pause="$health_interval"
    ((pause <= remaining)) || pause="$remaining"
    sleep "$pause"
  done
  return 1
}

fail_after_update() {
  local cause="$1"

  # Portainer 2.45 publishes final status before postDeploy restores/removes its
  # shared stack-file backup. Even a settled status cannot authorize another PUT:
  # compensation can overwrite a backup still consumed by that earlier callback.
  die "$cause; automatic rollback was not attempted because Portainer may still be completing stack-file cleanup; inspect Portainer and reconcile the previous Compose, environment and image before retrying"
}

printf 'Updating the protected Buntzen stack in place with an immutable image digest.\n'
if ! api_request_raw PUT "/api/stacks/$PORTAINER_STACK_ID?endpointId=$PORTAINER_ENDPOINT_ID" "$deploy_tmp/update-response.json" "$deploy_tmp/update.json"; then
  fail_after_update "stack update failed: $api_request_error"
fi

if ! wait_for_stack deployment "$BUNTZEN_IMAGE"; then
  fail_after_update "$wait_failure"
fi

printf 'Buntzen deployment is healthy and schedules remain disabled.\n'
