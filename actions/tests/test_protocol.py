from __future__ import annotations

import io
import json
import unittest

from buntzen_actions.errors import Cancelled, ProtocolError
from buntzen_actions.protocol import (
    JsonLineStream,
    MAX_FRAME_BYTES,
    validate_start_has_no_secrets,
)
from buntzen_actions.protocol import ControlInbox


class ProtocolTests(unittest.TestCase):
    def test_reads_a_versioned_object(self) -> None:
        reader = io.BytesIO(b'{"v":2,"type":"run.start"}\n')
        stream = JsonLineStream(reader, io.BytesIO())
        self.assertEqual(stream.read()["type"], "run.start")

    def test_rejects_boolean_version_and_non_finite_number(self) -> None:
        with self.assertRaises(ProtocolError):
            JsonLineStream(io.BytesIO(b'{"v":true,"type":"x"}\n'), io.BytesIO()).read()
        with self.assertRaises(ProtocolError):
            JsonLineStream(
                io.BytesIO(b'{"v":2,"type":"x","value":NaN}\n'), io.BytesIO()
            ).read()

    def test_rejects_oversized_or_unterminated_input(self) -> None:
        oversized = io.BytesIO(b"{" + b"x" * MAX_FRAME_BYTES + b"}\n")
        with self.assertRaises(ProtocolError):
            JsonLineStream(oversized, io.BytesIO()).read()
        with self.assertRaises(ProtocolError):
            JsonLineStream(io.BytesIO(b'{"v":2,"type":"x"}'), io.BytesIO()).read()

    def test_refuses_secret_fields_on_output(self) -> None:
        stream = JsonLineStream(io.BytesIO(), io.BytesIO())
        with self.assertRaises(ProtocolError):
            stream.write("status", password="do-not-echo")
        with self.assertRaises(ProtocolError):
            stream.write("status", nested={"code": "123456"})

    def test_serializes_one_compact_line(self) -> None:
        output = io.BytesIO()
        JsonLineStream(io.BytesIO(), output).write("worker.ready", protocol=2)
        frames = output.getvalue().splitlines()
        self.assertEqual(len(frames), 1)
        self.assertEqual(
            json.loads(frames[0]), {"v": 2, "type": "worker.ready", "protocol": 2}
        )

    def test_rejects_secrets_in_start_config(self) -> None:
        with self.assertRaises(ProtocolError):
            validate_start_has_no_secrets(
                {"config": {"nested": {"twilio_auth_token": "secret"}}}
            )

    def test_confirmation_wait_rejects_wrong_or_mismatched_frame(self) -> None:
        wrong_type = JsonLineStream(
            io.BytesIO(
                b'{"v":2,"type":"approval.approve","confirmation_id":"expected"}\n'
            ),
            io.BytesIO(),
        )
        with self.assertRaises(ProtocolError):
            ControlInbox(wrong_type).receive({"confirmation.ready"})

        wrong_id = JsonLineStream(
            io.BytesIO(
                b'{"v":2,"type":"confirmation.ready","confirmation_id":"wrong"}\n'
            ),
            io.BytesIO(),
        )
        with self.assertRaises(ProtocolError):
            ControlInbox(wrong_id).receive(
                {"confirmation.ready"},
                predicate=lambda frame: frame.get("confirmation_id") == "expected",
            )

    def test_confirmation_wait_treats_closed_stream_as_cancellation(self) -> None:
        stream = JsonLineStream(io.BytesIO(), io.BytesIO())
        with self.assertRaises(Cancelled):
            ControlInbox(stream).receive({"confirmation.ready"})


if __name__ == "__main__":
    unittest.main()
