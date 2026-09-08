# Buntzen Bot

Buntzen Bot is a self-hosted control plane for booking Buntzen Lake parking passes through Yodel. A Go service provides the web UI, scheduling, job state, and encrypted storage; separate supervised Python/Playwright processes perform the browser actions.

> [!WARNING]
> The default private HTTP mode sends traffic, including temporary OTPs, without encryption. Before exposing the app through an HTTPS tunnel, complete setup privately and configure [public HTTPS mode](docs/public-exposure.md). The app uses its own accounts; Cloudflare Access is optional. See [Security](SECURITY.md) for account boundaries and remaining runtime trust.

## Features

- Scheduled and on-demand bookings with dry-run, manual approval, and automatic confirmation modes.
- Configurable pass priority and an immediate, manually approved checkout for passes already released.
- Administrator and member accounts with isolated OTP sources, Yodel profiles, requests, and job history. Change your own username or password from Account.
- Read-only inbound OTP retrieval through either BlueBubbles or Twilio, with no provider fallback or outbound messaging.
- Durable jobs, restart recovery, and an `outcome_unknown` state that prevents unsafe retries after an ambiguous confirmation.

Only one booking attempt may reserve a Yodel profile and visit date, across
manual and scheduled runs. Success and unknown outcomes keep that reservation
even when job history is pruned. A cancellation or failure before confirmation
allows another attempt. Inspect the Yodel wallet after an unknown outcome;
creating another request or changing confirmation mode will not bypass the guard.

## Quick start with Docker Compose

1. Create the local configuration:

   ```bash
   cp .env.example .env
   ```

   Edit `.env` and:

   - set `BUNTZEN_ALLOWED_HOSTS` to the exact host and port users will open, such as `buntzen.example:8080`;
   - if using BlueBubbles, set `BLUEBUBBLES_URL` and approve its origin/network with `BUNTZEN_BLUEBUBBLES_ENDPOINTS` as described in [provider access](docs/public-exposure.md#outbound-provider-access); and
   - leave `SCHEDULES_ENABLED=false` until onboarding is complete.

   In private mode, if a reverse proxy rewrites the `Host` header, add the rewritten authority to `BUNTZEN_ALLOWED_HOSTS` and the browser-facing origin to `BUNTZEN_ALLOWED_ORIGINS`. These are exact allowlists; do not use `*`. Public mode instead requires its configured public Host and trusted connector settings from the linked guide.

2. Create the persistent data directory for the container's non-root user:

   ```bash
   mkdir -p appdata
   sudo chown -R 1001:1001 appdata
   ```

3. Build and start the service:

   ```bash
   docker compose up -d --build
   ```

4. If you did not set `BUNTZEN_SETUP_TOKEN`, read the generated one-time token from the startup log:

   ```bash
   docker compose logs buntzen-pass-bot
   ```

5. Open `http://<docker-host>:8080`, enter the setup token, and create the permanent administrator account. Passwords must be at least 12 characters.

Treat `appdata` as sensitive: it contains the database and browser profiles. The default encryption key is beside the database, so copying the whole directory also copies its decryption key. For a separate read-only key mount and matching backup/recovery procedure, see [key storage](docs/public-exposure.md#key-storage-and-recovery). Only one Buntzen instance may use an appdata directory.

## Portainer installs and updates

Use [deploy/portainer.yml](deploy/portainer.yml) for the published image. It defaults
to `ghcr.io/jaysqvl/buntzen-pass-bot:latest`, which advances after a stable release
passes the build, browser smoke test, vulnerability scan, and signature checks.
GitHub publishes the image; you choose when to update the existing stack in
Portainer with **Update the stack** and **Re-pull image and redeploy** enabled.
The app footer shows the version and build actually running.

See [Release and Portainer deployment](docs/release-and-deployment.md) for the
stack settings, upgrade notes, and optional version pinning. No GitHub deployment
runner or Portainer API key is needed.

## Set up and test a booking

Keep `SCHEDULES_ENABLED=false` while completing these steps:

1. Create an OTP source. For BlueBubbles, enter its operator-approved server URL and password, then use **Test connection**.
2. Create an enabled Yodel profile with its login URL, 10-digit Canadian or US mobile number, vehicle, and linked OTP source.
3. For BlueBubbles, return to the OTP source and choose **Pair with Yodel**. Select the fresh OTP candidate after Yodel sends a code. Pairing uses the linked profile and does not require a booking request.
4. Open **Bookings** and create an enabled request with a visit date. Choose up to three pass priorities: All-day, Afternoon, Morning, or None. The bot tries them in your saved order; select at least one pass without duplicates.
5. Run **Auth check**, then **Dry run**, from the booking card. Neither proves a pass can be issued.
6. For already released passes, choose **Book now · manual approval**. Approve only the intended reservation, then verify the issued pass in Yodel. See [Testing a live booking](docs/live-testing.md) for timing, expiry, cancellation and retry behavior.
7. Test **Queue for release** separately before relying on release timing or automatic confirmation. Verify the OTP provider still works after its host restarts before enabling unattended schedules.

The **Setup** tab opens Home with OTP sources first and profiles second,
with their links and setup order. Booking dates, pass URLs and priorities stay on the separate Bookings
page; the login URL belongs to the profile.

Before a booking, the Yodel cart must be empty. The bot checks that adding the
selected pass produces exactly one item of quantity one, then rechecks it before
confirmation. Cancelling a manual test can leave that item in Yodel's cart;
inspect and clear it in Yodel before starting the next booking test.

BlueBubbles can retrieve an OTP only when the SMS reaches Messages on its Mac through Messages in iCloud or text-message forwarding. Keep that Mac awake and connected to the network.

## Native macOS development

Native development requires Go 1.27, Python 3.12, `uv`, and a local BlueBubbles server:

```bash
brew install go uv
uv sync --project actions --locked --python 3.12

export APPDATA_DIR="$PWD/.native-appdata"
export BUNTZEN_PYTHON="$PWD/actions/.venv/bin/python"
export BLUEBUBBLES_URL="http://127.0.0.1:1234"
export BUNTZEN_BLUEBUBBLES_ENDPOINTS='[{"origin":"http://127.0.0.1:1234","networks":["127.0.0.1/32"]}]'
export SCHEDULES_ENABLED=false

go run ./cmd/buntzen serve
```

Open `http://127.0.0.1:8080`. Select `chrome` in a native Yodel profile, or bundled Chromium in Docker. If Chrome is installed elsewhere, the operator can set `BUNTZEN_BROWSER_EXECUTABLE` to its absolute executable path; this overrides channel choices for every worker. Members cannot supply executable paths. Edit and save any older profile with a path override to clear it before running jobs.

Do not share browser profiles between Docker and macOS or run the same Yodel identity from both at once.

## Common commands

Run CLI commands against the same appdata used by the service. In Docker Compose:

```bash
docker compose exec buntzen-pass-bot buntzen doctor
docker compose exec buntzen-pass-bot buntzen auth-check --booking 1
docker compose exec buntzen-pass-bot buntzen dry-run --booking 1
docker compose exec buntzen-pass-bot buntzen book --booking 1 --mode auto
```

Reset the permanent administrator's password without storing it in `.env`:

```bash
docker compose exec \
  -e BUNTZEN_ADMIN_PASSWORD='new-long-password' \
  buntzen-pass-bot buntzen admin-password reset
```

For live logs, use `docker compose logs --follow --tail=300 buntzen-pass-bot`. Set `BUNTZEN_DEBUG=true` in `.env` and recreate the container only while diagnosing a problem; return it to `false` afterward.

## Tests

```bash
go vet ./...
go test -race ./...
uvx --from ruff==0.12.10 ruff check actions scripts/release
uv run --project actions --locked python -m unittest discover -s actions/tests
```

See [Browser integration tests](integration/README.md) for the real Go/Python/Playwright test command.

## Documentation

- [Development guidelines and test expectations](CONTRIBUTING.md)
- [Security scope and invariants](SECURITY.md)
- [Python action protocol and artifact rules](actions/README.md)
- [Browser integration tests](integration/README.md)
- [Testing a live booking](docs/live-testing.md)
- [Release and Portainer deployment](docs/release-and-deployment.md)
- [Changelog](CHANGELOG.md)
