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
from urllib.parse import parse_qs, urlparse


OLD_IMAGE = "ghcr.io/example/buntzen-pass-bot@sha256:" + ("a" * 64)
ORIGINAL_ENV = [
    {"name": "SCHEDULES_ENABLED", "value": "false"},
    {"name": "BUNTZEN_IMAGE", "value": OLD_IMAGE},
    {"name": "BUNTZEN_WEB_PORT", "value": "18091"},
    {"name": "BUNTZEN_APPDATA_PATH", "value": "/srv/appdata/buntzen-pass-bot"},
    {"name": "BUNTZEN_LOG_LEVEL", "value": "debug"},
    {"name": "BUNTZEN_DEBUG", "value": "true"},
    {"name": "PRESERVED_VALUE", "value": "preserve-me"},
]
ROLLBACK_COMPOSE = (
    "services:\n"
    "  buntzen-pass-bot:\n"
    '    image: "${BUNTZEN_IMAGE:?old image required}"\n'
    "    environment:\n"
    '      SCHEDULES_ENABLED: "false"\n'
)


SCENARIOS = (
    "success",
    "rollback",
    "rollback-failure",
    "update-rejected",
    "status-query-failure",
    "status-query-malformed",
    "identity-mismatch",
    "git-backed",
    "preflight-unhealthy",
    "async-success",
    "async-failure",
    "async-timeout",
    "async-rollback-timeout",
    "update-ambiguous",
    "unexpected-status",
    "status-query-persistent",
    "status-request-timeout",
)
ASYNC_SCENARIOS = {
    "async-success",
    "async-failure",
    "async-timeout",
    "async-rollback-timeout",
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
        self.health_observations: list[dict[str, int]] = []
        self.overlap_attempts = 0
        self.write()

    def next_status(self) -> int:
        puts = len(self.puts)
        if puts == 0:
            return 1
        self.phase_reads += 1
        status = 1
        if self.scenario == "async-success" and puts == 1:
            status = 3 if self.phase_reads < 3 else 1
        elif self.scenario in {
            "async-failure",
            "async-rollback-timeout",
            "update-ambiguous",
        }:
            if puts == 1:
                status = 3 if self.phase_reads < 3 else 4
            elif self.scenario == "async-rollback-timeout":
                status = 3
            else:
                status = 3 if self.phase_reads < 3 else 1
        elif self.scenario == "async-timeout":
            status = 3
        elif self.scenario == "unexpected-status":
            status = 99
        elif puts >= 2 and self.scenario == "rollback-failure":
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
            {"method": self.command, "path": parsed.path, "query": parsed.query}
        )
        self.server.state.write()
        if self.headers.get("X-API-Key") != "test-api-key":
            self._json(401, {"message": "invalid API key"})
            return False
        return True

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlparse(self.path)
        if parsed.path == "/healthz":
            state = self.server.state
            state.health_requests += 1
            puts = len(state.puts)
            state.health_observations.append(
                {"puts": puts, "status": state.current_status}
            )
            state.write()
            # While an async update runs, its old container can still answer ok.
            healthy = (
                (puts == 0 and state.scenario != "preflight-unhealthy")
                or state.scenario
                in {"success", "async-success", "async-timeout", "unexpected-status"}
                or (
                    puts >= 2
                    and state.scenario
                    in {
                        "rollback",
                        "update-rejected",
                        "status-query-failure",
                        "status-query-malformed",
                        "async-failure",
                        "update-ambiguous",
                    }
                )
            )
            self._text(200 if healthy else 503, "ok\n" if healthy else "not ready\n")
            return

        if not self._record_api(parsed):
            return
        if parsed.path == "/api/stacks/2/file":
            self._json(200, {"StackFileContent": ROLLBACK_COMPOSE})
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
                "Env": ORIGINAL_ENV,
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
