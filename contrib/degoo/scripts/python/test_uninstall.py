"""Offline safety tests for the local Degoo install cleanup utility."""

from __future__ import annotations

import contextlib
import fcntl
import io
import json
import os
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import uninstall  # noqa: E402


class UninstallTest(unittest.TestCase):
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
            "cache_dir": str(self.root / "cache"),
        }
        self.vault_bytes = (json.dumps(self.vault, sort_keys=True) + "\n").encode()
        self.write_private(self.root / "config/vault.json", self.vault_bytes)
        self.write_private(self.root / "config/auth.json", b'{"access_token":"test-only-token"}\n')
        self.write_private(self.root / "config/rclone.conf", b"[degoo-live-test]\ntype = degoo\n")
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

    def test_default_root_is_the_contained_install(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = uninstall.main([])
        self.assertEqual(code, 0, stderr.getvalue())
        self.assertEqual(json.loads(stdout.getvalue())["root"], str(self.root))

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
        self.write_private(self.root / "cache/pending.bin", b"pending bytes")
        with (
            mock.patch.object(uninstall, "_default_retention_directory", return_value=destination),
            mock.patch.object(uninstall, "read_mountpoints", return_value=set()),
        ):
            code, stdout, stderr = self.invoke_terminal("y\ny\nn\n\nREMOVE\n")
        self.assertEqual(code, 0, stderr)
        self.assertIn("Remote encrypted files were left untouched", stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual((destination / "config/vault.json").read_bytes(), self.vault_bytes)
        self.assertEqual((destination / "config/auth.json").read_bytes(), b'{"access_token":"test-only-token"}\n')
        self.assertEqual((destination / "cache/pending.bin").read_bytes(), b"pending bytes")
        self.assertFalse((destination / "logs").exists())
        self.assertEqual(destination.stat().st_mode & 0o777, 0o700)
        self.assertEqual((destination / "config/vault.json").stat().st_mode & 0o777, 0o600)

    def test_wizard_requires_explicit_secret_discard_confirmation(self) -> None:
        code, stdout, stderr = self.invoke_terminal("n\nn\nn\nno\n")
        self.assertEqual(code, 1, stderr)
        self.assertIn("Cancelled", stdout)
        self.assertTrue(self.root.exists())

    def test_pending_uploads_can_be_retained_only_with_config_and_cache(self) -> None:
        destination = self.base / "pending-recovery"
        self.write_private(self.root / "cache/pending.bin", b"pending bytes")
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
        self.assertFalse(self.root.exists())
        self.assertEqual((destination / "cache/pending.bin").read_bytes(), b"pending bytes")
        self.assertEqual((destination / "config/vault.json").read_bytes(), self.vault_bytes)
        self.assertTrue((destination / "bin/rclone").stat().st_mode & 0o100)
        self.assertTrue((destination / "install.json").exists())

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
        self.write_private(mismatch, b'{"password":"different"}\n')
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

    def _fake_systemctl(self, direct: Path, wanted: Path, unit: Path):
        calls: list[list[str]] = []
        real_run = subprocess.run

        def run(command: list[str], **kwargs: object):
            if command[0] != "systemctl":
                return real_run(command, **kwargs)
            args = command[2:]
            calls.append(args)
            if args[0] == "show":
                stdout = str(direct).encode()
            else:
                stdout = b""
            if args[0] == "disable":
                for path in (wanted, direct):
                    if path.is_symlink():
                        path.unlink()
            return uninstall.subprocess.CompletedProcess(command, 0, stdout=stdout, stderr=b"")

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

    def test_uninstall_disables_and_removes_only_its_external_service_links(self) -> None:
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
        self.assertEqual([call[0] for call in calls], ["show", "disable", "daemon-reload"])


if __name__ == "__main__":
    unittest.main()
