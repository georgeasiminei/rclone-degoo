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


class UpdatePreparationProtocolTest(unittest.TestCase):
    def _connection(self, status, body):
        from unittest import mock
        response = mock.Mock()
        response.status = status
        response.read.return_value = body
        connection = mock.Mock()
        connection.getresponse.return_value = response
        return connection

    def test_prepare_sends_bounded_timeouts_and_validates_token(self):
        from unittest import mock
        socket_path = Path("/private/update.sock")
        token = "a" * 64
        connection = self._connection(200, json.dumps({"token": token, "prepared": True}).encode())
        with mock.patch.object(update.mount_sync, "_socket_path", return_value=socket_path), \
                mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection) as client:
            result = update.prepare_mount_for_update(Path("/private"), writer_timeout=12.5, upload_timeout=60.0)
        self.assertEqual(result, (socket_path, token))
        self.assertEqual(client.call_args.kwargs["timeout"], 82.5)
        self.assertEqual(connection.request.call_args.args[:2], ("POST", "/update/prepare"))
        self.assertEqual(json.loads(connection.request.call_args.kwargs["body"]),
                         {"writer_timeout": 12.5, "upload_timeout": 60.0})
        connection.close.assert_called_once_with()

    def test_prepare_fails_closed_for_timeout_old_mount_and_flush_error(self):
        from unittest import mock
        cases = [
            (409, b'{"error":"busy"}', "open file or directory handles"),
            (404, b'{"error":"unsupported"}', "does not support safe update preparation"),
            (500, b'{"error":"flush failed"}', "could not flush or verify"),
        ]
        for status, body, message in cases:
            with self.subTest(status=status):
                connection = self._connection(status, body)
                with mock.patch.object(update.mount_sync, "_socket_path", return_value=Path("/private/update.sock")), \
                        mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
                    with self.assertRaisesRegex(update.UpdateError, message):
                        update.prepare_mount_for_update(Path("/private"), writer_timeout=30.0, upload_timeout=0.0)

    def test_prepare_distinguishes_active_lease_from_handle_timeout(self):
        from unittest import mock
        connection = self._connection(409, b'{"error":"another update preparation is active"}')
        with mock.patch.object(update.mount_sync, "_socket_path", return_value=Path("/private/update.sock")), \
                mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
            with self.assertRaisesRegex(update.UpdateError, "another update preparation is already active") as raised:
                update.prepare_mount_for_update(Path("/private"), writer_timeout=30.0, upload_timeout=0.0)
        self.assertIn("2-minute lease", str(raised.exception))
        self.assertNotIn("increase", str(raised.exception))

    def test_prepare_transport_failure_explains_possible_lost_lease_without_server_text(self):
        from unittest import mock
        connection = self._connection(200, b"")
        connection.request.side_effect = OSError("private socket details")
        with mock.patch.object(update.mount_sync, "_socket_path", return_value=Path("/private/update.sock")), \
                mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
            with self.assertRaisesRegex(update.UpdateError, "old mount running") as raised:
                update.prepare_mount_for_update(Path("/private"), writer_timeout=30.0, upload_timeout=0.0)
        self.assertIn("up to 2 minutes", str(raised.exception))
        self.assertIn("wait for the preparation lease to expire, then retry", str(raised.exception))
        self.assertNotIn("private socket details", str(raised.exception))
        connection.close.assert_called_once_with()

    def test_prepare_rejects_malformed_success_without_exposing_response(self):
        from unittest import mock
        connection = self._connection(200, json.dumps({"token": "private-value", "prepared": True}).encode())
        with mock.patch.object(update.mount_sync, "_socket_path", return_value=Path("/private/update.sock")), \
                mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
            with self.assertRaisesRegex(update.UpdateError, "preparation response could not be verified") as raised:
                update.prepare_mount_for_update(Path("/private"), writer_timeout=30.0, upload_timeout=0.0)
        self.assertNotIn("private-value", str(raised.exception))

    def test_renew_and_cancel_require_their_explicit_success_replies(self):
        from unittest import mock
        socket_path = Path("/private/update.sock")
        token = "b" * 64
        for route, body, function in (
            ("/update/renew", b'{"renewed":true}', update.renew_mount_preparation),
            ("/update/cancel", b'{"cancelled":true}', update.cancel_mount_preparation),
        ):
            with self.subTest(route=route):
                connection = self._connection(200, body)
                with mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
                    function(socket_path, token)
                self.assertEqual(connection.request.call_args.args[:2], ("POST", route))
                self.assertEqual(json.loads(connection.request.call_args.kwargs["body"]), {"token": token})
        connection = self._connection(409, b'{"error":"expired"}')
        with mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection):
            with self.assertRaisesRegex(update.UpdateError, "could not be cancelled"):
                update.cancel_mount_preparation(socket_path, token)

    def test_update_timeout_arguments_are_bounded_rclone_durations(self):
        self.assertEqual(update.parse_writer_timeout("30s"), 30.0)
        self.assertEqual(update.parse_writer_timeout("1h"), 3600.0)
        self.assertEqual(update.parse_upload_timeout("off"), 0.0)
        self.assertEqual(update.parse_upload_timeout("1d"), 86400.0)
        for value in ("0s", "3601s", "junk"):
            with self.subTest(writer=value), self.assertRaises(update.argparse.ArgumentTypeError):
                update.parse_writer_timeout(value)
        for value in ("86401s", "-1s", "junk"):
            with self.subTest(upload=value), self.assertRaises(update.argparse.ArgumentTypeError):
                update.parse_upload_timeout(value)
        args = update.build_parser().parse_args(["--root", "/private/root", "--writer-timeout", "15s",
                                                 "--wait-for-uploads", "off"])
        self.assertEqual(args.writer_timeout, 15.0)
        self.assertEqual(args.wait_for_uploads, 0.0)


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

    def configure_external_cache(self, cache):
        cache.mkdir(mode=0o700)
        cache.chmod(0o700)
        vault_path = self.root / "config/vault.json"
        vault = json.loads(vault_path.read_bytes())
        vault["cache_dir"] = str(cache)
        vault_path.write_text(json.dumps(vault, sort_keys=True) + "\n", encoding="utf-8")
        vault_path.chmod(0o600)

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

    def test_unsafe_runtime_bytecode_is_actionable_before_runtime_stop(self):
        from unittest import mock
        bytecode_dir = self.root / "scripts/python/__pycache__"
        bytecode_dir.mkdir(mode=0o775)
        bytecode_dir.chmod(0o775)
        bytecode = bytecode_dir / "update.cpython-314.pyc"
        bytecode.write_bytes(b"generated bytecode")
        bytecode.chmod(0o644)
        state = {"service_active": False, "service_owned": False, "manual_arguments": None,
                 "manual_directory": None, "mounted": False}
        with mock.patch.object(update, "installed_matches", return_value=False), \
                mock.patch.object(update, "runtime_state", return_value=state) as read_state, \
                mock.patch.object(update, "_wait_for_service_mount", return_value=state), \
                mock.patch.object(update, "stop_runtime") as stop, \
                mock.patch.object(update, "start_runtime") as start, \
                mock.patch.object(update, "prepare_mount_for_update") as prepare, \
                mock.patch.object(update.uninstall, "_acquire_cache_lock", return_value=None) as cache_lock:
            with self.assertRaisesRegex(update.UpdateError, "scripts/python/__pycache__") as raised:
                update.apply_update(self.root, self.base / "unused-package", {"binary_sha256": "a" * 64}, {},
                                    "b" * 64, delete_cache=False)
        self.assertIn("remove that generated Python bytecode cache", str(raised.exception))
        self.assertNotIn(str(self.root), str(raised.exception))
        stop.assert_not_called()
        prepare.assert_not_called()
        cache_lock.assert_not_called()
        start.assert_not_called()
        read_state.assert_not_called()
        self.assertTrue(bytecode.is_file())
        self.assertFalse((self.root / "tmp/update-backup").exists())

    def test_writable_managed_source_is_rejected_before_runtime_stop(self):
        from unittest import mock
        source = self.root / "scripts/python/cli.py"
        original_mode = source.stat().st_mode & 0o777
        source.chmod(0o664)
        self.addCleanup(source.chmod, original_mode)
        state = {"service_active": False, "service_owned": False, "manual_arguments": None,
                 "manual_directory": None, "mounted": False}
        with mock.patch.object(update, "installed_matches", return_value=False), \
                mock.patch.object(update, "runtime_state", return_value=state) as read_state, \
                mock.patch.object(update, "_wait_for_service_mount", return_value=state), \
                mock.patch.object(update, "stop_runtime") as stop, \
                mock.patch.object(update, "start_runtime") as start:
            with self.assertRaisesRegex(update.UpdateError, "scripts/python/cli.py") as raised:
                update.apply_update(self.root, self.base / "unused-package", {"binary_sha256": "a" * 64}, {},
                                    "b" * 64, delete_cache=False)
        self.assertIn("restore that program path from a verified package", str(raised.exception))
        self.assertNotIn(str(self.root), str(raised.exception))
        stop.assert_not_called()
        read_state.assert_not_called()
        start.assert_not_called()

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

    def test_mounted_cwd_conflict_returns_cd_advice_before_preparation(self):
        import os
        from unittest import mock
        self.write_binary("new")
        self.package()
        mountpoint = self.root / "mount"
        original = os.getcwd()
        state = {"service_active": False, "service_owned": False, "manual_arguments": ["owned"],
                 "manual_directory": str(self.home), "mounted": True}
        try:
            os.chdir(mountpoint)
            with mock.patch.object(update, "runtime_state", return_value=state), \
                    mock.patch.object(update.wizard, "_mounted_state", return_value="mounted"), \
                    mock.patch.object(update, "prepare_mount_for_update") as prepare, \
                    mock.patch.object(update, "stop_runtime") as stop:
                code, output, error = self.invoke()
        finally:
            os.chdir(original)
        self.assertEqual(code, 2)
        self.assertIn("caller shell or updater is inside", error)
        self.assertIn("cd '", error, "spaces in the safe parent directory must be shell quoted")
        self.assertIn("degoo-rc update", error)
        prepare.assert_not_called()
        stop.assert_not_called()
        self.assertNotIn("Unmounting", output)
        self.assertFalse((self.root / "tmp/update-backup").exists())

    def test_identical_package_is_allowed_from_mount_cwd(self):
        import os
        from unittest import mock
        self.package()
        original = os.getcwd()
        try:
            os.chdir(self.root / "mount")
            with mock.patch.object(update, "preflight_mount_directory") as guard:
                code, output, error = self.invoke("--delete-cache")
        finally:
            os.chdir(original)
        self.assertEqual(code, 0, error)
        self.assertIn("Already up to date", output)
        guard.assert_not_called()

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
        cache = self.base / "external-cache"
        self.configure_external_cache(cache)
        decoy = self.root / "cache/legacy-decoy"
        decoy.write_bytes(b"must not be inspected or deleted")
        decoy.chmod(0o600)
        cache_name = update.uninstall._cache_lock_path(self.root).stem
        data_root = cache / "vfs" / cache_name
        meta_root = cache / "vfsMeta" / cache_name
        data_root.mkdir(parents=True, mode=0o700)
        meta_root.mkdir(parents=True, mode=0o700)
        (cache / "vfs").chmod(0o700)
        (cache / "vfsMeta").chmod(0o700)
        cached = data_root / "generated-clean-file"
        cached.write_bytes(b"clean")
        cached.chmod(0o600)
        metadata = meta_root / cached.name
        metadata.write_text(json.dumps({"Dirty": False, "Size": 5, "Fingerprint": "generated fingerprint"}))
        metadata.chmod(0o600)
        identity = cache / f"{cache_name}.identity"
        identity.write_text("a" * 64 + "\n")
        identity.chmod(0o600)
        metadata_dir = cache / "metadata" / cache_name
        metadata_dir.mkdir(parents=True, mode=0o700)
        metadata_dir.chmod(0o700)
        (cache / "metadata").chmod(0o700)
        metadata_entry = metadata_dir / ("b" * 64 + ".cache")
        metadata_entry.write_bytes(b"generated encrypted metadata cache")
        metadata_entry.chmod(0o600)
        metadata_lock = metadata_dir / ".lock"
        metadata_lock.write_bytes(b"")
        metadata_lock.chmod(0o600)
        lock = update.uninstall._cache_lock_path(self.root)
        lock.write_bytes(b"")
        lock.chmod(0o600)
        inode = lock.stat().st_ino
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime"), mock.patch.object(update.uninstall, "_pending_uploads", return_value=(0, 0)):
            code, output, error = self.invoke("--delete-cache")
        self.assertEqual(code, 0, output + error)
        self.assertFalse(cached.exists())
        self.assertFalse(metadata_entry.exists())
        self.assertFalse(metadata_lock.exists())
        self.assertEqual(lock.stat().st_ino, inode)
        self.assertEqual(decoy.read_bytes(), b"must not be inspected or deleted")

    def test_external_cache_delete_refuses_dirty_queue_without_touching_legacy_decoy(self):
        from unittest import mock
        self.write_binary("new")
        self.package()
        cache = self.base / "external-cache"
        self.configure_external_cache(cache)
        cache_name = update.uninstall._cache_lock_path(self.root).stem
        namespace = cache / "vfs" / cache_name
        namespace.mkdir(parents=True, mode=0o700)
        namespace.chmod(0o700)
        (cache / "vfs").chmod(0o700)
        cached = namespace / "pending"
        cached.write_bytes(b"generated pending data")
        cached.chmod(0o600)
        identity = cache / f"{cache_name}.identity"
        identity.write_text("a" * 64 + "\n", encoding="ascii")
        identity.chmod(0o600)
        decoy = self.root / "cache/legacy-decoy"
        decoy.write_bytes(b"must not be inspected or deleted")
        decoy.chmod(0o600)
        with mock.patch.object(update, "stop_runtime"), mock.patch.object(update, "start_runtime"), \
                mock.patch.object(update.uninstall, "_pending_uploads", return_value=(1, 23)):
            code, output, error = self.invoke("--delete-cache")
        self.assertEqual(code, 2, output + error)
        self.assertEqual(cached.read_bytes(), b"generated pending data")
        self.assertEqual(decoy.read_bytes(), b"must not be inspected or deleted")

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


    def test_preparation_timeout_keeps_runtime_and_program_untouched(self):
        from unittest import mock
        state = {"service_active": False, "service_owned": False, "manual_arguments": ["owned"],
                 "manual_directory": str(self.home), "mounted": True}
        identity = {"payload_sha256": "f" * 64, "binary_sha256": "e" * 64}
        with mock.patch.object(update, "runtime_state", return_value=state), \
                mock.patch.object(update.wizard, "_mount_preflight", return_value=[]), \
                mock.patch.object(update, "prepare_mount_for_update", side_effect=update.UpdateError("busy deadline")) as prepare, \
                mock.patch.object(update, "stop_runtime") as stop, \
                mock.patch.object(update, "start_runtime") as start:
            with self.assertRaisesRegex(update.UpdateError, "busy deadline"):
                update.apply_update(self.root, self.base / "unused-package", identity, {}, "a" * 64,
                                    delete_cache=False, writer_timeout=45.0, upload_timeout=600.0)
        prepare.assert_called_once_with(self.root, writer_timeout=45.0, upload_timeout=600.0)
        stop.assert_not_called()
        start.assert_not_called()
        self.assertEqual((self.root / "bin/rclone").read_bytes(), self.binary.read_bytes())

    def test_prepare_endpoint_404_prevents_service_stop(self):
        from unittest import mock
        state = {"service_active": True, "service_owned": True, "manual_arguments": None,
                 "manual_directory": None, "mounted": True}
        identity = {"payload_sha256": "f" * 64, "binary_sha256": "e" * 64}
        connection = mock.Mock()
        response = mock.Mock(status=404)
        response.read.return_value = b'{"error":"unsupported"}'
        connection.getresponse.return_value = response
        with mock.patch.object(update, "runtime_state", return_value=state), \
                mock.patch.object(update.wizard, "_mount_preflight", return_value=[]), \
                mock.patch.object(update.mount_sync, "_socket_path", return_value=self.root / "tmp/vfs-sync.sock"), \
                mock.patch.object(update.mount_sync, "UnixHTTPConnection", return_value=connection), \
                mock.patch.object(update, "stop_runtime") as stop, \
                mock.patch.object(update.uninstall, "_stop_owned_service") as service_stop:
            with self.assertRaisesRegex(update.UpdateError, "does not support safe update preparation"):
                update.apply_update(self.root, self.base / "unused-package", identity, {}, "a" * 64,
                                    delete_cache=False)
        stop.assert_not_called()
        service_stop.assert_not_called()

    def test_failed_unmount_releases_barrier_and_cancel_failure_is_an_error(self):
        from unittest import mock
        state = {"service_active": False, "service_owned": False, "manual_arguments": ["owned"],
                 "manual_directory": str(self.home), "mounted": True}
        identity = {"payload_sha256": "f" * 64, "binary_sha256": "e" * 64}
        token = "c" * 64
        with mock.patch.object(update, "runtime_state", return_value=state), \
                mock.patch.object(update.wizard, "_mount_preflight", return_value=[]), \
                mock.patch.object(update, "preflight_mount_directory"), \
                mock.patch.object(update, "prepare_mount_for_update", return_value=(Path("/private/sock"), token)), \
                mock.patch.object(update, "renew_mount_preparation") as renew, \
                mock.patch.object(update, "stop_runtime", side_effect=update.UpdateError("busy unmount")), \
                mock.patch.object(update.wizard, "_mounted_state", return_value="mounted"), \
                mock.patch.object(update, "cancel_mount_preparation", side_effect=update.UpdateError("cancel failed")) as cancel, \
                mock.patch.object(update, "start_runtime") as start:
            with self.assertRaisesRegex(update.UpdateError, "busy unmount; cancel failed"):
                update.apply_update(self.root, self.base / "unused-package", identity, {}, "a" * 64,
                                    delete_cache=False)
        renew.assert_called_once_with(Path("/private/sock"), token)
        cancel.assert_called_once_with(Path("/private/sock"), token)
        start.assert_not_called()

    def test_normal_unmount_happens_before_owned_service_stop(self):
        from unittest import mock
        import uninstall
        import wizard
        manifest = update.installer.load_manifest(self.root)
        events = []
        state = {"service_active": True, "service_owned": True, "mounted": True}
        with mock.patch.object(wizard, "_mounted_state", side_effect=["mounted", "not mounted", "not mounted"]), \
                mock.patch.object(wizard, "_stop_owned_mount", side_effect=lambda _root: events.append("unmount")), \
                mock.patch.object(uninstall, "_stop_owned_service", side_effect=lambda *_a, **_k: events.append("systemd-stop")):
            update.stop_runtime(self.root, manifest, state)
        self.assertEqual(events, ["unmount", "systemd-stop"])

    def test_busy_normal_unmount_never_stops_systemd_service(self):
        from unittest import mock
        import uninstall
        import wizard
        manifest = update.installer.load_manifest(self.root)
        state = {"service_active": True, "service_owned": True, "mounted": True}
        with mock.patch.object(wizard, "_mounted_state", side_effect=["mounted", "mounted"]), \
                mock.patch.object(wizard, "_stop_owned_mount"), \
                mock.patch.object(uninstall, "_stop_owned_service") as service_stop:
            with self.assertRaisesRegex(update.UpdateError, "normal FUSE unmount did not complete"):
                update.stop_runtime(self.root, manifest, state)
        service_stop.assert_not_called()

    def test_active_service_mount_start_race_must_be_confirmed(self):
        from unittest import mock
        manifest = update.installer.load_manifest(self.root)
        state = {"service_active": True, "mounted": False}
        with mock.patch.object(update.wizard, "_wait_for_mount", return_value="not mounted"), \
                mock.patch.object(update, "runtime_state") as recheck:
            with self.assertRaisesRegex(update.UpdateError, "active but its mount was not confirmed"):
                update._wait_for_service_mount(self.root, manifest, state)
        recheck.assert_not_called()


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
    def setUp(self):
        self.run = {"id": 42, "head_branch": "staging", "head_sha": "a" * 40,
                    "status": "completed", "conclusion": "success"}
        self.manifest = {"version": 1, "archive": update.ARCHIVE_NAME, "archive_sha256": "b" * 64,
                         "archive_size": 123, "binary_sha256": "c" * 64, "payload_sha256": "d" * 64,
                         "commit": "a" * 40, "source_dirty": False}

    def gh_response(self, arguments, **kwargs):
        if arguments[0] == "api":
            if "/workflows/" in arguments[1]:
                return json.dumps({"workflow_runs": [self.run]}).encode()
            return json.dumps({"artifacts": [{"name": update.UPDATE_ARTIFACT_NAME, "expired": False}]}).encode()
        target = Path(arguments[-1])
        if update.UPDATE_ARTIFACT_NAME in arguments:
            (target / update.UPDATE_MANIFEST_NAME).write_text(json.dumps(self.manifest))
            (target / (update.ARCHIVE_NAME + ".sha256")).write_text(f"{'b' * 64}  {update.ARCHIVE_NAME}\n")
        else:
            (target / update.ARCHIVE_NAME).write_bytes(b"generated")
        return b""

    def test_matching_manifest_skips_large_archive_and_runtime_preflight(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as temporary, \
                mock.patch.object(update.ActionsSource, "_run", side_effect=self.gh_response) as gh, \
                mock.patch.object(update, "installed_matches", return_value=True) as compare, \
                mock.patch.object(update, "preflight_mount_directory") as preflight:
            archive, source = update.ActionsSource().download(Path(temporary), installed_root=Path("/installed"))
        self.assertIsNone(archive)
        self.assertEqual(source["run_id"], 42)
        compare.assert_called_once_with(Path("/installed"), self.manifest)
        self.assertEqual(gh.call_count, 3)
        self.assertIn(update.UPDATE_ARTIFACT_NAME, gh.call_args_list[-1].args[0])
        preflight.assert_not_called()

    def test_changed_manifest_checks_cwd_before_downloading_archive(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as temporary, \
                mock.patch.object(update.ActionsSource, "_run", side_effect=self.gh_response) as gh, \
                mock.patch.object(update, "installed_matches", return_value=False), \
                mock.patch.object(update.installer, "load_manifest", return_value={}), \
                mock.patch.object(update, "preflight_mount_directory", side_effect=update.UpdateError("cd /mnt")):
            with self.assertRaisesRegex(update.UpdateError, "cd /mnt"):
                update.ActionsSource().download(Path(temporary), installed_root=Path("/installed"))
        self.assertEqual(gh.call_count, 3, "archive download must not happen before cwd conflict is resolved")

    def test_invalid_manifest_never_falls_back_to_large_archive(self):
        from unittest import mock
        for key, value in [("commit", "f" * 40), ("source_dirty", True), ("archive", "other.tar.gz"),
                           ("payload_sha256", "invalid"), ("archive_size", True), ("version", True),
                           ("archive_sha256", "e" * 64), ("extra", "field")]:
            with self.subTest(key=key), tempfile.TemporaryDirectory() as temporary:
                saved = dict(self.manifest)
                self.manifest[key] = value
                with mock.patch.object(update.ActionsSource, "_run", side_effect=self.gh_response) as gh:
                    with self.assertRaisesRegex(update.UpdateError, "manifest or checksum is invalid"):
                        update.ActionsSource().download(Path(temporary))
                self.assertEqual(gh.call_count, 3)
                self.manifest = saved

    def test_changed_manifest_downloads_full_artifact_and_binds_payload(self):
        from unittest import mock
        with tempfile.TemporaryDirectory() as temporary, \
                mock.patch.object(update.ActionsSource, "_run", side_effect=self.gh_response) as gh, \
                mock.patch.object(update, "installed_matches", return_value=False), \
                mock.patch.object(update.installer, "load_manifest", return_value={}), \
                mock.patch.object(update, "preflight_mount_directory"):
            source = update.ActionsSource()
            archive, provenance = source.download(Path(temporary), installed_root=Path("/installed"))
            self.assertEqual(gh.call_count, 4)
            self.assertIn(update.ARTIFACT_NAME, gh.call_args.args[0])
            self.assertEqual(provenance["commit"], self.run["head_sha"])
            source.manifest["archive_size"] = archive.stat().st_size
            source.verify_package(self.manifest, self.manifest["archive_sha256"], archive)
            for key in ("binary_sha256", "payload_sha256", "commit"):
                changed = dict(self.manifest, **{key: "f" * 64})
                with self.subTest(key=key), self.assertRaisesRegex(update.UpdateError, "does not match"):
                    source.verify_package(changed, self.manifest["archive_sha256"], archive)
            with self.assertRaisesRegex(update.UpdateError, "does not match"):
                source.verify_package(self.manifest, "f" * 64, archive)

    def test_filtered_workflow_lookup_and_pinned_download(self):
        from unittest import mock
        run = {"id": 42, "head_branch": "staging", "head_sha": "a" * 40, "status": "completed", "conclusion": "success"}
        with tempfile.TemporaryDirectory() as temporary:
            target = Path(temporary)
            (target / update.ARCHIVE_NAME).write_bytes(b"generated")
            with mock.patch.object(update.ActionsSource, "_run", side_effect=[json.dumps({"workflow_runs": [run]}).encode(), b'{"artifacts":[]}', b""]) as gh:
                archive, source = update.ActionsSource().download(target)
            self.assertEqual(archive, target / update.ARCHIVE_NAME)
            self.assertEqual(source["run_id"], 42)
            self.assertIn("branch=staging&status=success&per_page=1", gh.call_args_list[0].args[0][1])
            self.assertEqual(gh.call_args_list[2].args[0][:3], ["run", "download", "42"])

    def test_absent_or_invalid_run_fails_before_download(self):
        from unittest import mock
        for response in [{"workflow_runs": []}, {"workflow_runs": [{"id": "invalid"}]}]:
            with mock.patch.object(update.ActionsSource, "_run", return_value=json.dumps(response).encode()) as gh:
                with self.assertRaises(update.UpdateError):
                    update.ActionsSource().download(Path("/unused"))
                gh.assert_called_once()


class ProgressTest(unittest.TestCase):
    def test_slow_phase_reports_elapsed_time_and_stops_reporting_after_exit(self):
        import threading
        from unittest import mock
        emitted = threading.Event()
        lines = []

        def record(message, **kwargs):
            lines.append(message)
            emitted.set()

        with mock.patch("builtins.print", side_effect=record):
            with update.progress("Downloading archive", interval=0.005):
                self.assertTrue(emitted.wait(1))
            count = len(lines)
            self.assertTrue(all("Downloading archive: still working" in line for line in lines))
            self.assertTrue(all("elapsed" in line for line in lines))
            self.assertEqual(len(lines), count)


if __name__ == "__main__":
    unittest.main()
