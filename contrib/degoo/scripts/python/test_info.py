"""Offline tests for the contained Degoo installation status report."""

from __future__ import annotations

import base64
import json
import os
import shlex
import shutil
import stat
import sys
import tempfile
import unittest
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
        for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs"):
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
        script = self.root / "status.sh"
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
        self.write_private(auth_path, {"token": self.jwt(1_800_000_000)})
        self.assertIn("log in before", info.authentication_status(auth_path, now=1_900_000_000))

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
        self.assertIn("20G", report)
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
        wrapper = (self.root / "info.sh").read_text(encoding="utf-8")
        self.assertIn("exec python3", wrapper)
        self.assertIn("$@", wrapper)
        self.assertNotIn("XDG_CONFIG_HOME", wrapper)
        self.assertNotIn('HOME=', wrapper)
        self.assertEqual(stat.S_IMODE((self.root / "info.sh").stat().st_mode), 0o700)


def subprocess_result(stdout: bytes):
    import subprocess

    return subprocess.CompletedProcess(args=["systemctl"], returncode=0, stdout=stdout, stderr=b"")


if __name__ == "__main__":
    unittest.main()
