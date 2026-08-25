from __future__ import annotations

import unittest

from buntzen_actions.config import ActionConfig
from buntzen_actions.errors import ProtocolError


def start_frame() -> dict:
    return {
        "v": 2,
        "type": "run.start",
        "run_id": "job-1",
        "command": "book",
        "mode": "manual",
        "config": {
            "profile_dir": "/tmp/buntzen-profile",
            "target_date": "2030-01-15",
            "timezone": "UTC",
            "allowed_yodel_origins": ["https://yodelportal.com"],
            "login_probe_url": "https://yodelportal.com/buntzen-lake",
            "all_day_pass_url": "https://yodelportal.com/buntzen-lake/All-Day-Pass",
            "half_day_pass_url": "https://yodelportal.com/buntzen-lake/Half-Day-Pass",
            "vehicle_keyword": "Example Vehicle",
            "pass_order": ["all_day", "afternoon", "morning"],
            "headless": True,
            "release_at": "2030-01-14T07:00:00Z",
            "auth_deadline_at": "2030-01-14T06:55:00Z",
        },
    }


class ConfigTests(unittest.TestCase):
    def test_parses_control_plane_config(self) -> None:
        config = ActionConfig.from_start(start_frame())
        self.assertEqual(config.command, "book")
        self.assertEqual(config.pass_order, ("all_day", "afternoon", "morning"))
        self.assertEqual(config.release_at.utcoffset().total_seconds(), 0)
        self.assertEqual(config.auth_deadline_at.minute, 55)

    def test_dry_run_command_forces_dry_run_mode(self) -> None:
        frame = start_frame()
        frame["command"] = "dry-run"
        frame["mode"] = "auto"
        self.assertEqual(ActionConfig.from_start(frame).mode, "dry-run")

    def test_rejects_relative_profile_and_duplicate_pass(self) -> None:
        frame = start_frame()
        frame["config"]["profile_dir"] = "relative"
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)
        frame = start_frame()
        frame["config"]["pass_order"] = ["all_day", "all_day"]
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)

    def test_rejects_credentials_in_start_payload(self) -> None:
        frame = start_frame()
        frame["config"]["yodel_password"] = "not-allowed"
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)

    def test_rejects_auth_deadline_after_release(self) -> None:
        frame = start_frame()
        frame["config"]["auth_deadline_at"] = "2030-01-14T07:01:00Z"
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)

    def test_rejects_booking_urls_outside_approved_https_origin(self) -> None:
        for value in (
            "https://attacker.example/buntzen-lake",
            "https://yodelportal.com.attacker.example/buntzen-lake",
            "http://yodelportal.com/buntzen-lake",
        ):
            with self.subTest(value=value):
                frame = start_frame()
                frame["config"]["login_probe_url"] = value
                with self.assertRaises(ProtocolError):
                    ActionConfig.from_start(frame)

    def test_requires_exact_trusted_origin_list(self) -> None:
        frame = start_frame()
        del frame["config"]["allowed_yodel_origins"]
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)
        frame = start_frame()
        frame["config"]["allowed_yodel_origins"] = [
            "https://yodelportal.com/path"
        ]
        with self.assertRaises(ProtocolError):
            ActionConfig.from_start(frame)


if __name__ == "__main__":
    unittest.main()
