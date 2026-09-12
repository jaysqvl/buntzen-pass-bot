# Lakes and booking providers

Lake Pass Bot separates a destination's rules from the service used to book it.
The lake selector currently offers **Buntzen Lake**, booked through **Yodel**.
Adding an entry does not by itself establish that another destination works.

## Current boundaries

- `internal/destinations/catalog.go` defines the lake catalog and resolves stable
  lake IDs. `internal/destinations/buntzen.go` owns this lake's URLs, supported
  passes, local timezone, and release defaults.
- Booking requests persist a `lake_id`. The engine resolves that destination
  before dispatching a job and sends its lake and provider IDs to the worker.
  Existing requests migrate to their original destination without changing
  their dates, URLs, schedules, or credentials.
- `actions/src/lake_pass_actions/lakes/` defines destination-specific pass labels
  and matching rules. Its Buntzen module owns those choices.
- `actions/src/lake_pass_actions/providers/yodel/` owns Yodel browser behavior:
  authentication, calendar selection, vehicles, cart checks, and confirmation.
  The worker selects an allowlisted provider and rejects mismatched lake/provider
  combinations before browser work.

Unknown explicit lake IDs are rejected. Missing IDs are accepted only through the
compatibility path for requests made before lake selection existed. Profiles
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
