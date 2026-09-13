from __future__ import annotations

import os
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import Mock, patch

from lake_pass_actions.config import ActionConfig
from lake_pass_actions.environment import operator_env
from lake_pass_actions.errors import ProtocolError
from lake_pass_actions.lakes import LAKES, resolve_lake
from lake_pass_actions.providers import PROVIDERS, Provider, resolve_provider
from lake_pass_actions.providers.yodel.action import YodelAction
from lake_pass_actions.worker import run_action
from test_config import start_frame


class RegistryTests(unittest.TestCase):
    def test_provider_only_auth_works_without_any_registered_lake(self) -> None:
        frame = start_frame()
        frame["command"] = "auth-check"
        frame["config"].pop("vehicle_keyword")
        frame["config"].pop("pass_order")
        with patch("lake_pass_actions.lakes.LAKES", {}):
            config = ActionConfig.from_start(frame)
            page = Mock()
            action = YodelAction(page, config, Mock(), Mock())
            action.ensure_authenticated = Mock(return_value=True)
            result = action.execute()
            self.assertTrue(result.success)
            self.assertIsNone(action.lake)
            action.ensure_authenticated.assert_called_once()

    def test_legacy_start_and_explicit_selection_resolve_to_same_lake(self) -> None:
        legacy = ActionConfig.from_start(start_frame())
        frame = start_frame()
        frame["config"].update(lake_id="buntzen", provider_id="yodel")
        selected = ActionConfig.from_start(frame)
        self.assertEqual(legacy, selected)
        self.assertEqual(resolve_lake(selected.lake_id).label, "Buntzen Lake")
        self.assertEqual(resolve_provider(selected.lake_id, selected.provider_id).id, "yodel")

    def test_explicit_invalid_ids_never_fall_back(self) -> None:
        for field in ("lake_id", "provider_id"):
            for value in ("unknown", "", " ", None, False, [], {}):
                with self.subTest(field=field, value=value):
                    frame = start_frame()
                    frame["config"][field] = value
                    with self.assertRaises(ProtocolError):
                        ActionConfig.from_start(frame)

    def test_registered_provider_cannot_be_substituted_for_lake_provider(self) -> None:
        providers = {**PROVIDERS, "other": Provider("other", "Other", Mock())}
        frame = start_frame()
        frame["config"].update(lake_id="buntzen", provider_id="other")
        with patch("lake_pass_actions.providers.PROVIDERS", providers):
            with self.assertRaisesRegex(ProtocolError, "does not support"):
                ActionConfig.from_start(frame)
        providers["other"].create_action.assert_not_called()

    def test_worker_revalidates_constructed_config_before_any_browser_or_secret_access(self) -> None:
        for fields in ({"lake_id": "unknown"}, {"provider_id": "unknown"}):
            with self.subTest(fields=fields), tempfile.TemporaryDirectory() as directory:
                profile = Path(directory) / "profile"
                config = replace(ActionConfig.from_start(start_frame()), profile_dir=profile, **fields)
                control = Mock()
                with patch("playwright.sync_api.sync_playwright") as browser:
                    with self.assertRaises(ProtocolError):
                        run_action(config, control)
                browser.assert_not_called()
                self.assertFalse(profile.exists())
                self.assertEqual(control.mock_calls, [])

    def test_pass_validation_uses_selected_lake_rules(self) -> None:
        lake = replace(resolve_lake("buntzen"), id="synthetic", pass_preferences={})
        frame = start_frame()
        frame["config"]["lake_id"] = lake.id
        with patch("lake_pass_actions.lakes.LAKES", {**LAKES, lake.id: lake}):
            with self.assertRaisesRegex(ProtocolError, "unsupported pass keys"):
                ActionConfig.from_start(frame)

    def test_worker_dispatches_through_registered_factory(self) -> None:
        action = Mock()
        action.execute.return_value = SimpleNamespace(success=True, message="Done", pass_key=None)
        factory = Mock(return_value=action)
        provider = Provider("yodel", "Yodel", factory)
        context = Mock(pages=[object()])
        with tempfile.TemporaryDirectory() as directory:
            config = replace(ActionConfig.from_start(start_frame()), profile_dir=Path(directory) / "profile")
            with (
                patch("lake_pass_actions.providers.PROVIDERS", {"yodel": provider}),
                patch("playwright.sync_api.sync_playwright"),
                patch("lake_pass_actions.worker._open_context", return_value=context),
                patch("lake_pass_actions.worker.SafeDiagnostics"),
            ):
                self.assertEqual(run_action(config, Mock()), ("Done", None))
        factory.assert_called_once()
        self.assertIs(factory.call_args.kwargs["config"], config)
        context.close.assert_called_once()


class EnvironmentTests(unittest.TestCase):
    def test_canonical_values_including_empty_override_legacy(self) -> None:
        for suffix in ("ACTION_LOG_LEVEL", "BROWSER_EXECUTABLE", "ACTIONPROC_HELPER", "E2E_BROWSER_EXECUTABLE"):
            for value in ("new", ""):
                with self.subTest(suffix=suffix, value=value), patch.dict(os.environ, {
                    f"LAKE_PASS_{suffix}": value,
                    f"BUNTZEN_{suffix}": "legacy",
                }, clear=True):
                    self.assertEqual(operator_env(suffix, "default"), value)

    def test_legacy_and_default_apply_only_when_new_name_absent(self) -> None:
        with patch.dict(os.environ, {"BUNTZEN_ACTION_LOG_LEVEL": "debug"}, clear=True):
            self.assertEqual(operator_env("ACTION_LOG_LEVEL", "info"), "debug")
        with patch.dict(os.environ, {}, clear=True):
            self.assertEqual(operator_env("ACTION_LOG_LEVEL", "info"), "info")


if __name__ == "__main__":
    unittest.main()
