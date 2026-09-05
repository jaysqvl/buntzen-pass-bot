"""Read-only verification of the order submitted by Yodel's own checkout UI."""

from __future__ import annotations

import json
import time
from typing import Any
from urllib.parse import urlsplit

from .errors import ActionError


CONFIRMATION_TIMEOUT_SECONDS = 30.0
MAX_RECEIPT_BYTES = 1_048_576


def successful_receipt(value: Any) -> bool:
    # The public API adapter wraps this JSON as orderDetail.data. The wire
    # response itself contains payment and walletItems at the top level.
    if not isinstance(value, dict) or value.get("error"):
        return False
    payment = value.get("payment")
    items = value.get("walletItems")
    if not isinstance(payment, dict):
        return False
    order_id = payment.get("orderId")
    identified = (
        isinstance(order_id, int) and not isinstance(order_id, bool) and order_id > 0
    ) or (isinstance(order_id, str) and bool(order_id.strip()))
    return bool(
        payment.get("succeeded") is True
        and not payment.get("errorMessage")
        and identified
        and isinstance(items, list)
        and len(items) == 1
        and isinstance(items[0], dict)
        and items[0]
    )


class CheckoutConfirmation:
    def __init__(self, page: Any, config: Any) -> None:
        self.page = page
        self.config = config
        self.requests: list[Any] = []
        self.response_seen = False
        self.response_valid = False

    def check_not_already_confirmed(self) -> None:
        if self.page.locator("#orderConfirmModal").is_visible():
            raise ActionError("A previous Yodel confirmation is still open; inspect the wallet before booking")

    def __enter__(self) -> "CheckoutConfirmation":
        # Observe only a new request made after the durable confirmation gate.
        self.page.on("request", self._on_request)
        self.page.on("requestfinished", self._on_request_finished)
        return self

    def __exit__(self, *_args: Any) -> None:
        self.page.remove_listener("request", self._on_request)
        self.page.remove_listener("requestfinished", self._on_request_finished)
        self.requests.clear()

    def _on_request(self, request: Any) -> None:
        if request.method != "POST":
            return
        parsed = urlsplit(request.url)
        if parsed.path != "/api/orders/checkout" or parsed.username or parsed.password:
            return
        # Production Yodel uses this separate API origin. Custom approved
        # portals (including the HTTPS integration fixture) use their own origin.
        allowed = self.config.allows_yodel_url(request.url) or (
            self.config.allows_yodel_url("https://yodelportal.com/")
            and parsed.scheme == "https"
            and parsed.netloc == "api.yodelpass.com"
        )
        if allowed:
            self.requests.append(request)

    def _on_request_finished(self, request: Any) -> None:
        if request not in self.requests:
            return
        try:
            # Headers arrive before the response body. Wait for requestfinished
            # so a stalled download cannot block cancellation or our deadline.
            response = request.response()
            if response is None:
                self.response_valid = False
                return
            if not 200 <= response.status < 300:
                self.response_valid = False
                return
            body = response.body()
            self.response_valid = len(body) <= MAX_RECEIPT_BYTES and successful_receipt(json.loads(body))
        except Exception:
            self.response_valid = False
        finally:
            self.response_seen = True

    def wait(self, control: Any) -> None:
        deadline = time.monotonic() + CONFIRMATION_TIMEOUT_SECONDS
        heartbeat_at = time.monotonic() + 15.0
        while time.monotonic() < deadline:
            control.inbox.check_cancelled()
            if not self.config.allows_yodel_url(str(self.page.url)):
                raise ActionError("Yodel left the approved origin during confirmation")
            if self.page.locator("#orderErrorModal").is_visible():
                raise ActionError("Yodel displayed an order error after submission")
            if len(self.requests) > 1 or (self.response_seen and not self.response_valid):
                raise ActionError("Yodel did not return one verified parking pass")
            receipt = self.page.locator("#orderConfirmModal")
            if self.response_valid and receipt.is_visible():
                heading = receipt.locator("h2.heading")
                wallet = receipt.get_by_text("See My Pass", exact=True)
                if (
                    heading.is_visible()
                    and heading.inner_text(timeout=500).strip() == "Confirmed"
                    and wallet.is_visible()
                ):
                    return
            if time.monotonic() >= heartbeat_at:
                control.heartbeat("confirmation_verification")
                heartbeat_at = time.monotonic() + 15.0
            self.page.wait_for_timeout(100)
        raise ActionError("Yodel did not show a verified reservation before the confirmation timeout")
