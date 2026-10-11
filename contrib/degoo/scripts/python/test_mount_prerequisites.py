"""Offline checks for immutable Degoo mount prerequisites."""

from __future__ import annotations

import fcntl
import os
import shlex
import stat
import struct
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import mount_prerequisites as prerequisites


class MountPrerequisitesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.base = Path(self.temporary.name)
        self.root = self.base / "install root"
        (self.root / "immutable-mount").mkdir(parents=True)
        (self.root / "immutable-mount/helper.c").write_text("/* test source */\n", encoding="ascii")
        (self.root / "immutable-mount/README.md").write_text("helper instructions\n", encoding="ascii")

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def target(self, name: str = "mount") -> Path:
        path = self.base / name
        path.mkdir()
        path.chmod(0o700)
        return path

    def assert_issue(self, issues: list[str], prefix: str) -> str:
        found = next((issue for issue in issues if issue.startswith(prefix)), None)
        self.assertIsNotNone(found, f"expected {prefix!r} in {issues!r}")
        return found or ""

    def test_missing_mountpoint_has_creation_instructions(self) -> None:
        missing = self.base / "not created"

        with mock.patch.object(prerequisites.shutil, "which") as which:
            issues = prerequisites.check(self.root, missing)

        issue = self.assert_issue(issues, "mountpoint missing:")
        self.assertIn(str(missing), issue)
        which.assert_not_called()

    def test_mountpoint_flag_read_uses_read_only_descriptor_and_keeps_flag(self) -> None:
        target = self.target()
        before = target.stat()

        def report_immutable(fd: int) -> int:
            access = fcntl.fcntl(fd, fcntl.F_GETFL) & os.O_ACCMODE
            self.assertEqual(access, os.O_RDONLY)
            return prerequisites.FS_IMMUTABLE_FL

        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", side_effect=report_immutable),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value="/usr/bin/gcc"),
        ):
            issues = prerequisites.check(self.root, target)

        self.assert_issue(issues, "immutable mount helper invalid:")
        after = target.stat()
        self.assertEqual((after.st_ino, after.st_mode, after.st_uid), (before.st_ino, before.st_mode, before.st_uid))

    def test_getflags_uses_the_linux_read_ioctl_and_returns_flags(self) -> None:
        def ioctl(fd: int, request: int, buffer: bytearray, mutate: bool) -> int:
            self.assertEqual(request, prerequisites.FS_IOC_GETFLAGS)
            self.assertTrue(mutate)
            buffer[:] = struct.pack("I", prerequisites.FS_IMMUTABLE_FL)
            return 0

        with mock.patch.object(prerequisites.fcntl, "ioctl", side_effect=ioctl):
            flags = prerequisites._ioctl_get_flags(17)

        self.assertEqual(flags, prerequisites.FS_IMMUTABLE_FL)
        self.assertEqual(
            prerequisites.FS_IOC_GETFLAGS,
            (2 << 30) | (struct.calcsize("l") << 16) | (ord("f") << 8) | 1,
        )

    def test_unsupported_flag_ioctl_is_treated_as_mutable_filesystem(self) -> None:
        target = self.target()

        for error in (prerequisites.errno.ENOTTY, prerequisites.errno.EOPNOTSUPP):
            with self.subTest(error=error):
                with (
                    mock.patch.object(prerequisites, "_ioctl_get_flags", side_effect=OSError(error, "unsupported")),
                    mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
                    mock.patch.object(prerequisites.shutil, "which") as which,
                ):
                    self.assertEqual(prerequisites.check(self.root, target), [])

            which.assert_not_called()

    def test_unexpected_flag_inspection_error_blocks_mount(self) -> None:
        target = self.target()

        with mock.patch.object(prerequisites, "_ioctl_get_flags", side_effect=OSError(prerequisites.errno.EACCES, "denied")):
            issues = prerequisites.check(self.root, target)

        self.assert_issue(issues, "mountpoint unsafe:")
        self.assertTrue(any("immutable flag" in issue for issue in issues))

    def test_mutable_mountpoint_with_local_files_is_blocked_and_preserved(self) -> None:
        target = self.target()
        local_file = target / "local only.txt"
        local_file.write_text("keep me", encoding="ascii")

        with mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=0):
            issues = prerequisites.check(self.root, target)

        issue = self.assert_issue(issues, "mountpoint unsafe:")
        self.assertIn("not queued for upload", issue)
        self.assertEqual(local_file.read_text(encoding="ascii"), "keep me")

    def test_mutable_mountpoint_must_be_owned_by_current_user(self) -> None:
        target = self.target()
        info = target.stat()
        foreign_owner = SimpleNamespace(st_uid=os.getuid() + 1, st_mode=info.st_mode)
        with (
            mock.patch.object(prerequisites.os, "fstat", return_value=foreign_owner),
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=0),
        ):
            issues = prerequisites.check(self.root, target)

        self.assert_issue(issues, "mountpoint unsafe:")
        self.assertTrue(any("owned by the current user" in issue for issue in issues))

    def test_mutable_mountpoint_without_execute_permission_is_blocked(self) -> None:
        target = self.target()
        target.chmod(0o600)
        try:
            with mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=0):
                issues = prerequisites.check(self.root, target)
            self.assertTrue(any("read and execute permission" in issue for issue in issues))
            self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o600)
        finally:
            target.chmod(0o700)

    def test_missing_helper_reports_source_and_missing_gcc_only_for_immutable_target(self) -> None:
        target = self.target()
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value=None) as which,
        ):
            issues = prerequisites.check(self.root, target)

        self.assert_issue(issues, "immutable mount helper invalid:")
        self.assert_issue(issues, "GCC missing:")
        self.assertFalse(any(issue.startswith("helper source missing:") for issue in issues))
        which.assert_called_once_with("gcc")

        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=0),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites.shutil, "which") as mutable_which,
        ):
            self.assertEqual(prerequisites.check(self.root, target), [])
        mutable_which.assert_not_called()

    def test_missing_helper_source_is_reported_when_immutable_helper_is_invalid(self) -> None:
        target = self.target()
        (self.root / "immutable-mount/helper.c").unlink()
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value="/usr/bin/gcc"),
        ):
            issues = prerequisites.check(self.root, target)
        self.assert_issue(issues, "helper source missing:")
        source_issue = self.assert_issue(issues, "helper source missing:")
        self.assertIn("re-extract the complete archive", source_issue)
        self.assertIn("`./degoo-rc install --upgrade`", source_issue)
        self.assertIn("preserving the configuration and cache", source_issue)

    def test_symlinked_helper_source_is_reported_as_unavailable(self) -> None:
        target = self.target()
        source = self.root / "immutable-mount/helper.c"
        source.unlink()
        source.symlink_to(self.base / "outside-helper.c")
        (self.base / "outside-helper.c").write_text("/* external */\n", encoding="ascii")
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value="/usr/bin/gcc"),
        ):
            issues = prerequisites.check(self.root, target)

        source_issue = self.assert_issue(issues, "helper source missing:")
        self.assertIn("unavailable or unreadable", source_issue)

    def test_valid_helper_does_not_require_compiler_or_packaged_source(self) -> None:
        target = self.target()
        (self.root / "immutable-mount/helper.c").unlink()
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "_helper_problem", return_value=None),
            mock.patch.object(prerequisites.shutil, "which") as which,
        ):
            self.assertEqual(prerequisites.check(self.root, target), [])
        which.assert_not_called()

    def test_immutable_legacy_path_has_cache_preserving_migration_instructions(self) -> None:
        target = self.target()
        expected = Path("/mnt/degoo")
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", expected),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value="/usr/bin/gcc"),
        ):
            issues = prerequisites.check(self.root, target)

        issue = self.assert_issue(issues, "immutable mountpoint unsupported:")
        self.assertIn("degoo-rc install --mountpoint /mnt/degoo", issue)
        self.assertIn("preserv", issue.lower())

    def test_immutable_target_must_be_private_owned_and_empty(self) -> None:
        target = self.target()
        (target / "ordinary local file").write_text("keep", encoding="ascii")
        target.chmod(0o770)
        with (
            mock.patch.object(prerequisites, "_ioctl_get_flags", return_value=prerequisites.FS_IMMUTABLE_FL),
            mock.patch.object(prerequisites, "IMMUTABLE_MOUNTPOINT", target),
            mock.patch.object(prerequisites, "IMMUTABLE_HELPER", self.base / "absent-helper"),
            mock.patch.object(prerequisites.shutil, "which", return_value="/usr/bin/gcc"),
        ):
            issues = prerequisites.check(self.root, target)

        self.assert_issue(issues, "immutable mountpoint unsafe:")
        self.assertTrue(any("group/world write" in item for item in issues))
        self.assertTrue(any("empty" in item for item in issues))

    def test_helper_must_be_root_owned(self) -> None:
        helper = self.base / "helper"
        helper.write_text("binary", encoding="ascii")
        helper.chmod(0o4755)
        real_lstat = prerequisites._lstat

        def fake_lstat(path: Path):
            info = real_lstat(path)
            if path == helper:
                return SimpleNamespace(st_uid=os.getuid() + 1, st_mode=info.st_mode)
            return SimpleNamespace(st_uid=0, st_mode=(stat.S_IFDIR | 0o755) if path != Path("/") else info.st_mode)

        with mock.patch.object(prerequisites, "_lstat", side_effect=fake_lstat):
            reason = prerequisites._helper_problem(helper)

        self.assertIsNotNone(reason)
        self.assertIn("root-owned", reason or "")

    def test_helper_symlinks_are_rejected(self) -> None:
        helper = self.base / "helper"
        helper.write_text("binary", encoding="ascii")
        helper.chmod(0o4755)
        link = self.base / "helper-link"
        link.symlink_to(helper)

        reason = prerequisites._helper_problem(link)

        self.assertIsNotNone(reason)
        self.assertIn("symlink", reason or "")

    def test_helper_filesystem_must_allow_setuid_and_execution(self) -> None:
        helper = self.base / "helper"
        helper.write_text("binary", encoding="ascii")
        helper.chmod(0o4755)
        real_lstat = prerequisites._lstat

        def fake_lstat(path: Path):
            info = real_lstat(path)
            mode = info.st_mode if path == helper else stat.S_IFDIR | 0o755
            return SimpleNamespace(st_uid=0, st_mode=mode)

        for option, flag in (("nosuid", prerequisites.ST_NOSUID), ("noexec", prerequisites.ST_NOEXEC)):
            with self.subTest(option=option):
                with (
                    mock.patch.object(prerequisites, "_lstat", side_effect=fake_lstat),
                    mock.patch.object(prerequisites.os, "statvfs", return_value=SimpleNamespace(f_flag=flag)),
                ):
                    reason = prerequisites._helper_problem(helper)
                self.assertIsNotNone(reason)
                self.assertIn(option, reason or "")

    def test_setup_commands_separate_sudo_setup_from_normal_user_start(self) -> None:
        root = self.base / "installer's files"
        launcher = shlex.quote(str(root / "degoo-rc"))
        self.assertEqual(prerequisites.setup_commands(root), [
            f"sudo {launcher} mount-setup", f"{launcher} service enable", f"{launcher} info",
        ])
        self.assertEqual(prerequisites.setup_commands(root, start_service=False), [
            f"sudo {launcher} mount-setup --foreground", f"{launcher} mount", f"{launcher} info",
        ])


if __name__ == "__main__":
    unittest.main()
