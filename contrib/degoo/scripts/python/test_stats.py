"""Offline contracts for local Degoo runtime and API statistics."""

from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import sys
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import stats


def sample_stats() -> dict[str, object]:
    gib = 1024**3
    mib = 1024**2
    return {
        "api": {
            "metadata_cache_hits": 12,
            "metadata_cache_misses": 3,
            "metadata_cache_coalesced": 2,
            "metadata_pages": 5,
            "metadata_mutation_invalidations": 1,
            "metadata_parent_invalidations": 2,
            "metadata_global_invalidations": 0,
            "http_requests": 20,
            "read_http_requests": 13,
            "write_http_requests": 7,
            "requests_by_operation": {
                "GetOverlay4": 5,
                "GetBucketWriteAuth4": 2,
                "GCS upload": 2,
                "download": 4,
            },
        },
        "metadata_cache": {"hits": 8, "misses": 4, "fills": 3, "evictions": 1},
        "runtime": {
            "cache": {
                "bytes_used": 4 * gib,
                "bytes_dirty": gib,
                "bytes_clean": 3 * gib,
                "bytes_evictable": 2 * gib,
                "bytes_pinned": gib,
                "files": 4,
                "max_bytes": 6 * gib,
                "min_free_bytes": 100 * gib,
                "out_of_space": False,
                "files_dirty": 1,
                "files_clean": 3,
                "files_evictable": 2,
                "files_pinned": 1,
                "disk_total_bytes": 1000 * gib,
                "disk_free_bytes": 250 * gib,
                "errored_files": 0,
                "max_age_seconds": 3600.0,
                "poll_interval_seconds": 5.0,
            },
            "uploads": {"queued": 2, "uploading": 1, "retrying": 1},
            "transfer": {
                "upload_bytes": 10 * mib,
                "upload_bytes_per_second": 0.5 * mib,
                "active_uploads": 1,
                "download_bytes": 5 * mib,
                "download_bytes_per_second": 2 * mib,
                "active_downloads": 2,
            },
            "process": {
                "uptime_seconds": 3661.0,
                "goroutines": 42,
                "heap_alloc_bytes": 20 * mib,
                "heap_sys_bytes": 32 * mib,
                "sys_bytes": 40 * mib,
            },
        },
    }


class StatsTest(unittest.TestCase):
    def test_current_go_stats_json_schema_fixture_is_accepted(self) -> None:
        self.assertEqual(stats._validate_stats(sample_stats()), sample_stats())

    def test_local_socket_request_reads_bounded_stats_and_preserves_counters(self) -> None:
        response = mock.Mock(status=200)
        response.read.return_value = json.dumps(sample_stats()).encode("utf-8")
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with (
            mock.patch.object(stats.local_sync, "_validate_install"),
            mock.patch.object(stats.local_sync, "_socket_path", return_value=Path("/private/vfs-sync.sock")),
            mock.patch.object(stats.local_sync, "UnixHTTPConnection", return_value=connection) as make_connection,
        ):
            result = stats.request_stats(Path("/private"))

        make_connection.assert_called_once_with(Path("/private/vfs-sync.sock"), timeout=stats.STATS_TIMEOUT)
        connection.request.assert_called_once_with("GET", "/stats", headers={"Accept": "application/json"})
        response.read.assert_called_once_with(stats.MAX_STATS_RESPONSE + 1)
        connection.close.assert_called_once_with()
        self.assertEqual(result, sample_stats())

    def test_local_socket_request_accepts_a_caller_deadline(self) -> None:
        response = mock.Mock(status=200)
        response.read.return_value = json.dumps(sample_stats()).encode("utf-8")
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with (
            mock.patch.object(stats.local_sync, "_validate_install"),
            mock.patch.object(stats.local_sync, "_socket_path", return_value=Path("/private/vfs-sync.sock")),
            mock.patch.object(stats.local_sync, "UnixHTTPConnection", return_value=connection) as make_connection,
        ):
            result = stats.request_stats(Path("/private"), timeout=0.25)

        make_connection.assert_called_once_with(Path("/private/vfs-sync.sock"), timeout=0.25)
        self.assertEqual(result, sample_stats())

    def test_local_socket_request_rejects_nonpositive_or_nonfinite_timeouts(self) -> None:
        for timeout in (0.0, -1.0, float("nan"), float("inf")):
            with self.subTest(timeout=timeout), self.assertRaises(stats.StatsError):
                stats.request_stats(Path("/private"), timeout=timeout)

    def test_unsupported_mount_has_actionable_restart_error(self) -> None:
        response = mock.Mock(status=200)
        response.read.return_value = b'{"api":{},"metadata_cache":{}}'
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with (
            mock.patch.object(stats.local_sync, "_validate_install"),
            mock.patch.object(stats.local_sync, "_socket_path", return_value=Path("/private/vfs-sync.sock")),
            mock.patch.object(stats.local_sync, "UnixHTTPConnection", return_value=connection),
        ):
            with self.assertRaisesRegex(stats.StatsError, "update the client and restart the mount"):
                stats.request_stats(Path("/private"))

    def test_non_success_body_is_never_echoed_to_the_user(self) -> None:
        response = mock.Mock(status=500)
        response.read.return_value = b'{"password":"private test secret"}'
        connection = mock.Mock()
        connection.getresponse.return_value = response
        with (
            mock.patch.object(stats.local_sync, "_validate_install"),
            mock.patch.object(stats.local_sync, "_socket_path", return_value=Path("/private/vfs-sync.sock")),
            mock.patch.object(stats.local_sync, "UnixHTTPConnection", return_value=connection),
        ):
            with self.assertRaises(stats.StatsError) as raised:
                stats.request_stats(Path("/private"))
        self.assertNotIn("private test secret", str(raised.exception))

    def test_invalid_or_unbounded_runtime_fields_fail_closed(self) -> None:
        for mutate in (
            lambda value: value["runtime"]["cache"].update(out_of_space=0),
            lambda value: value["runtime"]["cache"].update(bytes_dirty=True),
            lambda value: value["runtime"]["transfer"].update(upload_bytes_per_second=float("inf")),
            lambda value: value["api"]["requests_by_operation"].update({"private/file": 1}),
            lambda value: value["api"].pop("http_requests"),
            lambda value: value.update(credentials="private test secret"),
        ):
            invalid = sample_stats()
            mutate(invalid)
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                stats._validate_stats(invalid)

    def test_human_format_shows_cache_partition_queue_and_transfer_speed(self) -> None:
        output = stats.format_stats(sample_stats())
        self.assertIn("Degoo API: 20 HTTP requests (13 reads, 7 writes)", output)
        self.assertIn("listing hits", output)
        self.assertIn("Persistent metadata cache: 8 hits, 4 misses, 3 fills, 1 evictions", output)
        self.assertRegex(output, r"Cache: 4\.0+ GiB used / 6\.0+ GiB soft target")
        bar = next(line[1:-1] for line in output.splitlines() if line.startswith("[") and line.endswith("]"))
        self.assertEqual(len(bar), stats.BAR_WIDTH)
        self.assertEqual(bar, "DDDDDDEEEEEEEEEEEEPPPPPP............")
        self.assertRegex(
            output,
            r"Clean: 3\.0+ GiB \(evictable 2\.0+ GiB, pinned 1\.0+ GiB\)",
        )
        self.assertRegex(output, r"Dirty: 1\.0+ GiB")
        self.assertIn("pinned data is open or within its grace period", output)
        self.assertIn("VFS cached ranges; excludes filesystem and metadata overhead", output)
        self.assertIn("Files with cache errors: 0", output)
        self.assertRegex(output, r"Upload: 10\.0+ MiB encrypted HTTP bodies \(retries included\); 0\.50 MiB/s")
        self.assertRegex(output, r"Download: 5\.0+ MiB received; 2\.00 MiB/s")
        self.assertIn("Mount process: up 1h 1m 1s, 42 goroutines", output)
        self.assertNotIn("private/file", output)

    def test_unlimited_cache_and_out_of_space_are_readable(self) -> None:
        value = sample_stats()
        for maximum in (-1, 0):
            with self.subTest(max_bytes=maximum):
                value = sample_stats()
                value["runtime"]["cache"].update(max_bytes=maximum, out_of_space=True)
                output = stats.format_stats(value)
                self.assertRegex(output, r"Cache: 4\.0+ GiB used / unlimited target")
                self.assertIn("OUT OF SPACE", output)

    def test_json_mode_preserves_full_structured_response(self) -> None:
        stdout = io.StringIO()
        with mock.patch.object(stats, "request_stats", return_value=sample_stats()), contextlib.redirect_stdout(stdout):
            code = stats.main(["--json"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout.getvalue()), sample_stats())

    def test_json_watch_conflict_and_bad_intervals_are_rejected(self) -> None:
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as raised:
            stats.main(["--json", "--watch"])
        self.assertEqual(raised.exception.code, 2)
        self.assertIn("cannot be combined", stderr.getvalue())
        for invalid in ("0", "0.01", "nan", "inf", "-1", "not-a-number"):
            with self.subTest(interval=invalid), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                stats.build_parser().parse_args(["--interval", invalid])
        self.assertEqual(stats.build_parser().parse_args(["--interval", "0.1"]).interval, 0.1)

    def test_watch_interrupt_is_clean_and_piped_output_has_no_terminal_codes(self) -> None:
        stdout = io.StringIO()
        with (
            mock.patch.object(stats, "request_stats", return_value=sample_stats()),
            mock.patch.object(stats.time, "sleep", side_effect=KeyboardInterrupt),
            contextlib.redirect_stdout(stdout),
        ):
            code = stats.main(["--watch", "--interval", "0.5"])
        self.assertEqual(code, 0)
        self.assertRegex(stdout.getvalue(), r"Cache: 4\.0+ GiB used")
        self.assertNotIn("\033", stdout.getvalue())

    def test_watch_clears_only_a_terminal_display(self) -> None:
        class TerminalOutput(io.StringIO):
            def isatty(self) -> bool:
                return True

        stdout = TerminalOutput()
        with (
            mock.patch.object(stats, "request_stats", return_value=sample_stats()),
            mock.patch.object(stats.time, "sleep", side_effect=KeyboardInterrupt),
            mock.patch.object(stats.sys, "stdout", stdout),
        ):
            code = stats.main(["--watch", "--interval", "0.5"])
        self.assertEqual(code, 0)
        self.assertTrue(stdout.getvalue().startswith("\033[2J\033[H"))


if __name__ == "__main__":
    unittest.main()
