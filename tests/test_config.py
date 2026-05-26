from __future__ import annotations

import os
import tempfile
import unittest
from unittest.mock import patch

from src.env_utils import ConfigError, load_config


BASE_ENV = {
    "USER_DATA_DIR": "",
    "TARGET_DATE": "2026-06-18",
    "VEHICLE_KEYWORD": "Tesla",
    "ALL_DAY_PASS_URL": "https://yodelportal.com/buntzen-lake",
    "HALF_DAY_PASS_URL": "https://yodelportal.com/buntzen-lake",
    "CHECK_ALL_DAY": "true",
    "CHECK_MORNING": "false",
    "CHECK_AFTERNOON": "false",
    "TWILIO_ACCOUNT_SID": "AC123",
    "TWILIO_AUTH_TOKEN": "secret",
    "TWILIO_OTP_NUMBER": "+16045551212",
    "TWILIO_ALERTS_ENABLED": "true",
    "TWILIO_ALERT_TO_NUMBER": "+16045559876",
}


class ConfigTests(unittest.TestCase):
    def load_with(self, updates: dict[str, str | None]):
        with tempfile.TemporaryDirectory() as tmpdir:
            env = dict(BASE_ENV)
            env["USER_DATA_DIR"] = tmpdir
            for key, value in updates.items():
                if value is None:
                    env.pop(key, None)
                else:
                    env[key] = value
            with patch.dict(os.environ, env, clear=True):
                return load_config(command="book")

    def test_loads_valid_config(self) -> None:
        config = self.load_with({})
        self.assertEqual(config.target_date.isoformat(), "2026-06-18")
        self.assertEqual(config.run_mode, "manual")
        self.assertTrue(config.check_all_day)

    def test_requires_selected_pass_type(self) -> None:
        with self.assertRaises(ConfigError):
            self.load_with({"CHECK_ALL_DAY": "false"})

    def test_requires_twilio_for_book(self) -> None:
        with self.assertRaises(ConfigError):
            self.load_with({"TWILIO_ACCOUNT_SID": None})

    def test_rejects_partial_yodel_credentials(self) -> None:
        with self.assertRaises(ConfigError):
            self.load_with({"YODEL_EMAIL": "me@example.com", "YODEL_PASSWORD": None})


if __name__ == "__main__":
    unittest.main()
