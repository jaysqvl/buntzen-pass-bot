# Release and Portainer deployment

GitHub builds, tests, scans, and publishes the Docker image. Portainer updates the
existing stack when you choose to install a release. No GitHub deployment
workflow, LAN runner, deployment approval, or Portainer API credential is needed.

Before installing an image, verify that its release publication completed and
that the registry exposes the intended digest. Read
[rebrand migration](rebrand-migration.md) when upgrading an installation from the
previous name. Publishing a repository or release does not update a running stack.

## Update from Portainer

The [Portainer Compose template](../deploy/portainer.yml) defaults to:

```text
ghcr.io/jaysqvl/lake-pass-bot:latest
```

`latest` follows the newest accepted stable release, including future minor and
major versions. Publishing a release does not restart your container. You decide
when to install it:

1. Read the release notes for configuration or database changes. Wait for active
   jobs to finish and keep auto-queueing disabled during the update.
2. In Portainer, open **Stacks**, select the existing Lake Pass Bot stack, and open
   **Editor**. Apply any required Compose or environment changes there.
3. Choose **Update the stack** and enable **Re-pull image and redeploy** so Docker
   checks the registry for the new image.
4. Wait for the container to become healthy. Open Lake Pass Bot and check the version
   and build in the footer against the release you intended to install.
5. Check the OTP source's **Test connection** and the relevant application flows.
   Container health alone does not prove that Yodel checkout works.

Use the existing stack, port, and appdata directory. Re-pulling an image does not
update your saved Compose file or add new environment settings. Portainer's
[stack editing documentation](https://docs.portainer.io/user/docker/stacks/edit)
explains the update controls.

If your existing stack uses a literal `@sha256:...` image or an explicit version,
change it to the `:latest` image above once. If it uses `${LAKE_PASS_IMAGE}`, set that
Portainer environment variable to the same `:latest` value, or use the current
template's default with the variable unset. A force update of an old digest
recreates the old image; it cannot choose a newer release.

To stay on a chosen release, set `LAKE_PASS_IMAGE` to its published version tag
or to the exact digest printed in the release workflow summary. To follow only
patch releases within a minor version, use its matching tag. The footer continues to display the version built into
the running image regardless of the tag used to install it.

## Existing stack settings

Preserve the existing stack's name, port, appdata path, and configuration. The
canonical [deploy/portainer.yml](../deploy/portainer.yml) uses:

- `LAKE_PASS_IMAGE`: optional image override; defaults to the project's `:latest`.
- `LAKE_PASS_WEB_PORT`: the host port already used by Lake Pass Bot.
- `LAKE_PASS_APPDATA_PATH`: the existing absolute appdata directory, owned by UID/GID
  1001. Never share it with another container or a native development instance.
- `LAKE_PASS_SECCOMP_PROFILE_PATH`: the Playwright seccomp profile path as seen by
  Portainer's Compose process. For containerized Portainer, keep it inside the
  persistent `/data` mount, for example `/data/lake-pass-bot/seccomp_profile.json`.
- `BLUEBUBBLES_URL` and `LAKE_PASS_BLUEBUBBLES_ENDPOINTS`: when using BlueBubbles,
  configure its exact [approved origin and network](public-exposure.md#outbound-provider-access).
- `LAKE_PASS_ALLOWED_HOSTS` and, when needed, `LAKE_PASS_ALLOWED_ORIGINS`: the exact
  authorities and origins used to access the app in private mode.
- `MAX_CONCURRENT_JOBS`: preserve your chosen concurrency.

The template keeps `SCHEDULES_ENABLED=false`. Complete the
[live booking tests](live-testing.md) before deliberately enabling unattended
scheduling in your saved stack. Updating an image does not itself enable it.

The optional `LAKE_PASS_KEY_DIRECTORY_PATH` must refer to an existing directory
containing the original `master.key`, owned by UID/GID 1001 with mode 0400 or
0600. To use its read-only mount, set
`LAKE_PASS_MASTER_KEY_FILE=/run/buntzen-key/master.key` after following the
[key relocation procedure](public-exposure.md#key-storage-and-recovery).
Otherwise preserve the existing key location.

Public HTTPS is optional for a private LAN installation. To enable it, follow
[public HTTPS configuration](public-exposure.md#public-https-transport), including
private administrator setup, `LAKE_PASS_PUBLIC_ORIGIN`, and
`LAKE_PASS_TRUSTED_PROXIES`. Public mode changes which hosts may access the UI.

If GHCR package visibility requires authentication, configure a read-only pull
credential in Portainer's registry settings. This is registry access; the app
does not need a Portainer API credential.

### Upgrading from 0.5.0 or earlier

Provider origin approval became required in 0.5.1. Existing BlueBubbles sources
now use `LAKE_PASS_BLUEBUBBLES_ENDPOINTS` (or the retained legacy variable).
Without it, provider network access is disabled even if the source has a saved
URL and password. Add the exact origin and its allowed network before
updating the image; see the provider policy linked above.

Apply the current template's CPU, memory, PID, read-only filesystem, and tmpfs
settings to the existing stack. Image tests verify those settings in CI; they do
not establish that an older Portainer stack has adopted them. Preserve the
original encryption key and any deliberate local paths or network settings.

## Release publication

Renamed releases continue from the 0.5.3 pre-rebrand baseline; see
[release continuity](rebrand-migration.md#release-continuity-and-later-choices).
Publication is enabled only in `jaysqvl/lake-pass-bot`. For the first renamed
release, review the generated comparison link, required CI checks, and GHCR
package permissions before installation.

`release-please.yml` calls `release-image.yml` when it creates a
`lake-pass-bot-v*` release. GitHub-token-created tags do not trigger a second
release workflow, so publication is explicitly connected in the same run.

The publication jobs:

- validate the release's exact commit on `main` and build `linux/amd64`;
- run the actual image through the application and Chromium smoke tests;
- reject HIGH and CRITICAL OS or library vulnerabilities, including those without
  an available fix, with no current exceptions;
- attach and verify build provenance, a signed SPDX SBOM, and a signed record of
  the passed vulnerability gate;
- promote the accepted digest to its version and commit tags; and
- advance `latest` only after those checks pass, without letting an older release
  rebuilt manually replace a newer stable release.

Third-party Actions and Docker build inputs remain pinned to immutable commits
or digests. The mutable `latest` tag is an installation choice; the workflow still
records the exact accepted digest and the app retains its version and revision.

If publication fails, rerun the failed jobs in that original run. **Publish
release image** also supports manual dispatch from `main` for an existing
published component tag and exact commit SHA. It repeats the release checks and
publishes the image without contacting your server.

Manual recovery also accepts historical component tags and passes both old and
new Docker build argument names. New releases use only the new prefix.

Keep `main` protected with reviewed pull requests and passing `go`,
`python-actions`, `integration`, and `docker` checks. Release Please still needs
permission to create release pull requests and publish releases. None of these
build/release permissions requires a runner or credentials on your LAN.

## Source-built canaries

A private canary can be built from a reviewed commit without publishing a GitHub
release. Use an isolated build context, a unique local image tag, and the full
source revision in the image metadata and binary. Keep `pull_policy: never` for
that local image and retain the existing stack's service identity and runtime
settings while replacing only its image.

Run the container smoke test and the current HIGH/CRITICAL vulnerability gate
against that exact image before cutover. Save the image configuration ID, source
revision, scan results, and SBOM. A local image configuration ID is not a registry
manifest digest; a source-built canary does not carry the release workflow's
signed attestations unless those were independently produced and verified.

## Verify and recover

Before an upgrade with configuration or schema changes, stop active work and
make a consistent private backup of appdata, its matching master key, the current
Compose file and environment, and the running image digest. Retain the key in a
separate private backup as well. Losing it makes encrypted credentials
unrecoverable.

Verify the new version in the app footer and, from the container console, with
`lake-pass-bot version` and `lake-pass-bot doctor`. Check the actual running image and health;
a successful pull or a saved stack file alone does not prove deployment finished.

If deployment fails, inspect Portainer and establish that its update operation
has finished before trying recovery. Do not overlap updates or automatically
restore an older image against changed data. Reconcile the backed-up Compose,
environment, image, and matching appdata/key before a deliberate rollback. A
healthy older binary alone does not establish schema compatibility. Releases
without support for an external master key require it at the legacy appdata
location.

Keep booking evidence private. Never run the same Yodel identity concurrently
from a development instance and the deployed service. Follow the
[live testing procedure](live-testing.md) to distinguish a successful login or
synthetic browser test from a pass actually issued by Yodel.
