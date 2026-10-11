"""Offline tests for the sudo-only immutable helper installer."""

from __future__ import annotations

import contextlib
import errno
import io
import os
import shutil
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import mount_setup


class MountSetupTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.base = Path(self.temporary.name)
        self.root = self.base / "install root with 'quote'"
        (self.root / "immutable-mount").mkdir(parents=True)
        (self.root / "immutable-mount/helper.c").write_text("int main(void) { return 0; }\n", encoding="ascii")
        self.identity = mount_setup.SudoIdentity(12345, 12345, "degoo-test", (12345, 23456))

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def test_parser_accepts_root_and_foreground(self) -> None:
        args = mount_setup.build_parser().parse_args(["--root", str(self.root), "--foreground"])
        self.assertEqual(args.root, str(self.root))
        self.assertTrue(args.foreground)

    def test_root_path_must_be_absolute_and_cannot_traverse_parent(self) -> None:
        with self.assertRaises(mount_setup.MountSetupError):
            mount_setup._root_path("relative")
        with self.assertRaises(mount_setup.MountSetupError):
            mount_setup._root_path("/tmp/../etc")

    def test_ordinary_user_is_refused_before_touching_destination(self) -> None:
        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=1000),
            mock.patch.object(mount_setup, "_open_destination") as open_destination,
        ):
            with self.assertRaisesRegex(mount_setup.MountSetupError, "through sudo"):
                mount_setup.run(self.root)
        open_destination.assert_not_called()

    def test_sudo_environment_must_match_local_account(self) -> None:
        passwd = SimpleNamespace(pw_name="degoo-test", pw_uid=12345, pw_gid=12345)
        env = {"SUDO_UID": "12345", "SUDO_GID": "12345", "SUDO_USER": "degoo-test"}
        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=0),
            mock.patch.object(mount_setup.pwd, "getpwuid", return_value=passwd),
            mock.patch.object(mount_setup.pwd, "getpwnam", return_value=passwd),
            mock.patch.object(mount_setup.os, "getgrouplist", return_value=[23456]),
        ):
            identity = mount_setup._sudo_identity(env)
        self.assertEqual(identity, self.identity)

        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=0),
            mock.patch.object(mount_setup.pwd, "getpwuid", return_value=passwd),
            mock.patch.object(mount_setup.pwd, "getpwnam", return_value=passwd),
            mock.patch.object(mount_setup.os, "getgrouplist", return_value=[23456]),
        ):
            with self.assertRaisesRegex(mount_setup.MountSetupError, "do not match"):
                mount_setup._sudo_identity({**env, "SUDO_USER": "someone-else"})

    def test_sudo_ids_outside_linux_id_range_are_rejected(self) -> None:
        for name, value in (("SUDO_UID", "4294967295"), ("SUDO_GID", "4294967296")):
            with self.subTest(name=name):
                with self.assertRaisesRegex(mount_setup.MountSetupError, "Linux ID range"):
                    mount_setup._positive_id(value, name)
        self.assertEqual(mount_setup._positive_id("4294967294", "SUDO_UID"), 4294967294)

    def test_valid_helper_is_idempotent_without_source_or_compiler(self) -> None:
        destination = mock.MagicMock()
        destination.complete = True
        output = io.StringIO()
        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=0),
            mock.patch.object(mount_setup, "_sudo_identity", return_value=self.identity),
            mock.patch.object(mount_setup, "_open_destination", return_value=destination),
            mock.patch.object(mount_setup.mount_prerequisites, "_helper_problem", return_value=None),
            mock.patch.object(mount_setup, "_read_source", side_effect=AssertionError("should not read source")),
            mock.patch.object(mount_setup, "_compiler", side_effect=AssertionError("should not inspect GCC")),
            mock.patch.dict(os.environ, {"HOME": str(self.base / "wrong-home")}),
            contextlib.redirect_stdout(output),
        ):
            result = mount_setup.run(self.root)

        self.assertEqual(result, 0)
        destination.close.assert_called_once_with()
        quoted_launcher = mount_setup.shlex.quote(str(self.root / "degoo-rc"))
        self.assertIn(quoted_launcher, output.getvalue())
        self.assertIn("service enable", output.getvalue())
        self.assertNotIn("wrong-home", output.getvalue())

    def test_valid_helper_foreground_continuation_uses_shell_quoted_launcher(self) -> None:
        output = io.StringIO()
        with contextlib.redirect_stdout(output):
            mount_setup._continuation(self.root, foreground=True)

        launcher = mount_setup.shlex.quote(str(self.root / "degoo-rc"))
        self.assertIn(f"{launcher} mount", output.getvalue())
        self.assertIn(f"{launcher} info", output.getvalue())
        self.assertNotIn("service enable", output.getvalue())

    def test_unsafe_destination_blocks_before_source_or_compiler(self) -> None:
        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=0),
            mock.patch.object(mount_setup, "_sudo_identity", return_value=self.identity),
            mock.patch.object(mount_setup, "_open_destination", side_effect=mount_setup.MountSetupError("unsafe parent")),
            mock.patch.object(mount_setup, "_read_source", side_effect=AssertionError("source should not be read")),
            mock.patch.object(mount_setup, "_compiler", side_effect=AssertionError("GCC should not be inspected")),
        ):
            with self.assertRaisesRegex(mount_setup.MountSetupError, "unsafe parent"):
                mount_setup.run(self.root)

    def test_destination_directories_must_be_root_owned_and_not_group_or_world_writable(self) -> None:
        directory = SimpleNamespace(st_mode=stat.S_IFDIR | 0o755, st_uid=0, st_gid=0)
        mount_setup._check_directory(directory, "/parent", self.identity, final=False)

        unowned = SimpleNamespace(st_mode=stat.S_IFDIR | 0o755, st_uid=1, st_gid=0)
        with self.assertRaisesRegex(mount_setup.MountSetupError, "root-owned"):
            mount_setup._check_directory(unowned, "/parent", self.identity, final=False)

        writable = SimpleNamespace(st_mode=stat.S_IFDIR | 0o777, st_uid=0, st_gid=0)
        with self.assertRaisesRegex(mount_setup.MountSetupError, "writable by group or other"):
            mount_setup._check_directory(writable, "/parent", self.identity, final=False)

        symlink = SimpleNamespace(st_mode=stat.S_IFLNK | 0o777, st_uid=0, st_gid=0)
        with self.assertRaisesRegex(mount_setup.MountSetupError, "not a directory"):
            mount_setup._check_directory(symlink, "/parent", self.identity, final=False)

    def test_helper_filesystem_rejects_nosuid_and_noexec(self) -> None:
        for flag, reason in (
            (mount_setup.mount_prerequisites.ST_NOSUID, "nosuid"),
            (mount_setup.mount_prerequisites.ST_NOEXEC, "noexec"),
        ):
            with self.subTest(flag=flag):
                with mock.patch.object(mount_setup.os, "fstatvfs", return_value=SimpleNamespace(f_flag=flag)):
                    with self.assertRaisesRegex(mount_setup.MountSetupError, reason):
                        mount_setup._check_filesystem(42, "/helper")

    def test_atomic_replacement_replaces_symlink_without_following_target(self) -> None:
        directory = self.base / "libexec"
        directory.mkdir()
        helper = directory / "fusermount3"
        target = self.base / "outside"
        target.write_bytes(b"untouched target")
        helper.symlink_to(target)
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        destination = mount_setup.Destination([directory_fd], [], True)
        with (
            mock.patch.object(mount_setup, "HELPER_PATH", helper),
            mock.patch.object(mount_setup, "HELPER_DIRECTORY", directory),
            mock.patch.object(mount_setup, "_check_filesystem"),
            mock.patch.object(mount_setup.os, "fchown") as fchown,
        ):
            try:
                mount_setup._write_atomic_helper(b"new ELF helper", destination)
                self.assertTrue(stat.S_ISREG(helper.lstat().st_mode))
                self.assertEqual(helper.read_bytes(), b"new ELF helper")
                self.assertEqual(target.read_bytes(), b"untouched target")
                self.assertEqual(stat.S_IMODE(helper.stat().st_mode), 0o4755)
                fchown.assert_called_once_with(mock.ANY, 0, 0)
            finally:
                destination.close()

    def test_atomic_replacement_repairs_unowned_world_writable_regular_helper(self) -> None:
        directory = self.base / "libexec"
        directory.mkdir()
        helper = directory / "fusermount3"
        helper.write_bytes(b"unsafe old helper")
        helper.chmod(0o777)
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        destination = mount_setup.Destination([directory_fd], [], True)
        with (
            mock.patch.object(mount_setup, "HELPER_PATH", helper),
            mock.patch.object(mount_setup, "HELPER_DIRECTORY", directory),
            mock.patch.object(mount_setup, "_check_filesystem"),
            mock.patch.object(mount_setup.os, "fchown") as fchown,
        ):
            try:
                mount_setup._write_atomic_helper(b"replacement helper", destination)
                self.assertEqual(helper.read_bytes(), b"replacement helper")
                self.assertEqual(stat.S_IMODE(helper.stat().st_mode), 0o4755)
                fchown.assert_called_once_with(mock.ANY, 0, 0)
            finally:
                destination.close()

    def test_atomic_helper_write_installs_mode_and_ownership(self) -> None:
        directory = self.base / "libexec"
        directory.mkdir()
        helper = directory / "fusermount3"
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        destination = mount_setup.Destination([directory_fd], [], True)
        with (
            mock.patch.object(mount_setup, "HELPER_PATH", helper),
            mock.patch.object(mount_setup, "HELPER_DIRECTORY", directory),
            mock.patch.object(mount_setup, "_existing_helper", return_value=None),
            mock.patch.object(mount_setup, "_check_filesystem"),
            mock.patch.object(mount_setup.os, "fchown") as fchown,
        ):
            try:
                mount_setup._write_atomic_helper(b"ELF payload", destination)
                self.assertEqual(helper.read_bytes(), b"ELF payload")
                self.assertEqual(stat.S_IMODE(helper.stat().st_mode), 0o4755)
                fchown.assert_called_once_with(mock.ANY, 0, 0)
            finally:
                destination.close()

    def test_atomic_helper_write_failure_preserves_existing_file_and_cleans_temporary(self) -> None:
        directory = self.base / "libexec"
        directory.mkdir()
        helper = directory / "fusermount3"
        helper.write_bytes(b"old helper")
        helper.chmod(0o755)
        directory_fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        destination = mount_setup.Destination([directory_fd], [], True)
        with (
            mock.patch.object(mount_setup, "HELPER_PATH", helper),
            mock.patch.object(mount_setup, "HELPER_DIRECTORY", directory),
            mock.patch.object(mount_setup, "_existing_helper", return_value=None),
            mock.patch.object(mount_setup, "_check_filesystem"),
            mock.patch.object(mount_setup.os, "fchown"),
            mock.patch.object(mount_setup.os, "write", side_effect=OSError(errno.ENOSPC, "full")),
        ):
            try:
                with self.assertRaisesRegex(mount_setup.MountSetupError, "installed atomically"):
                    mount_setup._write_atomic_helper(b"new helper", destination)
                self.assertEqual(helper.read_bytes(), b"old helper")
                self.assertEqual(list(directory.iterdir()), [helper])
            finally:
                destination.close()

    def test_missing_gcc_reports_package_command_and_quoted_sudo_retry(self) -> None:
        destination = mock.MagicMock()
        destination.complete = False
        def open_destination(identity, *, create_missing):
            if create_missing:
                raise AssertionError("destination creation must wait until after successful compilation")
            return destination

        def which(name: str, *, path: str | None = None):
            return "/usr/bin/apt-get" if name == "apt-get" else None

        with (
            mock.patch.object(mount_setup.os, "geteuid", return_value=0),
            mock.patch.object(mount_setup, "_sudo_identity", return_value=self.identity),
            mock.patch.object(mount_setup, "_open_destination", side_effect=open_destination),
            mock.patch.object(mount_setup, "_read_source", return_value=b"source"),
            mock.patch.object(mount_setup, "_compiler", return_value=None),
            mock.patch.object(mount_setup.shutil, "which", side_effect=which),
            mock.patch.object(mount_setup, "_workspace", side_effect=AssertionError("must not create a build workspace")),
        ):
            with self.assertRaises(mount_setup.MountSetupError) as caught:
                mount_setup.run(self.root)

        destination.close.assert_called_once_with()
        self.assertIn("sudo apt-get install build-essential", str(caught.exception))
        self.assertIn("sudo ", str(caught.exception))
        self.assertIn(mount_setup.shlex.quote(str(self.root / "degoo-rc")), str(caught.exception))
        self.assertIn("mount-setup", str(caught.exception))

    def test_source_fifo_is_rejected_without_blocking(self) -> None:
        source = self.root / "immutable-mount/helper.c"
        source.unlink()
        os.mkfifo(source)
        with self.assertRaisesRegex(mount_setup.MountSetupError, "regular file"):
            mount_setup._read_source(self.root)

    def test_compiler_command_has_hardening_and_minimal_environment(self) -> None:
        build = self.base / "build"
        build.mkdir()
        (build / "helper.c").write_text("int main(void) { return 0; }\n", encoding="ascii")
        identity = self.identity
        expected_hook = lambda: None
        result = SimpleNamespace(returncode=0, stderr=b"")
        with (
            mock.patch.object(mount_setup, "_drop_privileges", return_value=expected_hook),
            mock.patch.object(mount_setup.subprocess, "run", return_value=result) as run,
        ):
            self.assertIsNone(mount_setup._compile(str(self.base), Path("/usr/bin/gcc"), identity))

        args, kwargs = run.call_args
        command = args[0]
        for flag in ("-D_FORTIFY_SOURCE=2", "-fstack-protector-strong", "-fPIE", "-pie", "-Wl,-z,relro,-z,now", "-Werror"):
            self.assertIn(flag, command)
        self.assertEqual(kwargs["preexec_fn"], expected_hook)
        self.assertNotIn("HOME", kwargs["env"])
        self.assertEqual(kwargs["env"]["PATH"], mount_setup.SAFE_PATH)
        self.assertEqual(kwargs["stdin"], mount_setup.subprocess.DEVNULL)

    def test_compiler_privilege_hook_failure_is_reported_without_traceback(self) -> None:
        build = self.base / "build"
        build.mkdir()
        with (
            mock.patch.object(mount_setup, "_drop_privileges", return_value=lambda: None),
            mock.patch.object(mount_setup.subprocess, "run", side_effect=mount_setup.subprocess.SubprocessError("hook failed")),
        ):
            with self.assertRaisesRegex(mount_setup.MountSetupError, "sudo user's privileges"):
                mount_setup._compile(str(self.base), Path("/usr/bin/gcc"), self.identity)

    @unittest.skipUnless(shutil.which("gcc", path=mount_setup.SAFE_PATH), "GCC is unavailable")
    def test_actual_gcc_build_runs_as_current_unprivileged_test_user(self) -> None:
        identity = mount_setup.SudoIdentity(os.getuid(), os.getgid(), "test", tuple(set(os.getgroups()) | {os.getgid()}))
        compiler = mount_setup._compiler(identity)
        self.assertIsNotNone(compiler)
        workspace, build_fd = mount_setup._workspace(b"int main(void) { return 0; }\n", identity)
        try:
            with mock.patch.object(mount_setup, "_drop_privileges", return_value=lambda: os.umask(0o022)):
                mount_setup._compile(workspace, compiler, identity)
            binary = mount_setup._read_binary(workspace, build_fd, identity)
            self.assertTrue(binary.startswith(b"\x7fELF"))
        finally:
            os.close(build_fd)
            shutil.rmtree(workspace, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
