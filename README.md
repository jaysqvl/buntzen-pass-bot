# Lake Pass Bot

Lake Pass Bot is a self-hosted app for planning and booking lake passes. Choose a supported lake, connect its booking provider, and manage requests from one place. A Go service provides the web UI, scheduling, job state, and encrypted storage; separate supervised Python/Playwright processes perform the browser actions.

> [!WARNING]
> The default private HTTP mode sends traffic, including temporary OTPs, without encryption. Before exposing the app through an HTTPS tunnel, complete setup privately and configure [public HTTPS mode](docs/public-exposure.md). The app uses its own accounts; Cloudflare Access is optional. See [Security](SECURITY.md) for account boundaries and remaining runtime trust.

## Features

- Scheduled and on-demand bookings with dry-run, manual approval, and automatic confirmation modes.
- Configurable pass priority and an immediate, manually approved checkout for passes already released.
- Administrator and member accounts with isolated Yodel sign-ins, OTP sources, personal defaults, requests, and job history. Change your own username or password from Account.
- Lake connection status on Home, provider sign-in within each lake, an independent OTP sources page, and general defaults in Settings.
- Read-only inbound OTP retrieval through either BlueBubbles or Twilio, with no provider fallback or outbound messaging.
- Durable jobs, restart recovery, and an `outcome_unknown` state that prevents unsafe retries after an ambiguous confirmation.

Only one booking attempt may reserve a Yodel sign-in and visit date, across
manual and scheduled runs. Success and unknown outcomes keep that reservation
even when job history is pruned. A cancellation or failure before confirmation
allows another attempt. Inspect the Yodel wallet after an unknown outcome;
creating another request or changing confirmation mode will not bypass the guard.

## Supported lakes

| Lake | Provider | Support |
| --- | --- | --- |
| Buntzen Lake | Yodel | Parking passes, authentication, dry runs, and bookings |

The destination catalog supplies each lake's supported passes, URLs, and release
defaults. **Lakes** lets each account choose its vehicle keyword and customize
the lake's booking rules. Each lake owns its provider connection and booking preferences.
Provider browser behavior is kept in its adapter.
See [lake settings and provider extension](docs/lakes.md).
Only the lake listed above is currently supported.

## Quick start with Docker Compose

Build this checkout locally with the steps below. The renamed GHCR image is a
publication target; it has not been published by this rebrand. Existing installs
should read the [rebrand migration notes](docs/rebrand-migration.md) first.

1. Create the local configuration:

   ```bash
   cp .env.example .env
   ```

   Edit `.env` and:

   - set `LAKE_PASS_ALLOWED_HOSTS` to the exact host and port users will open, such as `lake-pass.example:8080`;
   - if using BlueBubbles, set `BLUEBUBBLES_URL` and approve its origin/network with `LAKE_PASS_BLUEBUBBLES_ENDPOINTS` as described in [provider access](docs/public-exposure.md#outbound-provider-access); and
   - leave `SCHEDULES_ENABLED=false` until onboarding is complete.

   In private mode, if a reverse proxy rewrites the `Host` header, add the rewritten authority to `LAKE_PASS_ALLOWED_HOSTS` and the browser-facing origin to `LAKE_PASS_ALLOWED_ORIGINS`. These are exact allowlists; do not use `*`. Public mode instead requires its configured public Host and trusted connector settings from the linked guide.

2. Create the persistent data directory for the container's non-root user:

   ```bash
   mkdir -p appdata
   sudo chown -R 1001:1001 appdata
   ```

3. Build and start the service:

   ```bash
   docker compose up -d --build
   ```

4. If you did not set `LAKE_PASS_SETUP_TOKEN`, read the generated one-time token from the startup log:

   ```bash
   docker compose logs lake-pass-bot
   ```

5. Open `http://<docker-host>:8080`, enter the setup token, and create the permanent administrator account. Passwords must be at least 12 characters.

Treat `appdata` as sensitive: it contains the database and browser profiles. The default encryption key is beside the database, so copying the whole directory also copies its decryption key. For a separate read-only key mount and matching backup/recovery procedure, see [key storage](docs/public-exposure.md#key-storage-and-recovery). Only one Lake Pass Bot instance may use an appdata directory.

## Portainer installs and updates

[deploy/portainer.yml](deploy/portainer.yml) is prepared for the future
`ghcr.io/jaysqvl/lake-pass-bot:latest` image. Keep your currently working image and
stack until a renamed release has been published and you choose to upgrade.
For local review, use the source-build Compose instructions above.

After publication, GitHub builds and verifies the image; you choose when to
update the existing stack in Portainer. The app footer shows the build actually
running. See [Release and Portainer deployment](docs/release-and-deployment.md)
for stack settings and version pinning, and [rebrand migration](docs/rebrand-migration.md)
for existing data and configuration compatibility.

## Set up and test a booking

Keep `SCHEDULES_ENABLED=false` while completing these steps:

1. Open **Lakes**, choose **Buntzen Lake**, and follow its connection setup. Open **OTP sources** and configure BlueBubbles or Twilio. For BlueBubbles, enter its operator-approved server URL and password, then use **Test connection**. The first source becomes your default; use **Make default** to select another source.
2. Return to **Lakes → Buntzen Lake**, choose **Add Yodel account**, enter a name and the 10-digit Canadian or US mobile number used by Yodel, and save it enabled. Set your preferred browser defaults in **Settings** before adding a sign-in if needed.
3. Choose **Sign in to Yodel** in that lake’s Connection section. With BlueBubbles, select the fresh OTP candidate after Yodel sends a code. This signs in without creating a booking request or reserving a pass.
4. Open **Lakes**, choose a lake, and save its vehicle keyword and booking rules. Then create a request in **Bookings**, choosing the lake, Yodel sign-in, and visit date. Choose up to three pass priorities: All-day, Afternoon, Morning, or None. The bot tries them in your saved order; select at least one pass without duplicates.
5. Run **Sign-in check**, then **Booking rehearsal**, from the booking card. Neither proves a pass can be issued.
6. For already released passes, choose **Book now · manual approval**. Approve only the intended reservation, then verify the issued pass in Yodel. See [Testing a live booking](docs/live-testing.md) for timing, expiry, cancellation and retry behavior.
7. Test **Queue for release** separately before relying on release timing or automatic confirmation. Verify the OTP provider still works after its host restarts before enabling unattended schedules.

**Home** shows lake connection status, upcoming visits, and recent jobs. Accounts without a configured lake connection are directed to **Lakes** to begin setup. **OTP sources** is an independent page for
configuring inbox connections and choosing the account's default source.
**Settings** holds personal preparation and retry timing shared across lakes,
browser defaults, and links to account management.
**Lakes** holds each lake’s provider connection, vehicle keyword, release schedule, pass preferences,
and booking URLs. Sign-in URLs are managed internally. The visit date,
Yodel sign-in, vehicle snapshot, and confirmation choice belong to the request.

New Yodel sign-ins copy the account's browser defaults. New booking requests copy the
selected lake's defaults and the account's preparation and retry timing. Each
request can override those values for that visit. Changing defaults
does not change existing sign-ins, requests, or queued jobs. Resetting lake
defaults removes only that account's saved lake overrides. Newly queued jobs
capture the selected default OTP source; changing that default does not reroute
already queued jobs. Multiple Yodel sign-ins can use one owned source, with
browser and inbox locks preventing concurrent use of the same resources.

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
export LAKE_PASS_PYTHON="$PWD/actions/.venv/bin/python"
export BLUEBUBBLES_URL="http://127.0.0.1:1234"
export LAKE_PASS_BLUEBUBBLES_ENDPOINTS='[{"origin":"http://127.0.0.1:1234","networks":["127.0.0.1/32"]}]'
export SCHEDULES_ENABLED=false

go run ./cmd/lake-pass-bot serve
```

Open `http://127.0.0.1:8080`. Select `chrome` in **Settings** for new native Yodel sign-ins, or bundled Chromium in Docker. Existing sign-ins keep their saved browser choice. If Chrome is installed elsewhere, the operator can set `LAKE_PASS_BROWSER_EXECUTABLE` to its absolute executable path; this overrides channel choices for every worker. User-supplied executable paths are rejected.

Do not share browser profiles between Docker and macOS or run the same Yodel identity from both at once.

## Common commands

Run CLI commands against the same appdata used by the service. In Docker Compose:

```bash
docker compose exec lake-pass-bot lake-pass-bot doctor
docker compose exec lake-pass-bot lake-pass-bot auth-check --booking 1
docker compose exec lake-pass-bot lake-pass-bot dry-run --booking 1
docker compose exec lake-pass-bot lake-pass-bot book --booking 1 --mode auto
```

Reset the permanent administrator's password without storing it in `.env`:

```bash
docker compose exec \
  -e LAKE_PASS_ADMIN_PASSWORD='new-long-password' \
  lake-pass-bot lake-pass-bot admin-password reset
```

For live logs, use `docker compose logs --follow --tail=300 lake-pass-bot`. Set `LAKE_PASS_DEBUG=true` in `.env` and recreate the container only while diagnosing a problem; return it to `false` afterward.

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
- [Lake settings and provider extension](docs/lakes.md)
- [Rebrand migration and publication status](docs/rebrand-migration.md)
- [Release and Portainer deployment](docs/release-and-deployment.md)
- [Changelog](CHANGELOG.md)
