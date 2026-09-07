#!/usr/bin/env python3
"""Assertions for the Portainer deployment shell integration test."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, unquote

from mock_portainer import (
    CONTAINER_ID,
    DOCKER_PROXY,
    EXPECTED_FILTERS,
    IMAGE_IDS,
    OLD_IMAGE,
    POSTDEPLOY_RACES,
    PREFLIGHT_FAILURES,
    RUNTIME_FAILURES,
    SCENARIOS,
)


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


def check_runtime_reads(state: dict[str, Any], image: str) -> None:
    assert state["route_errors"] == [], state
    for request in state["api_requests"]:
        path = request["path"]
        query = parse_qs(request["query"], strict_parsing=True)
        if request["method"] == "PUT":
            assert path == "/api/stacks/2" and query == {"endpointId": ["1"]}, request
            continue
        assert request["method"] == "GET", request
        if path == "/api/stacks/2":
            assert not query, request
        elif path == f"{DOCKER_PROXY}/containers/json":
            assert set(query) == {"all", "filters"}, request
            assert query["all"] == ["1"] and len(query["filters"]) == 1, request
            assert json.loads(query["filters"][0]) == EXPECTED_FILTERS, request
        elif path == f"{DOCKER_PROXY}/containers/{CONTAINER_ID}/json":
            assert not query, request
        else:
            expected = image if request["puts"] == 1 else OLD_IMAGE
            assert unquote(path) == f"{DOCKER_PROXY}/images/{expected}/json", request
            assert not query, request

    reads = state["runtime_reads"]
    for index, read in enumerate(reads):
        if read["stage"] == "image":
            assert [item["stage"] for item in reads[index - 2 : index]] == [
                "list",
                "inspect",
            ], reads
            assert all(
                item["puts"] == read["puts"] for item in reads[index - 2 : index]
            ), reads
            expected = image if read["puts"] == 1 else OLD_IMAGE
            assert read["coordinate"] == expected, read

    for observation in state["health_observations"]:
        assert observation["status"] == 1, observation
        puts = observation["puts"]
        if puts == 0:
            continue  # Initial /healthz intentionally precedes runtime preflight.
        expected = image if puts == 1 else OLD_IMAGE
        runtime = observation["runtime"]
        assert runtime["Id"] == CONTAINER_ID, observation
        assert runtime["Config"]["Image"] == expected, observation
        assert runtime["Image"] == IMAGE_IDS[expected], observation
        assert runtime["Config"]["Labels"] == {
            "com.docker.compose.project": "buntzen-pass-bot",
            "com.docker.compose.service": "buntzen-pass-bot",
        }, observation
        assert runtime["State"] == {
            "Status": "running",
            "Running": True,
            "Paused": False,
            "Restarting": False,
            "Dead": False,
            "Health": {"Status": "healthy"},
        }, observation
        assert [
            entry
            for entry in runtime["Config"]["Env"]
            if entry == "SCHEDULES_ENABLED" or entry.startswith("SCHEDULES_ENABLED=")
        ] == ["SCHEDULES_ENABLED=false"], observation
        last_read = reads[observation["runtime_reads"] - 1]
        assert last_read["stage"] == "image" and last_read["puts"] == puts, observation
        assert last_read["coordinate"] == expected, observation


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "scenario",
        choices=SCENARIOS,
    )
    parser.add_argument("record", type=Path)
    parser.add_argument("compose", type=Path)
    parser.add_argument("image")
    parser.add_argument("--finished-at", type=float, default=0)
    args = parser.parse_args()

    state = json.loads(args.record.read_text(encoding="utf-8"))
    puts = state["puts"]
    assert len(puts) <= 1, (
        "a second PUT can race with the preceding deployment's pending postDeploy callback",
        state,
    )
    assert state["overlap_attempts"] == 0, state
    assert state["postdeploy_overlap_attempts"] == 0, state
    check_runtime_reads(state, args.image)
    if args.scenario in POSTDEPLOY_RACES:
        assert state["postdeploy_pending"] is True, state
        assert state["backup_generation"] == 1, state
        assert state["callback_backup_generation"] is None, state
    if args.scenario in {
        "identity-mismatch",
        "git-backed",
        "preflight-unhealthy",
        *PREFLIGHT_FAILURES,
    }:
        assert puts == [], puts
        assert not any(
            request["path"] == "/api/stacks/2/file" for request in state["api_requests"]
        ), state
        if args.scenario in PREFLIGHT_FAILURES:
            assert state["health_requests"] == 1, state
        if (
            args.scenario.startswith("preflight-image-")
            and args.scenario != "preflight-image-id-mismatch"
        ):
            assert state["runtime_reads"] == [], state
        return

    assert len(puts) == 1, puts
    check_update(puts[0], args.compose.read_text(encoding="utf-8"), args.image)

    assert [read["stage"] for read in state["runtime_reads"] if read["puts"] == 0] == [
        "list",
        "inspect",
        "image",
    ], state

    if args.scenario in {"success", "async-success", "same-digest", "runtime-retry-healthy"}:
        assert len(puts) == 1, puts
        assert state["health_requests"] == 2, state
        if args.scenario == "async-success":
            assert [entry["status"] for entry in state["status_history"]] == [
                3,
                3,
                1,
            ], state
        if args.scenario == "runtime-retry-healthy":
            assert state["runtime_inspections"]["1"] == 2, state
            assert [
                read["stage"] for read in state["runtime_reads"] if read["puts"] == 1
            ] == ["list", "inspect", "list", "inspect", "image"], state
        return

    expected_health = (
        4
        if args.scenario in {"deployment-unhealthy", "postdeploy-active-cleanup-race"}
        else 1
    )
    assert state["health_requests"] == expected_health, state
    statuses = [entry["status"] for entry in state["status_history"]]
    if args.scenario == "async-timeout":
        assert statuses == [3, 3, 3], state
    elif args.scenario == "async-failure":
        assert statuses == [3, 3, 4], state
    elif args.scenario in {"deployment-inactive", "postdeploy-cleanup-race"}:
        assert statuses == [2 if args.scenario == "deployment-inactive" else 4], state
    elif args.scenario == "postdeploy-active-cleanup-race":
        assert statuses == [1, 1, 1], state
    elif args.scenario == "unexpected-status":
        assert statuses == [99], state
    elif args.scenario in {"update-rejected", "update-ambiguous"}:
        assert statuses == [], state
        assert not any(request["puts"] for request in state["api_requests"]), state
    if args.scenario in RUNTIME_FAILURES:
        assert [item["puts"] for item in state["health_observations"]] == [0], state
        if args.scenario == "runtime-request-timeout":
            phase_requests = [
                request
                for request in state["api_requests"]
                if request["puts"] == 1
            ]
            runtime_requests = [
                request for request in phase_requests if request["path"].startswith(DOCKER_PROXY)
            ]
            assert 2 <= len(runtime_requests) <= 3, phase_requests
            # Three reads each take 2.1 seconds. A five-second shared budget
            # must stop before all 6.3 seconds elapse, including HTTP I/O.
            elapsed = args.finished_at - runtime_requests[0]["time"]
            assert 0 < elapsed < 5.7, (elapsed, phase_requests)


if __name__ == "__main__":
    main()
