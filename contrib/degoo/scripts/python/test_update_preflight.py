"""Offline checks for working directories that can keep a FUSE mount busy."""

import os
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import update_preflight


class UpdatePreflightTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="degoo preflight 'case ")
        self.addCleanup(self.temporary.cleanup)
        self.workspace = Path(self.temporary.name)
        self.mount = self.workspace / "degoo mount"
        (self.mount / "sub").mkdir(parents=True)
        self.outside = self.workspace / "outside"
        self.outside.mkdir()
        self.proc = self.workspace / "proc"
        self.proc.mkdir()
        self.pid = 1200

    def process(self, pid, parent_pid, cwd):
        entry = self.proc / str(pid)
        entry.mkdir()
        (entry / "cwd").symlink_to(cwd, target_is_directory=True)
        # The command includes spaces and parentheses, as allowed in proc stat.
        (entry / "stat").write_text(
            f"{pid} (generated process (test)) S {parent_pid} 0 0\n", encoding="ascii"
        )

    def check(self, cwd, mountpoint=None, *, parent_cwd=None):
        for entry in self.proc.iterdir():
            if entry.is_dir():
                shutil.rmtree(entry)
        self.process(self.pid, self.pid + 1 if parent_cwd is not None else 1, cwd)
        if parent_cwd is not None:
            self.process(self.pid + 1, 1, parent_cwd)
        with mock.patch.object(update_preflight, "PROC_ROOT", self.proc), \
                mock.patch.object(update_preflight.os, "getpid", return_value=self.pid):
            update_preflight.require_outside_mount(mountpoint or self.mount)

    def assert_inside_error(self, cwd, mountpoint=None, *, parent_cwd=None):
        with self.assertRaises(update_preflight.PreflightError) as raised:
            self.check(cwd, mountpoint, parent_cwd=parent_cwd)
        message = str(raised.exception)
        self.assertIn("caller shell or updater is inside the Degoo mount", message)
        self.assertIn(f"cd {shlex.quote(str(self.mount.parent))}", message)
        self.assertIn("degoo-rc update", message)

    def test_mountpoint_and_descendant_cwds_are_rejected(self):
        for cwd in (self.mount, self.mount / "sub", self.mount / "sub" / ".."):
            with self.subTest(cwd=cwd):
                self.assert_inside_error(cwd)

    def test_outside_and_sibling_prefix_are_allowed(self):
        sibling = self.workspace / "degoo mount-old"
        sibling.mkdir()
        self.check(self.outside)
        self.check(sibling)

    def test_symlinked_mountpoint_and_cwd_are_normalized(self):
        mount_alias = self.workspace / "mount alias"
        mount_alias.symlink_to(self.mount, target_is_directory=True)
        cwd_alias = self.workspace / "cwd alias"
        cwd_alias.symlink_to(self.mount / "sub", target_is_directory=True)
        self.assert_inside_error(cwd_alias, mount_alias)

    def test_parent_shell_inside_is_detected_after_updater_moves_outside(self):
        self.assert_inside_error(self.outside, parent_cwd=self.mount / "sub")

    @unittest.skipUnless(Path("/proc").is_dir(), "the parent cwd check uses Linux /proc")
    def test_real_parent_process_cwd_is_detected_after_child_changes_directory(self):
        module_dir = Path(update_preflight.__file__).resolve().parent
        child = (
            "import sys\n"
            f"sys.path.insert(0, {str(module_dir)!r})\n"
            "from pathlib import Path\n"
            "import update_preflight\n"
            f"try: update_preflight.require_outside_mount(Path({str(self.mount)!r}))\n"
            "except update_preflight.PreflightError as error:\n"
            " print(error)\n"
            " raise SystemExit(23)\n"
            "raise SystemExit(0)\n"
        )
        parent = (
            "import subprocess, sys\n"
            f"result = subprocess.run([sys.executable, '-c', {child!r}], cwd={str(self.outside)!r})\n"
            "raise SystemExit(result.returncode)\n"
        )
        environment = dict(os.environ, PWD=str(self.mount / "sub"))
        result = subprocess.run([sys.executable, "-c", parent], cwd=self.mount / "sub", env=environment,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                text=True, timeout=10, check=False)
        self.assertEqual(result.returncode, 23, result.stdout + result.stderr)
        self.assertIn("caller shell or updater is inside the Degoo mount", result.stdout)
        self.assertIn(f"cd {shlex.quote(str(self.mount.parent))}", result.stdout)

    def test_deleted_cwd_inside_mount_is_rejected(self):
        deleted_cwd = Path(str(self.mount / "sub") + " (deleted)")
        self.assert_inside_error(deleted_cwd)

    def test_missing_proc_entry_fails_with_actionable_preflight_error(self):
        with mock.patch.object(update_preflight, "PROC_ROOT", self.proc), \
                mock.patch.object(update_preflight.os, "getpid", return_value=self.pid):
            with self.assertRaisesRegex(update_preflight.PreflightError, "working directory from /proc"):
                update_preflight.require_outside_mount(self.mount)

    def test_parent_walk_is_bounded(self):
        for index in range(update_preflight.MAX_PROCESS_ANCESTORS + 8):
            pid = self.pid + index
            parent_pid = pid + 1
            self.process(pid, parent_pid, self.outside)
        with mock.patch.object(update_preflight, "PROC_ROOT", self.proc), \
                mock.patch.object(update_preflight.os, "getpid", return_value=self.pid), \
                mock.patch.object(update_preflight.os, "readlink", wraps=os.readlink) as readlink:
            with self.assertRaisesRegex(update_preflight.PreflightError, "process tree is too deep"):
                update_preflight.require_outside_mount(self.mount)
        self.assertEqual(readlink.call_count, update_preflight.MAX_PROCESS_ANCESTORS)


if __name__ == "__main__":
    unittest.main()
