"""Regression coverage for deployment-host and authenticated landing checks."""
from __future__ import annotations

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import os
from pathlib import Path
import re
import subprocess
import tempfile
import threading
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location("docker_browser_smoke", ROOT / "scripts/docker_browser_smoke.py")
assert SPEC is not None and SPEC.loader is not None
BROWSER_SMOKE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BROWSER_SMOKE)
SWAP_ENV = "LAKE_PASS_SMOKE_SWAP_LIMIT_SUPPORTED"
SWAP_HEADER = "Filename\tType\tSize\tUsed\tPriority\n"


class SwapLimitTests(unittest.TestCase):
    def test_supported_accounting_requires_the_effective_limit(self) -> None:
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {SWAP_ENV: "true"}):
            limit = Path(directory) / "limit"
            for expected in (0, 4 << 30):
                limit.write_text(str(expected))
                BROWSER_SMOKE.check_swap_limit(limit, expected)
                limit.write_text(str(expected + 1))
                with self.assertRaises(AssertionError):
                    BROWSER_SMOKE.check_swap_limit(limit, expected)
            limit.unlink()
            with self.assertRaises(FileNotFoundError):
                BROWSER_SMOKE.check_swap_limit(limit, 0)

    def test_missing_limit_needs_explicit_unsupported_capability(self) -> None:
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {SWAP_ENV: ""}):
            root = Path(directory)
            (root / "swaps").write_text(SWAP_HEADER)
            with self.assertRaises(FileNotFoundError):
                BROWSER_SMOKE.check_swap_limit(root / "missing-limit", 0, root / "swaps")

    def test_unsupported_accounting_requires_verified_empty_host_swap(self) -> None:
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {SWAP_ENV: "false"}):
            root = Path(directory)
            swaps = root / "swaps"
            swaps.write_text(SWAP_HEADER)
            BROWSER_SMOKE.check_swap_limit(root / "missing-limit", 0, swaps)
            for contents in ("", "unrecognized\n", SWAP_HEADER + "/swapfile file 1024 0 -2\n"):
                swaps.write_text(contents)
                with self.subTest(contents=contents), self.assertRaises(AssertionError):
                    BROWSER_SMOKE.check_swap_limit(root / "missing-limit", 0, swaps)
            swaps.unlink()
            with self.assertRaises(FileNotFoundError):
                BROWSER_SMOKE.check_swap_limit(root / "missing-limit", 0, swaps)


class AuthenticatedLandingTests(unittest.TestCase):
    def run_landing(self, root_status=303, destination="/lakes", final_status=200,
                    identity=True, cache="no-store", csp="default-src 'none'"):
        paths = []

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                paths.append(self.path)
                redirect = self.path == "/" and root_status == 303
                self.send_response(303 if redirect else final_status)
                if redirect:
                    self.send_header("Location", destination)
                else:
                    self.send_header("Cache-Control", cache)
                    self.send_header("Content-Security-Policy", csp)
                self.end_headers()
                if not redirect and identity:
                    self.wfile.write(b'<a aria-label="Account settings for ci-admin">ci-admin</a>')

            def log_message(self, *_args):
                pass

        script = (ROOT / "scripts/docker_smoke.sh").read_text()
        functions = []
        for name in ("fail", "header_value", "fetch_authenticated_landing"):
            match = re.search(rf"(?ms)^{name}\(\) \{{\n.*?^\}}", script)
            assert match is not None
            functions.append(match.group())
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                cookies = root / "cookies"
                cookies.touch()
                environment = {**os.environ, "base_url": f"http://127.0.0.1:{server.server_port}",
                               "admin_username": "ci-admin", "SMOKE_FIXTURE": str(root)}
                command = "set -Eeuo pipefail\n" + "\n".join(functions) + '\nfetch_authenticated_landing "$SMOKE_FIXTURE/cookies" "$SMOKE_FIXTURE/page" "$SMOKE_FIXTURE/headers"\n'
                result = subprocess.run(["bash", "-c", command], env=environment,
                                        capture_output=True, text=True, timeout=10)
                return result, paths
        finally:
            server.shutdown()
            thread.join()
            server.server_close()

    def test_home_and_expected_lakes_redirect_keep_authenticated_checks(self):
        for status, paths in ((200, ["/"]), (303, ["/", "/lakes"])):
            with self.subTest(status=status):
                result, requested = self.run_landing(root_status=status)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(requested, paths)

    def test_unexpected_redirect_is_never_followed(self):
        for destination in ("/login", "https://example.test/"):
            with self.subTest(destination=destination):
                result, paths = self.run_landing(destination=destination)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(paths, ["/"])

    def test_final_page_must_be_authenticated_and_hardened(self):
        for options in ({"final_status": 401}, {"identity": False}, {"cache": "public"}, {"csp": ""}):
            with self.subTest(options=options):
                result, paths = self.run_landing(**options)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(paths, ["/", "/lakes"])


if __name__ == "__main__":
    unittest.main()
