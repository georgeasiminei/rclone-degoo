"""Offline checks for health-first Degoo status and per-file upload state."""

from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import shlex
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import info
import install
import status


class StatusTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home"
        self.home.mkdir(mode=0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.root = self.home / ".local/share/rclone-degoo"
        self.root.mkdir(parents=True, mode=0o700)
        (self.home / ".local").chmod(0o700)
        (self.home / ".local/share").chmod(0o700)
        for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs", "scripts"):
            (self.root / name).mkdir(mode=0o700)
        self.manifest = {
            "version": 1,
            "root_folder_id": "opaque-vault-folder-id",
            "remote_folder_name": "Vault",
            "remote_name": install.REMOTE_NAME,
            "mountpoint": "mount",
            "binary": "bin/rclone",
            "vault_config": "config/vault.json",
            "rclone_config": "config/rclone.conf",
            "cache_dir": "cache",
            "pidfile": "mount.pid",
            "service_links": [],
            "service_config_home": "",
        }
        self.write_private(self.root / "install.json", self.manifest)
        self.write_private(
            self.root / "config/vault.json",
            {
                "version": 1,
                "remote": "degoo-live-test:",
                "password": "private-test-vault-secret",
                "cache_dir": str(self.root / "cache"),
            },
        )
        self.write_private(self.root / "config/rclone.conf", "[degoo-live-test]\ntype = degoo\n")
        self.write_private(self.root / "config/auth.json", {"token": "opaque-token", "refresh_token": "opaque-refresh"})
        self.binary = self.root / "bin/rclone"
        self.write_binary("Pending local uploads: 0 files, 0 bytes\n")
        self.live_uploads = status.live_uploads
        live_patch = mock.patch.object(status, "live_uploads", return_value={"queued": 0, "uploading": 0, "retrying": 0})
        live_patch.start()
        self.addCleanup(live_patch.stop)

    @staticmethod
    def write_private(path: Path, value: object) -> None:
        data = value if isinstance(value, str) else json.dumps(value)
        path.write_text(data, encoding="utf-8")
        path.chmod(0o600)

    def write_binary(self, output: str, exit_code: int = 0) -> None:
        self.binary.write_text(
            "#!/bin/sh\nprintf '%s' " + shlex.quote(output) + f"\nexit {exit_code}\n",
            encoding="utf-8",
        )
        self.binary.chmod(0o700)

    def write_cache_metadata(self, relative: str, metadata: dict[str, object], *, data: bool = True) -> Path:
        cache_name = status.cache_name("degoo-live-test:")
        metadata_path = self.root / "cache/vfsMeta" / cache_name / relative
        data_path = self.root / "cache/vfs" / cache_name / relative
        metadata_path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        metadata_path.parent.chmod(0o700)
        data_path.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        data_path.parent.chmod(0o700)
        for tree in (metadata_path.parent, data_path.parent):
            while tree != self.root / "cache":
                tree.chmod(0o700)
                tree = tree.parent
        self.write_private(metadata_path, metadata)
        if data:
            self.write_private(data_path, "cached bytes")
        return metadata_path

    def test_health_errors_are_auth_first_and_zero_pending_is_not_cloud_confirmation(self) -> None:
        (self.root / "config/auth.json").unlink()
        local_file = self.root / "mount" / "left-behind.txt"
        local_file.write_text("local data", encoding="utf-8")
        local_file.chmod(0o600)
        with (
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(status, "pending_uploads", return_value=(0, 0)),
        ):
            report, code = status.build_report(self.root, self.manifest)

        output = status.format_report(report)
        self.assertEqual(code, 1)
        self.assertTrue(output.splitlines()[0].startswith("AUTHENTICATION ERROR:"))
        self.assertLess(output.index("AUTHENTICATION ERROR:"), output.index("MOUNT ERROR:"))
        self.assertIn("these files are local only", output)
        self.assertIn("Pending local uploads: 0 files, 0 bytes", output)
        self.assertIn("does not verify cloud contents, completeness, or integrity", output)

    def test_failed_configured_service_is_a_health_error(self) -> None:
        with (
            mock.patch.object(info, "mounted_state", return_value="mounted"),
            mock.patch.object(info, "service_status", return_value="enabled; failed"),
            mock.patch.object(status, "pending_uploads", return_value=(0, 0)),
        ):
            report, code = status.build_report(self.root, self.manifest)

        self.assertEqual(code, 1)
        self.assertIn("SERVICE ERROR:", status.format_report(report))

    def test_refreshable_expired_token_is_advisory_not_health_failure(self) -> None:
        self.write_private(
            self.root / "config/auth.json",
            {"token": "header.eyJleHAiOjF9.signature", "refresh_token": "still-private"},
        )
        with (
            mock.patch.object(info, "mounted_state", return_value="mounted"),
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(status, "pending_uploads", return_value=(0, 0)),
        ):
            report, code = status.build_report(self.root, self.manifest)

        self.assertEqual(code, 0)
        self.assertEqual(report["health_errors"], [])
        self.assertNotIn("still-private", status.format_report(report))

    def test_native_pending_count_is_parsed_with_bounded_output(self) -> None:
        self.assertEqual(status.pending_uploads(self.root), (0, 0))
        self.write_binary("Pending local uploads: 7 files, 2048 bytes\n")
        self.assertEqual(status.pending_uploads(self.root), (7, 2048))
        self.write_binary("Pending local uploads: 7 files, 2048 bytes\n" + "x" * (status.MAX_STATUS_OUTPUT + 10))
        self.assertIsNone(status.pending_uploads(self.root))

    def test_unmounted_mount_file_is_local_only(self) -> None:
        local_file = self.root / "mount" / "notes.txt"
        local_file.write_text("do not modify", encoding="utf-8")
        local_file.chmod(0o600)

        records, truncated = status.inspect_path(
            self.root, "notes.txt", mount_state="not mounted", cwd=self.root / "mount"
        )

        self.assertFalse(truncated)
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["state"], "local-only")
        self.assertIn("not part of the vault", records[0]["detail"])
        self.assertEqual(local_file.read_text(encoding="utf-8"), "do not modify")

    def test_offline_dirty_metadata_reports_pending_after_restart(self) -> None:
        self.write_cache_metadata("folder/file.bin", {"Dirty": True, "Size": 12, "Fingerprint": ""})

        records, _ = status.inspect_path(
            self.root, "folder/file.bin", mount_state="not mounted", cwd=self.root / "mount"
        )

        self.assertEqual(records[0]["state"], "pending")
        self.assertEqual(records[0]["evidence"], "private local cache metadata")
        self.assertEqual(records[0]["size_bytes"], 12)
        self.assertIn("survives process restarts", records[0]["detail"])

    def test_live_upload_failures_are_reported_before_pending_counts(self) -> None:
        with (
            mock.patch.object(info, "mounted_state", return_value="mounted"),
            mock.patch.object(info, "service_status", return_value="enabled; active"),
            mock.patch.object(status, "live_uploads", return_value={"queued": 1, "uploading": 0, "retrying": 1}),
            mock.patch.object(status, "pending_uploads", return_value=(1, 12)),
        ):
            report, code = status.build_report(self.root, self.manifest)
        self.assertEqual(code, 1)
        output = status.format_report(report)
        self.assertLess(output.index("UPLOAD ERROR:"), output.index("Pending local uploads:"))

    def test_offline_clean_fingerprint_is_only_cached_upload_evidence(self) -> None:
        self.write_cache_metadata("folder/file.bin", {"Dirty": False, "Size": 12, "Fingerprint": "remote-hash"})

        records, _ = status.inspect_path(
            self.root, "folder/file.bin", mount_state="not mounted", cwd=self.root / "mount"
        )

        self.assertEqual(records[0]["state"], "cached-uploaded")
        self.assertFalse(records[0]["live_cloud_verified"])
        self.assertIn("may be stale", records[0]["detail"])

    def test_mounted_status_reads_only_the_upload_status_xattr(self) -> None:
        file_path = self.root / "mount" / "current.txt"
        file_path.write_text("mounted view", encoding="utf-8")
        file_path.chmod(0o600)
        with mock.patch.object(status.os, "getxattr", return_value=b"uploaded") as getxattr:
            records, _ = status.inspect_path(
                self.root, "current.txt", mount_state="mounted", cwd=self.root / "mount"
            )

        self.assertEqual(records[0]["state"], "uploaded")
        self.assertFalse(records[0]["live_cloud_verified"])
        getxattr.assert_called_once_with(file_path, "user.rclone.upload-status", follow_symlinks=False)

    def test_path_refuses_traversal_and_symlink_components(self) -> None:
        outside = self.base / "outside.txt"
        outside.write_text("outside", encoding="utf-8")
        (self.root / "mount" / "link").symlink_to(outside)
        for selected in ("../outside.txt", "link"):
            with self.subTest(path=selected), self.assertRaises(status.StatusError):
                status.inspect_path(
                    self.root, selected, mount_state="not mounted", cwd=self.root / "mount"
                )

    def test_invalid_file_path_keeps_authentication_error_first(self) -> None:
        (self.root / "config/auth.json").unlink()
        with (
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(status, "pending_uploads", return_value=(0, 0)),
        ):
            report, code = status.build_report(self.root, self.manifest, local_path="../outside")
        self.assertEqual(code, 1)
        self.assertTrue(report["health_errors"][0].startswith("AUTHENTICATION ERROR:"))
        self.assertTrue(report["health_errors"][-1].startswith("FILE STATUS ERROR:"))

    def test_unreadable_selected_directory_is_not_reported_as_empty(self) -> None:
        with mock.patch.object(status.os, "scandir", side_effect=PermissionError("private provider text")):
            with self.assertRaisesRegex(status.StatusError, "selected directory could not be read"):
                status.inspect_path(self.root, ".", mount_state="mounted")

    def test_live_status_accepts_only_bounded_aggregate_counters(self) -> None:
        # Bypass the report fixture's live_uploads mock to exercise the transport parser.
        connection = mock.Mock()
        response = connection.getresponse.return_value
        response.status = 200
        with (
            mock.patch.object(status.local_sync, "_socket_path", return_value=self.root / "mount.sock"),
            mock.patch.object(status.local_sync, "UnixHTTPConnection", return_value=connection),
        ):
            for payload, expected in (
                (b'{"queued":1,"uploading":0,"retrying":1}', {"queued": 1, "uploading": 0, "retrying": 1}),
                (b'{"queued":true,"uploading":0,"retrying":0}', None),
                (b'{"queued":0,"uploading":0,"retrying":0,"name":"private"}', None),
                (b'not JSON', None),
            ):
                response.read.return_value = payload
                self.assertEqual(self.live_uploads(self.root), expected)
        connection.close.assert_called()

    def test_json_mode_returns_health_and_file_state(self) -> None:
        file_path = self.root / "mount" / "current.txt"
        file_path.write_text("mounted view", encoding="utf-8")
        file_path.chmod(0o600)
        stdout = io.StringIO()
        stderr = io.StringIO()
        with (
            mock.patch.object(info, "mounted_state", return_value="mounted"),
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(status, "pending_uploads", return_value=(1, 5)),
            mock.patch.object(status.os, "getxattr", return_value=b"pending"),
            contextlib.redirect_stdout(stdout),
            contextlib.redirect_stderr(stderr),
        ):
            code = status.main(["--root", str(self.root), "--json", "current.txt"])

        self.assertEqual(code, 0, stderr.getvalue())
        result = json.loads(stdout.getvalue())
        self.assertEqual(result["pending"]["files"], 1)
        self.assertEqual(result["files"][0]["state"], "pending")


if __name__ == "__main__":
    unittest.main()
