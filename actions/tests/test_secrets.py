from __future__ import annotations

import io
import logging
import os
import unittest
from unittest.mock import patch

from buntzen_actions.secrets import RedactingLogFilter, SecretRedactor
from buntzen_actions.worker import configure_logging


class SecretRedactorTests(unittest.TestCase):
    def test_diagnostic_redacts_credentials_otp_and_authenticated_url(self) -> None:
        redactor = SecretRedactor()
        redactor.add("person@example.test")
        redactor.add("private-password")
        raw = (
            'person@example.test private-password "code":"654321" '
            "authorization: Bearer header-secret; "
            "http://user:pass@example.test:1234/api/v1/ping?password=provider-secret"
        )
        clean = redactor.redact_diagnostic(raw)
        for forbidden in (
            "person@example.test",
            "private-password",
            "654321",
            "user:pass",
            "provider-secret",
            "header-secret",
            "?password=",
        ):
            self.assertNotIn(forbidden, clean)
        self.assertIn("http://example.test:1234/api/v1/ping", clean)

    def test_log_filter_redacts_positional_arguments_before_formatting(self) -> None:
        redactor = SecretRedactor()
        redactor.add("credential-secret")
        record = logging.LogRecord(
            "worker",
            logging.ERROR,
            __file__,
            1,
            "failure %s otp=%s",
            ("credential-secret", "482913"),
            None,
        )
        self.assertTrue(RedactingLogFilter(redactor).filter(record))
        rendered = record.getMessage()
        self.assertNotIn("credential-secret", rendered)
        self.assertNotIn("482913", rendered)

    def test_debug_environment_controls_python_log_level(self) -> None:
        redactor = SecretRedactor()
        stream = io.StringIO()
        with patch.dict(os.environ, {"BUNTZEN_ACTION_LOG_LEVEL": "debug"}):
            configure_logging(redactor)
        root = logging.getLogger()
        self.assertEqual(root.level, logging.DEBUG)
        # Replace the configured stderr stream before emitting so the test
        # never writes through unittest's captured process stderr.
        root.handlers[0].stream = stream
        root.debug("debug lifecycle event")
        self.assertIn("DEBUG", stream.getvalue())


if __name__ == "__main__":
    unittest.main()
