# Buntzen Pass Bot

Python + Playwright app for unattended Buntzen Lake parking pass booking through Yodel.

The app is designed for personal deployment with Docker/Portainer:

- web UI for multiple saved booking instances
- one persistent browser profile per account/person
- SQLite database in appdata
- job history, logs, screenshots, and Playwright traces in appdata
- Twilio SMS OTP polling for unattended 2FA

Use responsibly and respect the booking site's rules and rate limits. This app focuses on reliable browser automation with persistent profiles, conservative timing, and clear diagnostics.

## Docker Setup

```bash
docker compose up -d --build
```

Open:

```text
http://localhost:8090
```

For Portainer, deploy the included `docker-compose.yml`.

Recommended volume mapping:

```text
./appdata:/appdata
```

Optional environment variables:

- `APPDATA_DIR=/appdata`
- `WEB_PORT=8090`
- `MAX_CONCURRENT_JOBS=2`

Inside `/appdata`, the app stores:

- `buntzen.db`: SQLite database
- `profiles/`: persistent Playwright browser profiles, one per instance
- `artifacts/`: logs, screenshots, HTML captures, and traces

## LAN Workflow

The web UI is the main interface. Anyone on your LAN who can reach the container can create an instance, run validation jobs, and queue booking jobs.

Dashboard files live in:

- `app/templates/`: server-rendered HTML pages and partials
- `app/static/app.css`: dashboard styling

Button behavior:

- `Auth`: opens the saved browser profile and verifies login/OTP.
- `Dry Run`: verifies date, pass, and vehicle selection without checkout.
- `Queue Booking`: starts a job immediately, but the job waits internally until the configured prep/release window.
- `Auto-queue during prep window`: lets the background scheduler create the booking job automatically when the prep window arrives.

## First Test Flow

1. Open the web UI.
2. Create an instance for one account/person.
3. Fill in the target date, vehicle keyword, pass preferences, Twilio details, and Yodel credentials if needed.
4. Run `Auth` for that instance.
5. Run `Dry Run`.
6. Only after both pass, set `Run Mode` to `auto` and use `Queue Booking` or enable `Auto-queue during prep window`.

For unattended use, the Yodel account should use `TWILIO_OTP_NUMBER` as its SMS 2FA number. The app polls Twilio for fresh inbound OTP messages and enters the code in the browser.

The Docker/web-app path is the primary path. Older Selenium helper files remain in the repository as legacy reference code, but the active dependencies and Docker image use Playwright.

## Instance Fields

- `Name`: human-friendly account/person name.
- `Profile Name`: persistent browser profile folder under `/appdata/profiles`.
- `Target Date`: pass date, not release date.
- `Start Time`: release time, normally `07:00`.
- `Run Mode`: `dry-run`, `manual`, or `auto`.
- `Headless`: enabled by default for Docker.
- `Vehicle Keyword`: unique text matching the saved vehicle.
- `All Day`, `Morning`, `Afternoon`: pass preferences. Order is all-day, then afternoon, then morning.
- `Auto-queue during prep window`: queue the booking job automatically during the prep window.

## Local CLI

The old single-instance CLI still works for local debugging.

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
uv sync
cp .env_example .env
uv run python run.py auth-check
uv run python run.py dry-run
uv run python run.py book --mode auto
```

On this Ubuntu/WSL machine, Playwright-managed Chromium is not supported, so local CLI use needs Linux Google Chrome installed and `BROWSER_CHANNEL=chrome`.

In Docker, the Playwright base image provides the browser; leave per-instance `Browser Channel` blank unless you know you need a specific installed browser channel.

## Twilio 2FA

Required for fully unattended fresh login:

- `TWILIO_ACCOUNT_SID`
- `TWILIO_AUTH_TOKEN`
- `TWILIO_OTP_NUMBER`
- `TWILIO_ALERT_TO_NUMBER`

The OTP reader accepts only fresh inbound messages sent after the bot submits login or requests a code. If Yodel will not deliver OTP messages to a Twilio number, the fallback is using an already-authenticated persistent profile.

Twilio is the supported unattended OTP receiver today. An iPhone Shortcuts-based OTP inbox is possible, but it is not implemented yet. That would make your iPhone receive the Yodel SMS, then use a message automation to POST the code into the LAN app for the job to consume.

## Troubleshooting

- `TARGET_DATE is required`: your local `.env` is old; replace/update it from `.env_example`.
- Browser launch fails locally: install Linux Google Chrome and use `BROWSER_CHANNEL=chrome`, or run the Docker app.
- `docker` command not found in WSL: enable Docker Desktop WSL integration for this distro, or build/deploy from Portainer directly.
- OTP never arrives: verify Yodel accepts the Twilio number and Twilio can receive SMS from the sender.
- Dry run cannot find the pass or vehicle: open the job artifacts and inspect the screenshot/HTML; Yodel selectors may need tuning after a real session.
- Scheduled job does not start: make sure the instance is enabled, scheduled, and the current time is within the prep window for `TARGET_DATE - 1 day`.

## Development

```bash
uv sync
uv run python -m unittest discover -s tests
uv run uvicorn app.main:app --reload --host 0.0.0.0 --port 8080
```
