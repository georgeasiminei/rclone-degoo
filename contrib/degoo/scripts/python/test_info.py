"""Offline tests for the contained Degoo installation status report."""

from __future__ import annotations

import base64
import json
import os
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import info
import install


class InfoTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home with spaces"
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
            "root_folder_id": "own-folder-id",
            "remote_folder_name": "Vault",
            "remote_path": ["My Device", "Vault"],
            "parent_folder_id": "device-id",
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
        self.write_private(self.root / "config/mount.json", install.DEFAULT_MOUNT_SETTINGS)
        self.write_private(
            self.root / "config/vault.json",
            {"version": 1, "remote": "degoo-live-test:", "password": "vault-secret", "password_salt": "salt-secret", "chunk_size": "1Gi", "max_object_size": "19Gi"},
        )
        self.write_private(self.root / "config/rclone.conf", "[degoo-live-test]\ntype = degoo\n")
        self.write_private(self.root / "config/auth.json", {"token": "opaque-access-secret", "refresh_token": "refresh-secret"})
        self.status_script("Pending local uploads: 3 files, 4096 bytes\n")

    @staticmethod
    def write_private(path: Path, value: object) -> None:
        if isinstance(value, str):
            data = value
        else:
            data = json.dumps(value)
        path.write_text(data, encoding="utf-8")
        path.chmod(0o600)

    def status_script(self, output: str, code: int = 0) -> None:
        script = self.root / "scripts/status.sh"
        script.write_text(f"#!/bin/sh\nprintf '%s' {shlex.quote(output)}\nexit {code}\n", encoding="utf-8")
        script.chmod(0o700)

    @staticmethod
    def jwt(expiry: int) -> str:
        payload = base64.urlsafe_b64encode(json.dumps({"exp": expiry}).encode()).decode().rstrip("=")
        return f"header.{payload}.signature"

    def test_status_parser_accepts_only_expected_local_counts(self) -> None:
        self.assertEqual(info.pending_uploads(self.root), (3, 4096))
        self.status_script("Pending local uploads: 3 files, 4096 bytes\nprivate-token-data\n")
        self.assertIsNone(info.pending_uploads(self.root))
        with mock.patch.object(info, "linger_status", return_value="unknown"):
            report = info.report(self.root, self.manifest)
        self.assertNotIn("private-token-data", report)

    def test_auth_status_uses_expiry_hint_without_showing_tokens(self) -> None:
        auth_path = self.root / "config/auth.json"
        self.write_private(auth_path, {"token": self.jwt(2_000_000_000), "refresh_token": "do-not-show"})
        status = info.authentication_status(auth_path, now=1_900_000_000)
        self.assertIn("local claim only", status)
        self.assertNotIn("do-not-show", status)
        self.write_private(auth_path, {"token": self.jwt(1_800_000_000), "refresh_token": "do-not-show"})
        status = info.authentication_status(auth_path, now=1_900_000_000)
        self.assertIn("may renew it", status)
        self.assertNotIn("do-not-show", status)
        self.assertIsNone(info.authentication_error(auth_path, now=1_900_000_000, logs_dir=self.root / "logs"))
        self.write_private(auth_path, {"token": self.jwt(1_800_000_000)})
        self.assertIn("log in before", info.authentication_status(auth_path, now=1_900_000_000))

    def test_report_places_missing_auth_error_and_login_command_near_the_top(self) -> None:
        (self.root / "config/auth.json").unlink()
        with (
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "linger_status", return_value="unknown"),
        ):
            report = info.report(self.root, self.manifest)
        lines = report.splitlines()
        self.assertTrue(lines[2].startswith("AUTHENTICATION ERROR: No saved account token;"))
        self.assertIn(f"{shlex.quote(str(self.root / 'degoo-rc'))} login", lines[2])
        self.assertIn("log in before cloud operations", lines[2])

    def test_legacy_mount_settings_report_effective_defaults_for_missing_fields(self) -> None:
        settings = {
            key: install.DEFAULT_MOUNT_SETTINGS[key]
            for key in ("cache_max_size", "cache_min_free_space", "cache_max_age", "cache_poll_interval", "verify_upload")
        }
        self.write_private(self.root / "config/mount.json", settings)

        summary = "\n".join(info._settings_summary(self.root))

        self.assertIn("Delay before uploading closed files: 5s.", summary)
        self.assertIn("Cache maximum size: 60G", summary)
        self.assertIn("Read buffer size: 16Mi", summary)
        self.assertIn("Read chunk size: 128Mi", summary)
        self.assertIn("Read chunk size limit: off", summary)
        self.assertIn("Read chunk streams: 8", summary)
        self.assertIn("Directory cache time: 30m", summary)
        self.assertIn("Attribute cache timeout: 1s", summary)
        self.assertIn("Transfers: 4", summary)
        self.assertIn("Bandwidth limit: off", summary)
        self.assertIn("Network timeout: 5m", summary)
        self.assertIn("Upload verification: disabled", summary)

    def test_mount_settings_summary_reports_saved_custom_tuning(self) -> None:
        settings = install.DEFAULT_MOUNT_SETTINGS | {
            "buffer_size": "32Mi",
            "read_ahead": "2Mi",
            "read_chunk_size": "64Mi",
            "read_chunk_size_limit": "256Mi",
            "read_chunk_streams": 0,
            "dir_cache_time": "45m",
            "attr_timeout": "2s",
            "transfers": 6,
            "bwlimit": "10M:20M",
            "timeout": "6m",
        }
        self.write_private(self.root / "config/mount.json", settings)

        summary = "\n".join(info._settings_summary(self.root))

        for expected in (
            "Read buffer size: 32Mi",
            "Read ahead: 2Mi",
            "Read chunk size: 64Mi",
            "Read chunk size limit: 256Mi",
            "Read chunk streams: 0",
            "Directory cache time: 45m",
            "Attribute cache timeout: 2s",
            "Transfers: 6",
            "Bandwidth limit: 10M:20M",
            "Network timeout: 6m",
        ):
            self.assertIn(expected, summary)

    def test_write_back_microsecond_units_are_reported(self) -> None:
        for value in ("1µs", "1μs"):
            with self.subTest(value=value):
                settings = install.DEFAULT_MOUNT_SETTINGS | {"write_back": value}
                self.write_private(self.root / "config/mount.json", settings)

                summary = "\n".join(info._settings_summary(self.root))

                self.assertIn(f"Delay before uploading closed files: {value}.", summary)

    def test_report_promotes_expired_access_token_without_refresh(self) -> None:
        now = int(datetime.now().timestamp())
        self.write_private(self.root / "config/auth.json", {"token": self.jwt(now - 60)})
        with (
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "linger_status", return_value="unknown"),
        ):
            report = info.report(self.root, self.manifest)
        error_line = report.splitlines()[2]
        self.assertTrue(error_line.startswith("AUTHENTICATION ERROR: The saved access token has expired"))
        self.assertIn(f"{shlex.quote(str(self.root / 'degoo-rc'))} login", error_line)

    def test_remote_rejection_is_prominent_even_when_saved_jwt_looks_current(self) -> None:
        self.write_private(
            self.root / "config/auth.json",
            {"token": self.jwt(2_000_000_000), "refresh_token": "keep-private"},
        )
        with (
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "linger_status", return_value="unknown"),
            mock.patch.object(info, "remote_location", return_value=("login required for remote check", None)),
        ):
            report = info.report(self.root, self.manifest, check_remote=True)
        self.assertTrue(report.splitlines()[2].startswith("AUTHENTICATION ERROR: Degoo rejected the current account authentication."))
        self.assertIn(f"{shlex.quote(str(self.root / 'degoo-rc'))} login", report.splitlines()[2])

    def test_auth_status_only_requires_login_for_a_recent_post_auth_sentinel(self) -> None:
        auth_path = self.root / "config/auth.json"
        logs_dir = self.root / "logs"
        now = 1_900_000_000
        self.write_private(auth_path, {"token": self.jwt(now - 60), "refresh_token": "keep-private"})
        os.utime(auth_path, (now - 120, now - 120))
        failure_time = now - 60
        failure_stamp = datetime.fromtimestamp(failure_time).strftime("%Y/%m/%d %H:%M:%S")
        mount_log = logs_dir / "mount.log"
        mount_log.write_text(f"{failure_stamp} ERROR : degoo login required (auth HTTP 403)\n", encoding="utf-8")
        mount_log.chmod(0o600)

        status = info.authentication_status(auth_path, now=now, logs_dir=logs_dir)
        self.assertIn("rejected", status)
        self.assertIn(f"{shlex.quote(str(self.root / 'degoo-rc'))} login", status)
        self.assertNotIn("keep-private", status)

        os.utime(auth_path, (now - 30, now - 30))
        refreshed_status = info.authentication_status(auth_path, now=now, logs_dir=logs_dir)
        self.assertIn("may renew it", refreshed_status)
        self.assertNotIn("rejected", refreshed_status)

    def test_no_auth_is_actionable_without_disclosing_login_state(self) -> None:
        (self.root / "config/auth.json").unlink()
        self.assertIn("log in before cloud operations", info.authentication_status(self.root / "config/auth.json"))

    def test_report_uses_cached_location_and_never_prints_secrets(self) -> None:
        with mock.patch.object(info, "service_status", return_value="enabled; active"), mock.patch.object(
            info, "mounted_state", return_value="mounted"
        ), mock.patch.object(info, "linger_status", return_value="enabled"):
            report = info.report(self.root, self.manifest)
        self.assertIn("My Device / Vault", report)
        self.assertIn("enabled; active", report)
        self.assertIn("60G", report)
        self.assertIn("1Gi", report)
        self.assertIn("3 files", report)
        self.assertIn("Approximate cache allocation", report)
        self.assertIn("CLI overrides on a running mount are not confirmed", report)
        for secret in ("vault-secret", "salt-secret", "opaque-access-secret", "refresh-secret"):
            self.assertNotIn(secret, report)
        self.assertNotIn("owner@example.test", report)
        self.assertIn("does not contact Degoo", report)

    def test_scan_skips_mount_tree_symlinks_and_obeys_entry_limit(self) -> None:
        cached = self.root / "cache" / "plain"
        cached.write_bytes(b"cache-data")
        outside = self.base / "outside"
        outside.mkdir()
        (outside / "secret-file").write_bytes(b"do-not-follow-this")
        (self.root / "link-to-outside").symlink_to(outside, target_is_directory=True)
        (self.root / "mount" / "mounted-secret").write_bytes(b"do-not-scan-fuse")

        real_scandir = os.scandir

        def guarded_scandir(path: os.PathLike[str] | str):
            if Path(path) == self.root / "mount":
                raise AssertionError("scanner descended into the FUSE mount")
            return real_scandir(path)

        with mock.patch.object(info.os, "scandir", side_effect=guarded_scandir):
            result = info.scan_install_tree(
                self.root,
                self.root / "cache",
                self.root / "mount",
                mountpoints=set(),
            )
        self.assertEqual(result["cache_allocated"], cached.stat().st_blocks * 512)
        self.assertEqual(result["cache_fs_allocated"], result["allocated"])
        self.assertFalse(result["partial"])
        mounted_cache = info.scan_install_tree(
            self.root,
            self.root / "cache",
            self.root / "mount",
            mountpoints={self.root / "cache"},
        )
        self.assertEqual(mounted_cache["cache_allocated"], result["cache_allocated"])
        limited = info.scan_install_tree(
            self.root,
            self.root / "cache",
            self.root / "mount",
            max_entries=1,
            mountpoints=set(),
        )
        self.assertTrue(limited["partial"])

    def test_cache_filesystem_subtraction_uses_cache_files_not_all_app_files(self) -> None:
        scan = {
            "allocated": 900,
            "apparent": 1200,
            "cache_allocated": 100,
            "cache_fs_allocated": 300,
            "entries": 10,
            "partial": False,
        }
        fs = {"total": 1000, "used": 800, "available": 200}
        with (
            mock.patch.object(info, "service_status", return_value="not configured for this install"),
            mock.patch.object(info, "mounted_state", return_value="not mounted"),
            mock.patch.object(info, "linger_status", return_value="unknown"),
            mock.patch.object(info, "pending_uploads", return_value=(0, 0)),
            mock.patch.object(info, "filesystem_stats", return_value=fs),
            mock.patch.object(info, "scan_install_tree", return_value=scan),
        ):
            report = info.report(self.root, self.manifest)
        self.assertIn("Other data on this filesystem: approximately 500 B", report)

    def test_service_state_requires_the_recorded_owned_link(self) -> None:
        config_home = self.base / "xdg-config"
        unit_dir = config_home / "systemd/user"
        wants = unit_dir / "default.target.wants"
        wants.mkdir(parents=True, mode=0o700)
        source_unit = self.root / "rclone-degoo.service"
        source_unit.write_text("[Service]\n", encoding="ascii")
        source_unit.chmod(0o600)
        direct = unit_dir / "rclone-degoo.service"
        direct.symlink_to(source_unit)
        wanted = wants / "rclone-degoo.service"
        wanted.symlink_to(source_unit)
        self.manifest["service_config_home"] = str(config_home)
        self.manifest["service_links"] = [str(direct), str(wanted)]
        self.write_private(self.root / "install.json", self.manifest)

        def systemctl(*args: str, **_: object):
            if args[0] == "show":
                return subprocess_result(str(direct).encode())
            if args[0] == "is-enabled":
                return subprocess_result(b"enabled\n")
            return subprocess_result(b"active\n")

        with mock.patch.object(info, "_systemctl", side_effect=systemctl):
            self.assertEqual(info.service_status(self.root, self.manifest), "enabled; active")

    def test_remote_check_keeps_only_validated_folder_names(self) -> None:
        binary = self.root / "bin/rclone"
        binary.write_text(
            "#!/bin/sh\nprintf '%s\\n' '[{\"id\":\"secret-id\",\"name\":\"Fresh vault\",\"category\":\"folder\"}]'\n",
            encoding="utf-8",
        )
        binary.chmod(0o700)
        state, names = info.remote_location(self.root)
        self.assertEqual(state, "checked")
        self.assertEqual(names, ["Fresh vault"])

    def test_remote_location_parser_matches_degoo_json_contract(self) -> None:
        response = b'[{"id":"private-id","name":"Device","category":"device"}]'
        self.assertEqual(info._remote_names(response), ["Device"])
        self.assertIsNone(info._remote_names(b'[{"ID":"private-id","Name":"Device"}]'))

    def test_remote_check_sanitizes_login_error(self) -> None:
        binary = self.root / "bin/rclone"
        binary.write_text(
            "#!/bin/sh\necho 'degoo login required token=secret' >&2\nexit 1\n",
            encoding="utf-8",
        )
        binary.chmod(0o700)
        state, names = info.remote_location(self.root)
        self.assertEqual(state, "login required for remote check")
        self.assertIsNone(names)

    def test_installer_generates_offline_info_wrapper_and_packages_helper(self) -> None:
        binary = self.base / "rclone"
        binary.write_text("#!/bin/sh\necho 'degoo login mount status'\n", encoding="ascii")
        binary.chmod(0o700)
        shutil.rmtree(self.root)
        with mock.patch.object(install, "validate_binary"):
            args = install.build_parser().parse_args(["--binary", str(binary), "--folder-id", "folder-id"])
            self.assertEqual(install.install(args), 0)
        installed = self.root / "scripts/python/info.py"
        self.assertTrue(installed.is_file())
        self.assertTrue((self.root / "scripts/python/logs.py").is_file())
        self.assertFalse((self.root / "info.sh").exists())
        self.assertFalse((self.root / "logs.sh").exists())
        wrapper = (self.root / "scripts/info.sh").read_text(encoding="utf-8")
        self.assertIn("exec python3", wrapper)
        self.assertIn("$@", wrapper)
        self.assertNotIn("XDG_CONFIG_HOME", wrapper)
        self.assertNotIn('HOME=', wrapper)
        self.assertEqual(stat.S_IMODE((self.root / "scripts/info.sh").stat().st_mode), 0o700)
        logs_wrapper = (self.root / "scripts/logs.sh").read_text(encoding="utf-8")
        self.assertIn("scripts/python/logs.py", logs_wrapper)
        self.assertIn("--root", logs_wrapper)
        self.assertEqual(stat.S_IMODE((self.root / "scripts/logs.sh").stat().st_mode), 0o700)
        help_result = subprocess.run(
            [str(self.root / "scripts/logs.sh"), "--help"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )
        self.assertEqual(help_result.returncode, 0, help_result.stderr.decode("utf-8", errors="replace"))
        self.assertIn(b"last 100 lines", help_result.stdout)
        self.assertIn(b"--follow", help_result.stdout)


def subprocess_result(stdout: bytes):
    import subprocess

    return subprocess.CompletedProcess(args=["systemctl"], returncode=0, stdout=stdout, stderr=b"")


if __name__ == "__main__":
    unittest.main()
