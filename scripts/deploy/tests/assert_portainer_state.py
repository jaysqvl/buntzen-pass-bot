#!/usr/bin/env python3
"""Assertions for the Portainer deployment shell integration test."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from mock_portainer import ORIGINAL_ENV, ROLLBACK_COMPOSE, SCENARIOS


def check_update(payload: dict[str, Any], compose: str, image: str) -> None:
    assert set(payload) == {"StackFileContent", "Env", "RepullImageAndRedeploy"}, (
        payload
    )
    assert payload["StackFileContent"] == compose
    assert payload["RepullImageAndRedeploy"] is True
    env = payload["Env"]
    assert isinstance(env, list)
    assert sum(pair == {"name": "BUNTZEN_IMAGE", "value": image} for pair in env) == 1
    assert (
        sum(pair == {"name": "SCHEDULES_ENABLED", "value": "false"} for pair in env)
        == 1
    )
    assert {"name": "BUNTZEN_WEB_PORT", "value": "18091"} in env
    assert {
        "name": "BUNTZEN_APPDATA_PATH",
        "value": "/srv/appdata/buntzen-pass-bot",
    } in env
    assert {"name": "BUNTZEN_LOG_LEVEL", "value": "debug"} in env
    assert {"name": "BUNTZEN_DEBUG", "value": "true"} in env
    assert {"name": "PRESERVED_VALUE", "value": "preserve-me"} in env


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "scenario",
        choices=SCENARIOS,
    )
    parser.add_argument("record", type=Path)
    parser.add_argument("compose", type=Path)
    parser.add_argument("image")
    args = parser.parse_args()

    state = json.loads(args.record.read_text(encoding="utf-8"))
    puts = state["puts"]
    assert state["overlap_attempts"] == 0, state
    assert all(item["status"] == 1 for item in state["health_observations"]), state
    if args.scenario in {"identity-mismatch", "git-backed", "preflight-unhealthy"}:
        assert puts == [], puts
        return

    assert len(puts) >= 1, puts
    check_update(puts[0], args.compose.read_text(encoding="utf-8"), args.image)

    if args.scenario in {"success", "async-success"}:
        assert len(puts) == 1, puts
        assert state["health_requests"] == 2, state
        if args.scenario == "async-success":
            assert [entry["status"] for entry in state["status_history"]] == [
                3,
                3,
                1,
            ], state
        return

    if args.scenario in {
        "async-timeout",
        "unexpected-status",
        "status-query-persistent",
        "status-request-timeout",
    }:
        assert len(puts) == 1, puts
        assert state["health_requests"] == 1, state
        if args.scenario == "async-timeout":
            assert len(state["status_history"]) == 6, state
            assert all(entry["status"] == 3 for entry in state["status_history"]), state
        return

    assert len(puts) == 2, puts
    rollback = puts[1]
    assert set(rollback) == {"StackFileContent", "Env", "RepullImageAndRedeploy"}, (
        rollback
    )
    assert rollback["StackFileContent"] == ROLLBACK_COMPOSE
    assert rollback["Env"] == ORIGINAL_ENV
    assert rollback["RepullImageAndRedeploy"] is True
    if args.scenario == "rollback":
        assert state["health_requests"] == 5, state
    elif args.scenario in {
        "update-rejected",
        "status-query-failure",
        "status-query-malformed",
        "async-failure",
        "update-ambiguous",
    }:
        assert state["health_requests"] == 2, state
    elif args.scenario == "async-rollback-timeout":
        assert state["health_requests"] == 1, state
        rollback_states = [
            entry["status"] for entry in state["status_history"] if entry["puts"] == 2
        ]
        assert rollback_states == [3, 3, 3], state
    else:
        assert state["health_requests"] == 4, state
    if args.scenario in {"async-failure", "update-ambiguous"}:
        rollback_states = [
            entry["status"] for entry in state["status_history"] if entry["puts"] == 2
        ]
        assert rollback_states == [3, 3, 1], state


if __name__ == "__main__":
    main()
