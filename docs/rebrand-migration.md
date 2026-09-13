# Lake Pass Bot rebrand migration

**Lake Pass Bot** uses the `jaysqvl/lake-pass-bot` repository, `lake-pass-bot`
CLI, and `ghcr.io/jaysqvl/lake-pass-bot` release image namespace. Source changes,
repository publication, and installation are distinct steps: verify the selected
release and image before updating an installation. Preserve the pre-rebrand
history and data backups throughout the transition.

## Existing installations

Keep the original appdata directory, encryption key, browser profiles, stack
settings, and working image digest. Before an upgrade, stop active jobs
and make a consistent private backup as described in
[release and deployment](release-and-deployment.md#verify-and-recover). Never run
the old and new services against the same appdata at the same time.

Compatibility is intentional:

| Item | Behavior |
| --- | --- |
| App name and CLI | `Lake Pass Bot` and `lake-pass-bot`; the new Docker image includes a `buntzen` executable alias for existing operator commands. |
| Runtime environment | `LAKE_PASS_*` names are canonical. If a canonical name is unset, its matching `BUNTZEN_*` name remains accepted. An explicitly set canonical value, including an empty value, takes precedence. Existing unprefixed settings such as `APPDATA_DIR` and `SCHEDULES_ENABLED` retain their names. |
| Compose environment | Both templates translate old names into the canonical runtime settings. The native Compose port also accepts existing `WEB_PORT`. The service/container name becomes `lake-pass-bot`. |
| Database | New installs create `lake-pass-bot.db`. An existing `buntzen.db` is reused in place. Startup rejects ambiguous directories containing both database names. Keep the data directory together; do not create or rename a second database during the upgrade. |
| Encryption and browser state | The existing key and profile marker formats are retained. The read-only key mount stays at `/run/buntzen-key` so explicit existing master-key paths continue to resolve. |
| Bookings | The migration records the original lake on existing requests. Saved URLs, release timing, credentials, and duplicate-attempt safeguards are retained. |
| Lake connections | Provider sign-in setup now lives under Lakes → the owning lake; Buntzen Lake contains Yodel. Existing profile IDs, encrypted credentials, browser sessions, and queued job snapshots are retained. OTP sources and their account default remain global. |
| Sessions | New cookies use neutral names. Existing cookies are accepted within the same transport mode; public HTTPS still requires the hardened `__Host-` cookie mode. |
| Python worker | The package becomes `lake-pass-actions` and its import/worker module is `lake_pass_actions`. Use the renamed package in source development and refresh the locked virtual environment. |

The template uses `lake-pass-bot` as its service/container name. Existing stacks
can retain their current service name while updating the image; a display-name
change does not require replacing the stack or its data. If adopting the new
service name, stop the old service before starting its replacement against the
same appdata. Preserve custom host paths and the installed seccomp profile. Keep scheduling
disabled while validating the upgraded application, and verify an OTP connection
and booking setup before enabling unattended work.

## Isolated source builds and release images

Use the source-build `docker-compose.yml` from a separate checkout and a fresh,
isolated appdata directory. Run `docker compose up -d --build` after completing
the [README setup](../README.md#quick-start-with-docker-compose). Do not copy live
browser credentials into a second concurrent instance or initiate duplicate
bookings while the original service is running.

`deploy/portainer.yml` targets `ghcr.io/jaysqvl/lake-pass-bot:latest`. Verify a
completed release-image workflow and the intended immutable digest before using
a registry image. A private source-built canary should use a unique local image
tag with its full source revision recorded; keep image pulling disabled for that
local image. Its successful deployment does not establish registry publication
or GitHub attestation of the build.

## Release continuity and later choices

The rebrand starts from the last pre-rebrand release, **0.5.3**. Release Please
advances the manifest, Python project, and lockfile versions together. New
releases use the `lake-pass-bot-v` component prefix. The
Release Please bootstrap SHA anchors the first renamed release at the preserved
pre-rebrand commit, so the first changelog does not need to replay the old
history. Release promotion compares both old and new component tags, and manual
recovery accepts old tags without allowing an older stable release to replace a
newer `latest` image. [Release Please documents bootstrap and manifest versions](https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md#bootstrapping).

Review the first renamed release PR's comparison link before publishing.
Release Please can synthesize `lake-pass-bot-v0.5.3` as the previous tag from the
manifest even though that tag does not exist. Point that first comparison at the
retained `buntzen-pass-bot-v0.5.3` tag. For a first release of 0.6.0, the correct
comparison is:

```text
https://github.com/jaysqvl/lake-pass-bot/compare/buntzen-pass-bot-v0.5.3...lake-pass-bot-v0.6.0
```

Use the actual proposed version at the right-hand end. Correct both the
changelog entry and release notes if needed; historical tags do not need to be
recreated or deleted. Once a renamed release exists, Release Please discovers
its tag normally and no longer needs the bootstrap fallback.

Release workflows only publish from `jaysqvl/lake-pass-bot`. Complete the GitHub
repository rename before running them, update clone remotes, and verify the GHCR
package's repository association and pull permissions after its first publish.
A newly created package may require an explicit visibility change before
anonymous clients can pull it. Keep historical changelog entries and release
tags intact; repository renaming does not require a history rewrite.

The `pre-rebrand` branch preserves the working version. Deleting that branch or
rewriting published commits requires a separate decision after reviewing this
rebrand. A future history rewrite would also require coordinating tags, forks,
clones, links, and force pushes; renaming the app does not erase those references.
