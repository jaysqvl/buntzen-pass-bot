# Lake Pass Bot rebrand migration

This change prepares the source, UI, CLI, Python worker, documentation, and
release configuration for **Lake Pass Bot**. It does not rename the remote
repository, publish an image, change an existing deployment, rewrite history, or
delete the preserved pre-rebrand branch. Review the source build before deciding
on publication or removal of the old version.

## Existing installations

Keep the original appdata directory, encryption key, browser profiles, stack
settings, and working image digest. Before an eventual upgrade, stop active jobs
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

When eventually applying the new Compose file, account for the changed service
name in the existing stack and remove the stopped old service only after review.
Preserve custom host paths and the installed seccomp profile. Keep scheduling
disabled while validating the upgraded application, and verify an OTP connection
and booking setup before enabling unattended work.

## Review this source without a published image

Use the source-build `docker-compose.yml` from a separate checkout and a fresh,
isolated appdata directory. Run `docker compose up -d --build` after completing
the [README setup](../README.md#quick-start-with-docker-compose). Do not copy live
browser credentials into a second concurrent instance or initiate duplicate
bookings while the original service is running.

`deploy/portainer.yml` targets `ghcr.io/jaysqvl/lake-pass-bot:latest`; this rebrand
has not published that image. Keep the old installation on its existing image
until a renamed release exists and deployment is deliberately approved.

## Release continuity and later choices

The version baseline remains **0.5.3** in the release manifest, Python project,
and lockfile. New releases use the `lake-pass-bot-v` component prefix. The
Release Please bootstrap SHA anchors the first renamed release at the preserved
pre-rebrand commit, so the first changelog does not need to replay the old
history. Release promotion compares both old and new component tags, and manual
recovery accepts old tags without allowing an older stable release to replace a
newer `latest` image. [Release Please documents bootstrap and manifest versions](https://github.com/googleapis/release-please/blob/main/docs/manifest-releaser.md#bootstrapping).

Review the first renamed release PR's comparison link before publishing.
Release Please can synthesize `lake-pass-bot-v0.5.3` as the previous tag from the
manifest even though that tag does not exist. Point that first comparison at the
retained `buntzen-pass-bot-v0.5.3` tag, or explicitly authorize a baseline tag alias
as part of the later repository migration. This source change creates no tags.

Release workflows only publish from `jaysqvl/lake-pass-bot`. Renaming the GitHub
repository and reviewing package permissions/visibility are separate publication
steps. Existing historical changelog entries and release tags are retained.

The `pre-rebrand` branch preserves the working version. Deleting that branch or
rewriting published commits requires a separate decision after reviewing this
rebrand. A future history rewrite would also require coordinating tags, forks,
clones, links, and force pushes; renaming the app does not erase those references.
