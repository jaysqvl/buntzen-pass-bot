# Browser integration tests

The browser tests are guarded by the `integration` build tag because they start
the real Python worker and real headless Chromium processes. Together they cross:

1. the Go coordinator and bounded JSON-lines subprocess protocol;
2. the pinned `buntzen_actions` Python package and persistent Playwright context;
3. an ephemeral HTTPS fake Yodel login and OTP form; and
4. the real query-only BlueBubbles adapter backed by a bounded fake API.

All credentials and OTPs are synthetic. The OTP test verifies that BlueBubbles is
armed before the browser triggers MFA, that Chromium submits the resulting OTP,
that transient hub state is cleared, and that logs, durable event messages, and
post-authentication Playwright artifacts do not retain the synthetic secrets.
The booking test starts from a synthetic authenticated session and covers date,
pass, and vehicle selection plus dry-run, manual approve, manual cancel, and
automatic final confirmation. It verifies the manual decision barrier and that
an already-authenticated booking never touches BlueBubbles.

Run it from the repository root after syncing the locked Python environment and
installing the pinned Playwright browser:

```sh
uv sync --project actions --locked
uv run --project actions playwright install chromium
go test -race -tags=integration ./integration -count=1
```

The tests use `actions/.venv/bin/python` when available and otherwise invoke
`uv`. Set `BUNTZEN_E2E_PYTHON` or `BUNTZEN_E2E_BROWSER_EXECUTABLE` to explicit
absolute executables when a runner uses a non-standard layout.
