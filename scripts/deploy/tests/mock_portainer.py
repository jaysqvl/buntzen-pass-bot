#!/usr/bin/env python3
"""Small stateful HTTP double for portainer.sh."""

from __future__ import annotations

import argparse
import json
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote, urlparse


OLD_IMAGE = "ghcr.io/example/buntzen-pass-bot@sha256:" + ("a" * 64)
NEW_IMAGE = "ghcr.io/example/buntzen-pass-bot@sha256:" + ("b" * 64)
CONTAINER_ID = "c" * 64
IMAGE_IDS = {OLD_IMAGE: "sha256:" + "d" * 64, NEW_IMAGE: "sha256:" + "e" * 64}
DOCKER_PROXY = "/api/endpoints/1/docker"
EXPECTED_FILTERS = {
    "label": [
        "com.docker.compose.project=buntzen-pass-bot",
        "com.docker.compose.service=buntzen-pass-bot",
    ]
}
ORIGINAL_ENV = [
    {"name": "SCHEDULES_ENABLED", "value": "false"},
    {"name": "BUNTZEN_IMAGE", "value": OLD_IMAGE},
    {"name": "BUNTZEN_WEB_PORT", "value": "18091"},
    {"name": "BUNTZEN_APPDATA_PATH", "value": "/srv/appdata/buntzen-pass-bot"},
    {"name": "BUNTZEN_LOG_LEVEL", "value": "debug"},
    {"name": "BUNTZEN_DEBUG", "value": "true"},
    {"name": "PRESERVED_VALUE", "value": "preserve-me"},
]
RUNTIME_FAILURES = (
    "old-image",
    "wrong-image-id",
    "containers-missing",
    "containers-duplicate",
    "container-id-invalid",
    "container-id-mismatch",
    "project-label-mismatch",
    "service-label-mismatch",
    "schedules-missing",
    "schedules-duplicate",
    "schedules-true",
    "schedules-bare",
    "container-stopped",
    "container-unhealthy",
    "runtime-list-api",
    "runtime-list-malformed",
    "runtime-inspect-api",
    "runtime-inspect-malformed",
    "runtime-image-api",
    "runtime-image-malformed",
    "runtime-image-id-invalid",
    "runtime-request-timeout",
)
PREFLIGHT_FAILURES = (
    "preflight-image-absent",
    "preflight-image-duplicate",
    "preflight-image-tagged",
    "preflight-runtime-mismatch",
    "preflight-image-id-mismatch",
    "preflight-schedules-true",
)
POSTDEPLOY_RACES = (
    "postdeploy-cleanup-race",
    "postdeploy-active-cleanup-race",
)
SCENARIOS = (
    *POSTDEPLOY_RACES,
    *RUNTIME_FAILURES,
    *PREFLIGHT_FAILURES,
    "same-digest",
    "runtime-retry-healthy",
    "success",
    "deployment-unhealthy",
    "deployment-inactive",
    "update-rejected",
    "status-query-failure",
    "status-query-malformed",
    "identity-mismatch",
    "git-backed",
    "preflight-unhealthy",
    "async-success",
    "async-failure",
    "async-timeout",
    "update-ambiguous",
    "unexpected-status",
    "status-query-persistent",
    "status-request-timeout",
)
ASYNC_SCENARIOS = {
    "async-success",
    "async-failure",
    "async-timeout",
    "update-ambiguous",
    "status-query-persistent",
    "status-request-timeout",
}


class State:
    def __init__(self, scenario: str, record_file: Path) -> None:
        self.scenario = scenario
        self.record_file = record_file
        self.puts: list[dict[str, Any]] = []
        self.api_requests: list[dict[str, Any]] = []
        self.health_requests = 0
        self.failed_status_once = False
        self.current_status = 1
        self.phase_reads = 0
        self.status_history: list[dict[str, int]] = []
        self.health_observations: list[dict[str, Any]] = []
        self.overlap_attempts = 0
        self.runtime_inspections: dict[int, int] = {}
        self.runtime_reads: list[dict[str, Any]] = []
        self.route_errors: list[str] = []
        self.postdeploy_pending = False
        self.postdeploy_overlap_attempts = 0
        self.backup_generation = 0
        self.callback_backup_generation: int | None = None
        self.write()

    def environment(self) -> list[dict[str, str]]:
        env = [pair.copy() for pair in ORIGINAL_ENV]
        if self.scenario == "preflight-image-absent":
            env = [pair for pair in env if pair["name"] != "BUNTZEN_IMAGE"]
        elif self.scenario == "preflight-image-duplicate":
            env.append({"name": "BUNTZEN_IMAGE", "value": OLD_IMAGE})
        elif self.scenario == "preflight-image-tagged":
            env[1]["value"] = "ghcr.io/example/buntzen-pass-bot:latest"
        return env

    def runtime(self) -> dict[str, Any]:
        """Model actual lifecycle state, independently of Docker request paths."""
        puts = len(self.puts)
        image = NEW_IMAGE if puts == 1 else OLD_IMAGE
        if self.scenario == "same-digest" or (
            self.scenario == "old-image" and puts == 1
        ):
            image = OLD_IMAGE
        if self.scenario == "preflight-runtime-mismatch" and puts == 0:
            image = NEW_IMAGE
        result: dict[str, Any] = {
            "Id": CONTAINER_ID,
            "Image": IMAGE_IDS[image],
            "Config": {
                "Image": image,
                "Labels": {
                    "com.docker.compose.project": "buntzen-pass-bot",
                    "com.docker.compose.service": "buntzen-pass-bot",
                },
                "Env": ["OTHER_SETTING=preserved", "SCHEDULES_ENABLED=false"],
            },
            "State": {
                "Status": "running",
                "Running": True,
                "Paused": False,
                "Restarting": False,
                "Dead": False,
                "Health": {"Status": "healthy"},
            },
        }
        if (
            (puts == 1 and self.scenario == "wrong-image-id")
            or (puts == 0 and self.scenario == "preflight-image-id-mismatch")
        ):
            result["Image"] = IMAGE_IDS[OLD_IMAGE if puts == 1 else NEW_IMAGE]
        if puts == 0 and self.scenario == "preflight-schedules-true":
            result["Config"]["Env"] = ["SCHEDULES_ENABLED=true"]
        if puts != 1:
            return result
        if self.scenario == "container-id-mismatch":
            result["Id"] = "f" * 64
        for field in ("project", "service"):
            if self.scenario == f"{field}-label-mismatch":
                result["Config"]["Labels"][f"com.docker.compose.{field}"] = "wrong"
        env_faults = {
            "schedules-missing": ["OTHER_SETTING=preserved"],
            "schedules-duplicate": ["SCHEDULES_ENABLED=false"] * 2,
            "schedules-true": ["SCHEDULES_ENABLED=true"],
            "schedules-bare": ["SCHEDULES_ENABLED=false", "SCHEDULES_ENABLED"],
        }
        if self.scenario in env_faults:
            result["Config"]["Env"] = env_faults[self.scenario]
        if self.scenario == "container-stopped":
            result["State"].update(Status="exited", Running=False)
        if self.scenario == "container-unhealthy" or (
            self.scenario == "runtime-retry-healthy"
            and self.runtime_inspections.get(1, 0) < 2
        ):
            result["State"]["Health"]["Status"] = "unhealthy"
        return result

    def next_status(self) -> int:
        puts = len(self.puts)
        if puts == 0:
            return 1
        self.phase_reads += 1
        status = 1
        if self.scenario == "postdeploy-cleanup-race" and puts == 1:
            # Portainer persists finalStatus before its postDeploy callback.
            # Leave cleanup pending until a second PUT exposes the backup race.
            status = 4
        elif self.scenario == "async-success" and puts == 1:
            status = 3 if self.phase_reads < 3 else 1
        elif self.scenario in {"async-failure", "update-ambiguous"}:
            status = 3 if self.phase_reads < 3 else 4
        elif self.scenario == "async-timeout":
            status = 3
        elif self.scenario == "unexpected-status":
            status = 99
        elif self.scenario == "deployment-inactive":
            status = 2
        self.current_status = status
        self.status_history.append({"puts": puts, "status": status})
        self.write()
        return status

    def write(self) -> None:
        value = {
            "scenario": self.scenario,
            "puts": self.puts,
            "api_requests": self.api_requests,
            "health_requests": self.health_requests,
            "failed_status_once": self.failed_status_once,
            "status_history": self.status_history,
            "health_observations": self.health_observations,
            "overlap_attempts": self.overlap_attempts,
            "runtime_inspections": self.runtime_inspections,
            "runtime_reads": self.runtime_reads,
            "route_errors": self.route_errors,
            "postdeploy_pending": self.postdeploy_pending,
            "postdeploy_overlap_attempts": self.postdeploy_overlap_attempts,
            "backup_generation": self.backup_generation,
            "callback_backup_generation": self.callback_backup_generation,
        }
        temporary = self.record_file.with_suffix(".tmp")
        temporary.write_text(json.dumps(value, sort_keys=True), encoding="utf-8")
        os.replace(temporary, self.record_file)


class Handler(BaseHTTPRequestHandler):
    server: "MockServer"

    def log_message(self, _format: str, *_args: object) -> None:
        return

    def _json(self, status: int, value: object) -> None:
        body = json.dumps(value).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass  # A deadline test intentionally stops reading a slow response.

    def _text(self, status: int, value: str) -> None:
        body = value.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass  # A deadline test intentionally stops reading a slow response.

    def _record_api(self, parsed: Any) -> bool:
        self.server.state.api_requests.append(
            {
                "method": self.command,
                "path": parsed.path,
                "query": parsed.query,
                "puts": len(self.server.state.puts),
                "time": time.monotonic(),
            }
        )
        self.server.state.write()
        if self.headers.get("X-API-Key") != "test-api-key":
            self._json(401, {"message": "invalid API key"})
            return False
        return True

    def _docker(self, parsed: Any) -> None:
        state = self.server.state
        puts = len(state.puts)
        stage = ""
        coordinate = None
        if parsed.path == f"{DOCKER_PROXY}/containers/json":
            stage = "list"
            try:
                query = parse_qs(parsed.query, strict_parsing=True)
                assert set(query) == {"all", "filters"}
                assert query["all"] == ["1"]
                assert len(query["filters"]) == 1
                assert json.loads(query["filters"][0]) == EXPECTED_FILTERS
            except (AssertionError, KeyError, ValueError):
                state.route_errors.append(self.path)
                state.write()
                self._json(400, {"message": "wrong container inventory filters"})
                return
        elif parsed.path == f"{DOCKER_PROXY}/containers/{CONTAINER_ID}/json":
            stage = "inspect"
            state.runtime_inspections[puts] = state.runtime_inspections.get(puts, 0) + 1
        elif parsed.path.startswith(f"{DOCKER_PROXY}/images/") and parsed.path.endswith(
            "/json"
        ):
            stage = "image"
            coordinate = unquote(parsed.path[len(f"{DOCKER_PROXY}/images/") : -5])
            if coordinate not in IMAGE_IDS:
                state.route_errors.append(self.path)
                state.write()
                self._json(404, {"message": "image does not exist"})
                return
        if not stage or (stage != "list" and parsed.query):
            state.route_errors.append(self.path)
            state.write()
            self._json(400, {"message": "unexpected Docker proxy request"})
            return
        runtime = state.runtime()
        state.runtime_reads.append(
            {"puts": puts, "stage": stage, "coordinate": coordinate, "runtime": runtime}
        )
        state.write()
        if puts == 1 and state.scenario == "runtime-request-timeout":
            # Each read fits the five-second phase budget, but all three do not.
            time.sleep(2.1)
        if puts == 1 and state.scenario == f"runtime-{stage}-api":
            self._json(500, {"message": "synthetic Docker proxy failure"})
            return
        if puts == 1 and state.scenario == f"runtime-{stage}-malformed":
            self._text(200, '{"malformed":')
            return
        if stage == "list":
            inventory: list[dict[str, str]] = [{"Id": CONTAINER_ID}]
            if puts == 1:
                if state.scenario == "containers-missing":
                    inventory = []
                elif state.scenario == "containers-duplicate":
                    inventory.append({"Id": "f" * 64})
                elif state.scenario == "container-id-invalid":
                    inventory = [{"Id": "../wrong-container"}]
            self._json(200, inventory)
        elif stage == "inspect":
            self._json(200, runtime)
        else:
            # Registry manifest coordinates map to distinct Docker config IDs.
            # Neither value is taken from the update payload or container request.
            image_id = IMAGE_IDS[coordinate]
            if puts == 1 and state.scenario == "runtime-image-id-invalid":
                image_id = "not-a-config-digest"
            self._json(200, {"Id": image_id})

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlparse(self.path)
        if parsed.path == "/healthz":
            state = self.server.state
            state.health_requests += 1
            puts = len(state.puts)
            state.health_observations.append(
                {
                    "puts": puts,
                    "status": state.current_status,
                    "runtime": state.runtime(),
                    "runtime_reads": len(state.runtime_reads),
                }
            )
            state.write()
            # While an async update runs, its old container can still answer ok.
            healthy = (
                (puts == 0 and state.scenario != "preflight-unhealthy")
                or state.scenario
                in {"success", "async-success", "async-timeout", "unexpected-status"}
                or state.scenario in {*RUNTIME_FAILURES, "same-digest", "runtime-retry-healthy"}
            )
            self._text(200 if healthy else 503, "ok\n" if healthy else "not ready\n")
            return

        if not self._record_api(parsed):
            return
        if parsed.path.startswith(f"{DOCKER_PROXY}/"):
            self._docker(parsed)
            return
        if (
            parsed.path == "/api/stacks/2/file"
            and self.server.state.scenario in POSTDEPLOY_RACES
        ):
            # Keep the race reproducible against the old rollback implementation.
            # The candidate's assertions forbid this now-unnecessary snapshot GET.
            self._json(
                200,
                {
                    "StackFileContent": (
                        "services:\n  buntzen-pass-bot:\n"
                        '    image: "${BUNTZEN_IMAGE:?old image required}"\n'
                        '    environment:\n      SCHEDULES_ENABLED: "false"\n'
                    )
                },
            )
            return
        if parsed.path != "/api/stacks/2":
            self._json(404, {"message": "not found"})
            return

        if self.server.state.puts and self.server.state.scenario in {
            "status-query-persistent",
            "status-request-timeout",
        }:
            if self.server.state.scenario == "status-request-timeout":
                time.sleep(3)
            self._json(500, {"message": "synthetic unavailable status"})
            return

        if (
            self.server.state.scenario == "status-query-failure"
            and len(self.server.state.puts) == 1
            and not self.server.state.failed_status_once
        ):
            self.server.state.failed_status_once = True
            self.server.state.write()
            self._json(500, {"message": "synthetic status failure"})
            return
        if (
            self.server.state.scenario == "status-query-malformed"
            and len(self.server.state.puts) == 1
            and not self.server.state.failed_status_once
        ):
            self.server.state.failed_status_once = True
            self.server.state.write()
            self._text(200, '{"Status":')
            return

        status = self.server.state.next_status()

        name = (
            "wrong-stack"
            if self.server.state.scenario == "identity-mismatch"
            else "buntzen-pass-bot"
        )
        git_config = (
            {"URL": "https://example.invalid/repository.git"}
            if self.server.state.scenario == "git-backed"
            else None
        )
        self._json(
            200,
            {
                "Id": 2,
                "EndpointId": 1,
                "Name": name,
                "Type": 2,
                "Status": status,
                "Env": self.server.state.environment(),
                "GitConfig": git_config,
                "AutoUpdate": None,
            },
        )

    def do_PUT(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlparse(self.path)
        if not self._record_api(parsed):
            return
        if parsed.path != "/api/stacks/2" or parse_qs(parsed.query) != {
            "endpointId": ["1"]
        }:
            self._json(400, {"message": "wrong stack update target"})
            return

        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length < 2 or length > 2 * 1024 * 1024:
                raise ValueError("invalid payload length")
            payload = json.loads(self.rfile.read(length))
            if not isinstance(payload, dict):
                raise ValueError("payload is not an object")
        except (ValueError, json.JSONDecodeError):
            self._json(400, {"message": "invalid JSON"})
            return

        if self.server.state.current_status == 3:
            self.server.state.overlap_attempts += 1
            self.server.state.write()
            self._json(409, {"message": "stack is already deploying"})
            return
        self.server.state.phase_reads = 0
        if self.server.state.scenario in POSTDEPLOY_RACES:
            state = self.server.state
            state.backup_generation += 1
            if state.postdeploy_pending:
                state.postdeploy_overlap_attempts += 1
                # The old callback consumes the new operation's overwritten .bak.
                state.callback_backup_generation = state.backup_generation
                state.postdeploy_pending = False
            else:
                state.postdeploy_pending = True
        self.server.state.current_status = (
            3 if self.server.state.scenario in ASYNC_SCENARIOS else 1
        )
        self.server.state.puts.append(payload)
        self.server.state.write()
        if (
            self.server.state.scenario in {"update-rejected", "update-ambiguous"}
            and len(self.server.state.puts) == 1
        ):
            self._json(500, {"message": "synthetic deployment failure"})
            return
        self._json(200, {"Status": self.server.state.current_status})


class MockServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], state: State) -> None:
        super().__init__(address, Handler)
        self.state = state


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--scenario",
        choices=SCENARIOS,
        required=True,
    )
    parser.add_argument("--port-file", type=Path, required=True)
    parser.add_argument("--record-file", type=Path, required=True)
    args = parser.parse_args()

    state = State(args.scenario, args.record_file)
    server = MockServer(("127.0.0.1", 0), state)
    args.port_file.write_text(str(server.server_port), encoding="ascii")
    server.serve_forever(poll_interval=0.05)


if __name__ == "__main__":
    main()
