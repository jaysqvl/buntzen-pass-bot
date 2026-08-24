#!/usr/bin/env python3
"""Assertions for the Portainer deployment shell integration test."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any

from mock_portainer import ORIGINAL_ENV, ROLLBACK_COMPOSE


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
    assert {"name": "PRESERVED_VALUE", "value": "preserve-me"} in env


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "scenario",
        choices=(
            "success",
            "rollback",
            "rollback-failure",
            "update-rejected",
            "status-query-failure",
            "status-query-malformed",
            "identity-mismatch",
            "git-backed",
            "preflight-unhealthy",
        ),
    )
    parser.add_argument("record", type=Path)
    parser.add_argument("compose", type=Path)
    parser.add_argument("image")
    args = parser.parse_args()

    state = json.loads(args.record.read_text(encoding="utf-8"))
    puts = state["puts"]
    if args.scenario in {"identity-mismatch", "git-backed", "preflight-unhealthy"}:
        assert puts == [], puts
        return

    assert len(puts) >= 1, puts
    check_update(puts[0], args.compose.read_text(encoding="utf-8"), args.image)

    if args.scenario == "success":
        assert len(puts) == 1, puts
        assert state["health_requests"] == 2, state
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
    }:
        assert state["health_requests"] == 2, state
    else:
        assert state["health_requests"] == 4, state


if __name__ == "__main__":
    main()
