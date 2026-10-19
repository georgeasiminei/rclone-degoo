"""Offline safety tests for the local Degoo install cleanup utility."""

from __future__ import annotations

import contextlib
import fcntl
import io
import json
import os
import socket
import struct
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import install
import restore
import uninstall  # noqa: E402


class UninstallTest(unittest.TestCase):
    def test_inventory_allows_only_the_private_mount_refresh_socket(self) -> None:
        path = self.root / "tmp/vfs-sync.sock"
        with socket.socket(socket.AF_UNIX) as control:
            control.bind(str(path))
            path.chmod(0o600)
            self.assertIn({"path": "tmp/vfs-sync.sock", "type": "socket"}, uninstall._audit_tree(self.root))
            path.chmod(0o660)
            with self.assertRaises(uninstall.CleanupError):
                uninstall._audit_tree(self.root)
        path.unlink()
        with socket.socket(socket.AF_UNIX) as unexpected:
            unexpected.bind(str(self.root / "tmp/unexpected.sock"))
            with self.assertRaises(uninstall.CleanupError):
                uninstall._audit_tree(self.root)

    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home"
        self.home.mkdir(mode=0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home), "XDG_CONFIG_HOME": str(self.home / ".config")})
        self.env.start()
        self.addCleanup(self.env.stop)

        self.root = self.home / ".local" / "share" / "rclone-degoo"
        for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs"):
            path = self.root / name
            path.mkdir(parents=True, mode=0o700)
            path.chmod(0o700)
        self.root.chmod(0o700)
        for parent in (self.home / ".local", self.home / ".local/share"):
            parent.chmod(0o700)

        self.vault = {
            "version": 1,
            "remote": "degoo-live-test:",
            "password": "test-only-vault-secret",
            "password_salt": "test-only-vault-salt",
            "cache_dir": str(self.root / "cache"),
            "chunk_size": "128Mi",
            "max_object_size": "19Gi",
            "account_credentials": "generated-encrypted-account-password-blob",
        }
        self.vault_bytes = (json.dumps(self.vault, sort_keys=True) + "\n").encode()
        self.retained_vault = {key: value for key, value in self.vault.items() if key != "account_credentials"}
        self.retained_vault_bytes = (json.dumps(self.retained_vault, sort_keys=True) + "\n").encode()
        self.write_private(self.root / "config/vault.json", self.vault_bytes)
        self.write_private(self.root / "config/auth.json", b'{"access_token":"test-only-token"}\n')
        self.write_private(self.root / "config/auth.json.lock", b"private lock state\n")
        self.write_private(self.root / "config/login.json", b'{"email":"owner@example.test"}\n')
        self.write_private(self.root / "config/rclone.conf", install.rclone_config(self.root, "generated-test-folder-id"))
        self.write_private(self.root / "install.json", json.dumps({
            "version": 1,
            "root_folder_id": "generated-test-folder-id",
            "remote_folder_name": "generated-test-folder",
            "remote_created_by_setup": True,
            "remote_name": "degoo-live-test",
            "mountpoint": "mount",
            "binary": "bin/rclone",
            "vault_config": "config/vault.json",
            "rclone_config": "config/rclone.conf",
            "cache_dir": "cache",
            "pidfile": "mount.pid",
            "service_config_home": str(self.home / ".config"),
            "service_links": [],
        }, sort_keys=True).encode() + b"\n")
        self.write_rclone("Pending local uploads: 0 files, 0 bytes")
        (self.root / "tools/fusermount3").symlink_to("/usr/bin/fusermount3")

    @staticmethod
    def write_private(path: Path, data: bytes) -> None:
        path.write_bytes(data)
        path.chmod(0o600)

    def write_rclone(self, status: str) -> None:
        binary = self.root / "bin/rclone"
        binary.write_text("#!/bin/sh\nprintf '%s\\n' '" + status + "'\n", encoding="ascii")
        binary.chmod(0o700)

    def set_external_cache(self, path: Path) -> None:
        path.mkdir(mode=0o700)
        path.chmod(0o700)
        self.vault = self.vault | {"cache_dir": str(path)}
        self.vault_bytes = (json.dumps(self.vault, sort_keys=True) + "\n").encode()
        self.retained_vault = {key: value for key, value in self.vault.items() if key != "account_credentials"}
        self.retained_vault_bytes = (json.dumps(self.retained_vault, sort_keys=True) + "\n").encode()
        self.write_private(self.root / "config/vault.json", self.vault_bytes)

    def add_cache_file(self, name: str, data: bytes, *, cache: Path | None = None) -> Path:
        cache_root = cache or self.root / "cache"
        cache_name = restore._cache_name()
        for parent in ("vfs", "vfsMeta"):
            namespace = cache_root / parent / cache_name
            namespace.mkdir(parents=True, mode=0o700, exist_ok=True)
            namespace.chmod(0o700)
            namespace.parent.chmod(0o700)
        self.write_private(cache_root / f"{cache_name}.identity", b"a" * 64 + b"\n")
        path = cache_root / "vfs" / cache_name / name
        self.write_private(path, data)
        self.write_private(
            cache_root / "vfsMeta" / cache_name / name,
            json.dumps({"Dirty": True, "Size": len(data), "Fingerprint": "generated fingerprint"}).encode(),
        )
        return path

    def invoke(self, *extra: str) -> tuple[int, str, str]:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = uninstall.main(["--root", str(self.root), *extra])
        return result, stdout.getvalue(), stderr.getvalue()

    def invoke_terminal(self, responses: str, *extra: str) -> tuple[int, str, str]:
        class Terminal(io.StringIO):
            def isatty(self) -> bool:
                return True

        stdin = Terminal(responses)
        stdout = Terminal()
        stderr = Terminal()
        with (
            mock.patch.object(uninstall.sys, "stdin", stdin),
            mock.patch.object(uninstall.sys, "stdout", stdout),
            mock.patch.object(uninstall.sys, "stderr", stderr),
        ):
            result = uninstall.main(["--root", str(self.root), *extra])
        return result, stdout.getvalue(), stderr.getvalue()

    def backup(self) -> Path:
        directory = self.base / "backup"
        directory.mkdir(mode=0o700)
        directory.chmod(0o700)
        path = directory / "vault.json"
        self.write_private(path, self.vault_bytes)
        return path

    def test_default_is_inventory_and_never_prints_file_contents(self) -> None:
        code, stdout, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        report = json.loads(stdout)
        self.assertEqual(report["mode"], "inventory")
        names = {entry["path"] for entry in report["files"]}
        self.assertIn("config/vault.json", names)
        self.assertIn("config/auth.json", names)
        self.assertNotIn("test-only-vault-secret", stdout)
        self.assertNotIn("test-only-token", stdout)
        self.assertNotIn("generated-test-folder-id", stdout)
        self.assertTrue(self.root.exists())

    def test_layout_accepts_legacy_mount_settings_without_write_back(self) -> None:
        settings = {
            "cache_max_size": "20G",
            "cache_min_free_space": "20G",
            "cache_max_age": "10m",
            "cache_poll_interval": "5s",
            "verify_upload": True,
        }
        self.write_private(self.root / "config/mount.json", json.dumps(settings).encode() + b"\n")

        layout = uninstall._validate_layout(self.root)

        self.assertEqual(layout["manifest"]["version"], 1)

    def test_layout_accepts_new_mount_tuning_settings(self) -> None:
        settings = install.DEFAULT_MOUNT_SETTINGS | {
            "buffer_size": "32Mi",
            "read_ahead": "0",
            "read_chunk_streams": 0,
            "transfers": 6,
            "bwlimit": "off",
        }
        self.write_private(self.root / "config/mount.json", json.dumps(settings).encode() + b"\n")

        layout = uninstall._validate_layout(self.root)

        self.assertEqual(layout["manifest"]["version"], 1)

    def test_layout_rejects_invalid_mount_tuning_settings(self) -> None:
        settings = install.DEFAULT_MOUNT_SETTINGS | {"read_chunk_streams": -1}
        self.write_private(self.root / "config/mount.json", json.dumps(settings).encode() + b"\n")

        with self.assertRaisesRegex(uninstall.CleanupError, "mount settings are invalid"):
            uninstall._validate_layout(self.root)

    def test_default_root_is_the_contained_install(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = uninstall.main([])
        self.assertEqual(code, 0, stderr.getvalue())
        self.assertEqual(json.loads(stdout.getvalue())["root"], str(self.root))

    def test_inventory_reports_recorded_path_link_ownership(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        bin_dir.chmod(0o700)
        link = bin_dir / "degoo-rc"
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        link.symlink_to(self.root / "degoo-rc")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")

        code, stdout, stderr = self.invoke()

        self.assertEqual(code, 0, stderr)
        self.assertEqual(json.loads(stdout)["path_link"], {
            "path": str(link),
            "owned": True,
            "present": True,
            "type": "symlink",
        })

    def test_uninstall_removes_only_the_recorded_path_link(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        bin_dir.chmod(0o700)
        link = bin_dir / "degoo-rc"
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        link.symlink_to(self.root / "degoo-rc")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")

        code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertIn("owned ~/.local/bin/degoo-rc command link was removed", stdout)
        self.assertFalse(link.exists())
        self.assertFalse(os.path.lexists(link))
        self.assertFalse(self.root.exists())

    def test_uninstall_recovers_exact_unrecorded_path_link(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        bin_dir.chmod(0o700)
        link = bin_dir / "degoo-rc"
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        link.symlink_to(self.root / "degoo-rc")

        code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertIn("owned ~/.local/bin/degoo-rc command link was removed", stdout)
        self.assertFalse(os.path.lexists(link))
        self.assertFalse(self.root.exists())

    def test_uninstall_preserves_changed_recorded_path_link_and_install(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        bin_dir.chmod(0o700)
        link = bin_dir / "degoo-rc"
        unrelated = self.base / "unrelated-degoo-rc"
        unrelated.write_text("keep\n", encoding="ascii")
        link.symlink_to(unrelated)
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")

        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("recorded PATH command link changed before cleanup", stderr)
        self.assertEqual(os.readlink(link), str(unrelated))
        self.assertTrue(self.root.exists())
        self.assertTrue((self.root / "mount").is_dir())
        self.assertTrue((self.root / "config/vault.json").is_file())

    def test_uninstall_leaves_unowned_path_destination_untouched(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        bin_dir.chmod(0o700)
        destination = bin_dir / "degoo-rc"
        destination.write_text("unrelated command\n", encoding="ascii")
        destination.chmod(0o700)

        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertEqual(destination.read_text(encoding="ascii"), "unrelated command\n")
        self.assertTrue(destination.exists())

    def test_inventory_flag_forces_read_only_mode_on_a_terminal(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with (
            mock.patch.object(uninstall.sys.stdin, "isatty", return_value=True),
            mock.patch.object(uninstall.sys.stdout, "isatty", return_value=True),
            contextlib.redirect_stdout(stdout),
            contextlib.redirect_stderr(stderr),
        ):
            code = uninstall.main(["--root", str(self.root), "--inventory"])
        self.assertEqual(code, 0, stderr.getvalue())
        self.assertEqual(json.loads(stdout.getvalue())["mode"], "inventory")
        self.assertTrue(self.root.exists())

    def test_wizard_copies_selected_config_and_cache_to_private_directory(self) -> None:
        destination = self.base / "retained-state"
        self.add_cache_file("pending.bin", b"pending bytes")
        with (
            mock.patch.object(uninstall, "_default_retention_directory", return_value=destination),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke_terminal("y\ny\nn\n\nREMOVE\n")
        self.assertEqual(code, 0, stderr)
        self.assertIn(str(destination), stdout)
        self.assertIn("vault encryption key", stdout)
        self.assertIn("remembered email", stdout)
        self.assertIn("authentication tokens are excluded", stdout)
        self.assertIn("fresh Degoo login is required", stdout)
        self.assertIn("mount settings", stdout)
        self.assertIn("local cache", stdout)
        self.assertIn("local logs", stdout)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual((destination / "config/vault.json").read_bytes(), self.retained_vault_bytes)
        self.assertFalse((destination / "config/auth.json").exists())
        self.assertFalse((destination / "config/auth.json.lock").exists())
        self.assertEqual((destination / "config/login.json").read_bytes(), b'{"email":"owner@example.test"}\n')
        self.assertEqual((destination / "cache/vfs" / restore._cache_name() / "pending.bin").read_bytes(), b"pending bytes")
        self.assertFalse((destination / "logs").exists())
        self.assertEqual(destination.stat().st_mode & 0o777, 0o700)
        self.assertEqual((destination / "config/vault.json").stat().st_mode & 0o777, 0o600)

    def test_wizard_retries_invalid_retention_paths_without_changing_selection(self) -> None:
        destination = self.base / "retained-state"
        outside = self.base / "outside"
        outside.mkdir(mode=0o700)
        symlink = self.base / "backup-link"
        symlink.symlink_to(outside, target_is_directory=True)
        with (
            mock.patch.object(uninstall, "_default_retention_directory", return_value=destination),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke_terminal(
                f"y\nn\nn\nN\n{symlink / 'backup'}\n{destination}\nREMOVE\n"
            )
        self.assertEqual(code, 0, stderr)
        self.assertIn("created only if at least one keep option is answered Yes", stdout)
        self.assertIn("Backup directory path", stdout)
        self.assertIn("this is not a yes/no question", stdout)
        self.assertIn("must be absolute", stdout)
        self.assertIn("symlink path components", stdout)
        self.assertIn("selected retention still needs a valid private backup path", stdout)
        self.assertIn("press Ctrl+C to cancel and restart", stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual((destination / "config/vault.json").read_bytes(), self.retained_vault_bytes)
        self.assertFalse((destination / "cache").exists())
        self.assertFalse((destination / "logs").exists())

    def test_wizard_without_retention_skips_backup_path_and_summarizes_removal(self) -> None:
        with (
            mock.patch.object(uninstall, "_default_retention_directory", side_effect=AssertionError("unexpected backup prompt")),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke_terminal("n\nn\nn\nDISCARD\nREMOVE\n")
        self.assertEqual(code, 0, stderr)
        self.assertNotIn("Backup directory path", stdout)
        self.assertIn("vault encryption key", stdout)
        self.assertIn("remembered email", stdout)
        self.assertIn("authentication tokens are excluded", stdout)
        self.assertIn("mount settings", stdout)
        self.assertIn("local cache", stdout)
        self.assertIn("local logs", stdout)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())

    def test_wizard_requires_explicit_secret_discard_confirmation(self) -> None:
        code, stdout, stderr = self.invoke_terminal("n\nn\nn\nno\n")
        self.assertEqual(code, 1, stderr)
        self.assertIn("Cancelled", stdout)
        self.assertTrue(self.root.exists())

    def test_wizard_interrupt_during_requested_removal_reports_incomplete(self) -> None:
        with mock.patch.object(uninstall, "_acquire_cache_lock", side_effect=KeyboardInterrupt):
            code, stdout, stderr = self.invoke_terminal("n\nn\nn\nDISCARD\nREMOVE\n")

        self.assertEqual(code, 2)
        self.assertIn("uninstall incomplete", stderr)
        self.assertNotIn("Cancelled; the local install was left unchanged", stdout)
        self.assertTrue(self.root.exists())

    def test_pending_uploads_can_be_retained_only_with_config_and_cache(self) -> None:
        destination = self.base / "pending-recovery"
        self.add_cache_file("pending.bin", b"pending bytes")
        self.write_rclone("Pending local uploads: 2 files, 123 bytes")
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            code, stdout, stderr = self.invoke(
                "--delete-local",
                "--keep-config",
                "--keep-cache",
                "--keep-directory",
                str(destination),
            )
        self.assertEqual(code, 0, stderr)
        self.assertIn("were preserved in", stdout)
        self.assertIn("fresh Degoo login is required after restoration", stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual((destination / "cache/vfs" / restore._cache_name() / "pending.bin").read_bytes(), b"pending bytes")
        self.assertEqual((destination / "config/vault.json").read_bytes(), self.retained_vault_bytes)
        self.assertFalse((destination / "config/auth.json").exists())
        self.assertTrue((destination / "bin/rclone").stat().st_mode & 0o100)
        self.assertTrue((destination / "install.json").exists())

    def test_external_pending_cache_is_retained_then_only_owned_namespace_is_removed(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        sibling = self.base / "external-cache-not-owned"
        sibling.mkdir(mode=0o700)
        sibling_file = sibling / "keep"
        sibling_file.write_bytes(b"unrelated generated data")
        cache_data = self.add_cache_file("pending.bin", b"pending payload", cache=cache)
        self.write_rclone("Pending local uploads: 1 files, 15 bytes")
        destination = self.base / "retained-external-cache"

        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            code, stdout, stderr = self.invoke(
                "--delete-local", "--keep-config", "--keep-cache", "--keep-directory", str(destination)
            )

        self.assertEqual(code, 0, stderr)
        self.assertIn("were preserved in", stdout)
        self.assertFalse(self.root.exists())
        self.assertTrue((destination / "cache/vfs" / restore._cache_name() / "pending.bin").is_file())
        self.assertTrue(cache.is_dir())
        self.assertEqual(sorted(path.name for path in cache.iterdir()), [f"{restore._cache_name()}.lock"])
        self.assertFalse(cache_data.exists())
        self.assertEqual(sibling_file.read_bytes(), b"unrelated generated data")

    def test_external_cache_works_without_unused_contained_cache_directory(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        cache_name = restore._cache_name()
        pending = self.add_cache_file("pending.bin", b"pending payload", cache=cache)
        pending_meta = cache / "vfsMeta" / cache_name / "pending.bin"
        identity = cache / f"{cache_name}.identity"
        expected = {
            "identity": identity.read_bytes(),
            "pending": pending.read_bytes(),
            "metadata": pending_meta.read_bytes(),
        }
        self.assertTrue((self.root / "cache").is_dir())
        (self.root / "cache").rmdir()

        code, stdout, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        self.assertEqual(json.loads(stdout)["mode"], "inventory")
        self.assertEqual(identity.read_bytes(), expected["identity"])
        self.assertEqual(pending.read_bytes(), expected["pending"])
        self.assertEqual(pending_meta.read_bytes(), expected["metadata"])

        destination = self.base / "retained-external-cache-without-legacy-dir"
        self.write_rclone("Pending local uploads: 1 files, 15 bytes")
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            code, stdout, stderr = self.invoke(
                "--delete-local", "--keep-config", "--keep-cache", "--keep-directory", str(destination)
            )

        self.assertEqual(code, 0, stderr)
        self.assertIn("were preserved in", stdout)
        retained_cache = destination / "cache"
        self.assertEqual((retained_cache / f"{cache_name}.identity").read_bytes(), expected["identity"])
        self.assertEqual((retained_cache / "vfs" / cache_name / "pending.bin").read_bytes(), expected["pending"])
        self.assertEqual((retained_cache / "vfsMeta" / cache_name / "pending.bin").read_bytes(), expected["metadata"])
        self.assertTrue(cache.is_dir())
        self.assertEqual(sorted(path.name for path in cache.iterdir()), [f"{cache_name}.lock"])
        self.assertFalse(self.root.exists())

    def test_external_cache_still_rejects_symlink_at_missing_contained_cache_path(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        outside = self.base / "outside-cache-data"
        outside.mkdir(mode=0o700)
        (self.root / "cache").rmdir()
        (self.root / "cache").symlink_to(outside, target_is_directory=True)

        code, _, stderr = self.invoke()

        self.assertEqual(code, 2)
        self.assertIn("symlink path components are not allowed", stderr)
        self.assertTrue((self.root / "cache").is_symlink())
        self.assertTrue(outside.is_dir())
        self.assertTrue(cache.is_dir())

    def test_missing_configured_contained_cache_is_still_rejected(self) -> None:
        (self.root / "cache").rmdir()

        code, _, stderr = self.invoke()

        self.assertEqual(code, 2)
        self.assertIn("a required path is missing", stderr)
        self.assertTrue(self.root.exists())

    def test_cache_cleanup_refuses_an_unknown_file_added_after_audit(self) -> None:
        cache = self.base / "external cache"
        self.set_external_cache(cache)
        unknown = cache / "unrelated.txt"
        unknown.write_bytes(b"keep these bytes")
        unknown.chmod(0o600)
        with mock.patch.object(uninstall, "_audit_cache_namespace", return_value=False):
            with self.assertRaisesRegex(uninstall.CleanupError, "unknown path appeared"):
                uninstall._remove_owned_cache(self.root, cache, uninstall._cache_lock_path(self.root))
        self.assertEqual(unknown.read_bytes(), b"keep these bytes")

    def test_external_cache_uses_its_actual_writer_lock(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        path = uninstall._cache_lock_path(self.root)
        self.assertEqual(path.parent, cache)
        fd = uninstall._acquire_cache_lock(self.root, 0)
        try:
            with self.assertRaisesRegex(uninstall.CleanupError, "cache is active"):
                uninstall._acquire_cache_lock(self.root, 0)
        finally:
            os.close(fd)

    def test_external_uninstall_refuses_files_left_in_legacy_cache(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        legacy_file = self.root / "cache/old-pending.bin"
        self.write_private(legacy_file, b"old local pending data")

        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("legacy install cache still contains local files", stderr)
        self.assertTrue(legacy_file.is_file())
        self.assertTrue(self.root.exists())
        self.assertTrue(cache.is_dir())

    def test_external_uninstall_refuses_a_mismatched_cache_identity(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        self.add_cache_file("pending.bin", b"pending payload", cache=cache)
        destination = self.base / "retained-mismatch"
        self.write_rclone("Pending local uploads: 0 files, 0 bytes")
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                mock.patch.object(uninstall, "_verify_cache_identity", side_effect=uninstall.CleanupError("identity mismatch")):
            code, _, stderr = self.invoke(
                "--delete-local", "--keep-config", "--keep-cache", "--keep-directory", str(destination)
            )

        self.assertEqual(code, 2)
        self.assertIn("identity mismatch", stderr)
        self.assertTrue(self.root.exists())
        self.assertTrue((cache / "vfs" / restore._cache_name() / "pending.bin").is_file())
        self.assertFalse(destination.exists())

    def test_uninstall_accepts_metadata_writer_lock_with_nested_lock_suffix(self) -> None:
        cache_name = uninstall._cache_name()
        metadata_namespace = self.root / "cache" / "metadata" / cache_name
        metadata_namespace.mkdir(parents=True, mode=0o700)
        metadata_namespace.chmod(0o700)
        metadata_namespace.parent.chmod(0o700)
        lock = metadata_namespace / ".lock.lock"
        self.write_private(lock, b"")

        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                mock.patch.object(uninstall, "_verify_cache_identity", side_effect=AssertionError("metadata lock is not cache data")):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertIn("Removed the isolated local Degoo install", stdout)
        self.assertFalse(self.root.exists())

    def test_metadata_cache_lock_variants_are_excluded_from_the_8192_entry_limit(self) -> None:
        cache_root = self.root / "cache"
        namespace = cache_root / "metadata" / uninstall._cache_name()
        namespace.mkdir(parents=True, mode=0o700)
        namespace.chmod(0o700)
        namespace.parent.chmod(0o700)
        for number in range(uninstall.MAX_METADATA_CACHE_ENTRIES):
            entry = namespace / f"{number:064x}.cache"
            descriptor = os.open(entry, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(descriptor)
        for name in (".lock", ".lock.lock"):
            self.write_private(namespace / name, b"")

        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            self.assertTrue(uninstall._audit_cache_tree(namespace, cache_root, metadata=True))
            # Keep the total name count within the 8192 files plus two lock
            # names limit so the separate cache-record limit is exercised.
            (namespace / ".lock.lock").unlink()
            overflow = namespace / f"{uninstall.MAX_METADATA_CACHE_ENTRIES:064x}.cache"
            descriptor = os.open(overflow, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
            os.close(descriptor)
            with self.assertRaisesRegex(uninstall.CleanupError, "bounded entry count"):
                uninstall._audit_cache_tree(namespace, cache_root, metadata=True)

    def test_metadata_cache_rejects_unsafe_lock_variants_and_unknown_files(self) -> None:
        cache_root = self.root / "cache"
        namespace = cache_root / "metadata" / uninstall._cache_name()
        namespace.mkdir(parents=True, mode=0o700)
        namespace.chmod(0o700)
        namespace.parent.chmod(0o700)
        lock = namespace / ".lock.lock"
        target = self.base / "lock-target"
        self.write_private(target, b"generated lock target")

        self.write_private(lock, b"")
        lock.chmod(0o644)
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                self.assertRaisesRegex(uninstall.CleanupError, "unsafe ownership, permissions, or size"):
            uninstall._audit_cache_tree(namespace, cache_root, metadata=True)
        lock.unlink()

        lock.symlink_to(target)
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                self.assertRaisesRegex(uninstall.CleanupError, "unsafe ownership, permissions, or size"):
            uninstall._audit_cache_tree(namespace, cache_root, metadata=True)
        lock.unlink()

        self.write_private(lock, b"")
        alias = self.base / "lock-alias"
        os.link(lock, alias)
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                self.assertRaisesRegex(uninstall.CleanupError, "unsafe ownership, permissions, or size"):
            uninstall._audit_cache_tree(namespace, cache_root, metadata=True)
        alias.unlink()
        lock.unlink()

        self.write_private(lock, b"x" * 4097)
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                self.assertRaisesRegex(uninstall.CleanupError, "unsafe ownership, permissions, or size"):
            uninstall._audit_cache_tree(namespace, cache_root, metadata=True)
        lock.unlink()

        self.write_private(namespace / ".lock.lock.extra", b"")
        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()), \
                self.assertRaisesRegex(uninstall.CleanupError, "unknown file"):
            uninstall._audit_cache_tree(namespace, cache_root, metadata=True)

    def test_external_uninstall_refuses_orphaned_vfs_data_before_stopping_runtime(self) -> None:
        cache = self.base / "external-cache"
        self.set_external_cache(cache)
        cache_name = restore._cache_name()
        data = cache / "vfs" / cache_name / "orphaned-pending.bin"
        data.parent.mkdir(parents=True, mode=0o700)
        data.parent.chmod(0o700)
        data.parent.parent.chmod(0o700)
        self.write_private(data, b"generated pending bytes")
        self.write_private(cache / f"{cache_name}.identity", b"a" * 64 + b"\n")

        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("recovery metadata is incomplete", stderr)
        self.assertTrue(self.root.exists())
        self.assertEqual(data.read_bytes(), b"generated pending bytes")
        self.assertTrue((cache / "vfs" / cache_name).is_dir())

    def test_retention_filters_account_auth_and_restores_pending_cache(self) -> None:
        cache_name = restore._cache_name()
        for parent_name in ("vfs", "vfsMeta"):
            parent = self.root / "cache" / parent_name
            parent.mkdir(mode=0o700)
            parent.chmod(0o700)
            remote_cache = parent / cache_name
            remote_cache.mkdir(mode=0o700)
            remote_cache.chmod(0o700)
        identity_bytes = b"a" * 64 + b"\n"
        pending_bytes = b"encrypted pending upload data"
        metadata_bytes = b"pending upload metadata"
        self.write_private(self.root / "cache" / f"{cache_name}.identity", identity_bytes)
        self.write_private(self.root / "cache" / "vfs" / cache_name / "pending.bin", pending_bytes)
        self.write_private(self.root / "cache" / "vfsMeta" / cache_name / "pending.bin", metadata_bytes)

        source_vault = (self.root / "config/vault.json").read_bytes()
        source_auth = (self.root / "config/auth.json").read_bytes()
        source_auth_lock = (self.root / "config/auth.json.lock").read_bytes()
        source_login = (self.root / "config/login.json").read_bytes()
        destination = self.base / "recoverable-retention"
        uninstall._retain_state(
            self.root,
            destination,
            keep_config=True,
            keep_cache=True,
            keep_logs=False,
            pending=True,
        )

        self.assertEqual((self.root / "config/vault.json").read_bytes(), source_vault)
        self.assertEqual((self.root / "config/auth.json").read_bytes(), source_auth)
        self.assertEqual((self.root / "config/auth.json.lock").read_bytes(), source_auth_lock)
        self.assertEqual((self.root / "config/login.json").read_bytes(), source_login)
        self.assertEqual((self.root / "cache" / f"{cache_name}.identity").read_bytes(), identity_bytes)
        self.assertEqual((self.root / "cache" / "vfs" / cache_name / "pending.bin").read_bytes(), pending_bytes)
        retained_vault = json.loads((destination / "config/vault.json").read_bytes())
        self.assertNotIn("account_credentials", retained_vault)
        self.assertEqual(retained_vault["password"], self.vault["password"])
        self.assertFalse((destination / "config/auth.json").exists())
        self.assertFalse((destination / "config/auth.json.lock").exists())
        self.assertEqual((destination / "config/login.json").read_bytes(), source_login)
        retained_contents = b"".join(
            path.read_bytes()
            for path in destination.rglob("*")
            if path.is_file()
        )
        self.assertNotIn(self.vault["account_credentials"].encode(), retained_contents)
        self.assertNotIn(b"test-only-token", retained_contents)
        self.assertEqual((destination / "cache" / f"{cache_name}.identity").read_bytes(), identity_bytes)
        self.assertEqual((destination / "cache" / "vfs" / cache_name / "pending.bin").read_bytes(), pending_bytes)

        target = self.base / "fresh-install"
        for name in ("bin", "cache", "config", "tmp"):
            path = target / name
            path.mkdir(parents=True, mode=0o700)
            path.chmod(0o700)
        target.chmod(0o700)
        target_vault = self.retained_vault | {"cache_dir": str(target / "cache")}
        self.write_private(target / "config/vault.json", (json.dumps(target_vault, sort_keys=True) + "\n").encode())
        self.write_private(target / "config/rclone.conf", install.rclone_config(target, "generated-test-folder-id"))
        binary = target / "bin/rclone"
        binary.write_text("#!/bin/sh\nexit 0\n", encoding="ascii")
        binary.chmod(0o700)

        backup = restore.inspect_backup(destination, target)
        self.assertTrue(backup.cache_has_data)
        self.assertFalse(hasattr(backup, "auth_bytes"))
        self.assertEqual(backup.login_bytes, source_login)
        with mock.patch.object(restore.subprocess, "run", return_value=mock.Mock(returncode=0)):
            restore.restore_to_install(backup, target)

        self.assertEqual((target / "cache" / f"{cache_name}.identity").read_bytes(), identity_bytes)
        self.assertEqual((target / "cache" / "vfs" / cache_name / "pending.bin").read_bytes(), pending_bytes)
        self.assertEqual((target / "cache" / "vfsMeta" / cache_name / "pending.bin").read_bytes(), metadata_bytes)
        self.assertFalse((target / "config/auth.json").exists())
        self.assertEqual((target / "config/login.json").read_bytes(), source_login)

    def test_pending_uploads_still_refuse_removal_without_both_retained_groups(self) -> None:
        destination = self.base / "config-only"
        self.write_rclone("Pending local uploads: 1 files, 12 bytes")
        code, _, stderr = self.invoke(
            "--delete-local",
            "--keep-config",
            "--keep-directory",
            str(destination),
        )
        self.assertEqual(code, 2)
        self.assertIn("pending local uploads remain", stderr)
        self.assertTrue(self.root.exists())
        self.assertFalse(destination.exists())

    def test_retention_directory_rejects_symlinked_ancestors(self) -> None:
        outside = self.base / "outside"
        outside.mkdir(mode=0o700)
        link = self.base / "backup-link"
        link.symlink_to(outside, target_is_directory=True)
        destination = link / "retained"
        code, _, stderr = self.invoke(
            "--delete-local",
            "--keep-config",
            "--keep-directory",
            str(destination),
        )
        self.assertEqual(code, 2)
        self.assertIn("symlink path components", stderr)
        self.assertTrue(self.root.exists())
        self.assertFalse((outside / "retained").exists())

    def test_retention_copy_failure_preserves_install(self) -> None:
        destination = self.base / "partial-retention"
        with mock.patch.object(uninstall, "_copy_retained_file", side_effect=uninstall.CleanupError("copy failed")):
            code, _, stderr = self.invoke(
                "--delete-local",
                "--keep-config",
                "--keep-directory",
                str(destination),
            )
        self.assertEqual(code, 2)
        self.assertIn("copy failed", stderr)
        self.assertTrue(self.root.exists())

    def test_local_delete_requires_a_vault_backup_or_explicit_discard(self) -> None:
        code, _, stderr = self.invoke("--delete-local")
        self.assertEqual(code, 2)
        self.assertIn("requires --vault-backup or --discard-vault-secret", stderr)
        self.assertTrue(self.root.exists())

    def test_backup_must_be_private_and_match_vault_configuration(self) -> None:
        directory = self.base / "backup"
        directory.mkdir(mode=0o700)
        directory.chmod(0o700)
        mismatch = directory / "vault.json"
        mismatch_vault = self.retained_vault | {"password": "different"}
        self.write_private(mismatch, (json.dumps(mismatch_vault, sort_keys=True) + "\n").encode())
        code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(mismatch))
        self.assertEqual(code, 2)
        self.assertIn("does not match", stderr)
        self.assertTrue(self.root.exists())

        valid = directory / "valid.json"
        self.write_private(valid, self.vault_bytes)
        valid.chmod(0o644)
        code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(valid))
        self.assertEqual(code, 2)
        self.assertIn("unsafe ownership or permissions", stderr)
        self.assertTrue(self.root.exists())

    def test_sanitized_vault_backup_matches_without_changing_installed_credentials(self) -> None:
        directory = self.base / "sanitized-vault-backup"
        directory.mkdir(mode=0o700)
        directory.chmod(0o700)
        backup = directory / "vault.json"
        self.write_private(backup, self.retained_vault_bytes)
        original_vault_bytes = (self.root / "config/vault.json").read_bytes()
        self.write_rclone("Pending local uploads: 2 files, 123 bytes")

        code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))

        self.assertEqual(code, 2)
        self.assertIn("pending local uploads remain", stderr)
        self.assertNotIn("vault backup does not match", stderr)
        self.assertTrue(self.root.exists())
        self.assertEqual((self.root / "config/vault.json").read_bytes(), original_vault_bytes)
        self.assertEqual(
            json.loads((self.root / "config/vault.json").read_bytes())["account_credentials"],
            self.vault["account_credentials"],
        )

    def test_pending_uploads_preserve_local_install_and_cache(self) -> None:
        self.write_rclone("Pending local uploads: 2 files, 123 bytes")
        backup = self.backup()
        code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("pending local uploads remain", stderr)
        self.assertTrue(self.root.exists())
        self.assertTrue((self.root / "config/vault.json").exists())

    def test_refuses_when_install_root_is_a_mountpoint(self) -> None:
        with mock.patch.object(uninstall, "read_mountpoints", return_value={str(self.root)}):
            code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 2)
        self.assertIn("itself a mountpoint", stderr)
        self.assertTrue(self.root.exists())

    def test_refuses_unexpected_mount_inside_install(self) -> None:
        with mock.patch.object(uninstall, "read_mountpoints", return_value={str(self.root / "cache")}):
            code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 2)
        self.assertIn("unexpected mount exists inside", stderr)
        self.assertTrue(self.root.exists())

    def test_immutable_legacy_mountpoint_refuses_before_removing_owned_links(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(mode=0o700)
        bin_dir.chmod(0o700)
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        path_link = bin_dir / "degoo-rc"
        path_link.symlink_to(self.root / "degoo-rc")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(path_link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        (self.root / "mount").chmod(0o500)
        calls, fake = self._fake_systemctl(direct, wanted, unit)

        def get_immutable_flags(fd: int, request: int, flags: bytearray, mutate: bool) -> int:
            del fd, request, mutate
            struct.pack_into("@L", flags, 0, uninstall.FS_IMMUTABLE_FL)
            return 0

        with (
            mock.patch.object(uninstall.fcntl, "ioctl", side_effect=get_immutable_flags),
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn(f"sudo chattr -i -- {self.root / 'mount'}", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertEqual(calls, [])
        self.assertTrue(direct.is_symlink())
        self.assertTrue(wanted.is_symlink())
        self.assertTrue(path_link.is_symlink())
        self.assertTrue(self.root.exists())

    def test_read_only_install_directory_can_be_removed(self) -> None:
        self.write_private(self.root / "logs/old.log", b"old diagnostic")
        self.root.chmod(0o500)
        (self.root / "mount").chmod(0o500)
        (self.root / "logs").chmod(0o500)

        code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertIn("Removed the isolated local Degoo install", stdout)
        self.assertFalse(self.root.exists())

    def test_cleanup_refuses_success_if_install_root_remains(self) -> None:
        with mock.patch.object(uninstall, "_remove_root"):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("install root remains after cleanup", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertTrue(self.root.exists())

    def test_cleanup_refuses_success_if_owned_path_link_reappears(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(mode=0o700)
        bin_dir.chmod(0o700)
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        path_link = bin_dir / "degoo-rc"
        path_link.symlink_to(self.root / "degoo-rc")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(path_link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        remove_root = uninstall._remove_root

        def remove_root_and_restore_link(root: Path) -> None:
            remove_root(root)
            path_link.symlink_to(root / "degoo-rc")

        with mock.patch.object(uninstall, "_remove_root", side_effect=remove_root_and_restore_link):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("owned PATH command link remains after cleanup", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertFalse(self.root.exists())
        self.assertTrue(path_link.is_symlink())

    def test_cleanup_verification_does_not_treat_lstat_errors_as_absence(self) -> None:
        with (
            mock.patch.object(uninstall.os, "lstat", side_effect=PermissionError("denied")),
            self.assertRaisesRegex(uninstall.CleanupError, "could not be verified safely"),
        ):
            uninstall._verify_cleanup(self.root, [], False)

    def test_successful_uninstall_preserves_external_mountpoint(self) -> None:
        external_mount = self.base / "custom mount"
        external_mount.mkdir(mode=0o700)
        external_mount.chmod(0o500)
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["mountpoint"] = str(external_mount)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")

        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 0, stderr)
        self.assertTrue(external_mount.is_dir())
        self.assertEqual(external_mount.stat().st_mode & 0o777, 0o500)
        self.assertFalse(self.root.exists())

    def test_uninstall_refuses_nonempty_custom_external_mountpoint_without_touching_data(self) -> None:
        external_mount = self.base / "custom mount"
        external_mount.mkdir(mode=0o700)
        payload = external_mount / "ordinary-local-file.txt"
        payload.write_bytes(b"keep this data")
        payload.chmod(0o600)
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["mountpoint"] = str(external_mount)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")

        with mock.patch.object(uninstall, "read_mountpoints", return_value=set()):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("unmounted mount directory contains local files", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertEqual(payload.read_bytes(), b"keep this data")
        self.assertTrue(self.root.exists())

    def test_external_mount_selection_refuses_nonempty_contained_mount_before_cleanup(self) -> None:
        external_mount = self.base / "mnt/degoo"
        external_mount.mkdir(parents=True, mode=0o700)
        (self.root / "mount/local-state").write_text("keep", encoding="ascii")
        (self.root / "mount").chmod(0o500)
        direct, wanted, unit = self._add_owned_service_links()
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(mode=0o700)
        bin_dir.chmod(0o700)
        (self.root / "degoo-rc").write_text("#!/bin/sh\n", encoding="ascii")
        (self.root / "degoo-rc").chmod(0o700)
        path_link = bin_dir / "degoo-rc"
        path_link.symlink_to(self.root / "degoo-rc")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["path_link"] = str(path_link)
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        calls, fake = self._fake_systemctl(direct, wanted, unit)

        with (
            mock.patch.object(install, "configured_mountpoint", return_value=external_mount),
            mock.patch.object(uninstall, "read_mountpoints", return_value={str(external_mount)}),
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "_unmount") as unmount,
        ):
            code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")

        self.assertEqual(code, 2)
        self.assertIn("unused contained mount directory contains local files", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertEqual(calls, [])
        unmount.assert_not_called()
        self.assertTrue(direct.is_symlink())
        self.assertTrue(wanted.is_symlink())
        self.assertTrue(path_link.is_symlink())
        self.assertEqual((self.root / "mount").stat().st_mode & 0o777, 0o500)
        self.assertEqual((self.root / "mount/local-state").read_text(encoding="ascii"), "keep")
        self.assertTrue(self.root.exists())

    def test_unmount_targets_only_the_isolated_mountpoint(self) -> None:
        helper = self.root / "tools/fusermount3"
        with (
            mock.patch.object(uninstall, "_validate_helper_link", return_value=helper),
            mock.patch.object(uninstall.subprocess, "run", return_value=mock.Mock(returncode=0)) as run,
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            uninstall._unmount(self.root)
        command = run.call_args.args[0]
        self.assertEqual(command, [str(helper), "-u", str(self.root / "mount")])
        self.assertEqual(run.call_args.kwargs["stdout"], uninstall.subprocess.DEVNULL)
        self.assertEqual(run.call_args.kwargs["stderr"], uninstall.subprocess.DEVNULL)

    def test_refuses_symlink_root(self) -> None:
        moved = self.root.with_name("rclone-degoo-real")
        self.root.rename(moved)
        self.root.symlink_to(moved, target_is_directory=True)
        code, _, stderr = self.invoke()
        self.assertEqual(code, 2)
        self.assertIn("symlink path components", stderr)
        self.assertTrue(moved.exists())

    def test_refuses_a_root_outside_the_isolated_location(self) -> None:
        outside = self.base / "other-root"
        outside.mkdir(mode=0o700)
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = uninstall.main(["--root", str(outside)])
        self.assertEqual(code, 2)
        self.assertIn("must be the isolated", stderr.getvalue())
        self.assertTrue(outside.exists())

    def test_refuses_unexpected_symlinks_inside_install(self) -> None:
        target = self.base / "outside"
        target.mkdir(mode=0o700)
        (self.root / "cache/escape").symlink_to(target, target_is_directory=True)
        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 2)
        self.assertIn("unexpected symlinks", stderr)
        self.assertTrue(target.exists())
        self.assertTrue(self.root.exists())

    def test_refuses_active_vfs_cache_lock(self) -> None:
        lock_path = uninstall._cache_lock_path(self.root)
        fd = os.open(lock_path, os.O_CREAT | os.O_RDWR, 0o600)
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            backup = self.backup()
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
            self.assertEqual(code, 2)
            self.assertIn("cache is active", stderr)
            self.assertTrue(self.root.exists())
        finally:
            os.close(fd)

    def test_partial_install_can_be_removed_only_with_empty_cache(self) -> None:
        (self.root / "config/rclone.conf").unlink()
        (self.root / "bin/rclone").unlink()
        manifest_path = self.root / "install.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["root_folder_id"] = ""
        manifest["remote_folder_name"] = ""
        manifest["remote_created_by_setup"] = False
        self.write_private(manifest_path, json.dumps(manifest, sort_keys=True).encode() + b"\n")
        code, stdout, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 0, stderr)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())

    def test_partial_install_with_cache_files_is_preserved(self) -> None:
        (self.root / "config/rclone.conf").unlink()
        (self.root / "bin/rclone").unlink()
        manifest_path = self.root / "install.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["root_folder_id"] = ""
        manifest["remote_created_by_setup"] = False
        self.write_private(manifest_path, json.dumps(manifest, sort_keys=True).encode() + b"\n")
        self.write_private(self.root / "cache/pending", b"local cache data")
        code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 2)
        self.assertIn("cache is not empty", stderr)
        self.assertTrue(self.root.exists())

    def test_staged_install_without_vault_secret_can_be_removed_if_cache_is_empty(self) -> None:
        (self.root / "config/vault.json").unlink()
        (self.root / "config/rclone.conf").unlink()
        manifest_path = self.root / "install.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        manifest["root_folder_id"] = ""
        manifest["remote_folder_name"] = ""
        manifest["remote_created_by_setup"] = False
        self.write_private(manifest_path, json.dumps(manifest, sort_keys=True).encode() + b"\n")
        code, _, stderr = self.invoke("--delete-local")
        self.assertEqual(code, 0, stderr)
        self.assertFalse(self.root.exists())

    def test_success_removes_only_install_root_after_zero_status(self) -> None:
        backup = self.backup()
        outside = self.base / "outside.txt"
        outside.write_text("keep", encoding="utf-8")
        code, stdout, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual(outside.read_text(encoding="utf-8"), "keep")
        self.assertTrue(backup.exists())

    def _add_owned_service_links(self) -> tuple[Path, Path, Path]:
        config_home = self.home / ".config"
        unit_dir = config_home / "systemd/user"
        wants_dir = unit_dir / "default.target.wants"
        wants_dir.mkdir(parents=True, mode=0o700)
        for directory in (config_home, config_home / "systemd", unit_dir, wants_dir):
            directory.chmod(0o700)
        unit = self.root / "rclone-degoo.service"
        self.write_private(unit, b"[Service]\n")
        direct = unit_dir / "rclone-degoo.service"
        wanted = wants_dir / "rclone-degoo.service"
        direct.symlink_to(unit)
        wanted.symlink_to(unit)
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["service_links"] = [str(direct), str(wanted)]
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        return direct, wanted, unit

    def _fake_systemctl(
        self,
        direct: Path,
        wanted: Path,
        unit: Path,
        *,
        disable_status: int = 0,
        reload_status: int = 0,
        active_after_reload: str = "inactive",
        load_after_reload: str = "not-found",
        initial_fragment: Path | None = None,
        fragment_after_reload: Path | None = None,
    ):
        calls: list[list[str]] = []
        real_run = subprocess.run
        state = {
            "load": "loaded",
            "active": "active",
            "fragment": str(initial_fragment or direct),
        }

        def run(command: list[str], **kwargs: object):
            if command[0] != "systemctl":
                return real_run(command, **kwargs)
            args = command[2:]
            calls.append(args)
            if args[0] == "show":
                stdout = (
                    f"LoadState={state['load']}\n"
                    f"ActiveState={state['active']}\n"
                    f"FragmentPath={state['fragment']}\n"
                ).encode()
            else:
                stdout = b""
            returncode = 0
            if args[0] == "stop":
                state["active"] = "inactive"
            if args[0] == "disable":
                for path in (wanted, direct):
                    if path.is_symlink():
                        path.unlink()
                returncode = disable_status
            if args[0] == "daemon-reload":
                state["active"] = active_after_reload
                if not direct.exists() and not wanted.exists():
                    state["load"] = load_after_reload
                    state["fragment"] = str(fragment_after_reload) if fragment_after_reload else ""
                returncode = reload_status
            return uninstall.subprocess.CompletedProcess(command, returncode, stdout=stdout, stderr=b"")

        return calls, run

    def test_dry_run_lists_external_owned_service_links(self) -> None:
        direct, wanted, _ = self._add_owned_service_links()
        code, stdout, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        links = json.loads(stdout)["service_links"]
        self.assertEqual({entry["path"] for entry in links}, {str(direct), str(wanted)})
        self.assertTrue(all(entry["owned"] and entry["present"] for entry in links))
        self.assertTrue(direct.is_symlink())
        self.assertTrue(wanted.is_symlink())

    def test_pending_upload_check_happens_before_stopping_service(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        self.write_rclone("Pending local uploads: 1 files, 12 bytes")
        calls, fake = self._fake_systemctl(direct, wanted, unit)
        with mock.patch.object(uninstall.subprocess, "run", side_effect=fake):
            code, _, stderr = self.invoke("--delete-local", "--discard-vault-secret")
        self.assertEqual(code, 2)
        self.assertIn("pending local uploads remain", stderr)
        self.assertEqual(calls, [])
        self.assertTrue(direct.is_symlink())
        self.assertTrue(wanted.is_symlink())
        self.assertTrue(self.root.exists())

    def test_uninstall_stops_before_disabling_and_reloads_after_removing_owned_links(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        unrelated = direct.parent / "other.service"
        unrelated.write_text("keep", encoding="ascii")
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit)
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())
        self.assertFalse(direct.exists())
        self.assertFalse(wanted.exists())
        self.assertEqual(unrelated.read_text(encoding="ascii"), "keep")
        self.assertEqual(
            [call[0] for call in calls],
            ["show", "stop", "disable", "daemon-reload", "show"],
        )
        self.assertNotIn("--now", calls[2])
        self.assertIn("owned user service", stdout)
        self.assertIn("systemd was reloaded", stdout)

    def test_disable_failure_after_systemd_removes_links_keeps_install_for_retry(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit, disable_status=1)
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("could not be disabled safely", stderr)
        self.assertNotIn("Removed the isolated local Degoo install", stdout)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable"])
        self.assertNotIn("--now", calls[-1])
        self.assertFalse(direct.exists())
        self.assertFalse(wanted.exists())
        self.assertTrue(self.root.exists())

    def test_missing_recorded_links_still_stop_exact_loaded_root_unit(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        direct.unlink()
        wanted.unlink()
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit, initial_fragment=unit)
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "daemon-reload", "show"])
        self.assertFalse(self.root.exists())

    def test_daemon_reload_failure_keeps_root_and_mount_layout_for_retry(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit, reload_status=1)
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("could not reload after removing", stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable", "daemon-reload"])
        self.assertTrue(self.root.exists())
        self.assertTrue((self.root / "mount").is_dir())

    def test_exact_owned_link_missing_from_manifest_is_cleaned(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["service_links"] = []
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit)
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable", "daemon-reload", "show"])
        self.assertFalse(direct.exists())
        self.assertFalse(wanted.exists())
        self.assertFalse(self.root.exists())

    def test_unrecorded_home_link_is_found_when_wrapper_config_home_is_inside_install(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        wanted.unlink()
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        manifest["service_config_home"] = ""
        manifest["service_links"] = []
        self.write_private(self.root / "install.json", json.dumps(manifest, sort_keys=True).encode() + b"\n")
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit)
        with (
            mock.patch.dict(os.environ, {"XDG_CONFIG_HOME": str(self.root / "config")}),
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable", "daemon-reload", "show"])
        self.assertFalse(direct.exists())
        self.assertFalse(self.root.exists())

    def test_conflicting_systemd_fragment_is_never_stopped(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        backup = self.backup()
        conflict = self.base / "other" / "rclone-degoo.service"
        calls, fake = self._fake_systemctl(direct, wanted, unit, initial_fragment=conflict)
        with mock.patch.object(uninstall.subprocess, "run", side_effect=fake):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("resolved the service name to another unit", stderr)
        self.assertEqual([call[0] for call in calls], ["show"])
        self.assertTrue(self.root.exists())
        self.assertTrue(direct.is_symlink())
        self.assertTrue(wanted.is_symlink())

    def test_active_owned_unit_after_reload_prevents_root_removal(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        backup = self.backup()
        calls, fake = self._fake_systemctl(direct, wanted, unit, active_after_reload="active")
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("still active after cleanup", stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable", "daemon-reload", "show"])
        self.assertTrue(self.root.exists())

    def test_failed_unit_state_is_not_reported_as_inactive_after_cleanup(self) -> None:
        with self.assertRaisesRegex(uninstall.CleanupError, "inactive"):
            uninstall._check_service_fragment(
                {"LoadState": "not-found", "ActiveState": "failed", "FragmentPath": ""},
                set(),
            )

    def test_loaded_inactive_owned_unit_after_reload_prevents_root_removal(self) -> None:
        direct, wanted, unit = self._add_owned_service_links()
        backup = self.backup()
        calls, fake = self._fake_systemctl(
            direct,
            wanted,
            unit,
            load_after_reload="loaded",
            fragment_after_reload=unit,
        )
        with (
            mock.patch.object(uninstall.subprocess, "run", side_effect=fake),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 2)
        self.assertIn("still has the owned service loaded", stderr)
        self.assertEqual([call[0] for call in calls], ["show", "stop", "disable", "daemon-reload", "show"])
        self.assertTrue(self.root.exists())

    def test_unrecorded_unrelated_same_name_link_is_left_alone(self) -> None:
        unit_dir = self.home / ".config/systemd/user"
        unit_dir.mkdir(parents=True, mode=0o700)
        for directory in (self.home / ".config", self.home / ".config/systemd", unit_dir):
            directory.chmod(0o700)
        other_unit = self.base / "other.service"
        other_unit.write_text("keep", encoding="ascii")
        direct = unit_dir / "rclone-degoo.service"
        direct.symlink_to(other_unit)
        backup = self.backup()
        calls: list[list[str]] = []
        with (
            mock.patch.object(uninstall, "_systemctl_user", side_effect=AssertionError("unrelated unit queried")),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, _, stderr = self.invoke("--delete-local", "--vault-backup", str(backup))
        self.assertEqual(code, 0, stderr)
        self.assertTrue(direct.is_symlink())
        self.assertEqual(direct.resolve(), other_unit)
        self.assertEqual(other_unit.read_text(encoding="ascii"), "keep")


if __name__ == "__main__":
    unittest.main()
