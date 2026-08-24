# Buntzen Bot 0.2

Buntzen Bot is a personal, trusted-LAN control plane for Buntzen Lake parking passes on Yodel. One Go 1.27 process owns the admin UI, SQLite, scheduling, job state, encryption, concurrency, and OTP providers. A fresh Python 3.12 process performs the single allowlisted Playwright/Yodel action for each job.

This release intentionally has no legacy FastAPI/Selenium path and no second CLI configuration system.

## What is implemented

- Embedded Go templates, local HTMX/JavaScript/CSS, account login, first-run administrator setup, member management, CSRF, and Origin/Host checks.
- Clean versioned SQLite schema with durable queued jobs and encrypted provider/Yodel fields.
- Restart recovery: running and approval jobs become `interrupted`; a run past the final-click boundary becomes `outcome_unknown` and is never retried automatically.
- Profiles, exclusive OTP sources, separate booking requests, fixed pass order (all-day → afternoon → morning), target-minus-one-day releases, session warming, dry-run, manual approval, and automatic confirmation.
- BlueBubbles and Twilio are explicit alternatives. There is no fallback and no outbound messaging.
- Live SSE state. OTPs and supervised-pairing candidates are held only in memory and cleared after use/expiry.
- One versioned, bounded JSONL Go ↔ Python protocol with just-in-time credentials and OTPs.
- One Linux `amd64` image pinned to Python 3.12-compatible Playwright `1.62.0` and its matching Noble browser image.

## Security boundary

This is a trusted-LAN application, not an Internet-facing service. HTTP traffic—including the temporary OTP shown in the UI—is plaintext on the LAN.

The first account created at `/setup` is the sole permanent administrator. It cannot be deleted, disabled, demoted, or replaced. First-run setup also requires a random host-generated token, and the HTTP server rejects browser authorities not listed in `BUNTZEN_ALLOWED_HOSTS`; together these prevent a rebound or untrusted hostname from claiming an empty installation. The administrator can create, disable, rename, reset passwords for, and permanently delete regular member accounts. Deletion requires the member to be disabled, have no active jobs, and have their exact username confirmed; it transactionally removes all of that member's database state and releases globally exclusive resources. Managed browser and artifact files are reconciled immediately, with periodic maintenance as the retry path. Passwords use Argon2id. Sessions are associated with individual users and are `HttpOnly`, `SameSite=Strict`, rate-limited, CSRF-protected, and paired with Origin/Host checks. Every HTML/SSE response is `no-store`, and HTMX history snapshots are disabled.

Each member owns and can access only their OTP sources, Yodel profiles, booking requests, jobs, and job events. Physical OTP inbox identities and active profile/source leases remain globally exclusive so two users cannot accidentally drive the same inbox or browser identity concurrently. Persistent browser directories are assigned from immutable profile IDs and carry owner-bound markers; members cannot choose or reuse filesystem paths. Because the allowlisted BlueBubbles API does not expose a stable, scoped server UUID, this release conservatively allows one BlueBubbles Messages source per control plane; DNS, IP, port, or proxy aliases cannot be registered as separate inboxes. Twilio identities remain exclusive by account and receiving number.

Durable member-created state is bounded per account: 24 OTP sources, 16 profiles, 64 booking requests, 8 pending jobs, 200 retained jobs, 256 events per retained job, and 8 live sessions. The control plane rotates ordinary terminal history while preserving active work, `outcome_unknown`, and unexpired scheduler deduplication records. Book commands remain durably queued until their preparation window (at most 180 minutes before release) instead of occupying a browser early. Retained diagnostics have a 64-file/64-MiB per-job ceiling enforced by periodic monitoring and cleanup; this is not a hard filesystem write quota, so transient overshoot remains possible. Trace segments rotate through eight fixed names, and tracing is closed during both release and manual-approval waits. Hourly maintenance removes expired sessions, rotated-job artifacts, and marked browser-profile directories no longer referenced by a profile; unmarked operator directories are left alone.

On first launch, the app creates `/appdata/master.key` and encrypts Yodel, BlueBubbles, and Twilio credentials in SQLite. This is convenience encryption: copying all of `/appdata` copies both the ciphertext and key.

Yodel credentials and OTPs may be entered only on the exact HTTPS origins in the host-controlled `BUNTZEN_YODEL_ORIGINS` list (default: `https://yodelportal.com`). Booking records can select paths but cannot expand that origin boundary. Go validates the persisted URLs before decrypting credentials, and Python independently blocks cross-origin top-level navigation and checks the final page origin before requesting or filling secrets.

BlueBubbles exposes one unscoped server password. Buntzen Bot enforces query-only behavior in its adapter and tests: authenticated `GET /api/v1/ping` and bounded `POST /api/v1/message/query` are the only allowed operations. The client disables redirects and environment proxies, caps responses, and redacts authenticated URLs. Changing a source to a different canonical BlueBubbles server requires re-entering its write-only password, so a retained credential is never sent to a replacement host. Twilio only polls inbound messages.

## Portainer / Docker deployment

The supplied stack maps host port `8080` to the app’s port `8080`. Set `BLUEBUBBLES_URL` to the LAN URL of your BlueBubbles server before deployment; the repository examples use reserved `.example` hostnames and contain no private network addresses.

1. Copy `.env.example` to `.env`, set the BlueBubbles LAN URL, and replace `BUNTZEN_ALLOWED_HOSTS` with the exact authorities users will open (including the external port, plus any local reverse-proxy hostname). Account credentials do not belong in this file. You may optionally set a random one-time `BUNTZEN_SETUP_TOKEN`; otherwise the service generates one at first startup.
2. Create the bind-mounted data directory and make it writable by the image’s non-root `pwuser` (UID/GID 1001):

   ```bash
   mkdir -p appdata
   sudo chown -R 1001:1001 appdata
   ```

3. Build and start with schedules disabled:

   ```bash
   docker compose up -d --build
   ```

4. Read the one-time setup token from `docker compose logs buntzen-pass-bot` unless you supplied it, then open `http://<docker-host>:8080`. An empty installation redirects to `/setup`, where you enter that token and choose the permanent administrator username and a password of at least 12 characters. Once an administrator exists, the setup token is ignored and repeated setup requests are rejected before password hashing. Later visits use the normal username-and-password login. A local reverse proxy may expose a dedicated
   origin such as `http://buntzen.example`. It works automatically when the proxy
   preserves `Host`. If the proxy rewrites `Host`, add the rewritten authority to
   `BUNTZEN_ALLOWED_HOSTS` and set
   `BUNTZEN_ALLOWED_ORIGINS=http://buntzen.example`. Allowed origins are also accepted as Host authorities. Both settings are exact trusted lists; do not use `*`.
5. From **Users**, the administrator may create regular member accounts and later disable, rename, reset, or delete them. A reset requires the member to choose a new password at the next login. Deletion is permanent and becomes available only after the account is disabled and active jobs have finished cancellation.

The Compose stack runs as a non-root user, enables Docker’s init shim, allocates 1 GiB shared memory, and installs the official Playwright seccomp profile additions required for the Chromium sandbox. The Go service supervises each Python/Chromium process group and gets a 45-second shutdown grace period.

Persistent layout:

```text
/appdata/buntzen.db          SQLite state
/appdata/master.key          local encryption key
/appdata/profiles/profile-*/ persistent Linux browser identities assigned by the app
/appdata/artifacts/job-*/    post-authentication diagnostics only
```

Do not share this browser-profile directory with native macOS, and never run the same Yodel identity from Docker and native mode at the same time.

## First onboarding and rollout

Leave `SCHEDULES_ENABLED=false` while onboarding.

1. Create a BlueBubbles OTP source with its LAN server URL (for example, `http://bluebubbles.example:1234`) and server password. Use **Test connection**; this performs only the authenticated ping.
2. Create a Yodel profile and assign that source exclusively. The app assigns its private persistent browser identity automatically.
3. Create an enabled booking request. This supplies the Yodel URLs used by auth checks as well as bookings.
4. Choose **Pair with Yodel** on the BlueBubbles source. The bot snapshots the inbox before Python triggers a code, shows only new bounded candidates with a temporary code and masked sender, and waits for your selection. Only the selected chat/sender/service fingerprint is saved.
5. Run **Auth check**, then **Dry run** from the booking card.
6. Queue a manual booking and test both approval and cancellation. There is no application approval timeout; a lost browser/session or restart ends the wait without confirming.
7. Verify one explicitly initiated automatic booking before enabling unattended schedules.

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

`BUNTZEN_ADMIN_PASSWORD` is read only by the explicit `admin-password reset` recovery command. `serve` never creates or overwrites an account from the environment. Recovery locates the permanent administrator by role, so it still works if that username was changed, and it revokes that administrator's existing sessions. For a Compose deployment, run the recovery command with a one-off environment value rather than storing it in the stack:

```bash
docker compose exec -e BUNTZEN_ADMIN_PASSWORD='new-long-password' buntzen-pass-bot buntzen admin-password reset
```

Runtime commands enqueue a durable job and wait for its terminal state. If `serve` already owns `/appdata`, it executes the job. Otherwise the command temporarily owns the control plane and runs the workers itself. A standalone manual booking is rejected because there is no web UI to approve it.

`doctor` checks SQLite, negotiates the Python protocol, and runs each configured provider’s read-only health operation. It never requests an OTP or sends a message.

Important environment settings:

| Variable | Default | Meaning |
|---|---:|---|
| `APPDATA_DIR` | `./appdata` | Database, key, profiles, and artifacts |
| `BUNTZEN_LISTEN` | `:8080` | HTTP listen address |
| `BUNTZEN_ADMIN_PASSWORD` | empty | Explicit `admin-password reset` command only; ignored and removed by `serve` |
| `BLUEBUBBLES_URL` | `http://127.0.0.1:1234` | New-source form default; set the Compose value to the server's LAN URL |
| `BUNTZEN_YODEL_ORIGINS` | `https://yodelportal.com` | Host-controlled comma-separated exact HTTPS origins allowed to receive Yodel credentials; override only for a trusted test site |
| `BUNTZEN_ALLOWED_HOSTS` | loopback only | Comma-separated exact browser Host authorities; required for LAN IP/port and reverse-proxy access |
| `BUNTZEN_ALLOWED_ORIGINS` | empty | Comma-separated trusted browser origins for a reverse proxy that rewrites `Host` |
| `BUNTZEN_SETUP_TOKEN` | generated on an empty database | Optional one-time first-run setup token; ignored after the administrator exists |
| `MAX_CONCURRENT_JOBS` | `2` | Worker limit (1–8) |
| `SCHEDULES_ENABLED` | `false` | Global schedule gate |
| `BUNTZEN_PYTHON` | `python3` | Python 3.12 executable |
| `BUNTZEN_ACTIONS_MODULE` | `buntzen_actions` | Fixed action worker module |

Only one control plane can own an appdata directory; an advisory lock prevents a second `serve` from interrupting the first.

## Verification

```bash
gofmt -w cmd internal integration
go vet ./...
go test -race ./...

uvx --from ruff==0.12.10 ruff check actions scripts/deploy/tests
uv run --project actions python -m unittest discover -s actions/tests
uv run --project actions python -m compileall -q actions/src
bash scripts/deploy/tests/test_portainer.sh
BUNTZEN_E2E_PYTHON="$PWD/actions/.venv/bin/python" go test -race -tags=integration ./integration/...
```

CI also runs the real Go/Python/Playwright/BlueBubbles OTP integration, the Portainer rollback suite, a Linux `amd64` container setup/login/restart smoke test, and the strict Trivy image gate. Dependabot covers Go modules, Python, Docker, and GitHub Actions.

See [`actions/README.md`](actions/README.md) for the exact protocol and sensitive-artifact rules.
See [`docs/release-and-deployment.md`](docs/release-and-deployment.md) for the GHCR release gate and protected in-place Portainer workflow.
