"""Exercise retained encrypted credentials using only the local app's UI."""
from __future__ import annotations

from html.parser import HTMLParser
from http.cookiejar import CookieJar
import os
import sqlite3
import sys
from urllib.parse import urlencode
from urllib.request import HTTPCookieProcessor, Request, build_opener


class FormParser(HTMLParser):
    def __init__(self, action):
        super().__init__()
        self.action = action
        self.in_form = False
        self.csrf = []

    def handle_starttag(self, tag, attrs):
        fields = dict(attrs)
        if tag == "form":
            self.in_form = fields.get("action") == self.action
        if self.in_form and tag == "input" and fields.get("name") == "csrf_token":
            self.csrf.append(fields.get("value"))

    def handle_endtag(self, tag):
        if tag == "form":
            self.in_form = False


def main():
    base = "http://127.0.0.1:8080"
    client = build_opener(HTTPCookieProcessor(CookieJar()))

    def submit(path, fields):
        with client.open(base + path, timeout=10) as response:
            parser = FormParser(path)
            parser.feed(response.read().decode())
        assert len(parser.csrf) == 1 and parser.csrf[0]
        payload = urlencode(dict(fields, csrf_token=parser.csrf[0])).encode()
        request = Request(base + path, data=payload, headers={"Origin": base})
        with client.open(request, timeout=10) as response:
            assert response.url.startswith(base + "/"), "form left the local app"
            assert response.url != base + path, "form submission was rejected"
            return response.read()

    dashboard = submit("/login", {"username": "ci-admin", "password": os.environ["CI_ADMIN_PASSWORD"]})
    assert b"Account settings for ci-admin" in dashboard
    if sys.argv[1] == "create":
        submit("/sources/new", {
            "name": "Synthetic key relocation", "provider": "twilio",
            "twilio_account_sid": "AC" + "1" * 32,
            "twilio_auth_token": "synthetic-key-relocation-secret",
            "twilio_to_number": "+15550100123",
        })
    # Read-only synthetic DB inspection establishes there really is encrypted
    # state. No provider health/pairing/booking request is ever sent.
    with sqlite3.connect("file:/appdata/lake-pass-bot.db?mode=ro", uri=True) as database:
        rows = database.execute("SELECT id, config_ciphertext FROM otp_sources").fetchall()
    assert len(rows) == 1 and rows[0][1]
    assert "synthetic-key-relocation-secret" not in str(rows[0][1])
    source_id = rows[0][0]
    # The edit path decrypts the stored config and retains its write-only token.
    # Supplying no credential prevents a replacement from masking a wrong key.
    submit(f"/sources/{source_id}", {
        "name": "Synthetic key relocation verified", "provider": "twilio",
    })
    print("Encrypted source retained and updated without replacing its credential.")


if __name__ == "__main__":
    main()
