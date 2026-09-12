"""Run inside the candidate image; exercise its installed browser launch helper."""
from __future__ import annotations

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class FixtureHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/sw.js":
            body = b"""
self.addEventListener('install', () => self.skipWaiting());
self.addEventListener('activate', event => event.waitUntil(self.clients.claim()));
self.addEventListener('fetch', event => {
  if (new URL(event.request.url).pathname === '/controlled') {
    event.respondWith(new Response('service worker active'));
  }
});
"""
            content_type = "text/javascript"
        else:
            body = b"<!doctype html><title>Local browser fixture</title>Browser ready"
            content_type = "text/html"
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


def cgroup_events() -> dict[str, int]:
    root = Path("/sys/fs/cgroup")
    if (root / "cgroup.controllers").exists():
        quota, period = (root / "cpu.max").read_text().split()
        assert int(quota) == 2 * int(period), "CPU quota differs from smoke contract"
        assert int((root / "memory.max").read_text()) == 4 << 30
        assert int((root / "memory.swap.max").read_text()) == 0
        assert int((root / "pids.max").read_text()) == 512
        memory = dict(line.split() for line in (root / "memory.events").read_text().splitlines())
        pids = dict(line.split() for line in (root / "pids.events").read_text().splitlines())
        return {"oom": int(memory["oom"]), "oom_kill": int(memory["oom_kill"]), "pids_max": int(pids["max"])}
    memory = root / "memory"
    cpu = root / "cpu"
    assert int((cpu / "cpu.cfs_quota_us").read_text()) == 2 * int((cpu / "cpu.cfs_period_us").read_text())
    assert int((memory / "memory.limit_in_bytes").read_text()) == 4 << 30
    assert int((memory / "memory.memsw.limit_in_bytes").read_text()) == 4 << 30
    assert int((root / "pids/pids.max").read_text()) == 512
    events = dict(line.split() for line in (root / "pids/pids.events").read_text().splitlines())
    return {"memory_fail": int((memory / "memory.failcnt").read_text()), "pids_max": int(events["max"])}


def check_browser_arguments():
    # Headless shell does not expose Browser.getBrowserCommandLine. Inspect
    # only this Python worker's real descendants in the Linux image instead.
    # The worker helper can also be checked natively; /proc evidence is Linux-only.
    if sys.platform != "linux":
        return
    processes = {}
    for directory in Path("/proc").glob("[0-9]*"):
        try:
            status = dict(line.split(":", 1) for line in (directory / "status").read_text().splitlines())
            with (directory / "cmdline").open("rb") as command:
                arguments = command.read(65536).decode().split("\x00")
            processes[int(directory.name)] = (int(status["PPid"]), arguments)
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
    descendants = {os.getpid()}
    while True:
        discovered = {pid for pid, (parent, _) in processes.items() if parent in descendants}
        if discovered.issubset(descendants):
            break
        descendants.update(discovered)
    browsers = [
        arguments for pid, (_, arguments) in processes.items()
        if pid in descendants and arguments and
        any(name in Path(arguments[0]).name for name in ("chrome", "chromium", "headless_shell"))
    ]
    assert browsers, "no live Chromium descendants observed"
    forbidden = {"--no-sandbox", "--disable-namespace-sandbox", "--disable-seccomp-filter-sandbox"}
    for arguments in browsers:
        assert not forbidden.intersection(arg.split("=", 1)[0] for arg in arguments)


def worker(profile: Path, base_url: str, barrier: Path):
    from playwright.sync_api import sync_playwright
    from lake_pass_actions.config import ActionConfig
    from lake_pass_actions.worker import _chromium_user_agent, _open_context

    config = ActionConfig.from_start({
        "v": 2, "type": "run.start", "run_id": "container-browser-smoke",
        "command": "auth-check", "mode": "auto",
        "config": {
            "lake_id": "buntzen", "provider_id": "yodel",
            "profile_dir": str(profile), "headless": True,
            "vehicle_keyword": "fixture",
            "login_probe_url": "https://yodelportal.com/buntzen-lake",
            "allowed_yodel_origins": ["https://yodelportal.com"],
        },
    })
    with sync_playwright() as playwright:
        with _open_context(playwright, config) as context:
            page = context.new_page()
            page.goto(base_url)
            assert page.title() == "Local browser fixture"
            user_agent = page.evaluate("navigator.userAgent")
            assert user_agent == _chromium_user_agent(playwright.chromium.executable_path)
            assert "HeadlessChrome" not in user_agent
            check_browser_arguments()
            page.evaluate("navigator.serviceWorker.register('/sw.js')")
            page.wait_for_function("navigator.serviceWorker.controller !== null", timeout=15000)
            assert page.evaluate("fetch('/controlled').then(response => response.text())") == "service worker active"
            page.evaluate("localStorage.setItem('persistent-smoke', 'retained')")
            profile.with_suffix(".ready").touch()
            deadline = time.monotonic() + 30
            while not barrier.exists():
                if time.monotonic() >= deadline:
                    raise RuntimeError("the second browser did not become ready")
                time.sleep(0.05)
        with _open_context(playwright, config) as context:
            page = context.new_page()
            page.goto(base_url)
            assert page.evaluate("localStorage.getItem('persistent-smoke')") == "retained"


def main():
    assert os.getuid() == 1001, "browser smoke must use the service UID"
    before = cgroup_events()
    with tempfile.TemporaryDirectory(prefix=".ci-browser-", dir="/appdata/profiles") as directory:
        root = Path(directory)
        barrier = root / "both-ready"
        server = ThreadingHTTPServer(("127.0.0.1", 0), FixtureHandler)
        server_thread = threading.Thread(target=server.serve_forever, daemon=True)
        server_thread.start()
        processes = []
        try:
            for index in range(2):
                processes.append(subprocess.Popen([
                    sys.executable, __file__, "worker", str(root / f"browser-{index}"),
                    f"http://127.0.0.1:{server.server_port}", str(barrier),
                ]))
            deadline = time.monotonic() + 60
            while len(list(root.glob("*.ready"))) != 2:
                if any(process.poll() is not None for process in processes):
                    raise RuntimeError("browser worker exited before the concurrency check")
                if time.monotonic() >= deadline:
                    raise RuntimeError("browser workers did not become ready")
                time.sleep(0.05)
            barrier.touch()
            for process in processes:
                assert process.wait(timeout=max(1, deadline - time.monotonic())) == 0
        finally:
            for process in processes:
                if process.poll() is None:
                    process.kill()
                process.wait()
            server.shutdown()
            server.server_close()
            server_thread.join()
    assert cgroup_events() == before, "browser smoke hit memory or PID limits"
    print(json.dumps({"concurrent_browsers": 2, "service_workers": True, "profile_restart": True, "resource_events": before}))


if __name__ == "__main__":
    if len(sys.argv) == 5 and sys.argv[1] == "worker":
        worker(Path(sys.argv[2]), sys.argv[3], Path(sys.argv[4]))
    else:
        main()
