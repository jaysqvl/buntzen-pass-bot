# Public HTTPS transport

Buntzen authenticates users with its own accounts. A Cloudflare Tunnel can carry
public HTTPS traffic to the application; Cloudflare Access is not required by
the app. Configure this transport boundary before exposing the login page.

Create the administrator through the private local interface first. Public mode
refuses to start on an uninitialized database, so the one-time setup token is
never an Internet-facing authentication method. Then set these deployment
environment variables:

```dotenv
BUNTZEN_PUBLIC_ORIGIN=https://buntzen.example
BUNTZEN_TRUSTED_PROXIES=127.0.0.1/32,::1/128
```

Replace the origin with the exact public HTTPS hostname, including a nondefault
port if needed, and omit the trailing slash. Replace the proxy entries with the
actual socket addresses of your connector as seen by Buntzen. The loopback
example applies only when the connector connects over loopback. A connector in
another Docker container or on another host has a different socket address.
Prefer fixed connector addresses and `/32` or `/128` entries. Buntzen rejects
networks broader than IPv4 `/24` or IPv6 `/64`. These are connector addresses,
not Cloudflare edge IP ranges; never trust an entire LAN or shared container
network unless every host on it is authorized to supply visitor identity.

Public mode requires the original public `Host` header. Leave Tunnel's optional
`httpHostHeader` unset, or set it to the public hostname. The app accepts
`X-Forwarded-Proto: https` and a single `CF-Connecting-IP` only from a configured
connector socket. Missing, duplicated, malformed or insecure values are
rejected. It ignores `X-Forwarded-For` and `X-Forwarded-Host` for this purpose.
Cloudflare documents [visitor headers](https://developers.cloudflare.com/fundamentals/reference/http-headers/)
and [Tunnel origin parameters](https://developers.cloudflare.com/tunnel/advanced/origin-parameters/).
Do not remove visitor IP headers or interpose an untrusted Worker/proxy that
can rewrite them. A compromised trusted connector can impersonate visitor IPs.

This mode issues Secure, HttpOnly, SameSite=Strict authentication and CSRF
cookies with browser-enforced `__Host-` names, `Path=/`, and no Domain. These
attributes prevent sibling subdomains from injecting the same cookies in
[current browsers](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Set-Cookie#cookie_prefixes).
Enabling public mode requires signing in again; legacy private-mode cookie names
are ignored. The app enforces the exact HTTPS browser origin, and emits HSTS without
applying it to sibling subdomains. HTTP requests to UI routes are rejected unless
the trusted connector establishes that the visitor used HTTPS. Direct TLS
requests use the actual socket peer for throttling and ignore forwarded IPs.
HSTS cannot protect a user's very first HTTP request; publish and use the HTTPS
URL and enable HTTPS enforcement for the public hostname at the tunnel edge.

Keep the application port reachable only by the connector and intended health
probes. `GET`/`HEAD /healthz` remains a cookieless HTTP health check on the public
hostname and configured `BUNTZEN_ALLOWED_HOSTS`; the implicit localhost health
authority is accepted only from an actual loopback socket. This exception grants
no access to the login, account, job, or other UI routes. Public-mode UI requests
do not inherit the legacy allowed-host/origin aliases.

Leaving `BUNTZEN_PUBLIC_ORIGIN` empty preserves the existing private HTTP mode.
That mode must not be exposed publicly. These transport controls complement
application authentication, ownership checks, provider restrictions, resource
limits, private storage, and release verification; they do not make a browser
worker or dependency inherently trustworthy.

## Authentication admission

First-run setup accepts the randomly generated token printed by the host. If
`BUNTZEN_SETUP_TOKEN` is supplied, it must encode 32 random bytes as unpadded
URL-safe base64 (43 characters). Generate it with
`python3 -c 'import secrets; print(secrets.token_urlsafe(32))'`, or leave it unset
for automatic generation. The app validates its format only while setup is
needed; an obsolete setup variable does not block an initialized installation.
A token's format does not prove that its bytes were generated randomly.

In private mode, invalid setup submissions have persistent per-visitor and global
rolling budgets for recording failures. These budgets bound stored failure rows;
the token comparison itself remains cheap and can still be attempted.
A valid token bypasses these failure budgets so anonymous guesses cannot lock
out the operator. Login and setup admit one request at a time after body and
CSRF validation; excess requests receive HTTP 503 with Retry-After. All password
hash/check paths share two nonqueueing Argon2 slots. Saturation is a retryable
service error, not a failed password guess. These controls bound expensive work;
they do not guarantee availability against a sustained distributed flood.

## Session lifetime and transport changes

Sessions expire after 30 minutes without an authenticated request and always
expire 24 hours after sign-in. Job event streams do not extend the idle deadline;
they clear transient OTP/pairing content and request a new sign-in when the
session expires. A failed session refresh prevents the protected action from
running. Sign-out reports success only after database revocation succeeds; an
error leaves cookies available for retry.

New public sessions are bound to the exact configured public origin. Private
sessions cannot be made public by renaming cookies; public sessions cannot be
replayed in private mode or at a different configured origin. No database schema
migration is required. Origin binding is not permanent revocation: switching
back to an earlier origin can accept its still-active sessions. Password changes,
resets, disabling an account and explicit logout remain the revocation controls.

## Live streams and stalled clients

Each account may open eight live job event streams, shared across its sessions
and jobs; the server admits at most 128 total. Additional streams receive HTTP
429 with Retry-After. Closing a stream returns capacity. Job ownership is checked
before admission, including when the budget is full.

Each event batch and flush has a five-second write deadline. Write/flush failures
close the stream and release its subscription. Idle periods clear that deadline;
the next batch starts a new one. The final HTTP chunk is bounded as well. Ordinary
HTTP responses have a 30-second write timeout. Event resume cursors, session
rechecks, and final-event delivery remain enabled.

## Key storage and recovery

The default is `APPDATA_DIR/master.key`. A new installation creates this file
once with private permissions and publishes it only after a complete write and
sync. An existing database with a missing default key is refused; the app never
silently generates a replacement. `BUNTZEN_MASTER_KEY_FILE` selects an absolute,
existing key file and never falls back to the default or creates external paths.
The file must be a regular file owned by the service user, with no group/other
permissions (0400 or 0600). Symlinks, special files, oversized and malformed keys
are rejected. Existing key bytes and permissions are never rewritten on load.

To separate an existing installation's key from appdata:

1. Stop the service and make a consistent private database/appdata backup. Retain
   the original matching key in a separate private backup.
2. Copy the **existing key bytes** into an existing private host directory such
   as `/srv/buntzen-key/master.key`. Set directory mode 0700 and file mode 0400;
   both must be owned by the service UID (1001 for the published container).
3. In Compose/Portainer set `BUNTZEN_KEY_DIRECTORY_PATH=/srv/buntzen-key` and
   `BUNTZEN_MASTER_KEY_FILE=/run/buntzen-key/master.key`. The canonical templates
   mount that directory read-only and refuse to create a missing host directory.
   Native runs use the actual absolute key-file path, without a container mount.
4. Start privately and verify existing provider/profile data can be read. Remove
   the old appdata key copy only after verifying recovery and retaining the
   separate matching backup. Never generate new bytes as a relocation step.

Without these settings, the templates preserve the legacy key path. Their
unused read-only mount defaults to appdata; that fallback adds no key separation.
With the service stopped, restore the matching database and original key together
when recovering or rolling back. No key-rotation command is provided here.

Before write-capable database opening and before migrations, Buntzen authenticates
all current and legacy encrypted fields, including disabled profiles and committed
WAL records. A wrong key or corrupt ciphertext stops startup with a generic error.
The read-only preflight does not modify database/WAL contents or schema; SQLite
may use WAL coordination files. An empty database (or one with no encrypted
credentials) cannot prove which valid key was intended, so retain matching backups
even when startup succeeds. Separate key storage protects against a database-only
copy; the running service and its Python workers can still read the key. This is
not isolation from a compromised service process.

## Outbound provider access

Before using BlueBubbles, set `BUNTZEN_BLUEBUBBLES_ENDPOINTS` in the operator's
deployment environment. Its JSON array approves at most 16 exact server origins.
An empty/unset policy disables BlueBubbles network access, including existing
saved sources. `BLUEBUBBLES_URL` only pre-fills the form and grants no access.
Members cannot expand this policy by editing a source or its encrypted settings.

For a public HTTPS server:

```dotenv
BUNTZEN_BLUEBUBBLES_ENDPOINTS='[{"origin":"https://messages.example"}]'
```

For a server on the same native host, reached over loopback:

```dotenv
BLUEBUBBLES_URL=http://127.0.0.1:1234
BUNTZEN_BLUEBUBBLES_ENDPOINTS='[{"origin":"http://127.0.0.1:1234","networks":["127.0.0.1/32"]}]'
```

For a LAN server, replace that example's origin and network with the actual
server authority and its fixed private IP `/32` (IPv6 `/128`). Container loopback
identifies the container itself. Prefer exact addresses; each origin permits at
most 16 prefixes, no broader than IPv4 `/24` or IPv6 `/64`. In Portainer, enter
the JSON value without the surrounding shell quotes. HTTP requires private or
loopback pins; approving it explicitly accepts cleartext on that trusted network.
HTTPS verifies the original hostname's certificate. No TLS verification bypass
is provided.

When pins are present, **every** resolved address must match a pin, including
public addresses. Without pins, only public HTTPS addresses are accepted. Each
new connection resolves once, rejects the whole result if any address is unsafe,
and dials a validated literal IP while preserving Host and TLS SNI. Existing
connections remain attached to their originally validated peer. Every request's
authority is checked before connection reuse. Environment proxies and redirects
remain disabled. Link-local/metadata, multicast, unspecified, reserved and
transition addresses are refused even with pins. The conservative public-address
exclusions follow the [IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry/)
and [IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry/) registries.

Twilio always uses `https://api.twilio.com` with the same public-address transport.
Stored endpoint overrides are rejected. BlueBubbles still uses only ping and
bounded message queries, and Twilio only reads account/inbound-message resources.
Source creation/editing, connection tests, doctor, job execution and pairing all
use the policy; there is no provider fallback. Operator policy changes take effect
after restart. These controls govern the Go OTP adapters; they do not contain an
arbitrary compromised Python worker or all browser network traffic.

## Worker execution deadlines

Job input loading has a 15-second budget. Authentication checks, supervised
pairing and dry-runs have 15 minutes from execution start, including provider
preparation. Immediate bookings retain their original persisted 15-minute expiry;
queueing or restart never resets it. Scheduled bookings may run until the polling
window ends plus 15 minutes for checkout, preserving the full configured
preparation window. That extra grace does not extend queue admission or polling.

A deadline cancels provider requests and the worker, then enforces the existing
bounded process-group cleanup grace. Before final confirmation it becomes a
time-limit failure. After final confirmation may have started, an unverified
outcome remains unknown and its booking reservation remains held. A matching
verified-confirmation event preserves success even if browser cleanup stalls
before the final worker result. These deadlines bound elapsed execution, rather
than promising an immediate kill or automatically retrying an ambiguous booking.

## Browser-profile storage

Each persistent browser profile may occupy 512 MiB of logical file data and
20,000 total entries, with directory nesting limited to 64 levels. The app checks
before launching a worker and every two seconds while it runs. A limit breach or
an inspection error stops that job; it never automatically removes a retained
profile or its login state. With the job stopped, the operator can review cache
usage and recover space, then retry. Normal saved sessions and Chromium's
singleton symlinks remain supported.

The inspection reads directory names in small batches through directory file
descriptors, does not follow symlinks, tolerates deleted cache descendants and
checks cancellation during traversal. Marker reads and unmarked-directory checks
are bounded too. Diagnostic storage uses the same inspection and counts empty
directories/symlinks toward its existing 64-entry, 64-MiB job budget.

These are periodic detection limits, not filesystem quotas. Writes can overshoot
between checks, during inspection, and during process cancellation grace. The
five-second inspection context bounds traversal work between filesystem calls;
it cannot interrupt a kernel call stalled on an unavailable filesystem. Use host
filesystem/container storage quotas for a strict physical-space ceiling. A
compromised same-user worker can write outside its profile, so this control does
not replace process/filesystem isolation.
