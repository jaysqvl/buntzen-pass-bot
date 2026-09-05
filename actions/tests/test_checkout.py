from __future__ import annotations

import copy
import json
import unittest
from types import SimpleNamespace

from buntzen_actions.checkout import CheckoutConfirmation, successful_receipt


def receipt():
    return {
        "payment": {"succeeded": True, "errorMessage": None, "orderId": 123},
        "walletItems": [{"summaryField1": {"value": "Synthetic pass"}}],
    }


class CheckoutTests(unittest.TestCase):
    def test_payment_success_requires_one_issued_pass_and_order_id(self):
        self.assertTrue(successful_receipt(receipt()))
        for field, value in (("succeeded", False), ("succeeded", "true"),
                             ("errorMessage", "sold out"), ("orderId", None),
                             ("orderId", False), ("orderId", "")):
            payload = receipt()
            payload["payment"][field] = value
            with self.subTest(field=field, value=value):
                self.assertFalse(successful_receipt(payload))
        for items in (None, [], [{}, {}], [None], [{}], "pass", [receipt(), receipt()]):
            payload = receipt()
            payload["walletItems"] = items
            with self.subTest(items=items):
                self.assertFalse(successful_receipt(payload))
        for payload in (None, [], {"data": receipt()}, {"payment": None}):
            self.assertFalse(successful_receipt(payload))

    def test_only_new_checkout_requests_on_approved_origins_count(self):
        observer = CheckoutConfirmation(None, SimpleNamespace(
            allows_yodel_url=lambda url: url.startswith("https://yodelportal.com/")
        ))
        for method, url in (
            ("GET", "https://api.yodelpass.com/api/orders/checkout"),
            ("POST", "https://attacker.example/api/orders/checkout"),
            ("POST", "https://api.yodelpass.com.evil.example/api/orders/checkout"),
            ("POST", "https://api.yodelpass.com/api/cart"),
        ):
            observer._on_request(SimpleNamespace(method=method, url=url))
        self.assertEqual(observer.requests, [])
        request = SimpleNamespace(method="POST", url="https://api.yodelpass.com/api/orders/checkout")
        response = SimpleNamespace(request=request, status=200, body=lambda: json.dumps(receipt()).encode())
        request.response = lambda: response
        observer._on_request_finished(request)
        self.assertFalse(observer.response_seen, "a response from before the final click must not count")
        observer._on_request(request)
        observer._on_request_finished(request)
        self.assertTrue(observer.response_valid)
        for status, payload in ((500, receipt()), (200, {"error": "sold out"}), (200, None)):
            failed = copy.copy(response)
            failed.status = status
            failed.body = lambda: json.dumps(payload).encode()
            request.response = lambda: failed
            observer._on_request_finished(request)
            self.assertFalse(observer.response_valid)


if __name__ == "__main__":
    unittest.main()
