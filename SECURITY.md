# Security scope

Buntzen is a self-hosted application with an administrator and invited member
accounts. The default mode is private HTTP and must stay on a trusted private
network. Public access through an HTTPS tunnel is supported only with the
[public HTTPS configuration](docs/public-exposure.md), after creating the initial
administrator privately. Buntzen enforces its own authentication; Cloudflare
Access is not a prerequisite.

## HTTP and account boundaries

The complete router is in `internal/web/server.go`. GET routes also accept HEAD.

| Surface | Access and protections |
| --- | --- |
| `/healthz` | Anonymous database-availability check; no cookies or application records. Limited Host/HTTP health exception. |
| `/static/…` | Anonymous embedded assets; no filesystem or appdata serving. Public UI transport rules apply. |
| GET/POST `/login` | Anonymous sign-in, public-form CSRF and browser-origin checks, Argon2id password verification, persistent username/visitor failure limits and bounded expensive work. |
| GET/POST `/setup` | One-time administrator setup requiring a host-generated token, CSRF and browser-origin checks. Public mode refuses an uninitialized database; initialized setup redirects to sign-in. |
| `/account`, `/account/password`, `/account/username`; POST `/logout` | Authenticated self-service. Account changes verify the current password. Mutations require session-bound CSRF and browser-origin checks. |
| `/admin/users`, `/admin/users/new`, `/admin/users/{id}`, and password/delete actions | Active administrator only, plus authentication and mutation checks. The permanent administrator has additional deletion/role protections. |
| `/`; `/sources` with new/edit/health/pair actions; `/profiles` with new/edit actions; `/bookings` with new/edit/run actions | Authenticated owner. Resource lookups and linked IDs are scoped to the account in storage; mutations require CSRF and browser-origin checks. |
| `/jobs`, `/jobs/{id}`, `/jobs/{id}/events`, POST `/jobs/{id}/decision` | Authenticated owner. Streams recheck session validity; decisions also require CSRF and browser-origin checks. |

No public registration, password-recovery link, webhook, file upload, diagnostic
archive download, or separate public JSON API is registered. Administrator status
does not bypass ownership of another account's sources, profiles, bookings or jobs.

Public mode requires an exact HTTPS origin and trusts visitor headers only from
explicit connector socket addresses. Session and CSRF cookies use `__Host-`
names, Secure, HttpOnly, SameSite=Strict, Path=/ and no Domain. Server sessions
have a 30-minute idle and 24-hour absolute lifetime and are bound to the configured
origin. Password changes, resets, disablement and logout revoke access. A failed
session lookup/refresh prevents protected work. There is no application MFA;
a stolen valid password or live session remains a material risk.

Every mutation requires CSRF plus the exact browser origin, with an absent-Origin
compatibility path requiring `Sec-Fetch-Site: same-origin`. Browser headers alone
are not authentication. Go templates escape HTML; dynamic browser text uses text
nodes. CSP, framing denial, nosniff and no-store protect application responses;
embedded static assets are deliberately cacheable. Public mode adds HSTS.

## Required application properties

- Member data cannot select host executables or expand operator-approved provider
  and credential origins. Browser process launch uses fixed argument construction,
  no shell, bounded version probing and supervised process-group cleanup.
- OTP provider selection is explicit and never falls back. BlueBubbles uses only
  ping and bounded message queries; Twilio reads account/inbound-message resources
  and never sends messages. Provider transport rejects redirects, environment
  proxies, disallowed destinations and unsafe DNS results.
- Credentials and OTPs must not enter durable events, rendered HTML history, raw
  screenshots, DOM dumps, traces or stdout. Raw authenticated browser diagnostics
  are disabled. Live OTPs intentionally exist in the Go/Python exchange and
  authenticated owner-only temporary SSE state.
- Final-confirmation ambiguity becomes `outcome_unknown`, never an automatic
  retry. A verified success remains successful if worker cleanup later times out.
- One profile/source has at most one active job, and one appdata directory has at
  most one control plane. Successful or unknown bookings retain their reservation
  even after job-history pruning.
- Expensive password work, request bodies, streams, writes, worker execution and
  profile inspection have finite budgets. Canonical containers use finite CPU,
  memory and PID limits, a read-only root and bounded temporary filesystems.

## Trusted runtime and private state

The Go service and Python workers share UID 1001 and access to appdata. Chromium's
renderer sandbox is enabled, but workers are not isolated from the database,
master key or other accounts' profile files by OS identities or mount namespaces.
A malicious Python dependency, Python execution vulnerability or sufficient
browser compromise could therefore compromise all application accounts. Provider
adapter restrictions are not a whole-browser egress firewall; browser subresources
and service workers remain enabled. Treat this as a single trusted application
runtime, not a sandbox for untrusted code or hostile tenants.

The default encryption key sits beside the database and does not protect a copied
whole appdata directory. An existing private key can be mounted separately and
read-only with `BUNTZEN_MASTER_KEY_FILE`. Startup rejects missing replacement keys,
unsafe key files and keys that cannot authenticate existing encrypted records
before write-capable SQLite opening or migration. Separation protects a
database-only backup; it cannot prevent a compromised service from reading its
key. Browser profiles themselves contain reusable session state. Keep appdata,
keys, old diagnostic exports, snapshots and backups private and protect backups
separately. Follow [key storage and recovery](docs/public-exposure.md#key-storage-and-recovery).

Profile limits detect growth periodically; they are not a physical disk quota.
Container limits reduce host impact but do not guarantee availability against a
distributed flood or a malicious admitted user. Keep the origin port restricted
to the connector and intended health probes. Never disable Chromium's sandbox,
run the service as root, or grant broad host capabilities to work around a
container-host incompatibility.

BlueBubbles' password is unscoped at the provider; read-only behavior is an
application invariant, not a provider permission boundary. Explicitly approved
private HTTP provider connections remain cleartext, including the provider's
query-string credential. Protect that network and provider/proxy logs.

## Build, release and verification

Docker inputs and Actions use immutable digests/commits. Go checksums, locked
Python wheel hashes and the pinned build backend are verified. CI tests altered
backend-hash rejection and audits both runtime and build dependencies. Image
scanning has no vulnerability exceptions and rejects HIGH/CRITICAL results,
including unfixed vulnerabilities. Weekly CI refreshes advisory checks; reviewed
updates are still required to replace pinned vulnerable code.

The exact release image is smoke-tested before version and `latest` tags are
promoted. Signed provenance, SBOM and vulnerability-gate attestations bind
publication to its source and hosted workflow. `latest` advances only after the
release checks pass. Portainer updates are operator-triggered and do not rerun
vulnerability scans or attestation verification. Review release notes, preserve
the configured schedule gate, and verify the actual running version and health
after updating. Inspect failed or ambiguous updates before another operation;
Portainer can publish final status before stack-file cleanup completes. See
[release controls](docs/release-and-deployment.md).
Hashes and signatures establish artifact identity and origin, not that accepted
upstream code is benign. A clean scan is not proof of no unknown vulnerabilities.

Security changes should include a reproducible negative test, a valid operation
control and review of the affected callers. Synthetic CI browser/image checks are
separate from live provider, tunnel and production-host verification. Assess any
new endpoint, credential destination, writable path, worker capability or build
input against these boundaries before exposing it.
