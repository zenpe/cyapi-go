import copy
import hashlib
import json
from pathlib import Path
import tempfile
import subprocess
import unittest
from unittest.mock import patch

import publish_release as publisher


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.work = tempfile.TemporaryDirectory()
        self.addCleanup(self.work.cleanup)
        self.directory = Path(self.work.name)
        checksums = []
        for name in publisher.ASSETS[:-1]:
            path = self.directory / name
            path.write_bytes(("test asset " + name).encode())
            checksums.append(publisher.checksum(path) + "  " + name)
        (self.directory / "SHA256SUMS").write_text("\n".join(checksums) + "\n", encoding="ascii")
        self.release = {"draft": False, "assets": [], "html_url": "https://example.invalid/release", "name": "User title"}
        self.calls = []
        self.actual_sha = "expected-commit"

    def asset(self, name):
        return {"name": name, "digest": "sha256:" + publisher.checksum(self.directory / name)}

    def gh(self, *args):
        self.calls.append(args)
        if args[0] == "api":
            return json.dumps({"sha": self.actual_sha})
        self.assertNotIn("--clobber", args)
        action = args[1]
        if action == "create":
            self.release = {"draft": True, "assets": [], "html_url": "https://example.invalid/release"}
        elif action == "upload":
            for file in args[args.index("--repo") + 2:]:
                name = Path(file).name
                self.assertNotIn(name, [asset["name"] for asset in self.release["assets"]])
                self.release["assets"].append(self.asset(name))
        elif action == "edit":
            if "--draft=false" in args:
                self.release["draft"] = False
        else:
            self.fail("Unexpected gh action: " + action)
        return ""

    def publish(self, keep_draft=True):
        with patch.object(publisher, "gh", self.gh), patch.object(publisher, "release_info", lambda *_: copy.deepcopy(self.release)):
            publisher.publish(self.directory, "owner/repo", "v0.0.1", "expected-commit", keep_draft, False)

    def test_fill_empty_public_release_without_changing_metadata(self):
        self.publish()
        self.assertEqual(len(self.release["assets"]), 7)
        self.assertFalse(self.release["draft"])
        self.assertEqual(self.release["name"], "User title")
        self.assertFalse(any(call[:2] == ("release", "edit") for call in self.calls))

    def test_resume_partial_release_without_overwriting(self):
        self.release["assets"] = [self.asset(name) for name in publisher.ASSETS[:2]]
        self.publish()
        uploads = [call for call in self.calls if call[:2] == ("release", "upload")]
        self.assertEqual(len(uploads), 1)
        self.assertEqual(len(uploads[0][uploads[0].index("--repo") + 2:]), 5)

    def test_matching_complete_release_is_idempotent(self):
        self.release["assets"] = [self.asset(name) for name in publisher.ASSETS]
        self.publish()
        self.assertFalse(any(call[0] == "release" for call in self.calls))

    def test_conflicting_asset_blocks_all_uploads(self):
        self.release["assets"] = [{"name": publisher.ASSETS[-1], "digest": "sha256:wrong"}]
        with self.assertRaisesRegex(ValueError, "refusing to overwrite"):
            self.publish()
        self.assertFalse(any(call[0] == "release" for call in self.calls))

    def test_new_release_stays_draft_until_requested(self):
        self.release = None
        self.publish()
        self.assertTrue(self.release["draft"])
        self.release = None
        self.publish(keep_draft=False)
        self.assertFalse(self.release["draft"])

    def test_moved_tag_blocks_publication(self):
        self.actual_sha = "different-commit"
        with self.assertRaisesRegex(ValueError, "Tag changed"):
            self.publish()
        self.assertFalse(any(call[0] == "release" for call in self.calls))


class ReleaseLookupTests(unittest.TestCase):
    def test_public_release_uses_tag_endpoint(self):
        release = {"tag_name": "v0.0.2", "draft": False, "assets": []}
        with patch.object(publisher, "gh", return_value=json.dumps(release)) as call:
            self.assertEqual(publisher.release_info("owner/repo", "v0.0.2"), release)
            self.assertEqual(call.call_count, 1)

    def test_draft_found_in_paginated_authenticated_list(self):
        release = {"tag_name": "v0.0.2", "draft": True, "assets": []}
        missing = subprocess.CalledProcessError(1, "gh", stderr="HTTP 404: Not Found")
        pages = [[{"tag_name": "v0.0.1"}], [release]]
        with patch.object(publisher, "gh", side_effect=[missing, json.dumps(pages)]) as call:
            self.assertEqual(publisher.release_info("owner/repo", "v0.0.2"), release)
            self.assertEqual(call.call_args.args, ("api", "repos/owner/repo/releases?per_page=100", "--paginate", "--slurp"))

    def test_nonexistent_release(self):
        missing = subprocess.CalledProcessError(1, "gh", stderr="HTTP 404: Not Found")
        with patch.object(publisher, "gh", side_effect=[missing, "[]"]):
            self.assertIsNone(publisher.release_info("owner/repo", "v0.0.2"))

    def test_auth_failure_is_not_treated_as_missing(self):
        denied = subprocess.CalledProcessError(1, "gh", stderr="HTTP 403: Forbidden")
        with patch.object(publisher, "gh", side_effect=denied) as call:
            with self.assertRaises(subprocess.CalledProcessError):
                publisher.release_info("owner/repo", "v0.0.2")
            self.assertEqual(call.call_count, 1)


if __name__ == "__main__":
    unittest.main()
