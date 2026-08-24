# Buntzen Bot 0.2

Buntzen Bot is a personal, trusted-LAN control plane for Buntzen Lake parking passes on Yodel. One Go 1.27 process owns the admin UI, SQLite, scheduling, job state, encryption, concurrency, and OTP providers. A fresh Python 3.12 process performs the single allowlisted Playwright/Yodel action for each job.

This release intentionally has no legacy FastAPI/Selenium path and no second CLI configuration system.

## What is implemented

- Embedded Go templates, local HTMX/JavaScript/CSS, admin login, CSRF and Origin/Host checks.
- Clean versioned SQLite schema with durable queued jobs and encrypted provider/Yodel fields.
- Restart recovery: running and approval jobs become `interrupted`; a run past the final-click boundary becomes `outcome_unknown` and is never retried automatically.
- Profiles, exclusive OTP sources, separate booking requests, fixed pass order (all-day → afternoon → morning), target-minus-one-day releases, session warming, dry-run, manual approval, and automatic confirmation.
- BlueBubbles and Twilio are explicit alternatives. There is no fallback and no outbound messaging.
- Live SSE state. OTPs and supervised-pairing candidates are held only in memory and cleared after use/expiry.
- One versioned, bounded JSONL Go ↔ Python protocol with just-in-time credentials and OTPs.
- One Linux `amd64` image pinned to Python 3.12-compatible Playwright `1.62.0` and its matching Noble browser image.

## Security boundary

This is a trusted-LAN application, not an Internet-facing service. HTTP traffic—including the temporary OTP shown in the UI—is plaintext on the LAN.

The built-in administrator password uses Argon2id. Sessions are `HttpOnly`, `SameSite=Strict`, rate-limited, CSRF-protected, and paired with Origin/Host checks. Every HTML/SSE response is `no-store`, and HTMX history snapshots are disabled.

On first launch, the app creates `/appdata/master.key` and encrypts Yodel, BlueBubbles, and Twilio credentials in SQLite. This is convenience encryption: copying all of `/appdata` copies both the ciphertext and key.

BlueBubbles exposes one unscoped server password. Buntzen Bot enforces query-only behavior in its adapter and tests: authenticated `GET /api/v1/ping` and bounded `POST /api/v1/message/query` are the only allowed operations. The client disables redirects and environment proxies, caps responses, and redacts authenticated URLs. Twilio only polls inbound messages.

## Portainer / Docker deployment

The supplied stack maps host port `8080` to the app’s port `8080`. Set `BLUEBUBBLES_URL` to the LAN URL of your BlueBubbles server before deployment; the repository examples use reserved `.example` hostnames and contain no private network addresses.

1. Copy `.env.example` to `.env` and set a unique initial password of at least 12 characters.
2. Create the bind-mounted data directory and make it writable by the image’s non-root `pwuser` (UID/GID 1001):

   ```bash
   mkdir -p appdata
   sudo chown -R 1001:1001 appdata
   ```

3. Build and start with schedules disabled:

   ```bash
   docker compose up -d --build
   ```

4. Open `http://<docker-host>:8080`. A local reverse proxy may expose a dedicated
   origin such as `http://buntzen.example`. It works automatically when the proxy
   preserves `Host`; if the proxy rewrites `Host`, set
   `BUNTZEN_ALLOWED_ORIGINS=http://buntzen.example`. This is an exact trusted-origin
   list; do not use `*`.
5. Once the administrator exists, `BUNTZEN_ADMIN_PASSWORD` may be removed from `.env`; the stored hash is authoritative.

The Compose stack runs as a non-root user, enables Docker’s init shim, allocates 1 GiB shared memory, and installs the official Playwright seccomp profile additions required for the Chromium sandbox. The Go service supervises each Python/Chromium process group and gets a 45-second shutdown grace period.

Persistent layout:

```text
/appdata/buntzen.db          SQLite state
/appdata/master.key          local encryption key
/appdata/profiles/<slug>/    persistent Linux browser identity
/appdata/artifacts/job-*/    post-authentication diagnostics only
```

Do not share this browser-profile directory with native macOS, and never run the same Yodel identity from Docker and native mode at the same time.

## First onboarding and rollout

Leave `SCHEDULES_ENABLED=false` while onboarding.

1. Create a BlueBubbles OTP source with its LAN server URL (for example, `http://bluebubbles.example:1234`) and server password. Use **Test connection**; this performs only the authenticated ping.
2. Create a Yodel profile and assign that source exclusively. Use a lowercase browser-profile slug such as `home`.
3. Create an enabled booking request. This supplies the Yodel URLs used by auth checks as well as bookings.
4. Choose **Pair with Yodel** on the BlueBubbles source. The bot snapshots the inbox before Python triggers a code, shows only new bounded candidates with a temporary code and masked sender, and waits for your selection. Only the selected chat/sender/service fingerprint is saved.
5. Run **Auth check**, then **Dry run** from the booking card.
6. Queue a manual booking and test both approval and cancellation. There is no application approval timeout; a lost browser/session or restart ends the wait without confirming.
7. Verify one automatic canary before setting `SCHEDULES_ENABLED=true` and restarting the container.

BlueBubbles can read the OTP only if the SMS reaches Messages on the Mac through Apple’s Messages/iCloud or text-message-forwarding setup. Keep the Mac awake and its configured network adapter connected.

Before relying on unattended schedules, perform an approved Mac reboot/login test and verify that BlueBubbles actually restarts and answers the authenticated ping. Automatic login alone is not sufficient if the BlueBubbles LaunchAgent/app startup is unhealthy.

## Native Intel macOS development

Native mode uses loopback BlueBubbles and a separate appdata directory.

```bash
brew install go uv
uv sync --project actions --locked --python 3.12

export APPDATA_DIR="$PWD/.native-appdata"
export BUNTZEN_PYTHON="$PWD/actions/.venv/bin/python"
export BLUEBUBBLES_URL="http://127.0.0.1:1234"
export BUNTZEN_ADMIN_PASSWORD="replace-with-at-least-12-characters"
export SCHEDULES_ENABLED=false

go run ./cmd/buntzen serve
```

Open `http://127.0.0.1:8080`. In a native profile, use browser channel `chrome`; in Docker, leave the channel/executable fields blank to use bundled Chromium.

## Commands

All commands use the same database and services:

```bash
buntzen serve
buntzen migrate
buntzen doctor
buntzen auth-check --booking 1
buntzen dry-run --booking 1
buntzen book --booking 1 --mode auto
BUNTZEN_ADMIN_PASSWORD='new-long-password' buntzen admin-password reset
```

Runtime commands enqueue a durable job and wait for its terminal state. If `serve` already owns `/appdata`, it executes the job. Otherwise the command temporarily owns the control plane and runs the workers itself. A standalone manual booking is rejected because there is no web UI to approve it.

`doctor` checks SQLite, negotiates the Python protocol, and runs each configured provider’s read-only health operation. It never requests an OTP or sends a message.

Important environment settings:

| Variable | Default | Meaning |
|---|---:|---|
| `APPDATA_DIR` | `./appdata` | Database, key, profiles, and artifacts |
| `BUNTZEN_LISTEN` | `:8080` | HTTP listen address |
| `BUNTZEN_ADMIN_USERNAME` | `admin` | Built-in administrator name |
| `BUNTZEN_ADMIN_PASSWORD` | empty | First-launch/reset password only |
| `BLUEBUBBLES_URL` | `http://127.0.0.1:1234` | New-source form default; set the Compose value to the server's LAN URL |
| `BUNTZEN_ALLOWED_ORIGINS` | empty | Comma-separated trusted browser origins for a reverse proxy that rewrites `Host` |
| `MAX_CONCURRENT_JOBS` | `2` | Worker limit (1–8) |
| `SCHEDULES_ENABLED` | `false` | Global schedule gate |
| `BUNTZEN_PYTHON` | `python3` | Python 3.12 executable |
| `BUNTZEN_ACTIONS_MODULE` | `buntzen_actions` | Fixed action worker module |

Only one control plane can own an appdata directory; an advisory lock prevents a second `serve` from interrupting the first.

## Verification

```bash
gofmt -w cmd internal
go vet ./...
go test -race ./...

uvx --from ruff==0.12.10 ruff check actions
uv run --project actions python -m unittest discover -s actions/tests
uv run --project actions python -m compileall -q actions/src
```

CI runs Go formatting/vetting/race tests, the Python protocol/action tests and dependency audit, Compose validation, and a Linux `amd64` image build. Dependabot covers Go modules, Python, Docker, and GitHub Actions.

See [`actions/README.md`](actions/README.md) for the exact protocol and sensitive-artifact rules.
