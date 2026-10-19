"""Exercise the local Degoo read and write benchmark safely offline."""

from __future__ import annotations

import contextlib
import os
from pathlib import Path
import sys
import tempfile
import unittest
from io import StringIO
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import benchmark


class BenchmarkTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory(prefix="degoo-benchmark-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name) / "install"
        self.mount = self.root / "mount"
        self.root.mkdir(mode=0o700)
        self.mount.mkdir(mode=0o700)
        self.install_patch = mock.patch.object(
            benchmark.local_sync, "_validate_install", return_value=self.mount
        )
        self.install_patch.start()
        self.addCleanup(self.install_patch.stop)

    def args(self, *values: str) -> object:
        return benchmark.build_parser().parse_args(
            ["--root", str(self.root), *values]
        )

    def run_benchmark(self, *values: str) -> int:
        with contextlib.redirect_stdout(StringIO()):
            return benchmark.run(self.args(*values))

    def test_write_defaults_to_delete_only_after_upload_completion(self) -> None:
        def uploaded(path: Path, timeout: float, poll_interval: float, started: float) -> None:
            self.assertEqual(path, self.mount / "benchmark.bin")
            self.assertTrue(path.is_file())
            self.assertEqual(path.stat().st_size, 23)
            self.assertEqual(path.read_bytes(), bytes(23))

        with mock.patch.object(benchmark, "wait_for_upload", side_effect=uploaded) as wait:
            self.run_benchmark("write", "--size", "23B", "--block-size", "8B")

        wait.assert_called_once()
        self.assertFalse((self.mount / "benchmark.bin").exists())

    def test_write_no_delete_keeps_exact_zero_filled_size_with_partial_block(self) -> None:
        path = self.mount / "keep.bin"
        with mock.patch.object(benchmark, "wait_for_upload") as wait:
            self.run_benchmark(
                "write", str(path), "--size", "17B", "--block-size", "8B", "--no-delete"
            )

        wait.assert_called_once()
        self.assertEqual(path.stat().st_size, 17)
        self.assertEqual(path.read_bytes(), bytes(17))

        self.run_benchmark("read", str(path), "--buffered", "--no-delete", "--block-size", "8B")
        self.assertEqual(path.read_bytes(), bytes(17))

    def test_existing_file_and_symlink_are_never_overwritten(self) -> None:
        existing = self.mount / "existing.bin"
        existing.write_bytes(b"preserve this")
        external = Path(self.temporary.name) / "outside.bin"
        external.write_bytes(b"outside data")
        link = self.mount / "link.bin"
        link.symlink_to(external)

        with mock.patch.object(benchmark.subprocess, "run") as run_dd:
            with self.assertRaises(benchmark.BenchmarkError):
                self.run_benchmark("write", str(existing), "--size", "8B")
            with self.assertRaises(benchmark.BenchmarkError):
                self.run_benchmark("write", str(link), "--size", "8B")
            with self.assertRaises(OSError):
                self.run_benchmark("read", str(link), "--buffered")

        run_dd.assert_not_called()
        self.assertEqual(existing.read_bytes(), b"preserve this")
        self.assertTrue(link.is_symlink())
        self.assertEqual(external.read_bytes(), b"outside data")

    def test_paths_outside_mount_and_through_parent_symlinks_are_refused(self) -> None:
        outside = Path(self.temporary.name) / "outside"
        outside.mkdir()
        linked = self.mount / "linked"
        linked.symlink_to(outside, target_is_directory=True)
        direct_target = outside / "direct.bin"
        linked_target = linked / "through-link.bin"

        with mock.patch.object(benchmark.subprocess, "run") as run_dd:
            for target in (direct_target, linked_target):
                with self.subTest(target=target), self.assertRaises(benchmark.local_sync.SyncError):
                    self.run_benchmark("write", str(target), "--size", "8B")

        run_dd.assert_not_called()
        self.assertFalse(direct_target.exists())
        self.assertFalse((outside / "through-link.bin").exists())

    def test_fifo_read_refuses_without_blocking_on_open(self) -> None:
        fifo = self.mount / "pipe"
        os.mkfifo(fifo, 0o600)
        real_open = os.open
        opened_flags: list[int] = []

        def check_nonblocking_open(path: str | os.PathLike[str], flags: int, *args: object) -> int:
            opened_flags.append(flags)
            self.assertTrue(flags & os.O_NONBLOCK, "FIFO reads must use nonblocking open")
            return real_open(path, flags, *args)

        with (
            mock.patch.object(benchmark.os, "open", side_effect=check_nonblocking_open),
            mock.patch.object(benchmark.subprocess, "run") as run_dd,
        ):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "regular file"):
                self.run_benchmark("read", str(fifo), "--buffered")

        self.assertEqual(len(opened_flags), 1)
        run_dd.assert_not_called()

    def test_read_dd_failure_preserves_source(self) -> None:
        path = self.mount / "unread.bin"
        path.write_bytes(b"preserve after failed read")

        with mock.patch.object(
            benchmark.subprocess, "run", return_value=SimpleNamespace(returncode=1)
        ) as run_dd:
            with self.assertRaises(benchmark.BenchmarkError):
                self.run_benchmark("read", str(path), "--buffered")

        run_dd.assert_called_once()
        self.assertEqual(path.read_bytes(), b"preserve after failed read")

    def test_dd_failure_preserves_partial_file_and_skips_upload_wait(self) -> None:
        def fail_after_partial_write(command: list[str], **kwargs: object) -> SimpleNamespace:
            os.write(kwargs["stdout"], b"partial")
            return SimpleNamespace(returncode=1)

        path = self.mount / "failed.bin"
        with (
            mock.patch.object(benchmark.subprocess, "run", side_effect=fail_after_partial_write),
            mock.patch.object(benchmark, "wait_for_upload") as wait,
        ):
            with self.assertRaises(benchmark.BenchmarkError):
                self.run_benchmark("write", str(path), "--size", "16B", "--block-size", "8B")

        wait.assert_not_called()
        self.assertEqual(path.read_bytes(), b"partial")

    def test_upload_failure_preserves_written_file(self) -> None:
        path = self.mount / "pending.bin"
        with mock.patch.object(
            benchmark, "wait_for_upload", side_effect=benchmark.BenchmarkError("upload timed out")
        ):
            with self.assertRaises(benchmark.BenchmarkError):
                self.run_benchmark("write", str(path), "--size", "16B", "--block-size", "8B")

        self.assertEqual(path.stat().st_size, 16)
        self.assertEqual(path.read_bytes(), bytes(16))

    def test_upload_state_unknown_or_unavailable_refuses_and_preserves_file(self) -> None:
        states = (
            ("unknown.bin", b"mystery", "unknown"),
            ("unavailable.bin", OSError("xattr unavailable"), "unavailable"),
        )
        for filename, state, message in states:
            path = self.mount / filename
            with self.subTest(state=state):
                with mock.patch.object(benchmark.os, "getxattr", side_effect=state):
                    with self.assertRaisesRegex(benchmark.BenchmarkError, message):
                        self.run_benchmark(
                            "write", str(path), "--size", "8B", "--block-size", "8B"
                        )
                self.assertEqual(path.read_bytes(), bytes(8))

    def test_file_changed_during_upload_is_preserved_instead_of_deleted(self) -> None:
        path = self.mount / "changed.bin"

        def change_during_upload(
            selected: Path, timeout: float, poll_interval: float, started: float
        ) -> None:
            self.assertEqual(selected, path)
            path.write_bytes(b"replacement data")
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))

        with mock.patch.object(benchmark, "wait_for_upload", side_effect=change_during_upload):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "cleanup refused"):
                self.run_benchmark(
                    "write", str(path), "--size", "16B", "--block-size", "8B"
                )

        self.assertEqual(path.read_bytes(), b"replacement data")

    def test_writeback_timestamp_change_with_unchanged_data_succeeds(self) -> None:
        for keep in (False, True):
            with self.subTest(keep=keep):
                path = self.mount / f"timestamp-{keep}.bin"

                def timestamp_changes(*args: object) -> None:
                    info = path.stat()
                    os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))

                options = ["--no-delete"] if keep else []
                with mock.patch.object(benchmark, "wait_for_upload", side_effect=timestamp_changes):
                    self.run_benchmark(
                        "write", str(path), "--size", "17B", "--block-size", "8B", *options
                    )

                self.assertEqual(path.exists(), keep)
                if keep:
                    self.assertEqual(path.read_bytes(), bytes(17))

    def test_same_size_content_change_during_upload_is_preserved(self) -> None:
        path = self.mount / "same-size.bin"

        def change_during_upload(*args: object) -> None:
            path.write_bytes(b"x" * 16)
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))

        with mock.patch.object(benchmark, "wait_for_upload", side_effect=change_during_upload):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "cleanup refused"):
                self.run_benchmark("write", str(path), "--size", "16B", "--block-size", "8B")

        self.assertEqual(path.read_bytes(), b"x" * 16)

    def test_same_size_zero_replacement_during_upload_is_preserved(self) -> None:
        path = self.mount / "replaced.bin"
        replacement = self.mount / "replacement.bin"

        def replace_during_upload(*args: object) -> None:
            replacement.write_bytes(bytes(16))
            replacement.replace(path)

        with mock.patch.object(benchmark, "wait_for_upload", side_effect=replace_during_upload):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "cleanup refused"):
                self.run_benchmark("write", str(path), "--size", "16B", "--block-size", "8B")

        self.assertEqual(path.read_bytes(), bytes(16))

    def test_read_timestamp_change_still_refuses_cleanup(self) -> None:
        path = self.mount / "read-changed.bin"
        path.write_bytes(bytes(16))
        real_run = benchmark.subprocess.run

        def change_after_read(*args: object, **kwargs: object) -> object:
            result = real_run(*args, **kwargs)
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))
            return result

        with mock.patch.object(benchmark.subprocess, "run", side_effect=change_after_read):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "cleanup refused"):
                self.run_benchmark("read", str(path), "--buffered")

        self.assertEqual(path.read_bytes(), bytes(16))

    def test_writeback_timestamp_check_reads_full_chunks_and_partial_tail(self) -> None:
        path = self.mount / "large-tail.bin"
        size = 1024 * 1024 + 17

        def timestamp_changes(*args: object) -> None:
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))

        with mock.patch.object(benchmark, "wait_for_upload", side_effect=timestamp_changes):
            self.run_benchmark("write", str(path), "--size", str(size), "--block-size", "1MiB", "--no-delete")

        self.assertEqual(path.stat().st_size, size)
        self.assertEqual(path.read_bytes(), bytes(size))

    def test_writeback_content_check_timeout_preserves_file(self) -> None:
        path = self.mount / "timeout.bin"

        def timestamp_changes(*args: object) -> None:
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))

        with (
            mock.patch.object(benchmark, "wait_for_upload", side_effect=timestamp_changes),
            mock.patch.object(benchmark.time, "monotonic", side_effect=[10.0, 10.1, 11.0]),
        ):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "content check timed out"):
                self.run_benchmark("write", str(path), "--size", "16B", "--timeout", "1")

        self.assertEqual(path.read_bytes(), bytes(16))

    def test_writeback_content_check_timeout_on_final_read_preserves_file(self) -> None:
        path = self.mount / "final-read-timeout.bin"
        path.write_bytes(bytes(16))
        identity = path.stat()

        with mock.patch.object(benchmark.time, "monotonic", side_effect=[10.1, 11.0]):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "content check timed out"):
                benchmark.verify_written_data(path, identity, started=10.0, timeout=1.0)

        self.assertEqual(path.read_bytes(), bytes(16))

    def test_writeback_content_check_rejects_nonzero_byte_at_tail(self) -> None:
        path = self.mount / "nonzero-tail.bin"
        size = 1024 * 1024 + 17
        path.write_bytes(bytes(size))
        identity = path.stat()
        with path.open("r+b") as selected:
            selected.seek(-1, os.SEEK_END)
            selected.write(b"x")

        with self.assertRaisesRegex(benchmark.BenchmarkError, "cleanup refused"):
            benchmark.verify_written_data(path, identity, benchmark.time.monotonic(), 5.0)

        self.assertEqual(path.stat().st_size, size)
        self.assertEqual(path.read_bytes()[-1:], b"x")

    def test_writeback_content_check_rejects_eof_during_read(self) -> None:
        path = self.mount / "eof-during-check.bin"
        size = 1024 * 1024 + 17
        path.write_bytes(bytes(size))
        identity = path.stat()
        real_read = os.read
        calls = 0

        def truncate_after_first_read(descriptor: int, count: int) -> bytes:
            nonlocal calls
            data = real_read(descriptor, count)
            calls += 1
            if calls == 1:
                with path.open("r+b") as selected:
                    selected.truncate(0)
            return data

        with mock.patch.object(benchmark.os, "read", side_effect=truncate_after_first_read):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "changed during the test"):
                benchmark.verify_written_data(path, identity, benchmark.time.monotonic(), 5.0)

        self.assertTrue(path.exists())
        self.assertEqual(path.stat().st_size, 0)

    def test_writeback_content_check_detects_change_while_reading(self) -> None:
        path = self.mount / "changed-during-check.bin"
        path.write_bytes(bytes(32))
        identity = path.stat()
        real_read = os.read

        def change_after_read(descriptor: int, count: int) -> bytes:
            data = real_read(descriptor, count)
            path.write_bytes(b"x" * 32)
            info = path.stat()
            os.utime(path, ns=(info.st_atime_ns, info.st_mtime_ns + 1_000_000_000))
            return data

        with mock.patch.object(benchmark.os, "read", side_effect=change_after_read):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "during the content check"):
                benchmark.verify_written_data(path, identity, benchmark.time.monotonic(), 5.0)

        self.assertEqual(path.read_bytes(), b"x" * 32)

    def test_writeback_content_check_does_not_follow_symlink(self) -> None:
        path = self.mount / "symlink-during-check.bin"
        path.write_bytes(bytes(16))
        identity = path.stat()
        external = Path(self.temporary.name) / "outside.bin"
        external.write_bytes(b"preserve outside")
        path.unlink()
        path.symlink_to(external)

        with self.assertRaises(OSError):
            benchmark.verify_written_data(path, identity, benchmark.time.monotonic(), 5.0)

        self.assertTrue(path.is_symlink())
        self.assertEqual(external.read_bytes(), b"preserve outside")

    def test_read_deletes_source_after_success(self) -> None:
        path = self.mount / "read.bin"
        path.write_bytes(b"read this" * 64)

        self.run_benchmark("read", str(path), "--buffered", "--block-size", "512B")

        self.assertFalse(path.exists())

    def test_upload_wait_times_out_against_one_total_deadline(self) -> None:
        path = self.mount / "pending.bin"
        with (
            mock.patch.object(benchmark.os, "getxattr", return_value=b"uploading"),
            mock.patch.object(benchmark.time, "monotonic", side_effect=[10.0, 10.08, 10.11]),
            mock.patch.object(benchmark.time, "sleep") as sleep,
        ):
            with self.assertRaisesRegex(benchmark.BenchmarkError, "timed out"):
                benchmark.wait_for_upload(path, timeout=0.1, poll_interval=0.05, started=10.0)

        self.assertEqual(sleep.call_count, 2)
        self.assertAlmostEqual(sleep.call_args_list[0].args[0], 0.05)
        self.assertAlmostEqual(sleep.call_args_list[1].args[0], 0.02)

    def test_upload_wait_returns_when_vfs_marks_the_file_uploaded(self) -> None:
        path = self.mount / "uploaded.bin"
        with (
            mock.patch.object(benchmark.os, "getxattr", return_value=b"uploaded") as getxattr,
            mock.patch.object(benchmark.time, "monotonic", return_value=10.01),
            mock.patch.object(benchmark.time, "sleep") as sleep,
        ):
            benchmark.wait_for_upload(path, timeout=1.0, poll_interval=0.05, started=10.0)

        getxattr.assert_called_once_with(
            path, benchmark.local_status.UPLOAD_STATUS_XATTR, follow_symlinks=False
        )
        sleep.assert_not_called()


if __name__ == "__main__":
    unittest.main()
