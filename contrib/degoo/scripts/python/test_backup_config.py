"""Offline checks for private, portable Degoo configuration backups."""

from __future__ import annotations

import contextlib
import fcntl
import hashlib
import io
import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import backup_config
import install
import restore


class BackupConfigTest(unittest.TestCase):
    def test_backup_excludes_encrypted_account_password_and_preserves_installed_credential(self) -> None:
        vault_path = self.root / "config/vault.json"
        vault = json.loads(vault_path.read_bytes())
        encrypted = "generated-encrypted-account-password"
        vault["account_credentials"] = encrypted
        original = (json.dumps(vault) + "\n").encode()
        self.write_private(vault_path, original)
        self.write_private(self.root / "config/auth.json.lock", b"")
        destination = backup_config.run(self.root, self.base / "password-free-backup")
        backed_up = json.loads((destination / "config/vault.json").read_bytes())
        self.assertNotIn("account_credentials", backed_up)
        self.assertEqual(backed_up["password"], self.secret)
        self.assertEqual(vault_path.read_bytes(), original)
        for path in destination.rglob("*"):
            if path.is_file():
                self.assertNotIn(encrypted.encode(), path.read_bytes())
                self.assertNotIn(b"test-only-token", path.read_bytes())
                self.assertNotIn(b"test-only-refresh", path.read_bytes())
        self.assertFalse((destination / "config/auth.json").exists())
        self.assertFalse((destination / "config/auth.json.lock").exists())
        restored = restore.inspect_backup(destination, self.base / "restored-install")
        self.assertNotIn("account_credentials", restored.vault_settings)

    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.home = self.base / "home"
        self.home.mkdir(mode=0o700)
        self.home.chmod(0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)

        self.root = self.home / ".local" / "share" / "rclone-degoo"
        for directory in (self.home / ".local", self.home / ".local" / "share", self.root, self.root / "config", self.root / "cache"):
            directory.mkdir(mode=0o700)
            directory.chmod(0o700)
        self.folder_id = "dedicated-vault-folder-123"
        self.secret = "test-only-vault-secret"
        self.auth = b'{"token":"test-only-token","refresh_token":"test-only-refresh"}\n'
        self.login = b'{"email":"owner@example.test"}\n'
        self.pending = self.root / "cache" / "pending-local-write.bin"
        self.pending.write_bytes(b"local data that must stay in the source cache")
        self.pending.chmod(0o600)
        self._write_install_config(include_auth=True)

    @staticmethod
    def write_private(path: Path, data: bytes) -> None:
        path.write_bytes(data)
        path.chmod(0o600)

    def _write_install_config(self, *, include_auth: bool) -> None:
        manifest = {
            "version": 1,
            "root_folder_id": self.folder_id,
            "remote_folder_name": "Portable Vault",
            "remote_created_by_setup": True,
            "parent_folder_id": "device-root-123",
            "remote_name": install.REMOTE_NAME,
            "binary": "bin/rclone",
            "vault_config": "config/vault.json",
            "rclone_config": "config/rclone.conf",
            "cache_dir": "cache",
            "mountpoint": "mount",
            "pidfile": "mount.pid",
            "path_link": "",
            "service_links": [],
            "service_config_home": "",
        }
        self.write_private(self.root / "install.json", (json.dumps(manifest, sort_keys=True) + "\n").encode())
        vault = {
            "version": 1,
            "remote": f"{install.REMOTE_NAME}:",
            "password": self.secret,
            "cache_dir": str(self.root / "cache"),
            "chunk_size": "128Mi",
            "max_object_size": "19Gi",
        }
        self.write_private(self.root / "config" / "vault.json", (json.dumps(vault, sort_keys=True) + "\n").encode())
        self.write_private(
            self.root / "config" / "rclone.conf",
            install.rclone_config(self.root, self.folder_id),
        )
        self.write_private(
            self.root / "config" / "mount.json",
            (json.dumps(install.DEFAULT_MOUNT_SETTINGS, sort_keys=True) + "\n").encode(),
        )
        if include_auth:
            self.write_private(self.root / "config" / "auth.json", self.auth)
            self.write_private(self.root / "config" / "login.json", self.login)

    def _lock_source_cache(self) -> int:
        digest = hashlib.sha256(f"{install.REMOTE_NAME}:".encode()).hexdigest()[:24]
        lock_path = self.root / "cache" / f"degoo-vault-{digest}.lock"
        lock_path.write_bytes(b"")
        lock_path.chmod(0o600)
        fd = os.open(lock_path, os.O_RDONLY)
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        return fd

    def test_backup_is_restore_compatible_private_and_config_only_during_active_cache_use(self) -> None:
        cache_lock = self._lock_source_cache()
        self.addCleanup(os.close, cache_lock)
        self.addCleanup(fcntl.flock, cache_lock, fcntl.LOCK_UN)
        original_manifest = (self.root / "install.json").read_bytes()
        original_config = {path.name: path.read_bytes() for path in (self.root / "config").iterdir()}
        pending_before = self.pending.read_bytes()

        destination = self.base / "portable backup"
        result = backup_config.run(self.root, destination)

        self.assertEqual(result, destination)
        self.assertEqual((destination.stat().st_mode & 0o777), 0o700)
        self.assertEqual((destination / "config").stat().st_mode & 0o777, 0o700)
        self.assertEqual((destination / "install.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual(
            {path.name for path in destination.iterdir()},
            {"install.json", "config"},
        )
        self.assertEqual(
            {path.name for path in (destination / "config").iterdir()},
            {"login.json", "mount.json", "rclone.conf", "vault.json"},
        )
        for path in (destination / "config").iterdir():
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertFalse((destination / "cache").exists())
        self.assertFalse((destination / "logs").exists())
        self.assertFalse((destination / "bin").exists())

        target_root = self.base / "new machine" / ".local" / "share" / "rclone-degoo"
        restored = restore.inspect_backup(destination, target_root)
        self.assertEqual(restored.folder_id, self.folder_id)
        self.assertEqual(restored.vault_settings["password"], self.secret)
        self.assertEqual(restored.vault_settings["cache_dir"], str(target_root / "cache"))
        self.assertFalse(hasattr(restored, "auth_bytes"))
        self.assertEqual(restored.login_bytes, self.login)

        self.assertEqual((self.root / "install.json").read_bytes(), original_manifest)
        self.assertEqual({path.name: path.read_bytes() for path in (self.root / "config").iterdir()}, original_config)
        self.assertEqual(self.pending.read_bytes(), pending_before)
        self.assertTrue((self.root / "cache").joinpath(next(path.name for path in (self.root / "cache").iterdir() if path.name.endswith(".lock"))).exists())

    def test_command_creates_missing_private_parent_and_explains_backup_exclusions(self) -> None:
        destination = self.home / "private-backups" / "backup one"
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = backup_config.main(["--root", str(self.root), "--directory", str(destination)])

        self.assertEqual(result, 0, stderr.getvalue())
        self.assertIn(str(destination), stdout.getvalue())
        self.assertIn("authentication tokens are excluded", stdout.getvalue())
        self.assertIn("File data, cache, and pending uploads are excluded", stdout.getvalue())
        self.assertNotIn(self.secret, stdout.getvalue())
        self.assertNotIn("test-only-refresh", stdout.getvalue())
        self.assertEqual((destination.parent.stat().st_mode & 0o777), 0o700)

    def test_existing_destination_is_never_overwritten(self) -> None:
        destination = self.base / "existing backup"
        destination.mkdir(mode=0o700)
        destination.chmod(0o700)
        marker = destination / "keep.txt"
        self.write_private(marker, b"existing user data")

        with self.assertRaisesRegex(backup_config.BackupConfigError, "already exists"):
            backup_config.run(self.root, destination)

        self.assertEqual(marker.read_bytes(), b"existing user data")
        self.assertEqual({path.name for path in destination.iterdir()}, {"keep.txt"})

    def test_unsafe_config_symlink_is_refused_without_publishing(self) -> None:
        destination = self.base / "must not exist"
        outside = self.base / "outside auth config"
        self.write_private(outside, b"do not copy through links")
        config = self.root / "config" / "rclone.conf"
        config.unlink()
        config.symlink_to(outside)

        with self.assertRaisesRegex(backup_config.BackupConfigError, "unsafe|safely"):
            backup_config.run(self.root, destination)

        self.assertFalse(destination.exists())
        self.assertFalse(any(self.base.glob(".degoo-backup-config-*")))

    def test_invalid_staged_snapshot_is_removed_without_publishing(self) -> None:
        destination = self.base / "invalid staged backup"
        with mock.patch.object(restore, "inspect_backup", side_effect=restore.RestoreError("invalid staged config")):
            with self.assertRaisesRegex(restore.RestoreError, "invalid staged config"):
                backup_config.run(self.root, destination)

        self.assertFalse(destination.exists())
        self.assertFalse(any(self.base.glob(".degoo-backup-config-*")))

    def test_unconfigured_install_is_refused_with_actionable_status(self) -> None:
        manifest_path = self.root / "install.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["root_folder_id"] = ""
        self.write_private(manifest_path, (json.dumps(manifest, sort_keys=True) + "\n").encode())
        destination = self.base / "unconfigured backup"

        with self.assertRaisesRegex(backup_config.BackupConfigError, "run setup"):
            backup_config.run(self.root, destination)

        self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
