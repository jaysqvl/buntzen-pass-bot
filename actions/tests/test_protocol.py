from __future__ import annotations

import io
import json
import os
import queue
import threading
import time
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

    def test_ignored_late_otp_frames_do_not_restart_a_receive_deadline(self) -> None:
        reader_fd, writer_fd = os.pipe()
        reader = os.fdopen(reader_fd, "rb", buffering=0)
        writer = os.fdopen(writer_fd, "wb", buffering=0)
        inbox = ControlInbox(JsonLineStream(reader, io.BytesIO()))
        inbox.ignore_otp_challenge("old")
        started = threading.Event()
        send_confirmation = threading.Event()
        confirmation_sent = threading.Event()
        stop = threading.Event()

        def produce_replies() -> None:
            with writer:
                # Keep traffic flowing longer than the receive deadline. The
                # fallback bounds this test even if an old implementation keeps
                # restarting its timeout for every discarded OTP reply.
                finish_at = time.monotonic() + 0.6
                while not send_confirmation.is_set() and time.monotonic() < finish_at:
                    writer.write(b'{"v":2,"type":"otp.expired","challenge_id":"old"}\n')
                    started.set()
                    if stop.wait(0.01):
                        return
                writer.write(b'{"v":2,"type":"confirmation.ready","confirmation_id":"next"}\n')
                confirmation_sent.set()
                stop.wait(1.0)

        producer = threading.Thread(target=produce_replies, daemon=True)
        producer.start()
        try:
            self.assertTrue(started.wait(1.0), "reply producer did not start")
            with self.assertRaises(queue.Empty):
                inbox.receive({"confirmation.ready"}, timeout=0.15)
            self.assertTrue(producer.is_alive())
            self.assertFalse(confirmation_sent.is_set(), "receive waited through the OTP traffic")
            send_confirmation.set()
            # The timed-out wait must not consume the next legitimate reply.
            self.assertEqual(
                inbox.receive({"confirmation.ready"}, timeout=0.5)["confirmation_id"],
                "next",
            )
        finally:
            stop.set()
            producer.join(timeout=1.0)
            inbox._thread.join(timeout=1.0)
            reader.close()

    def test_zero_timeout_can_read_an_already_available_response(self) -> None:
        stream = JsonLineStream(io.BytesIO(
            b'{"v":2,"type":"confirmation.ready","confirmation_id":"ready"}\n'
        ), io.BytesIO())
        inbox = ControlInbox(stream)
        inbox._thread.join(timeout=1.0)
        self.assertEqual(inbox.receive({"confirmation.ready"}, timeout=0)["confirmation_id"], "ready")


if __name__ == "__main__":
    unittest.main()
