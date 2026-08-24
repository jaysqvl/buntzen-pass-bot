# Buntzen actions protocol v1

`buntzen-actions` is a fresh-process Python worker for one allowlisted action:
Yodel browser automation. It does not read the application database, contact an
OTP provider, or import Twilio/BlueBubbles libraries. The Go process retains all
provider and persistence responsibility.

The worker uses bounded JSON Lines (UTF-8, one object per line, maximum 65,536
bytes) on stdin/stdout. All frames contain `{"v":1,"type":"..."}`. Logs go only
to stderr. Unknown object fields are ignored, but unknown frame types and invalid
state transitions fail the run.

The control plane passes its validated log threshold to each worker through
`BUNTZEN_ACTION_LOG_LEVEL`; operators should configure `BUNTZEN_LOG_LEVEL` or
the `BUNTZEN_DEBUG` convenience toggle rather than setting this internal
variable directly. Worker stderr is redacted and bounded again by Go before it
is written to the container log with a durable job ID. Protocol payloads are
never logged.

## Start and completion

The child first emits:

```json
{"v":1,"type":"worker.ready","protocol":1,"action":"yodel","commands":["auth-check","dry-run","book"]}
```

Go then sends `run.start`:

```json
{
  "v": 1,
  "type": "run.start",
  "run_id": "job-42",
  "command": "book",
  "mode": "manual",
  "config": {
    "profile_dir": "/appdata/profiles/profile-42",
    "target_date": "2030-01-15",
    "timezone": "UTC",
    "allowed_yodel_origins": ["https://yodelportal.com"],
    "login_probe_url": "https://yodelportal.com/buntzen-lake",
    "all_day_pass_url": "https://yodelportal.com/buntzen-lake",
    "half_day_pass_url": "https://yodelportal.com/buntzen-lake",
    "vehicle_keyword": "Example Vehicle",
    "pass_order": ["all_day", "afternoon", "morning"],
    "headless": true,
    "browser_channel": null,
    "executable_path": null,
    "default_timeout_ms": 15000,
    "poll_deadline_seconds": 120,
    "poll_min_seconds": 1.4,
    "poll_max_seconds": 3.6,
    "artifacts_dir": "/appdata/artifacts/job-42",
    "release_at": "2030-01-14T07:00:00Z",
    "auth_deadline_at": "2030-01-14T06:55:00Z"
  }
}
```

`release_at` and `auth_deadline_at` are optional RFC3339 timestamps.
`auth_deadline_at` cannot be after `release_at`. When a release is supplied,
Python authenticates immediately, emits heartbeats while preserving the browser
session, then emits `release.ready` and starts booking. Go can omit both fields
for immediate jobs. The child terminates with one `run.complete` whose status is
`succeeded`, `failed`, `cancelled`, or `outcome_unknown`.

For a `book` run with an authentication deadline, Python always checks for an
already-authenticated session first. Such a session remains valid even when the
check occurs at or after the deadline. If the browser is logged out, however,
the deadline bounds provider arming, OTP delivery, and OTP submission. Python
does not trigger a new login/resend or submit an OTP after the deadline. A
session that expires during release waiting after the deadline fails immediately
without requesting credentials or a new OTP. `auth-check` and `dry-run` ignore
the optional scheduling deadline.

Credentials and OTP/provider secrets are forbidden in `run.start`.
`allowed_yodel_origins` is a non-empty, host-controlled list of exact HTTPS
origins. Every configured Yodel URL must match it. Python blocks top-level
navigation away from those origins and re-checks the current page immediately
before requesting or filling credentials and OTPs.

## Just-in-time secrets and OTP handshake

When Yodel actually displays a login form, Python emits
`credentials.request {request_id}`. Go replies with
`credentials.provide {request_id,email,password}`. Both values may be null only
for a profile that does not display a form; Python never echoes either field.

Every action that may generate a new code follows this sequence:

1. Python emits `otp.prepare {challenge_id,trigger}`.
2. Go snapshots/arms the selected provider and sends `otp.ready {challenge_id}`.
3. Python clicks login or resend and emits `otp.triggered {challenge_id}`.
4. Go polls and sends `otp.provide {challenge_id,code}`.
5. Python fills/submits it and emits `otp.submitted {challenge_id}`.

Go may respond with `otp.error` or `otp.expired` at either wait point. Python
then emits `otp.failed` and fails without exposing the provider error or code.
If login completes without MFA, Python emits `otp.not_required` so Go can stop
polling. OTP values must be 4-8 ASCII digits.

## Manual approval and cancellation

Immediately before the final Yodel click, manual mode emits
`approval.request {approval_id,pass_key,label}`. It waits without an application
deadline for `approval.approve` or `approval.cancel`, emitting a `heartbeat`
about every 15 seconds. A closed browser or disappeared confirmation control
emits `approval.expired` and fails the job. `control.cancel` cancels any protocol
wait and is checked between browser operations and during release/poll sleeps.

Both manual and automatic confirmation use a durable, correlated barrier.
Python emits
`confirmation.starting {confirmation_id,pass_key,label}` and waits indefinitely
for the matching `confirmation.ready {confirmation_id}`. Go sends that reply
only after durably recording that the final click may begin. A matching
`confirmation.error {confirmation_id}`, cancellation, closed stream, unexpected
frame, or mismatched ID aborts without clicking. Python emits
`confirmation.completed {confirmation_id,pass_key,label}` only after Playwright
returns successfully. A click failure after `confirmation.ready` ends as
`outcome_unknown`; Go must never automatically retry that state.

## Artifact guarantees

Tracing is off while navigating, filling credentials, requesting, receiving,
and submitting MFA. It starts only after an authenticated page is observed and
is stopped before every re-authentication. Future-release and manual modes also
close the current trace segment before waiting and start a new segment only
after the release/authentication or approval gate succeeds. Trace files rotate
through eight fixed names. The Go control plane periodically monitors and cleans
retained artifacts to a 64-file/64-MiB per-job ceiling; this is not a hard
filesystem write quota, so transient overshoot remains possible. Screenshots are
likewise disabled in sensitive auth state. The worker never writes page HTML,
credentials, OTPs, or provider identifiers to disk.
