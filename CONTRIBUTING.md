# Development guidelines

Prefer code that makes the booking lifecycle easy to follow. A small, explicit
implementation is usually better here than a reusable framework.

## Names and boundaries

- Use domain names consistently: a booking request is saved configuration, a
  job is one execution, a profile is a Yodel identity, and an OTP source is its
  inbox configuration. Do not use these terms interchangeably.
- Use Go's `ID`, `URL`, `HTTP`, `OTP`, and `CSRF` initialisms. Short names such as
  `ctx`, `err`, `req`, and receiver names are fine in small scopes; give long-lived
  state descriptive names. Python uses `snake_case` and explicit unit suffixes
  such as `_seconds` or `_ms` for numeric time values.
- Keep each file focused on one responsibility within its existing package.
  Split by behavior, not an arbitrary line limit. Add a package only for a real
  dependency boundary, and avoid generic `utils`, `helpers`, or manager layers.
- Put domain validation on the domain value (`profile.Validate()`). Validate
  external input at HTTP, storage, provider, and worker-protocol boundaries.
  Do not repeatedly validate a private helper's own fixed constants.
- Prefer concrete dependencies. Introduce an interface at the consumer only
  when implementations actually vary, such as OTP providers or action processes.
- Share policy when it must stay identical, such as HTTP origin normalization or
  booking mode selection. Do not force different provider pagination protocols
  through one configurable abstraction to remove a few similar lines.

## Runtime ownership

| Area | Responsibility |
| --- | --- |
| `cmd/buntzen` | Process setup and CLI dispatch |
| `internal/web` | HTTP authorization, forms, rendering, live job presentation |
| `internal/engine` | Queueing, scheduling, job execution, provider composition, storage maintenance |
| `internal/store` | SQLite transactions, ownership, leases, durable state and booking admission |
| `internal/model`, `internal/scheduler`, `internal/origin` | Domain values and pure policies; no database or browser access |
| `internal/control` | Worker protocol orchestration and transient OTP/approval state |
| `internal/actionproc` | Process lifetime, bounded JSON-lines and stderr transport |
| `internal/otp` and adapters | Inbox matching and provider-specific read operations |
| `actions/src/buntzen_actions` | Browser interaction behind the versioned worker protocol |

Keep owner-scoped and system-authorized store entry points distinct. Keep
reservation uniqueness in SQLite so it covers multiple requests and processes.
The Python worker does not read the Go database or receive provider credentials.

The code starting a goroutine, subprocess, timer, or browser context owns its
cleanup. Cancellation and deadlines must still work when a peer stops reading
or responding. Retrying after final confirmation requires particular care:
an uncertain outcome must retain the booking reservation.

Errors should explain the failed operation without exposing credentials, inbox
content, or private page data. Catch exceptions at an intentional recovery or
process boundary; do not silently turn programming errors into success.

## Tests and review

- Assert externally observable behavior and failure outcomes. A test that checks
  a constant, searches source for a call, or configures a mock to return the
  expected answer is weak evidence by itself.
- Keep regressions for wrong dates, false confirmations, duplicate bookings,
  cancellation, recovery, ownership, and secret handling. These represent
  costly failures, even if their fixtures are longer than the implementation.
- Use table cases for one behavior with several inputs. Give cases names that
  explain the scenario. Include a valid control when testing validation.
- Keep tests beside the policy they exercise. Use HTTP test servers for provider
  contracts, subprocesses for pipe/lifetime behavior, and real Playwright DOM
  tests for selectors. Small mocks belong at actual dependency boundaries.
- Avoid asserting internal call order unless the order is part of the contract,
  such as arming an OTP inbox before requesting a code or persisting the final
  confirmation barrier before clicking.
- Prefer synchronization over sleeps. Bound asynchronous tests and clean up
  processes on failure. Isolate environment variables and temporary state.
- Do not add tests just to raise a count or coverage percentage. For a bug fix,
  check that the regression fails against the old behavior when practical.

For a Go change, run the affected packages, then the full suite when integration
or shared behavior changes:

```bash
gofmt -w cmd internal integration
go vet ./...
go test -race -count=1 ./...
```

For Python changes:

```bash
uv sync --project actions --locked
uvx --from ruff==0.12.10 ruff check actions scripts/deploy/tests integration/*.py
uv run --project actions --locked python -m unittest discover -s actions/tests
```

The UI event tests use Node 18 or newer and its built-in test runner:

```bash
node --test internal/web/testdata/client.test.cjs
```

Run [browser integration](integration/README.md) for browser or cross-process
changes and `bash scripts/deploy/tests/test_portainer.sh` for deployment changes.
CI additionally checks release metadata, public-tree privacy, dependencies, and
the built container. A passing synthetic test is not proof of a completed live
Yodel booking; report that distinction in reviews.
