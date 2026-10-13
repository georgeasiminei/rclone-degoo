"""Offline contracts for unattended package updates."""

import hashlib
import io
import json
import tarfile
import tempfile
import unittest
from pathlib import Path

import update


class ArchiveTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.archive = self.base / update.ARCHIVE_NAME

    def package(self, entries):
        with tarfile.open(self.archive, "w:gz") as archive:
            for name, data, kind in entries:
                member = tarfile.TarInfo(name)
                member.mode = 0o755
                member.type = kind
                member.size = len(data) if kind == tarfile.REGTYPE else 0
                archive.addfile(member, io.BytesIO(data) if member.isfile() else None)
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        self.archive.with_name(self.archive.name + ".sha256").write_text(f"{digest}  {self.archive.name}\n")

    def test_bad_checksum_is_rejected_before_extraction(self):
        self.package([])
        self.archive.write_bytes(b"changed")
        with self.assertRaisesRegex(update.UpdateError, "checksum"):
            update.extract_package(self.archive, self.base / "extracted")
        self.assertFalse((self.base / "extracted").exists())

    def test_unsafe_archive_entries_are_rejected(self):
        for name, kind in [("rclone-degoo/../config/auth.json", tarfile.REGTYPE),
                           ("/rclone-degoo/degoo-rc", tarfile.REGTYPE),
                           ("rclone-degoo/degoo-rc", tarfile.SYMTYPE),
                           ("rclone-degoo/config/vault.json", tarfile.REGTYPE)]:
            with self.subTest(name=name, kind=kind):
                self.package([(name, b"generated", kind)])
                with self.assertRaises(update.UpdateError):
                    update.extract_package(self.archive, self.base / "extracted")
                self.assertFalse((self.base / "extracted").exists())

    def test_duplicate_paths_are_rejected(self):
        self.package([("rclone-degoo/degoo-rc", b"one", tarfile.REGTYPE),
                      ("rclone-degoo/degoo-rc", b"two", tarfile.REGTYPE)])
        with self.assertRaises(update.UpdateError):
            update.extract_package(self.archive, self.base / "extracted")


class UpdateTest(unittest.TestCase):
    def setUp(self):
        import os
        from unittest import mock
        import install
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home with spaces"
        self.home.mkdir(mode=0o700)
        self.environment = mock.patch.dict(os.environ, HOME=str(self.home), PYTHONDONTWRITEBYTECODE="1")
        self.environment.start()
        self.addCleanup(self.environment.stop)
        self.root = install.expected_root()
        self.binary = self.base / "rclone"
        self.write_binary("old")
        with mock.patch.object(install, "install_fuse_helper"):
            with self.output():
                self.assertEqual(install.main(["--binary", str(self.binary), "--folder-id", "generated-vault", "--mountpoint", "mount", "--no-wizard"]), 0)
        self.archive = self.base / update.ARCHIVE_NAME

    def output(self):
        import contextlib
        return contextlib.redirect_stdout(io.StringIO())

    def write_binary(self, revision):
        self.binary.write_text("#!/bin/sh\n# " + revision + "\ncase \"$*\" in\n*'degoo status'*) echo 'pending files: 0, pending bytes: 0';;\n*) echo 'Usage: degoo login mount status folders create-vault location';;\nesac\n")
        self.binary.chmod(0o700)

    def package(self):
        import install
        source = Path(install.__file__).resolve().parents[2]
        entries = {"bin/rclone": self.binary.read_bytes(),
                   "degoo-rc": (source / "degoo-rc").read_bytes()}
        for name in install.SOURCE_FILES:
            entries[f"scripts/python/{name}"] = (source / "scripts/python" / name).read_bytes()
        for name in install.SOURCE_SHELL_FILES:
            entries[f"scripts/{name}"] = (source / "scripts" / name).read_bytes()
        for name in install.IMMUTABLE_MOUNT_FILES:
            entries[f"immutable-mount/{name}"] = (source / "immutable-mount" / name).read_bytes()
        entries["docs/BUILD.json"] = json.dumps({"version": 1, "binary_sha256": hashlib.sha256(self.binary.read_bytes()).hexdigest(), "commit": "a" * 40, "source_dirty": False}).encode()
        with tarfile.open(self.archive, "w:gz") as archive:
            for name, data in entries.items():
                member = tarfile.TarInfo("rclone-degoo/" + name)
                member.size = len(data)
                member.mode = 0o755 if name == "degoo-rc" or name.startswith("bin/") or name.endswith(".sh") else 0o644
                archive.addfile(member, io.BytesIO(data))
        digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        self.archive.with_name(self.archive.name + ".sha256").write_text(f"{digest}  {self.archive.name}\n")

    def invoke(self, *arguments):
        import contextlib
        output, error = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(error):
            code = update.main(["--root", str(self.root), "--archive", str(self.archive), *arguments])
        return code, output.getvalue(), error.getvalue()

    def test_actual_staged_installer_preserves_config_cache_and_records_hash(self):
        from unittest import mock
        config = {path.name: path.read_bytes() for path in (self.root / "config").iterdir()}
        pending = self.root / "cache/generated-pending-file"
        pending.write_bytes(b"generated local pending bytes")
        self.write_binary("new")
        self.package()
        with mock.patch.object(update, "stop_runtime") as stop, mock.patch.object(update, "start_runtime") as start:
            code, output, error = self.invoke()
        self.assertEqual(code, 0, output + error)
        self.assertEqual((self.root / "bin/rclone").read_bytes(), self.binary.read_bytes())
        self.assertEqual(config, {path.name: path.read_bytes() for path in (self.root / "config").iterdir()})
        self.assertEqual(pending.read_bytes(), b"generated local pending bytes")
        build = json.loads((self.root / "install.json").read_bytes())["build"]
        self.assertEqual(build["package_sha256"], hashlib.sha256(self.archive.read_bytes()).hexdigest())
        self.assertEqual(build["binary_sha256"], hashlib.sha256(self.binary.read_bytes()).hexdigest())
        self.assertEqual(build["commit"], "a" * 40)
        stop.assert_called_once()
        start.assert_called_once()
        self.assertFalse((self.root / "tmp/update-backup").exists())

    def test_identical_payload_does_nothing_even_with_delete_cache_and_legacy_identity(self):
        from unittest import mock
        self.package()
        manifest = json.loads((self.root / "install.json").read_bytes())
        manifest.pop("build")
        (self.root / "install.json").write_text(json.dumps(manifest))
        before = (self.root / "install.json").read_bytes()
        pending = self.root / "cache/pending"
        pending.write_bytes(b"generated")
        with mock.patch.object(update, "apply_update") as apply:
            code, output, error = self.invoke("--delete-cache")
        self.assertEqual(code, 0, error)
        self.assertIn("Already up to date", output)
        apply.assert_not_called()
        self.assertEqual((self.root / "install.json").read_bytes(), before)
        self.assertEqual(pending.read_bytes(), b"generated")
        self.assertFalse((self.root / "tmp/update.lock").exists())

    def test_installer_failure_restores_program_and_restart_is_attempted(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        old_binary = (self.root / "bin/rclone").read_bytes()
        old_manifest = (self.root / "install.json").read_bytes()
        def failing_install(root, package, lock):
            (root / "bin/rclone").write_bytes(b"broken install")
            (root / "scripts/python/cli.py").write_bytes(b"broken helper")
            raise update.UpdateError("generated installation failure")
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime") as start, mock.patch.object(update, "install_package", side_effect=failing_install):
            code, output, error = self.invoke()
        self.assertEqual(code, 2)
        self.assertIn("restored", error)
        self.assertEqual((self.root / "bin/rclone").read_bytes(), old_binary)
        self.assertEqual((self.root / "install.json").read_bytes(), old_manifest)
        start.assert_called_once()
        self.assertFalse((self.root / "tmp/update-backup").exists())

    def test_delete_cache_refuses_pending_and_unknown_state(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        original = (self.root / "bin/rclone").read_bytes()
        cached = self.root / "cache/generated"
        cached.write_bytes(b"pending")
        for status in [(1, 7), update.uninstall.CleanupError("pending-upload status could not be verified")]:
            with self.subTest(status=status), mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime"), mock.patch.object(update.uninstall, "_pending_uploads") as check:
                if isinstance(status, Exception):
                    check.side_effect = status
                else:
                    check.return_value = status
                code, output, error = self.invoke("--delete-cache")
            self.assertEqual(code, 2, output + error)
            self.assertEqual(cached.read_bytes(), b"pending")
            self.assertEqual((self.root / "bin/rclone").read_bytes(), original)

    def test_delete_cache_preserves_held_lock_inode(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        cache_name = update.uninstall._cache_lock_path(self.root).stem
        data_root = self.root / "cache/vfs" / cache_name
        meta_root = self.root / "cache/vfsMeta" / cache_name
        data_root.mkdir(parents=True, mode=0o700)
        meta_root.mkdir(parents=True, mode=0o700)
        (self.root / "cache/vfs").chmod(0o700)
        (self.root / "cache/vfsMeta").chmod(0o700)
        cached = data_root / "generated-clean-file"
        cached.write_bytes(b"clean")
        cached.chmod(0o600)
        metadata = meta_root / cached.name
        metadata.write_text(json.dumps({"Dirty": False, "Size": 5, "Fingerprint": "generated fingerprint"}))
        metadata.chmod(0o600)
        identity = self.root / "cache" / f"{cache_name}.identity"
        identity.write_text("a" * 64 + "\n")
        identity.chmod(0o600)
        lock = update.uninstall._cache_lock_path(self.root)
        lock.write_bytes(b"")
        lock.chmod(0o600)
        inode = lock.stat().st_ino
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime"), mock.patch.object(update.uninstall, "_pending_uploads", return_value=(0, 0)):
            code, output, error = self.invoke("--delete-cache")
        self.assertEqual(code, 0, output + error)
        self.assertFalse(cached.exists())
        self.assertEqual(lock.stat().st_ino, inode)

    def test_restart_failure_reports_error_with_new_build_and_cache_preserved(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        cached = self.root / "cache/pending"
        cached.write_bytes(b"generated")
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime", side_effect=update.UpdateError("restart failed")):
            code, output, error = self.invoke()
        self.assertEqual(code, 2)
        self.assertIn("restart failed", error)
        self.assertEqual((self.root / "bin/rclone").read_bytes(), self.binary.read_bytes())
        self.assertEqual(cached.read_bytes(), b"generated")

    def test_busy_mount_does_not_launch_duplicate_manual_mount(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        with mock.patch.object(update, "stop_runtime", side_effect=update.UpdateError("busy")), mock.patch.object(update, "start_runtime") as start:
            code, output, error = self.invoke()
        self.assertEqual(code, 2)
        start.assert_not_called()


    def test_cache_lock_conflict_does_not_launch_duplicate_mount(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime") as start, mock.patch.object(update.uninstall, "_acquire_cache_lock", side_effect=update.uninstall.CleanupError("cache active")):
            code, output, error = self.invoke()
        self.assertEqual(code, 2)
        start.assert_not_called()

    def test_cleanup_failure_after_commit_does_not_leave_stale_backup(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime"), mock.patch.object(update, "require_clean_cache"), mock.patch.object(update, "delete_clean_cache", side_effect=update.UpdateError("cleanup failed")):
            code, output, error = self.invoke("--delete-cache")
        self.assertEqual(code, 2)
        self.assertIn("cleanup failed", error)
        self.assertFalse((self.root / "tmp/update-backup").exists())
        self.assertEqual((self.root / "bin/rclone").read_bytes(), self.binary.read_bytes())

    def test_orphan_cache_data_is_not_deleted_even_when_pending_count_is_zero(self):
        from unittest import mock
        orphan = self.root / "cache/vfs" / update.uninstall._cache_lock_path(self.root).stem / "generated"
        orphan.parent.mkdir(parents=True, mode=0o700)
        orphan.parent.parent.chmod(0o700)
        orphan.write_bytes(b"possibly pending")
        orphan.chmod(0o600)
        with mock.patch.object(update.uninstall, "_pending_uploads", return_value=(0, 0)):
            with self.assertRaisesRegex(update.UpdateError, "incomplete"):
                update.require_clean_cache(self.root)
        self.assertEqual(orphan.read_bytes(), b"possibly pending")

    def test_unrelated_service_fragment_is_refused(self):
        from unittest import mock
        manifest = update.installer.load_manifest(self.root)
        with mock.patch.object(update.uninstall, "_owned_service_links", return_value=([], {"/generated/rclone-degoo.service"})), mock.patch.object(update.uninstall, "_systemctl_service_state", return_value={"FragmentPath": "/another/service", "LoadState": "loaded", "ActiveState": "active"}):
            with self.assertRaisesRegex(update.UpdateError, "another unit"):
                update.runtime_state(self.root, manifest)

    def test_manual_restart_uses_original_arguments_and_private_log(self):
        from unittest import mock
        import os
        arguments = [str(self.root / "bin/rclone"), "degoo", "mount", str(self.root / "config/vault.json"), str(self.root / "mount"), "--read-only"]
        state = {"service_owned": False, "service_active": False, "manual_arguments": arguments, "manual_directory": str(self.home)}
        with mock.patch.object(update.subprocess, "Popen") as process, mock.patch.object(update.wizard, "_wait_for_mount", return_value="mounted"):
            update.start_runtime(self.root, state)
        self.assertEqual(process.call_args.args[0], arguments)
        self.assertTrue(process.call_args.kwargs["start_new_session"])
        self.assertEqual(process.call_args.kwargs["cwd"], str(self.home))
        self.assertEqual((self.root / "logs/update-mount.log").stat().st_mode & 0o777, 0o600)
        self.assertEqual(process.call_args.kwargs["stdin"], update.subprocess.DEVNULL)
        log = self.root / "logs/update-mount.log"
        log.unlink()
        outside = self.base / "outside"
        outside.write_bytes(b"generated")
        os.symlink(outside, log)
        with mock.patch.object(update.subprocess, "Popen") as process:
            with self.assertRaises(update.installer.InstallError):
                update.start_runtime(self.root, state)
            process.assert_not_called()
        self.assertEqual(outside.read_bytes(), b"generated")

    def test_inactive_service_is_reloaded_without_starting(self):
        from unittest import mock
        import subprocess
        state = {"service_owned": True, "service_active": False, "manual_arguments": None}
        with mock.patch.object(update.service, "_systemctl", return_value=subprocess.CompletedProcess([], 0)) as systemctl, mock.patch.object(update.wizard, "_wait_for_mount") as wait:
            update.start_runtime(self.root, state)
        systemctl.assert_called_once_with(self.root, "daemon-reload")
        wait.assert_not_called()


class ActionsSourceTest(unittest.TestCase):
    def test_filtered_workflow_lookup_and_pinned_download(self):
        from unittest import mock
        run = {"id": 42, "head_branch": "staging", "head_sha": "a" * 40, "status": "completed", "conclusion": "success"}
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary)
            (target / update.ARCHIVE_NAME).write_bytes(b"generated")
            with mock.patch.object(update.ActionsSource, "_run", side_effect=[json.dumps({"workflow_runs": [run]}).encode(), b""]) as gh:
                archive, source = update.ActionsSource().download(target)
            self.assertEqual(archive, target / update.ARCHIVE_NAME)
            self.assertEqual(source["run_id"], 42)
            self.assertIn("branch=staging&status=success&per_page=1", gh.call_args_list[0].args[0][1])
            self.assertEqual(gh.call_args_list[1].args[0][:3], ["run", "download", "42"])

    def test_absent_or_invalid_run_fails_before_download(self):
        from unittest import mock
        for response in [{"workflow_runs": []}, {"workflow_runs": [{"id": "invalid"}]}]:
            with mock.patch.object(update.ActionsSource, "_run", return_value=json.dumps(response).encode()) as gh:
                with self.assertRaises(update.UpdateError):
                    update.ActionsSource().download(Path("/unused"))
                gh.assert_called_once()


if __name__ == "__main__":
    unittest.main()
