from __future__ import annotations

import importlib.util
import json
import subprocess
import unittest
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[3]
SPEC = importlib.util.spec_from_file_location("promote_latest", ROOT / "scripts/release/promote_latest.py")
assert SPEC is not None and SPEC.loader is not None
PROMOTION = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PROMOTION)
DIGEST = "sha256:" + "a" * 64


def release(version: str, **flags: bool) -> dict[str, object]:
    return {"tag_name": f"buntzen-pass-bot-v{version}", "draft": False, "prerelease": False, **flags}


class PromotionTests(unittest.TestCase):
    def test_newest_stable_promotes_exact_accepted_digest_and_verifies_it(self) -> None:
        # Numeric ordering and pagination matter: 0.5.10 is newer than 0.5.9.
        inventory = [[release("0.5.9")], [release("0.5.10"), release("1.0.0-rc.1", prerelease=True)]]
        with patch.object(PROMOTION, "command", side_effect=[json.dumps(inventory), "", json.dumps({"digest": DIGEST})]) as command:
            message = PROMOTION.promote("Jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.10", DIGEST)
        self.assertIn("Published", message)
        self.assertEqual(command.call_args_list[0].args, (
            "gh", "api", "--paginate", "--slurp", "repos/Jaysqvl/buntzen-pass-bot/releases?per_page=100",
        ))
        self.assertEqual(command.call_args_list[1].args, (
            "docker", "buildx", "imagetools", "create", "--tag",
            "ghcr.io/jaysqvl/buntzen-pass-bot:latest", f"ghcr.io/jaysqvl/buntzen-pass-bot@{DIGEST}",
        ))
        self.assertEqual(command.call_args_list[2].args[4], "ghcr.io/jaysqvl/buntzen-pass-bot:latest")

    def test_older_manual_release_does_not_regress_latest(self) -> None:
        with patch.object(PROMOTION, "command", return_value=json.dumps([[release("0.5.2"), release("0.5.1")]])) as command:
            message = PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.1", DIGEST)
        self.assertIn("newer", message)
        self.assertEqual(command.call_count, 1)

    def test_prerelease_does_not_update_latest(self) -> None:
        with patch.object(PROMOTION, "command") as command:
            message = PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v1.0.0-rc.1", DIGEST)
        self.assertIn("prereleases", message)
        command.assert_not_called()

    def test_drafts_prerelease_flags_and_missing_releases_do_not_promote(self) -> None:
        for inventory in ([], [release("0.5.2", draft=True)], [release("0.5.2", prerelease=True)]):
            with self.subTest(inventory=inventory), patch.object(PROMOTION, "command", return_value=json.dumps([inventory])) as command:
                message = PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.2", DIGEST)
                self.assertIn("not a published stable release", message)
                self.assertEqual(command.call_count, 1)

    def test_newer_drafts_and_prereleases_do_not_block_stable(self) -> None:
        inventory = [[release("0.5.2"), release("0.6.0", draft=True), release("1.0.0", prerelease=True)]]
        with patch.object(PROMOTION, "command", side_effect=[json.dumps(inventory), "", json.dumps({"digest": DIGEST})]):
            self.assertIn("Published", PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.2", DIGEST))

    def test_registry_digest_mismatch_fails(self) -> None:
        with patch.object(PROMOTION, "command", side_effect=[json.dumps([[release("0.5.2")]]), "", json.dumps({"digest": "sha256:" + "b" * 64})]):
            with self.assertRaisesRegex(ValueError, "did not resolve"):
                PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.2", DIGEST)

    def test_inventory_and_registry_errors_fail_closed(self) -> None:
        with patch.object(PROMOTION, "command", side_effect=subprocess.CalledProcessError(1, "gh")) as command:
            with self.assertRaises(subprocess.CalledProcessError):
                PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.2", DIGEST)
            self.assertEqual(command.call_count, 1)
        for malformed in ({}, [{}], [[{}]], [["release"]]):
            with self.subTest(malformed=malformed), patch.object(PROMOTION, "command", return_value=json.dumps(malformed)) as command:
                with self.assertRaises(ValueError):
                    PROMOTION.promote("jaysqvl/buntzen-pass-bot", "buntzen-pass-bot-v0.5.2", DIGEST)
                self.assertEqual(command.call_count, 1)

    def test_invalid_coordinates_fail_before_network_access(self) -> None:
        with patch.object(PROMOTION, "command") as command:
            for repository, digest in (("invalid", DIGEST), ("jaysqvl/buntzen-pass-bot", "latest")):
                with self.assertRaises(ValueError):
                    PROMOTION.promote(repository, "buntzen-pass-bot-v0.5.2", digest)
            command.assert_not_called()


if __name__ == "__main__":
    unittest.main()
