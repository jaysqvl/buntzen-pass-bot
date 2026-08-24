#!/usr/bin/env bash

set -Eeuo pipefail

image="${1:-buntzen-pass-bot:ci}"
run_suffix="${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$$"
container="buntzen-ci-smoke-${run_suffix}"
volume="buntzen-ci-appdata-${run_suffix}"
setup_token="ci-only-setup-token"
admin_username="ci-admin"
admin_password="ci-only-administrator-password"
workspace="$(mktemp -d)"
base_url=""

cleanup() {
  status=$?
  trap - EXIT
  if ((status != 0)); then
    echo "Container smoke test failed; service logs follow:" >&2
    docker logs "$container" >&2 2>/dev/null || true
  fi
  docker rm --force "$container" >/dev/null 2>&1 || true
  docker volume rm --force "$volume" >/dev/null 2>&1 || true
  rm -rf "$workspace"
  exit "$status"
}
trap cleanup EXIT

fail() {
  echo "container smoke test: $*" >&2
  return 1
}

header_value() {
  local file="$1"
  local name="$2"
  awk -F ':[[:space:]]*' -v name="$name" '
    tolower($1) == tolower(name) {
      value = substr($0, index($0, ":") + 1)
      sub(/^[[:space:]]+/, "", value)
      sub(/\r$/, "", value)
      print value
      exit
    }
  ' "$file"
}

extract_csrf() {
  docker exec --interactive "$container" python -c '
from html.parser import HTMLParser
import sys


class CSRFParser(HTMLParser):
    def __init__(self):
        super().__init__()
        self.values = []

    def handle_starttag(self, tag, attrs):
        values = dict(attrs)
        if tag == "input" and values.get("name") == "csrf_token":
            self.values.append(values.get("value") or "")


parser = CSRFParser()
parser.feed(sys.stdin.read())
if len(parser.values) != 1 or not parser.values[0]:
    raise SystemExit("expected exactly one non-empty CSRF field")
print(parser.values[0])
' <"$1"
}

wait_for_health() {
  local body="$workspace/health-body"
  local code=""
  for _ in $(seq 1 60); do
    code="$(curl --silent --show-error --max-time 3 --output "$body" --write-out '%{http_code}' "$base_url/healthz" 2>/dev/null || true)"
    if [[ "$code" == "200" ]] && grep -qx 'ok' "$body"; then
      return 0
    fi
    sleep 1
  done
  fail "health endpoint did not become ready"
}

start_container() {
  docker run --detach \
    --name "$container" \
    --init \
    --shm-size 1g \
    --security-opt "seccomp=$PWD/docker/seccomp_profile.json" \
    --publish 127.0.0.1::8080 \
    --volume "$volume:/appdata" \
    --env APPDATA_DIR=/appdata \
    --env BLUEBUBBLES_URL=http://bluebubbles.example:1234 \
    --env BUNTZEN_SETUP_TOKEN="$setup_token" \
    --env MAX_CONCURRENT_JOBS=2 \
    --env SCHEDULES_ENABLED=false \
    "$image" >/dev/null

  local published=""
  for _ in $(seq 1 20); do
    published="$(docker port "$container" 8080/tcp 2>/dev/null | head -n 1 || true)"
    if [[ -n "$published" ]]; then
      break
    fi
    sleep 1
  done
  [[ "$published" == 127.0.0.1:* ]] || fail "container did not publish its loopback HTTP port"
  base_url="http://$published"
  wait_for_health
}

validate_doctor() {
  local report
  report="$(docker exec "$container" /usr/local/bin/buntzen doctor)"
  printf '%s\n' "$report" | jq -e '
    .ok == true and
    .schema_version == 1 and
    .appdata_dir == "/appdata" and
    .database_path == "/appdata/buntzen.db" and
    .profiles_dir == "/appdata/profiles" and
    .artifacts_dir == "/appdata/artifacts" and
    .python_executable == "/usr/bin/python" and
    .python_module == "buntzen_actions" and
    .python_ready == true and
    .schedules_enabled == false and
    .otp_sources == []
  ' >/dev/null || fail "doctor returned an unexpected runtime report"
}

perform_setup() {
  local cookies="$workspace/setup-cookies"
  local page="$workspace/setup-page"
  local headers="$workspace/setup-headers"
  local response="$workspace/setup-response"
  local csrf=""
  local code=""

  curl --fail --silent --show-error --max-time 10 \
    --cookie-jar "$cookies" --dump-header "$workspace/setup-get-headers" \
    --output "$page" "$base_url/setup"
  csrf="$(extract_csrf "$page")"

  code="$(curl --silent --show-error --max-time 10 \
    --cookie "$cookies" --cookie-jar "$cookies" \
    --header "Origin: $base_url" \
    --data-urlencode "csrf_token=$csrf" \
    --data-urlencode "setup_token=$setup_token" \
    --data-urlencode "username=$admin_username" \
    --data-urlencode "password=$admin_password" \
    --data-urlencode "password_confirm=$admin_password" \
    --dump-header "$headers" --output "$response" \
    --write-out '%{http_code}' "$base_url/setup")"
  [[ "$code" == "303" ]] || fail "first-run setup returned HTTP $code"
  [[ "$(header_value "$headers" Location)" == "/?ok=setup" ]] || fail "first-run setup returned an unexpected redirect"
  tr -d '\r' < "$headers" | grep -Eiq '^set-cookie: buntzen_session=.*HttpOnly; SameSite=Strict$' || fail "setup did not issue the hardened session cookie"
  tr -d '\r' < "$headers" | grep -Eiq '^set-cookie: buntzen_csrf=.*HttpOnly; SameSite=Strict$' || fail "setup did not issue the hardened CSRF cookie"

  curl --fail --silent --show-error --max-time 10 \
    --cookie "$cookies" --output "$workspace/setup-dashboard" "$base_url/"
  grep -Fq "Account settings for $admin_username" "$workspace/setup-dashboard" || fail "setup session did not reach the authenticated dashboard"
}

perform_login() {
  local label="$1"
  local cookies="$workspace/${label}-cookies"
  local page="$workspace/${label}-login-page"
  local headers="$workspace/${label}-login-headers"
  local response="$workspace/${label}-login-response"
  local csrf=""
  local code=""

  curl --fail --silent --show-error --max-time 10 \
    --cookie-jar "$cookies" --output "$page" "$base_url/login"
  csrf="$(extract_csrf "$page")"
  code="$(curl --silent --show-error --max-time 10 \
    --cookie "$cookies" --cookie-jar "$cookies" \
    --header "Origin: $base_url" \
    --data-urlencode "csrf_token=$csrf" \
    --data-urlencode "username=$admin_username" \
    --data-urlencode "password=$admin_password" \
    --dump-header "$headers" --output "$response" \
    --write-out '%{http_code}' "$base_url/login")"
  [[ "$code" == "303" ]] || fail "login returned HTTP $code"
  [[ "$(header_value "$headers" Location)" == "/" ]] || fail "login returned an unexpected redirect"

  curl --fail --silent --show-error --max-time 10 \
    --cookie "$cookies" --dump-header "$workspace/${label}-dashboard-headers" \
    --output "$workspace/${label}-dashboard" "$base_url/"
  grep -Fq "Account settings for $admin_username" "$workspace/${label}-dashboard" || fail "authenticated dashboard did not identify the administrator"
  [[ "$(header_value "$workspace/${label}-dashboard-headers" Cache-Control)" == "no-store" ]] || fail "authenticated HTML was cacheable"
  [[ -n "$(header_value "$workspace/${label}-dashboard-headers" Content-Security-Policy)" ]] || fail "authenticated HTML omitted its Content Security Policy"
}

for command in docker curl jq; do
  command -v "$command" >/dev/null || fail "$command is required"
done
docker image inspect "$image" >/dev/null
[[ "$(docker image inspect --format '{{.Os}}' "$image")" == "linux" ]] || fail "smoke image is not a Linux image"
[[ "$(docker image inspect --format '{{.Architecture}}' "$image")" == "amd64" ]] || fail "smoke image is not linux/amd64"
[[ "$(docker image inspect --format '{{json .Config.Healthcheck.Test}}' "$image")" != "null" ]] || fail "image does not configure a Docker health check"
configured_user="$(docker image inspect --format '{{.Config.User}}' "$image")"
[[ -n "$configured_user" && "$configured_user" != "root" && "$configured_user" != "0" ]] || fail "image does not configure a non-root runtime user"
[[ "$(docker run --rm --entrypoint id "$image" -u)" != "0" ]] || fail "image runtime user resolves to root"

docker volume create "$volume" >/dev/null
start_container

runtime_uid="$(docker exec "$container" id -u)"
[[ "$runtime_uid" != "0" ]] || fail "running service executes as root"
docker exec "$container" python -c '
import importlib.util

unexpected = [
    module
    for module in ("msgpack", "setuptools")
    if importlib.util.find_spec(module) is not None
]
if unexpected:
    raise SystemExit("unexpected runtime Python modules: " + ", ".join(unexpected))
' || fail "build-only Python modules remained importable in the runtime image"
docker exec "$container" sh -eu -c '
  test -w /appdata
  test -f /appdata/buntzen.db
  test -f /appdata/master.key
  printf "%s\n" persisted > /appdata/.ci-persistence-marker
'
[[ "$(docker exec "$container" stat -c '%u' /appdata/master.key)" == "$runtime_uid" ]] || fail "the encryption key is not owned by the runtime user"

key_digest="$(docker exec "$container" sha256sum /appdata/master.key | awk '{print $1}')"
validate_doctor
perform_setup
perform_login before-restart

service_logs="$(docker logs "$container" 2>&1)"
[[ "$service_logs" != *"$setup_token"* ]] || fail "service logs exposed the setup token"
[[ "$service_logs" != *"$admin_password"* ]] || fail "service logs exposed the administrator password"
docker exec --env "CI_SECRET=$admin_password" "$container" sh -eu -c '
  for path in /appdata/buntzen.db*; do
    ! grep -aF -- "$CI_SECRET" "$path" >/dev/null
  done
' || fail "database contains the administrator password in plaintext"
! grep -R -aF -- "$setup_token" "$workspace" >/dev/null || fail "HTTP artifacts exposed the setup token"
! grep -R -aF -- "$admin_password" "$workspace" >/dev/null || fail "HTTP artifacts exposed the administrator password"

docker stop --time 45 "$container" >/dev/null
docker rm "$container" >/dev/null
start_container

[[ "$(docker exec "$container" sha256sum /appdata/master.key | awk '{print $1}')" == "$key_digest" ]] || fail "recreated container replaced the persistent encryption key"
docker exec "$container" sh -eu -c 'grep -qx persisted /appdata/.ci-persistence-marker' || fail "recreated container did not retain appdata"

curl --fail --silent --show-error --max-time 10 \
  --cookie "$workspace/setup-cookies" --output "$workspace/restarted-session-dashboard" "$base_url/"
grep -Fq "Account settings for $admin_username" "$workspace/restarted-session-dashboard" || fail "durable session did not survive container recreation"

setup_code="$(curl --silent --show-error --max-time 10 \
  --dump-header "$workspace/restart-setup-headers" --output /dev/null \
  --write-out '%{http_code}' "$base_url/setup")"
[[ "$setup_code" == "303" ]] || fail "completed setup was not retained after container recreation"
[[ "$(header_value "$workspace/restart-setup-headers" Location)" == "/login" ]] || fail "completed setup did not redirect to login after container recreation"

validate_doctor
perform_login after-restart

service_logs="$(docker logs "$container" 2>&1)"
[[ "$service_logs" != *"$setup_token"* ]] || fail "restarted service logs exposed the setup token"
[[ "$service_logs" != *"$admin_password"* ]] || fail "restarted service logs exposed the administrator password"

echo "Container smoke test passed: non-root runtime, writable persistent appdata, health, Python protocol, setup, login, and restart."
