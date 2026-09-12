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
| **Home** | Yodel sign-in names, mobile numbers, enabled state, and the **Sign in to Yodel** action. |
| **OTP sources** | BlueBubbles or Twilio configuration, connection checks, pairing, and the default source for newly queued jobs. |
| **Settings** | Preparation and sign-in deadlines, availability check window, and retry delay defaults shared across lakes; browser channel, headless mode, and action timeout defaults for new sign-ins; links to account management. |
| **Lakes → a lake** | Vehicle keyword, release schedule, pass preferences, and booking URLs. |
| **Bookings** | Each visit's lake, Yodel sign-in, date, saved vehicle keyword and other defaults, confirmation mode, and automation choices. |

Yodel sign-ins belong to the account and work across supported lakes using
Yodel. Existing sign-in IDs and credentials remain separate; the app does not
merge identities. The Home form edits only the name, mobile number, and enabled
state. New sign-ins receive an internally managed approved login URL and the
account's browser defaults. Editing an existing sign-in preserves its saved
browser and login configuration. Saving an older sign-in fills a missing login
URL from the approved provider defaults and clears any retired executable-path
override. If its mobile number was not migrated, re-enter the number to enable it.

OTP sources are independent account resources. The first source becomes the
default; choose **Make default** on another source to change it. Newly queued
jobs capture that source, and subsequent preference changes do not reroute jobs
already queued. Multiple Yodel sign-ins may use the same owned source. Browser
profile and inbox locks still serialize work on shared resources, and sources
cannot be selected from another account. Legacy callers without an account
preference retain their existing source association.

## Defaults and existing bookings

New sign-ins copy the account's browser defaults from **Settings**. New booking
requests combine the selected lake's personal defaults with the account's
preparation and retry defaults. Each falls back to its built-in defaults when
none have been saved. Selecting another lake in a booking form loads that lake's
defaults and compatible Yodel sign-ins. The request can override those values for the
visit. On an existing editable request, **Use lake defaults** loads the current
lake defaults into the form; saving applies them to that request. Changing the
selected lake or applying lake defaults preserves the request's preparation and
retry timing. Only new requests copy the account's current timing defaults.

Lake defaults include the vehicle keyword, local timezone, release time and
number of days before the visit, pass preference order, and pass URLs. Preparation and retry timing
belong to the account's global **Settings**. Supported pass types and provider
identity remain defined by the catalog. Custom URLs must still use an
operator-approved origin.

Each saved booking stores its own values, including the vehicle keyword and
release-day offset. Changing or resetting personal defaults never rewrites existing sign-ins,
bookings, or queued jobs. **Reset to built-in defaults** removes only the current
account's override for that lake; global preparation and retry settings remain
unchanged. Existing bookings retain their original vehicle choice and release
schedule. Older bookings receive their former profile's vehicle keyword during
migration; credentials, identities, schedules, and job history are retained.

## Current boundaries

- `internal/destinations/catalog.go` defines the lake catalog and resolves stable
  lake IDs. `internal/destinations/buntzen.go` owns this lake's URLs, supported
  passes, local timezone, and release defaults.
- Sign-ins persist a `provider_id`; booking requests persist `lake_id`,
  `vehicle_keyword`, and `release_days_before`. Legacy profile lake/vehicle
  fields remain for migration compatibility. Account and lake default tables
  are scoped by `user_id`. The engine resolves the destination
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
compatibility path for records and callers predating lake selection. Sign-ins
currently represent Yodel identities; the engine rejects a destination using a
provider incompatible with that sign-in. Sign-in jobs operate without a booking
request and cannot reserve a pass.

## Adding a lake using Yodel

1. Add a Go catalog entry with a stable ID, visible name, provider ID, URL defaults,
   supported passes, and release rules. Register it in the catalog list.
2. Add a corresponding Python lake definition and register it in the worker
   catalog, keeping IDs and pass keys consistent across both processes.
3. Confirm sign-in compatibility and approved provider origins. The lake
   catalog must not bypass the operator's outbound-origin policy.
4. Exercise selection, persistence, release timing, and provider dispatch in
   tests. Use isolated Playwright fixtures for its date/pass/vehicle UI and retain
   the cart, confirmation, duplicate-attempt, and unknown-outcome safeguards.
5. Document support only after checking the real provider flow. A synthetic test
   or successful login is not evidence that a pass was issued.

## Adding a booking provider

Implement a separate provider adapter and register it with the worker. Extend
sign-in configuration, provider policy, engine compatibility checks, and OTP
composition for that provider before enabling it in the catalog. Preserve the
versioned worker protocol and keep provider credentials out of booking requests.

The existing sign-in/date reservation guard remains deliberately conservative.
If future providers permit distinct same-day lake bookings, design and migrate
that rule explicitly; adding a lake must not silently weaken duplicate-booking
protection or allow a retry after an uncertain confirmation.
