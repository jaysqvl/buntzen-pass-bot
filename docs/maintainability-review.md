# Maintainability review — September 5, 2026

This pass reviewed the application, tests, and build/deployment tooling for
consistent naming, useful tests, clear responsibilities, and avoidable coupling.
It builds on the booking-verification fixes in PR #60. It does not certify a live
Yodel booking or change the deployed NAS instance.

## Findings and changes

| Area reviewed | Result |
| --- | --- |
| Go domain and configuration | Split booking, job, profile, and OTP models; standardized receiver validation; removed unused pass-order metadata; centralized origin normalization shared by configuration, booking policy, and HTTP middleware. Matched Go's minimum poll interval to the worker's 50 ms minimum. Configuration tests now isolate the developer's environment. |
| Engine and scheduler | Separated job execution, storage maintenance, provider/pairing composition, and booking queueing within the same package. Manual and system queue entry points share command/mode policy. Replaced a UTC-only timezone test with historical Vancouver DST cases; moved pass-order testing to the model package. |
| Store and migrations | Separated accounts, authentication, sessions, job admission, lifecycle, decisions, and events. Preserved transaction ownership, leases, reservation triggers, migration behavior, and recovery tests. Removed an unused ownership helper and moved generic SQL helpers out of OTP-source persistence. |
| Worker coordination and subprocess transport | Gave each run ownership of its child context and outstanding OTP/approval waits. Cancellation's kill deadline now starts independently of a blocked stdin write; forced cleanup also releases an abandoned event stream. Regression tests exercise an actual waiting provider, a subprocess that refuses to read, and a caller that stops consuming events. |
| Python browser worker | Removed pass-through wrappers and duplicate pass metadata. Extracted scoped calendar selection using named date-button fields. Simplified configuration conversion and mock browser fixtures. Fixed late OTP messages extending receive deadlines, non-ASCII OTP acceptance, and diagnostic cleanup preventing browser cleanup. |
| Web and CLI | Split routing, middleware, authentication, accounts, resource forms, live jobs, server startup, CLI jobs, and doctor output by responsibility. Template failure now returns HTTP 500 without committing partial HTML. Decision buttons recover after network failure and prompt a status check before manual retry. |
| Test quality and integration | Replaced JavaScript source searches with executed event tests; removed constant-only/weak token tests and source-spelling checks for release shell expressions. Shared browser harness and artifact inspection helpers avoid duplicate logic and interpreter-argument slicing. Preserved real-browser, concurrency, recovery, privacy, and protocol-order regressions. |
| OTP adapters, auth, encryption, locking, logging | Kept existing focused boundaries. Removed unused OTP length constants and redundant validation of internally constructed BlueBubbles queries. Preserved external response validation. Strengthened key persistence testing by reopening the encryption key before decryption. |
| Build and deployment | Reviewed Docker/Compose, CI, release workflows, scripts, and their tests. Added client event tests to CI and corrected deployment documentation that disagreed with workflow tags, manual dispatch, revision selection, and Portainer path visibility. Kept the runtime deployment policy intact. |

The blocked-stdin and abandoned-event regressions both reproduced their cleanup
failures before the fixes. The OTP deadline regression also failed against the
previous implementation; it uses a real pipe with arriving replies, not a
sequence of mocked clock calls.

## Deliberate choices

- File splits retain the existing packages. No repository framework, browser
  inheritance hierarchy, dependency-injection layer, or interface for every
  concrete type was added.
- The Yodel orchestration still keeps login, OTP, release timing, reauthentication,
  and trace suspension together because they share one browser session and its
  privacy lifecycle. Calendar, cart, receipt, protocol, and diagnostics operations
  have separate responsibilities.
- The two OTP adapters keep their own pagination and configuration. Their cursor
  semantics differ; the shared HTTP guard and message matcher are the useful
  common boundaries.
- Defensive checks remain where inputs cross a boundary or a mistake could cause
  duplicate bookings, lost cancellation, leaked credentials, or corrupt state.
  Their presence alone is not a reason to remove them.
- Small client tests execute the actual JavaScript with DOM doubles. They prove
  event and request behavior, not browser layout. Synthetic Yodel integration
  proves local browser/protocol scenarios, not live login or reservation issuance.
- Remaining release/Compose text checks verify explicit wiring and configuration.
  They are not presented as execution of release verification or a deployment.

Future reviews should use [the development guidelines](../CONTRIBUTING.md), with
test scope tied to the behavior changed rather than a target test count.
