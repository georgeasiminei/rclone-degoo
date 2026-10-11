"""Offline safety checks for restoring retained Degoo install state."""

from __future__ import annotations

import errno
import fcntl
import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import install
import restore


class RestoreTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home"
        self.home.mkdir(mode=0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.backup = self.base / "retained state"
        self.target = self.base / "new install"
        self.folder_id = "saved-vault-folder-123"
        self.folder_name = "Saved Vault"
        self.remote = "degoo-live-test:"
        self.cache_name = "degoo-vault-" + hashlib.sha256(self.remote.encode()).hexdigest()[:24]
        self.vault = {
            "version": 1,
            "remote": self.remote,
            "password": "test-only-vault-secret",
            "cache_dir": str(self.base / "old install" / "cache"),
            "chunk_size": "128Mi",
            "max_object_size": "19Gi",
        }
        self.vault_bytes = (json.dumps(self.vault, sort_keys=True) + "\n").encode()
        self.auth_bytes = b'{"token":"test-only-access","refresh_token":"test-only-refresh"}\n'
        self.login_bytes = b'{"email":"owner@example.test"}\n'
        self.mount_settings = install.DEFAULT_MOUNT_SETTINGS | {"transfers": 6, "cache_max_age": "10h"}
        self._make_backup()
        self._make_target()

    @staticmethod
    def write_private(path: Path, data: bytes) -> None:
        path.write_bytes(data)
        path.chmod(0o600)

    def _make_backup(self) -> None:
        for name in ("config", "cache"):
            (self.backup / name).mkdir(parents=True, mode=0o700)
            (self.backup / name).chmod(0o700)
        self.backup.chmod(0o700)
        manifest = {
            "version": 1,
            "root_folder_id": self.folder_id,
            "remote_folder_name": self.folder_name,
            "remote_created_by_setup": True,
            "remote_name": install.REMOTE_NAME,
            "mountpoint": "mount",
            "binary": "bin/rclone",
            "vault_config": "config/vault.json",
            "rclone_config": "config/rclone.conf",
            "cache_dir": "cache",
            "pidfile": "mount.pid",
            "service_links": ["/ignored/old/rclone-degoo.service"],
        }
        self.write_private(self.backup / "install.json", (json.dumps(manifest) + "\n").encode())
        self.write_private(self.backup / "config/vault.json", self.vault_bytes)
        self.write_private(self.backup / "config/rclone.conf", install.rclone_config(self.base / "old install", self.folder_id))
        self.write_private(self.backup / "config/auth.json", self.auth_bytes)
        self.write_private(self.backup / "config/login.json", self.login_bytes)
        self.write_private(self.backup / "config/mount.json", (json.dumps(self.mount_settings) + "\n").encode())

    def _make_target(self) -> None:
        for name in ("bin", "cache", "config", "logs", "tmp"):
            path = self.target / name
            path.mkdir(parents=True, mode=0o700)
            path.chmod(0o700)
        self.target.chmod(0o700)
        target_vault = self.vault | {"cache_dir": str(self.target / "cache")}
        self.write_private(self.target / "config/vault.json", (json.dumps(target_vault, sort_keys=True) + "\n").encode())
        self.write_private(self.target / "config/rclone.conf", install.rclone_config(self.target, self.folder_id))
        binary = self.target / "bin/rclone"
        binary.write_text("#!/bin/sh\nexit 0\n", encoding="ascii")
        binary.chmod(0o700)

    def add_cache_data(self) -> tuple[Path, Path, bytes]:
        cache = self.backup / "cache"
        for parent_name in ("vfs", "vfsMeta"):
            parent = cache / parent_name
            parent.mkdir(mode=0o700)
            parent.chmod(0o700)
        for relative in (Path("vfs") / self.cache_name, Path("vfsMeta") / self.cache_name):
            (cache / relative).mkdir(parents=True, mode=0o700)
            (cache / relative).chmod(0o700)
        identity = cache / f"{self.cache_name}.identity"
        self.write_private(identity, b"a" * 64 + b"\n")
        pending = cache / "vfs" / self.cache_name / "pending data.bin"
        self.write_private(pending, b"pending plaintext payload")
        metadata = cache / "vfsMeta" / self.cache_name / "pending data.bin"
        self.write_private(metadata, b"pending metadata")
        return identity, pending, pending.read_bytes()

    def test_inspection_recovers_settings_vault_and_saved_auth_defaults(self) -> None:
        backup = restore.inspect_backup(self.backup, self.target)

        self.assertEqual(backup.folder_id, self.folder_id)
        self.assertEqual(backup.folder_name, self.folder_name)
        self.assertEqual(backup.parent_id, "")
        self.assertEqual(backup.mount_settings, self.mount_settings)
        self.assertEqual(backup.vault_config_path, self.backup / "config/vault.json")
        self.assertEqual(backup.vault_settings["cache_dir"], str(self.target / "cache"))
        self.assertEqual(backup.vault_settings["remote"], install.REMOTE_NAME + ":")
        self.assertEqual(backup.auth_bytes, self.auth_bytes)
        self.assertEqual(backup.login_bytes, self.login_bytes)
        self.assertFalse(backup.cache_has_data)

    def test_restore_copies_auth_login_and_cache_without_changing_backup(self) -> None:
        identity, pending, pending_bytes = self.add_cache_data()
        identity_before = identity.read_bytes()
        backup = restore.inspect_backup(self.backup, self.target)

        with mock.patch.object(restore.subprocess, "run", return_value=mock.Mock(returncode=0)) as run:
            restore.restore_to_install(backup, self.target)

        self.assertEqual((self.target / "config/auth.json").read_bytes(), self.auth_bytes)
        self.assertEqual((self.target / "config/login.json").read_bytes(), self.login_bytes)
        self.assertEqual((self.target / "config/auth.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.target / "config/login.json").stat().st_mode & 0o777, 0o600)
        self.assertEqual((self.target / "cache" / "vfs" / self.cache_name / pending.name).read_bytes(), pending_bytes)
        self.assertEqual((self.target / "cache" / f"{self.cache_name}.identity").read_bytes(), identity_before)
        self.assertEqual(identity.read_bytes(), identity_before)
        self.assertEqual(pending.read_bytes(), pending_bytes)
        self.assertFalse((self.backup / "cache" / f"{self.cache_name}.lock").exists())
        self.assertEqual(self.target.stat().st_mode & 0o777, 0o700)
        run.assert_called_once()
        command = run.call_args.args[0]
        self.assertEqual(command[0], str(self.target / "bin/rclone"))
        self.assertEqual(command[1:3], ["--config", str(self.target / "config/rclone.conf")])
        self.assertIn("restore-cache", command)
        self.assertIn(str(self.backup / "config/vault.json"), command)
        self.assertIn(str(self.target / "config/vault.json"), command)
        self.assertIn(self.folder_id, command)
        self.assertFalse((self.target / "install.json").exists())

    def test_restore_rejects_an_active_source_cache_lock(self) -> None:
        self.add_cache_data()
        lock_path = self.backup / "cache" / f"{self.cache_name}.lock"
        self.write_private(lock_path, b"")
        lock_fd = os.open(lock_path, os.O_RDONLY)
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            backup = restore.inspect_backup(self.backup, self.target)
            with mock.patch.object(restore.subprocess, "run") as run:
                with self.assertRaisesRegex(restore.RestoreError, "active"):
                    restore.restore_to_install(backup, self.target)
            run.assert_not_called()
            self.assertFalse(any((self.target / "cache").iterdir()))
            self.assertTrue((self.backup / "cache" / "vfs" / self.cache_name / "pending data.bin").exists())
        finally:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
            os.close(lock_fd)

    def test_sparse_cache_copy_preserves_holes_and_handles_short_writes(self) -> None:
        if not hasattr(os, "SEEK_DATA") or not hasattr(os, "SEEK_HOLE"):
            self.skipTest("sparse extent seeking is unavailable")
        self.add_cache_data()
        sparse_path = self.backup / "cache" / "vfs" / self.cache_name / "sparse pending.bin"
        logical_size = 64 * 1024 * 1024
        payloads = (
            (0, b"pending at the beginning"),
            (logical_size // 2, b"pending in the middle"),
            (logical_size - 64, b"pending at the end"),
        )
        fd = os.open(sparse_path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        try:
            os.ftruncate(fd, logical_size)
            for offset, payload in payloads:
                os.lseek(fd, offset, os.SEEK_SET)
                os.write(fd, payload)
        finally:
            os.close(fd)
        source_stat = sparse_path.stat()
        if source_stat.st_blocks * 512 >= logical_size:
            self.skipTest("the test filesystem does not preserve sparse source files")

        backup = restore.inspect_backup(self.backup, self.target)
        original_write = os.write

        def short_write(file_descriptor: int, data: bytes | memoryview) -> int:
            return original_write(file_descriptor, data[:7])

        with mock.patch.object(restore.subprocess, "run", return_value=mock.Mock(returncode=0)):
            with mock.patch.object(restore.os, "write", side_effect=short_write):
                restore.restore_to_install(backup, self.target)

        copied = self.target / "cache" / "vfs" / self.cache_name / sparse_path.name
        copied_stat = copied.stat()
        self.assertEqual(copied_stat.st_size, logical_size)
        self.assertLess(copied_stat.st_blocks * 512, logical_size // 2)
        with copied.open("rb") as stream:
            for offset, payload in payloads:
                stream.seek(offset)
                self.assertEqual(stream.read(len(payload)), payload)
            stream.seek(logical_size // 4)
            self.assertEqual(stream.read(4096), bytes(4096))

    def test_sparse_copy_falls_back_to_dense_when_extent_seeking_is_unsupported(self) -> None:
        _, pending, pending_bytes = self.add_cache_data()
        backup = restore.inspect_backup(self.backup, self.target)
        original_lseek = os.lseek

        def unsupported_data_seek(file_descriptor: int, offset: int, whence: int) -> int:
            if whence == os.SEEK_DATA:
                raise OSError(errno.EINVAL, "unsupported")
            return original_lseek(file_descriptor, offset, whence)

        with mock.patch.object(restore.subprocess, "run", return_value=mock.Mock(returncode=0)):
            with mock.patch.object(restore.os, "lseek", side_effect=unsupported_data_seek):
                restore.restore_to_install(backup, self.target)

        copied = self.target / "cache" / "vfs" / self.cache_name / pending.name
        self.assertEqual(copied.read_bytes(), pending_bytes)

    def test_rebind_failure_keeps_copied_identity_and_backup_cache(self) -> None:
        self.add_cache_data()
        backup = restore.inspect_backup(self.backup, self.target)

        with mock.patch.object(restore.subprocess, "run", return_value=mock.Mock(returncode=1)):
            with self.assertRaises(restore.RestoreError):
                restore.restore_to_install(backup, self.target)

        self.assertTrue((self.target / "cache" / f"{self.cache_name}.identity").is_file())
        self.assertTrue((self.target / "cache" / "vfs" / self.cache_name / "pending data.bin").is_file())
        self.assertTrue((self.backup / "cache" / "vfs" / self.cache_name / "pending data.bin").is_file())

    def test_nonempty_cache_requires_matching_vault_and_root_configuration(self) -> None:
        _, pending, _ = self.add_cache_data()
        (self.backup / "config/vault.json").unlink()

        with self.assertRaises(restore.RestoreError):
            restore.inspect_backup(self.backup, self.target)

        self.assertTrue(pending.exists())

    def test_cache_rejects_unknown_paths_and_symlinks(self) -> None:
        unknown = self.backup / "cache" / "other-cache"
        unknown.mkdir(mode=0o700)
        with self.assertRaises(restore.RestoreError):
            restore.inspect_backup(self.backup, self.target)
        unknown.rmdir()

        link = self.backup / "cache" / "outside"
        link.symlink_to(self.base, target_is_directory=True)
        with self.assertRaises(restore.RestoreError):
            restore.inspect_backup(self.backup, self.target)

    def test_restore_rejects_existing_auth_or_nonempty_target_cache(self) -> None:
        self.add_cache_data()
        backup = restore.inspect_backup(self.backup, self.target)
        self.write_private(self.target / "config/auth.json", b"existing auth")
        with self.assertRaises(restore.RestoreError):
            restore.restore_to_install(backup, self.target)
        self.assertEqual((self.target / "config/auth.json").read_bytes(), b"existing auth")

        (self.target / "config/auth.json").unlink()
        self.write_private(self.target / "cache/other", b"existing cache")
        with self.assertRaises(restore.RestoreError):
            restore.restore_to_install(backup, self.target)
        self.assertEqual((self.target / "cache/other").read_bytes(), b"existing cache")

    def test_insecure_secret_file_is_refused(self) -> None:
        (self.backup / "config/auth.json").chmod(0o644)
        with self.assertRaises(restore.RestoreError):
            restore.inspect_backup(self.backup, self.target)

    def test_missing_mount_settings_use_installer_defaults(self) -> None:
        (self.backup / "config/mount.json").unlink()
        backup = restore.inspect_backup(self.backup, self.target)
        self.assertEqual(backup.mount_settings, install.DEFAULT_MOUNT_SETTINGS)

    def test_inspection_accepts_a_full_install_root_without_importing_service_state(self) -> None:
        for name in ("bin", "mount", "tools", "tmp", "logs", "scripts"):
            path = self.backup / name
            path.mkdir(mode=0o700)
            path.chmod(0o700)
        binary = self.backup / "bin/rclone"
        binary.write_text("#!/bin/sh\nexit 0\n", encoding="ascii")
        binary.chmod(0o700)
        (self.backup / "tools/fusermount3").symlink_to("/usr/bin/fusermount3")

        backup = restore.inspect_backup(self.backup, self.target)

        self.assertEqual(backup.folder_id, self.folder_id)
        self.assertFalse(hasattr(backup, "service_links"))
        self.assertTrue((self.backup / "tools/fusermount3").is_symlink())

    def test_cache_requires_saved_remote_config_for_the_manifest_folder(self) -> None:
        self.add_cache_data()
        self.write_private(
            self.backup / "config/rclone.conf",
            install.rclone_config(self.base / "old install", "another-folder-id"),
        )

        with self.assertRaises(restore.RestoreError):
            restore.inspect_backup(self.backup, self.target)

    def test_parent_folder_id_is_available_when_a_backup_records_it(self) -> None:
        manifest_path = self.backup / "install.json"
        manifest = json.loads(manifest_path.read_bytes())
        manifest["parent_folder_id"] = "saved-device-folder-77"
        self.write_private(manifest_path, (json.dumps(manifest) + "\n").encode())

        backup = restore.inspect_backup(self.backup, self.target)
        self.assertEqual(backup.parent_id, "saved-device-folder-77")


if __name__ == "__main__":
    unittest.main()
