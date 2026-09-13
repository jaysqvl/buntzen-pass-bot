"""Retained publication and Compose contracts from the removed deploy harness."""

from __future__ import annotations

import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]


class WorkflowContractTests(unittest.TestCase):
    def test_release_publication_remains_reusable_and_recoverable(self) -> None:
        image = (ROOT / ".github/workflows/release-image.yml").read_text()
        release = (ROOT / ".github/workflows/release-please.yml").read_text()
        self.assertIn("workflow_call:", image)
        self.assertIn("workflow_dispatch:", image)
        self.assertIn("value: ${{ jobs.publish.outputs.image_digest }}", image)
        self.assertIn("image_digest: ${{ steps.build.outputs.digest }}", image)
        self.assertIn("uses: ./.github/workflows/release-image.yml", release)
        self.assertIn("startsWith(needs.release-please.outputs.tag_name, 'lake-pass-bot-v')", release)

    def test_latest_waits_for_all_publication_gates(self) -> None:
        image = (ROOT / ".github/workflows/release-image.yml").read_text()
        publication, latest = image.split("\n  promote-latest:\n")
        gates = (
            "Smoke test the immutable release image",
            "Verify the registry digest and BuildKit attestations",
            "Reject high and critical image vulnerabilities",
            "Sign the SPDX SBOM", "Sign the passed vulnerability gate",
            "Sign GitHub build provenance", "Verify signed provenance",
        )
        for gate in gates:
            self.assertLess(publication.index(f"name: {gate}"), publication.index("name: Promote the accepted digest"))
        self.assertIn("needs: publish", latest)
        self.assertIn("group: release-image-latest", latest)
        # GitHub otherwise replaces pending jobs; an old manual rebuild could
        # discard the newest release before its serialized promotion starts.
        self.assertEqual(latest.splitlines().count("      queue: max"), 1)
        self.assertEqual(image.count("queue:"), 1)
        self.assertIn("cancel-in-progress: false", latest)
        self.assertIn("ref: ${{ github.workflow_sha }}", latest)
        self.assertIn("RELEASE_DIGEST: ${{ needs.publish.outputs.image_digest }}", latest)
        self.assertIn("run: python3 scripts/release/promote_latest.py", latest)

    def test_compose_retains_container_identity_logs_and_schedule_default(self) -> None:
        portainer = (ROOT / "deploy/portainer.yml").read_text()
        self.assertEqual(portainer.count("    container_name: lake-pass-bot\n"), 1)
        self.assertIn('      LAKE_PASS_LOG_LEVEL: "${LAKE_PASS_LOG_LEVEL-${BUNTZEN_LOG_LEVEL:-info}}"', portainer)
        self.assertIn('      LAKE_PASS_DEBUG: "${LAKE_PASS_DEBUG-${BUNTZEN_DEBUG:-false}}"', portainer)
        self.assertIn('      SCHEDULES_ENABLED: "false"', portainer)
        for path in ("deploy/portainer.yml", "docker-compose.yml"):
            compose = (ROOT / path).read_text()
            with self.subTest(path=path):
                self.assertEqual(compose.count("      driver: json-file\n"), 1)
                self.assertIn('max-size: "10m"', compose)
                self.assertIn('max-file: "3"', compose)


if __name__ == "__main__":
    unittest.main()
