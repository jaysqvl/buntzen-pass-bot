# Buntzen Bot

Buntzen Bot is a self-hosted control plane for booking Buntzen Lake parking passes through Yodel. A Go service provides the web UI, scheduling, job state, and encrypted storage; isolated Python/Playwright workers perform the browser actions.

> [!WARNING]
> This application is for a trusted private LAN. Its HTTP traffic, including temporary OTPs shown in the UI, is not encrypted. Do not expose it to the public Internet. See [Security](SECURITY.md) for the full trust model.

## Features

- Scheduled and on-demand bookings with dry-run, manual approval, and automatic confirmation modes.
- Administrator and member accounts with isolated OTP sources, Yodel profiles, requests, and job history.
- Read-only inbound OTP retrieval through either BlueBubbles or Twilio, with no provider fallback or outbound messaging.
- Durable jobs, restart recovery, and an `outcome_unknown` state that prevents unsafe retries after an ambiguous confirmation.

## Quick start with Docker Compose

1. Create the local configuration:

   ```bash
   cp .env.example .env
   ```

   Edit `.env` and:

   - set `BUNTZEN_ALLOWED_HOSTS` to the exact host and port users will open, such as `192.168.1.20:8080`;
   - replace `BLUEBUBBLES_URL` with the server's LAN URL if you use BlueBubbles; and
   - leave `SCHEDULES_ENABLED=false` until onboarding is complete.

   If a reverse proxy rewrites the `Host` header, add the rewritten authority to `BUNTZEN_ALLOWED_HOSTS` and the browser-facing origin to `BUNTZEN_ALLOWED_ORIGINS`. These are exact allowlists; do not use `*`.

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

Treat `appdata` as sensitive and back it up as a unit: it contains the database, encryption key, browser profiles, and diagnostics. The key is stored beside the encrypted data, so a copied directory contains both. Only one Buntzen instance may use an appdata directory.

## Set up and test a booking

Keep `SCHEDULES_ENABLED=false` while completing these steps:

1. Create an OTP source. For BlueBubbles, enter its LAN URL and server password, then use **Test connection**.
2. Create a Yodel profile with its 10-digit Canadian or US mobile number and assign the OTP source.
3. Create and enable a booking request.
4. For BlueBubbles, choose **Pair with Yodel** and select the new OTP candidate after Yodel sends a code.
5. Run **Auth check**, then **Dry run**, from the booking card.
6. Test a manual booking, including approval and cancellation, then explicitly test one automatic booking.
7. Verify the OTP provider still works after its host restarts before enabling unattended schedules.

BlueBubbles can retrieve an OTP only when the SMS reaches Messages on its Mac through Messages in iCloud or text-message forwarding. Keep that Mac awake and connected to the network.

## Native macOS development

Native development requires Go 1.27, Python 3.12, `uv`, and a local BlueBubbles server:

```bash
brew install go uv
uv sync --project actions --locked --python 3.12

export APPDATA_DIR="$PWD/.native-appdata"
export BUNTZEN_PYTHON="$PWD/actions/.venv/bin/python"
export BLUEBUBBLES_URL="http://127.0.0.1:1234"
export SCHEDULES_ENABLED=false

go run ./cmd/buntzen serve
```

Open `http://127.0.0.1:8080`. Set a native Yodel profile's browser channel to `chrome`; leave the channel and executable empty in Docker to use the bundled Chromium.

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
uvx --from ruff==0.12.10 ruff check actions scripts/deploy/tests
uv run --project actions --locked python -m unittest discover -s actions/tests
```

See [Browser integration tests](integration/README.md) for the real Go/Python/Playwright test command.

## Documentation

- [Security scope and invariants](SECURITY.md)
- [Python action protocol and artifact rules](actions/README.md)
- [Browser integration tests](integration/README.md)
- [Release and Portainer deployment](docs/release-and-deployment.md)
- [Changelog](CHANGELOG.md)
