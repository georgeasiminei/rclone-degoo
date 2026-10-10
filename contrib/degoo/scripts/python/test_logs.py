"""Offline checks for private access to the contained Degoo logs."""

from __future__ import annotations

import contextlib
import io
import json
import os
import sys
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import install
import logs


class LogsTest(unittest.TestCase):
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
        for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs"):
            (self.root / name).mkdir(mode=0o700)
        manifest = {
            "version": 1,
            "root_folder_id": "folder-id",
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
        manifest_path = self.root / "install.json"
        manifest_path.write_text(json.dumps(manifest), encoding="utf-8")
        manifest_path.chmod(0o600)

    def test_default_shows_last_100_lines_from_all_contained_logs(self) -> None:
        mount_lines = [f"mount line {number}\n" for number in range(150)]
        (self.root / "logs/mount.log").write_text("".join(mount_lines), encoding="utf-8")
        (self.root / "logs/mount.log").chmod(0o600)
        (self.root / "logs/login.log").write_text("login completed\n", encoding="utf-8")
        (self.root / "logs/login.log").chmod(0o600)
        (self.root / "logs/setup.log").write_text("setup error details\n", encoding="utf-8")
        (self.root / "logs/setup.log").chmod(0o600)
        (self.root / "logs/status.log").write_text("status error details\n", encoding="utf-8")
        (self.root / "logs/status.log").chmod(0o600)

        output = logs.report(self.root)

        self.assertIn("mount line 50", output)
        self.assertIn("mount line 149", output)
        self.assertNotIn("mount line 49\n", output)
        self.assertIn("login completed", output)
        self.assertIn("setup error details", output)
        self.assertIn("status error details", output)
        self.assertIn("journalctl --user -u rclone-degoo.service -n 100 --no-pager", output)

    def test_missing_logs_are_reported_without_reading_other_files(self) -> None:
        secret = self.root / "config/auth.json"
        secret.write_text('{"token":"never-display-this"}', encoding="utf-8")
        secret.chmod(0o600)

        output = logs.report(self.root)

        self.assertIn("No mount.log yet", output)
        self.assertIn("No login.log yet", output)
        self.assertIn("No setup.log yet", output)
        self.assertIn("No status.log yet", output)
        self.assertNotIn("never-display-this", output)

    def test_log_symlinks_and_non_private_files_are_refused(self) -> None:
        outside = self.base / "outside.log"
        outside.write_text("outside secret log\n", encoding="utf-8")
        outside.chmod(0o600)
        (self.root / "logs/mount.log").symlink_to(outside)
        with self.assertRaises(logs.LogsError):
            logs.report(self.root)

        (self.root / "logs/mount.log").unlink()
        log_path = self.root / "logs/login.log"
        log_path.write_text("too broadly readable\n", encoding="utf-8")
        log_path.chmod(0o644)
        with self.assertRaises(logs.LogsError):
            logs.report(self.root)

    def test_display_redacts_credential_fields_bearer_jwts_and_signed_urls(self) -> None:
        log_path = self.root / "logs/mount.log"
        log_path.write_text(
            "access_token=access-token-secret\n"
            '{"refresh_token":"refresh-token-secret"}\n'
            "Authorization: Bearer bearer-value-secret\n"
            "JWT eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzZWNyZXQifQ.signaturesegment123\n"
            "https://cdn.example/object?X-Amz-Credential=credential-secret&X-Amz-Signature=signed-secret\n",
            encoding="utf-8",
        )
        log_path.chmod(0o600)

        output = logs.report(self.root)

        for secret in (
            "access-token-secret",
            "refresh-token-secret",
            "bearer-value-secret",
            "eyJhbGciOiJIUzI1NiJ9",
            "credential-secret",
            "signed-secret",
        ):
            self.assertNotIn(secret, output)
        self.assertIn("[redacted]", output)
        self.assertIn("?[query redacted]", output)

    def test_auth_marker_requires_timestamp_newer_than_saved_auth_state(self) -> None:
        now = datetime.now().replace(microsecond=0).timestamp()
        auth_path = self.root / "config/auth.json"
        auth_path.write_text("{}", encoding="utf-8")
        auth_path.chmod(0o600)
        auth_path.touch()
        auth_time = now - 60
        os.utime(auth_path, (auth_time, auth_time))
        stamp = datetime.fromtimestamp(now).strftime("%Y/%m/%d %H:%M:%S")
        log_path = self.root / "logs/mount.log"
        log_path.write_text(f"{stamp} ERROR : degoo login required (auth HTTP 403)\n", encoding="utf-8")
        log_path.chmod(0o600)

        self.assertTrue(logs.auth_required_after(self.root / "logs", auth_time, now=now))
        self.assertFalse(logs.auth_required_after(self.root / "logs", now, now=now))

    def test_auth_marker_rejects_unstamped_or_unrecognized_lines(self) -> None:
        auth_time = 1_800_000_000
        log_path = self.root / "logs/mount.log"
        log_path.write_text(
            "degoo login required without a logger timestamp\n"
            "2026/10/10 12:00:00 ERROR : degoo token refresh failed (HTTP 403)\n",
            encoding="utf-8",
        )
        log_path.chmod(0o600)
        now = datetime(2026, 10, 10, 12, 5).timestamp()
        self.assertFalse(logs.auth_required_after(self.root / "logs", auth_time, now=now))

        log_path.write_text("", encoding="utf-8")
        status_log = self.root / "logs/status.log"
        status_log.write_text("2026/10/10 12:00:00 ERROR : degoo login required (auth HTTP 403)\n", encoding="utf-8")
        status_log.chmod(0o600)
        self.assertFalse(logs.auth_required_after(self.root / "logs", auth_time, now=now))

    def test_cli_help_describes_follow_and_default_tail(self) -> None:
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout), self.assertRaises(SystemExit) as raised:
            logs.main(["--help"])
        self.assertEqual(raised.exception.code, 0)
        self.assertIn("last 100 lines", stdout.getvalue())
        self.assertIn("--follow", stdout.getvalue())

    def test_follow_waits_for_a_log_created_after_start(self) -> None:
        output = io.StringIO()
        calls = 0

        def create_then_interrupt(_: float) -> None:
            nonlocal calls
            calls += 1
            if calls == 1:
                log_path = self.root / "logs/mount.log"
                log_path.write_text("created after follow started\n", encoding="utf-8")
                log_path.chmod(0o600)
                return
            raise KeyboardInterrupt

        with contextlib.redirect_stdout(output), mock.patch.object(logs.time, "sleep", side_effect=create_then_interrupt):
            with self.assertRaises(KeyboardInterrupt):
                logs._follow(self.root, {})
        self.assertIn("created after follow started", output.getvalue())

    def test_root_path_rejects_parent_traversal(self) -> None:
        with self.assertRaises(logs.LogsError):
            logs.validate_root(str(self.root / ".." / "elsewhere"))


if __name__ == "__main__":
    unittest.main()
