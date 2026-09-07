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
- fails on any HIGH or CRITICAL OS or library vulnerability reported by Trivy, including vulnerabilities without a published fix, using an explicitly empty exception policy;
- creates keyless GitHub/Sigstore provenance and a separate signed vulnerability-gate attestation only after Trivy passes, recording an empty exception list in the gate, then verifies all three attestations' predicate types, repository, workflow, source branch, and GitHub-hosted-runner identity;
- creates the semantic-version tags only after every gate passes and verifies that every published tag resolves to the accepted digest.

There are no current vulnerability exceptions. The image removes unused WebKit
GStreamer packages and Python build tools, including their cached wheels. Runtime
tests exercise Chromium, while a separate container without the service's home
tmpfs inspects the image for retained caches. Release scanning uses a policy
written by the executing workflow, so manually rebuilding an older tag cannot
reuse that tag's historical waivers while signing an empty exception list.

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
  - `PORTAINER_API_KEY`: a dedicated API token with access only to the Buntzen stack where the Portainer edition permits that restriction, including read access to that endpoint's Docker container list, container inspect and image inspect proxy APIs;
  - `BUNTZEN_HEALTH_URL`: exact Buntzen URL ending in `/healthz`;
- environment variables:
  - `PORTAINER_ENDPOINT_ID`;
  - `PORTAINER_STACK_ID`;
  - `PORTAINER_STACK_NAME`.

Keep endpoint addresses and tokens in the protected environment, not in repository files. Do not use a Portainer administrator password as the API token.

Protect `main` with required pull requests and passing `go`, `python-actions`, `integration` and `docker` checks from GitHub Actions, including for administrators. Disable force pushes and branch deletion. A sole owner can use zero required peer approvals while retaining the PR and check gates. Require owner approval for the deployment environment, restrict it to the `main` branch, and disable administrator bypass. Environment approval is a second gate, not a substitute for protecting the code that the runner will execute.

Require full-SHA action references and an allowlist of the actions used by these workflows, including nested Trivy setup and cache actions. Keep workflow token defaults read-only. Release Please's explicit job permissions and the repository setting permitting Actions to create pull requests are needed for release PRs. Token-created release PRs can require owner approval of their CI runs; approve those runs or dispatch CI on the exact release branch before merging.

Register one isolated runner with all four labels `self-hosted`, `linux`, `x64`, and `buntzen-deploy`. It needs outbound GitHub access, LAN access to Portainer and the Buntzen health endpoint, plus `bash`, `curl`, and `jq`. It does not need a Docker socket. Use a dedicated low-privilege host or VM and do not assign the `buntzen-deploy` label to a general-purpose runner.

Every version published by Release Please passes its verified image digest directly to the protected deployment workflow. A GitHub-hosted job verifies the digest, signed release provenance, signed SPDX SBOM, and signed Trivy-gate attestation, then re-runs the strict Trivy scan against the current vulnerability database and the empty exception policy. GitHub applies the environment approval only after those checks pass, before scheduling the LAN job. The LAN job checks out the workflow's exact `main` revision; a manual redeploy uses the selected current workflow revision even when the image was built from an older release. Pull-request code is never executed on that runner. `workflow_dispatch` remains available for an explicit redeploy of an already published digest.

## Existing Portainer stack setup

The workflow updates the existing `buntzen-pass-bot` standalone Docker Compose stack in place from `deploy/portainer.yml`. Do not create a second rollout stack or allocate another port or appdata directory. The deployment guard requires the selected stack to be file-based, with no Git configuration or automatic-update configuration, and already active with an exact `ok` response from `/healthz` before it will change anything. Confirm these environment values on the existing stack in Portainer:

- `BUNTZEN_IMAGE`: an initial `ghcr.io/<owner>/<repository>@sha256:<digest>` release coordinate;
- `BUNTZEN_WEB_PORT`: the configured host port already used by Buntzen;
- `BUNTZEN_APPDATA_PATH`: the configured absolute Buntzen appdata directory, owned by UID/GID 1001;
- optional `BUNTZEN_KEY_DIRECTORY_PATH`: an existing separate host directory containing the original `master.key`, owned by UID/GID 1001 with mode 0400 or 0600; set `BUNTZEN_MASTER_KEY_FILE=/run/buntzen-key/master.key` to use its read-only mount. Follow the [key relocation procedure](public-exposure.md#key-storage-and-recovery) before changing these values;
- `BUNTZEN_SECCOMP_PROFILE_PATH`: absolute path to this repository's `docker/seccomp_profile.json` as seen by Portainer's Compose process. For containerized Portainer, place the file inside its persistent `/data` mount and use a container-visible path such as `/data/buntzen/seccomp_profile.json`; a host-only path is not sufficient;
- `BLUEBUBBLES_URL` and, when using BlueBubbles, `BUNTZEN_BLUEBUBBLES_ENDPOINTS`: the [operator-approved provider origins and network pins](public-exposure.md#outbound-provider-access). Existing saved sources require this explicit policy too;
- `BUNTZEN_ALLOWED_HOSTS` and, only when required, `BUNTZEN_ALLOWED_ORIGINS`;
- for public HTTPS, `BUNTZEN_PUBLIC_ORIGIN` and `BUNTZEN_TRUSTED_PROXIES` after [private bootstrap and connector configuration](public-exposure.md); private-mode Host/origin aliases do not grant public UI access;
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

Before writing, the script requires exactly one immutable `BUNTZEN_IMAGE` in the current stack environment and verifies the existing container against it. It finds exactly one container with the stack's Compose project label and the `buntzen-pass-bot` service label, rechecks its identity and labels through inspect, and requires running/healthy state and exactly one `SCHEDULES_ENABLED=false` environment entry. Both its configured image coordinate and its actual Docker image ID must match the requested image. Docker image inspect resolves that coordinate to the configuration ID; registry manifest digests and configuration IDs are different identifiers. These are read-only Docker proxy calls; the runner needs no Docker socket or container-exec permission. An old stack using a mutable tag must first be reconciled privately to its actual immutable release coordinate; changing only its Portainer variable will not satisfy this guard.

The script then preserves the Portainer-managed environment, replaces only the Compose revision and image digest, reasserts the false schedule gate, and asks Portainer to pull the image. Portainer 2.45 [updates Compose stacks asynchronously](https://github.com/portainer/portainer/blob/2.45.0/api/http/handler/stacks/stack_update.go): its successful update response means deployment has started, not that the new container is ready. The script waits through Deploying (status 3), requires Active (status 1), repeats the running-container checks against the new image, and requires an exact `ok` response from `/healthz`. A healthy old image cannot prove success even after the stack becomes Active. Each verification phase defaults to a two-minute budget shared by all its HTTP requests. Redeploying the same digest is supported.

If the update fails, its response is ambiguous, or verification expires, the script stops with a failed deployment and sends no compensating update. Portainer 2.45 publishes final stack status before its stack-file cleanup callback finishes; another PUT can overwrite or remove the backup still used by that callback. Neither a settled status nor a fixed delay proves cleanup completion. Automatic rollback is therefore disabled. Inspect Portainer, establish that the previous operation has finished, and reconcile the backed-up Compose, environment, appdata/key compatibility and image before a deliberate recovery. The script makes at most one update request per invocation and never reports a failed update as recovered.

Do not edit the Buntzen stack in Portainer while a deployment is running: workflow concurrency serializes workflow runs, but it cannot serialize an operator's direct Portainer edits. Portainer 2.45 rejects another update while the stack is Deploying, but final status alone does not prove its cleanup callback has completed.

A healthy deployment still leaves schedules disabled. Complete setup/login, BlueBubbles connection, pairing, `auth-check`, dry-run, manual approval/cancellation, and one explicitly initiated automatic booking before enabling unattended schedules. Enabling schedules remains a deliberate Portainer change outside this workflow. Disable schedules again before every later rollout; the deployment preflight refuses a stack whose Portainer schedule gate is not exactly `false`.

Before deploying schema changes, stop active work and make a consistent private backup of appdata, plus the current Compose file, environment and image digest. Retain the database's matching master key in a separate private backup, including when its live path is outside appdata; losing that key makes encrypted credentials unrecoverable. This revision uses schema 6 for immediate booking timing, profile login URLs and ordered pass preferences. Restore matching appdata and key with the container stopped if rolling back to an older release; an older binary may start against newer additive columns while ignoring their behavior, so a healthy image-only rollback is insufficient evidence of compatibility. Releases predating `BUNTZEN_MASTER_KEY_FILE` also require the original key at their legacy appdata path.

Use [the live booking test procedure](live-testing.md) to test remaining availability with **Book now · manual approval** while schedules stay disabled. The fixed 15-minute expiry survives queueing and restarts; this path does not prove preparation or polling at the next release time.
