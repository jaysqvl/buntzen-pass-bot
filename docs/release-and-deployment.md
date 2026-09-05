# Release and Portainer deployment

The release path has two deliberately separate trust boundaries:

1. GitHub-hosted runners build and verify a release image.
2. A protected, manually approved environment may deploy an already verified image digest to the existing LAN Buntzen stack.

No pull-request workflow targets the LAN runner, and a deployment never accepts a branch, tag, image name, or Compose file from the operator. It accepts only an immutable digest and an exact stack-name confirmation.

## Release image

`release-please.yml` calls `release-image.yml` in the same workflow run when Release Please creates a `buntzen-pass-bot-v*` release. This is intentional: tags and releases created with the repository `GITHUB_TOKEN` do not trigger a second workflow.

The publication job:

- resolves the published GitHub release and requires its semantic-version tag to point to a commit on `main`;
- checks out that exact commit and builds `linux/amd64` once;
- pushes the build under `sha-<full commit>`, then promotes the accepted digest to the Release Please `v<major>.<minor>.<patch>`, `<major>.<minor>.<patch>`, and `<major>.<minor>` tags (no mutable `latest` tag);
- records the resulting immutable `sha256` digest;
- attaches and validates BuildKit max-mode provenance and an SPDX 2.3 SBOM, then signs that SBOM separately with GitHub/Sigstore;
- fails on any HIGH or CRITICAL OS or library vulnerability reported by Trivy, including vulnerabilities without a published fix, except the exact package-scoped and expiring exception in `.trivyignore.yaml`;
- creates keyless GitHub/Sigstore provenance and a separate signed vulnerability-gate attestation only after Trivy passes, recording the scoped exception in the gate, then verifies all three attestations' predicate types, repository, workflow, source branch, and GitHub-hosted-runner identity;
- creates the semantic-version tags only after every gate passes and verifies that every published tag resolves to the accepted digest.

The current temporary exceptions are recorded as exact package PURLs in `.trivyignore.yaml`: `CVE-2025-3887` for the `gstreamer1.0-plugins-bad` and `libgstreamer-plugins-bad1.0-0` Ubuntu packages, plus `GHSA-6v7p-g79w-8964` for `msgpack@1.1.2` and `CVE-2025-47273` for `setuptools@70.3.0`. The Playwright Chromium booking path does not use the affected GStreamer plugins. The Python findings persist in base-layer catalog metadata even though the build tooling was removed and the runtime smoke test verifies that msgpack and setuptools are absent from the final image. Every exception expires on 2026-09-30; a changed package PURL or the passing of that date makes Trivy fail until the exception is deliberately reviewed or removed.

All third-party actions in these workflows are pinned to full commit SHAs. Dependabot continues to propose reviewed updates.

If a release job is interrupted, use **Re-run failed jobs** on that original run. A retry repeats all trust checks and may repeat the failed job's build; it never bypasses scanning or attestation. **Publish release image** also supports manual dispatch from `main` using an existing published component tag and its exact commit SHA. That path repeats the release checks and passes the resulting digest to the protected deployment workflow.

The workflow summary prints the deployment coordinate:

```text
ghcr.io/<owner>/<repository>@sha256:<digest>
```

An operator can independently verify it after authenticating to GHCR:

```bash
gh attestation verify \
  oci://ghcr.io/<owner>/<repository>@sha256:<digest> \
  --repo <owner>/<repository> \
  --signer-workflow <owner>/<repository>/.github/workflows/release-image.yml \
  --source-ref refs/heads/main \
  --deny-self-hosted-runners \
  --predicate-type https://slsa.dev/provenance/v1

gh attestation verify \
  oci://ghcr.io/<owner>/<repository>@sha256:<digest> \
  --repo <owner>/<repository> \
  --signer-workflow <owner>/<repository>/.github/workflows/release-image.yml \
  --source-ref refs/heads/main \
  --deny-self-hosted-runners \
  --predicate-type https://spdx.dev/Document/v2.3

gh attestation verify \
  oci://ghcr.io/<owner>/<repository>@sha256:<digest> \
  --repo <owner>/<repository> \
  --signer-workflow <owner>/<repository>/.github/workflows/release-image.yml \
  --source-ref refs/heads/main \
  --deny-self-hosted-runners \
  --predicate-type https://github.com/<owner>/<repository>/attestations/trivy/v1
```

If the repository is private, ensure the Portainer Docker environment can pull the inherited private GHCR package. Use a read-only package credential; no registry write credential belongs on the Docker host.

## Protected Buntzen environment

Create a GitHub Environment named `portainer-buntzen` with:

- required reviewers;
- deployment branches restricted to `main`;
- environment secrets:
  - `PORTAINER_URL`: exact Portainer HTTP(S) origin, with no path;
  - `PORTAINER_API_KEY`: a dedicated API token with access only to the Buntzen stack where the Portainer edition permits that restriction;
  - `BUNTZEN_HEALTH_URL`: exact Buntzen URL ending in `/healthz`;
- environment variables:
  - `PORTAINER_ENDPOINT_ID`;
  - `PORTAINER_STACK_ID`;
  - `PORTAINER_STACK_NAME`.

Keep endpoint addresses and tokens in the protected environment, not in repository files. Do not use a Portainer administrator password as the API token.

Protect `main` with required pull-request review and passing CI checks before attaching a LAN runner. Disable force pushes and branch deletion. Environment approval is a second gate, not a substitute for protecting the code that the runner will execute.

Register one isolated runner with all four labels `self-hosted`, `linux`, `x64`, and `buntzen-deploy`. It needs outbound GitHub access, LAN access to Portainer and the Buntzen health endpoint, plus `bash`, `curl`, and `jq`. It does not need a Docker socket. Use a dedicated low-privilege host or VM and do not assign the `buntzen-deploy` label to a general-purpose runner.

Every version published by Release Please passes its verified image digest directly to the protected deployment workflow. A GitHub-hosted job verifies the digest, signed release provenance, signed SPDX SBOM, and signed Trivy-gate attestation, then re-runs the strict Trivy scan against the current vulnerability database and the reviewed, expiring exception file. GitHub applies the environment approval only after those checks pass, before scheduling the LAN job. The LAN job checks out the workflow's exact `main` revision; a manual redeploy uses the selected current workflow revision even when the image was built from an older release. Pull-request code is never executed on that runner. `workflow_dispatch` remains available for an explicit redeploy of an already published digest.

## Existing Portainer stack setup

The workflow updates the existing `buntzen-pass-bot` standalone Docker Compose stack in place from `deploy/portainer.yml`. Do not create a second rollout stack or allocate another port or appdata directory. The deployment guard requires the selected stack to be file-based, with no Git configuration or automatic-update configuration, and already active with an exact `ok` response from `/healthz` before it will change anything. Confirm these environment values on the existing stack in Portainer:

- `BUNTZEN_IMAGE`: an initial `ghcr.io/<owner>/<repository>@sha256:<digest>` release coordinate;
- `BUNTZEN_WEB_PORT`: the configured host port already used by Buntzen;
- `BUNTZEN_APPDATA_PATH`: the configured absolute Buntzen appdata directory, owned by UID/GID 1001;
- `BUNTZEN_SECCOMP_PROFILE_PATH`: absolute path to this repository's `docker/seccomp_profile.json` as seen by Portainer's Compose process. For containerized Portainer, place the file inside its persistent `/data` mount and use a container-visible path such as `/data/buntzen/seccomp_profile.json`; a host-only path is not sufficient;
- `BLUEBUBBLES_URL`;
- `BUNTZEN_ALLOWED_HOSTS` and, only when required, `BUNTZEN_ALLOWED_ORIGINS`;
- optional `BUNTZEN_SETUP_TOKEN`, `BUNTZEN_YODEL_ORIGINS`, `BUNTZEN_LOG_LEVEL`, `BUNTZEN_DEBUG`, and `MAX_CONCURRENT_JOBS`;
- `SCHEDULES_ENABLED=false`.

The checked-in deployment Compose file hard-codes `SCHEDULES_ENABLED: "false"`; neither a workflow input nor a Portainer environment value can enable it during a rollout. The deployment script also refuses an inactive or unhealthy stack, a Git-backed or auto-updated stack, an unexpected environment shape, or any existing stack whose Portainer environment does not contain exactly one false schedule gate. Use the existing stack's exact ID, endpoint ID, and name so a configuration mistake cannot silently target a different stack.

Never share this appdata with another container or native macOS run, and never exercise the same Yodel identity concurrently from two installations.

## Deploy and observe

For a normal release:

1. Merge the Release Please pull request.
2. Release Please publishes and verifies the immutable image.
3. Approve the `portainer-buntzen` environment deployment.

The approved deployment replaces the existing `buntzen-pass-bot` container in the same stack, on the same port and appdata path. It never creates a version-specific or parallel rollout container. For a manual redeploy, use **Actions → Deploy Buntzen → Run workflow**, paste the immutable `sha256:<digest>`, and type the exact protected stack name.

The script validates the stack identity and current health, preserves its Portainer-managed environment, replaces only the Compose revision and image digest, reasserts the false schedule gate, and asks Portainer to pull the image. It checks `/healthz` for up to two minutes. If the update API fails or the new revision never returns the exact `ok` response, it restores the prior Compose file and environment. The workflow reports a successful rollback only after Portainer again reports the stack active and `/healthz` returns exact `ok`; an unverified rollback is reported as a failure.

Portainer 2.39.6 does not expose an ETag or compare-and-swap guard for stack updates. Do not edit the Buntzen stack in Portainer while a deployment is running: workflow concurrency serializes workflow runs, but it cannot serialize an operator's direct Portainer edits.

A healthy deployment still leaves schedules disabled. Complete setup/login, BlueBubbles connection, pairing, `auth-check`, dry-run, manual approval/cancellation, and one explicitly initiated automatic booking before enabling unattended schedules. Enabling schedules remains a deliberate Portainer change outside this workflow. Disable schedules again before every later rollout; the deployment preflight refuses a stack whose Portainer schedule gate is not exactly `false`.
