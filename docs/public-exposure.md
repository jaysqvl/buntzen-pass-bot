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
