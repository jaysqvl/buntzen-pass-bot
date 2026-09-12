# Lake settings and booking providers

Lake Pass Bot separates a destination's rules from the service used to book it.
The lake selector currently offers **Buntzen Lake**, booked through **Yodel**.
Adding an entry does not by itself establish that another destination works.

## Where settings live

All preferences and resources below belong to the signed-in account, including
when that account is an administrator. Saving personal defaults does not change
another account or the server's deployment configuration.

| Page | Settings and resources |
| --- | --- |
| **OTP sources** | Independent inbox connections, connection checks, and BlueBubbles pairing. |
| **Settings** | Browser channel, headless mode, and action timeout defaults for new profiles; links to account management. |
| **Lakes → a lake** | Profiles and their vehicles, plus personal release, pass, URL, preparation, and retry defaults. |
| **Bookings** | Each visit's lake, profile, date, copied defaults, confirmation mode, and automation choices. |

A profile belongs to one lake and keeps its vehicle, login URL, mobile number,
linked OTP source, and browser settings there. Its lake cannot be changed after
creation; create a profile under the intended lake instead. A booking can use
only a profile belonging to the same account and lake.

OTP sources remain independent account resources. Each profile links to one
source, and each source can link to only one profile, including across different
lakes. Adding another lake does not remove this safeguard.

## Defaults and existing bookings

New profiles copy the account's browser defaults from **Settings**. New booking
requests copy the selected lake's personal defaults, falling back to the built-in
defaults if none have been saved. Selecting another lake in a booking form
loads that lake's defaults and available profiles. The request can then override
those values for the visit. On an existing editable request, **Use lake defaults**
loads the current defaults into the form; saving applies them to that request.

Lake defaults include the local timezone, release time and number of days before
the visit, pass preference order, pass URLs, preparation and sign-in deadlines,
and availability retry timing. Supported pass types and provider identity remain
defined by the catalog. Custom URLs must still use an operator-approved origin.

Each saved booking stores its own values, including the release-day offset.
Changing or resetting personal defaults never rewrites existing profiles,
bookings, or queued jobs. **Reset to built-in defaults** removes only the current
account's override for that lake. Existing profiles and bookings from before lake
settings migrate to Buntzen Lake. Existing bookings retain a release offset of
one day before the visit, with credentials, schedules, and job history retained.

## Current boundaries

- `internal/destinations/catalog.go` defines the lake catalog and resolves stable
  lake IDs. `internal/destinations/buntzen.go` owns this lake's URLs, supported
  passes, local timezone, and release defaults.
- Profiles and booking requests persist a `lake_id`; bookings also snapshot
  `release_days_before`. Account and lake default tables are scoped by `user_id`.
  The engine resolves the destination
  before dispatching a job and sends its lake and provider IDs to the worker.
  The scheduler uses the booking's saved release policy rather than looking up
  current personal defaults.
- `actions/src/lake_pass_actions/lakes/` defines destination-specific pass labels
  and matching rules. Its Buntzen module owns those choices.
- `actions/src/lake_pass_actions/providers/yodel/` owns Yodel browser behavior:
  authentication, calendar selection, vehicles, cart checks, and confirmation.
  The worker selects an allowlisted provider and rejects mismatched lake/provider
  combinations before browser work.

Unknown explicit lake IDs are rejected. Missing IDs are accepted only through the
compatibility path for records and callers predating lake selection. Profiles
currently represent Yodel identities; the engine rejects a destination using a
provider incompatible with that profile.

## Adding a lake using Yodel

1. Add a Go catalog entry with a stable ID, visible name, provider ID, URL defaults,
   supported passes, and release rules. Register it in the catalog list.
2. Add a corresponding Python lake definition and register it in the worker
   catalog, keeping IDs and pass keys consistent across both processes.
3. Confirm profile/login compatibility and approved provider origins. The lake
   catalog must not bypass the operator's outbound-origin policy.
4. Exercise selection, persistence, release timing, and provider dispatch in
   tests. Use isolated Playwright fixtures for its date/pass/vehicle UI and retain
   the cart, confirmation, duplicate-attempt, and unknown-outcome safeguards.
5. Document support only after checking the real provider flow. A synthetic test
   or successful login is not evidence that a pass was issued.

## Adding a booking provider

Implement a separate provider adapter and register it with the worker. Extend
profile configuration, provider policy, engine compatibility checks, and OTP
composition for that provider before enabling it in the catalog. Preserve the
versioned worker protocol and keep provider credentials out of booking requests.

The existing profile/date reservation guard remains deliberately conservative.
If future providers permit distinct same-day lake bookings, design and migrate
that rule explicitly; adding a lake must not silently weaken duplicate-booking
protection or allow a retry after an uncertain confirmation.
