"""Offline contracts for waiting on local Degoo writeback."""

from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import stats
import wait


def sample_stats(
    *,
    queued: int = 0,
    uploading: int = 0,
    retrying: int = 0,
    dirty: int = 0,
    files_dirty: int = 0,
    active_uploads: int = 0,
    out_of_space: bool = False,
    errored_files: int = 0,
) -> dict[str, object]:
    return {
        "runtime": {
            "cache": {
                "bytes_dirty": dirty,
                "files_dirty": files_dirty,
                "out_of_space": out_of_space,
                "errored_files": errored_files,
            },
            "uploads": {"queued": queued, "uploading": uploading, "retrying": retrying},
            "transfer": {"active_uploads": active_uploads},
        }
    }


class FakeTime:
    def __init__(self, *, monotonic: float = 0.0, wall: float = 1_000.0) -> None:
        self.current = monotonic
        self.wall = wall

    def monotonic(self) -> float:
        return self.current

    def time(self) -> float:
        return self.wall + self.current

    def sleep(self, seconds: float) -> None:
        self.current += seconds


class WaitTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory(prefix="degoo-wait-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "install"
        config = self.root / "config"
        config.mkdir(parents=True, mode=0o700)
        self.root.chmod(0o700)
        config.chmod(0o700)
        self.rate_limit_path = config / "auth.json.rate-limit.json"
        self.fake_time = FakeTime()

    @staticmethod
    def write_private(path: Path, content: bytes | str, mode: int = 0o600) -> None:
        path.write_bytes(content.encode("utf-8") if isinstance(content, str) else content)
        path.chmod(mode)

    def run_wait(self, request, *, timeout: float = 10.0, interval: float = 1.0):
        with mock.patch.object(wait.local_info, "authentication_error", return_value=None):
            return wait.wait_for_completion(
                self.root,
                timeout,
                interval=interval,
                request_stats=request,
                monotonic=self.fake_time.monotonic,
                wall_clock=self.fake_time.time,
                sleep=self.fake_time.sleep,
            )

    def test_persisted_cooldown_delays_two_empty_samples(self) -> None:
        self.write_private(self.rate_limit_path, '{"version":1,"until_unix":1002}')
        timeouts: list[float] = []

        def request(_root: Path, *, timeout: float):
            timeouts.append(timeout)
            return sample_stats()

        result = self.run_wait(request)

        self.assertTrue(result.complete)
        self.assertTrue(result.cooldown_observed)
        self.assertEqual(result.waited_seconds, 3.0)
        self.assertEqual(len(timeouts), 4)
        self.assertTrue(all(timeout > 0 for timeout in timeouts))

    def test_pending_then_empty_requires_two_consecutive_empty_samples(self) -> None:
        samples = iter((sample_stats(queued=1), sample_stats(), sample_stats()))
        result = self.run_wait(lambda _root, *, timeout: next(samples))

        self.assertTrue(result.complete)
        self.assertFalse(result.cooldown_observed)
        self.assertEqual(result.waited_seconds, 2.0)

    def test_timeout_preserves_incomplete_result_and_cooldown_observation(self) -> None:
        self.write_private(self.rate_limit_path, '{"version":1,"until_unix":2000}')
        result = self.run_wait(lambda _root, *, timeout: sample_stats(queued=1), timeout=2.0)

        self.assertFalse(result.complete)
        self.assertTrue(result.cooldown_observed)
        self.assertEqual(result.error, wait.TIMEOUT_ERROR)
        self.assertEqual(result.waited_seconds, 2.0)

    def test_local_stats_timeout_is_bounded_by_remaining_deadline(self) -> None:
        observed: list[float] = []

        def request(_root: Path, *, timeout: float):
            observed.append(timeout)
            return sample_stats(queued=1)

        result = self.run_wait(request, timeout=2.5)

        self.assertEqual(result.error, wait.TIMEOUT_ERROR)
        self.assertEqual(observed, [2.5, 1.5, 0.5])

    def test_local_stats_timeout_is_capped_at_the_stats_socket_timeout(self) -> None:
        observed: list[float] = []

        def request(_root: Path, *, timeout: float):
            observed.append(timeout)
            return sample_stats(queued=1)

        result = self.run_wait(request, timeout=10.0)

        self.assertEqual(result.error, wait.TIMEOUT_ERROR)
        self.assertEqual(observed[0], wait.local_stats.STATS_TIMEOUT)
        self.assertEqual(observed[-1], 1.0)

    def test_optional_dirty_file_count_may_be_absent_but_unknown_errors_fail_closed(self) -> None:
        stats_without_optional_file_count = sample_stats()
        del stats_without_optional_file_count["runtime"]["cache"]["files_dirty"]
        result = self.run_wait(lambda _root, *, timeout: stats_without_optional_file_count)
        self.assertTrue(result.complete)

        self.fake_time = FakeTime()
        stats_without_error_count = sample_stats()
        del stats_without_error_count["runtime"]["cache"]["errored_files"]
        result = self.run_wait(lambda _root, *, timeout: stats_without_error_count)
        self.assertEqual(result.error, wait.STATS_ERROR)

    def test_out_of_space_and_cache_errors_never_confirm_completion(self) -> None:
        for sample in (sample_stats(out_of_space=True), sample_stats(errored_files=1)):
            with self.subTest(sample=sample):
                self.fake_time = FakeTime()
                result = self.run_wait(lambda _root, *, timeout, value=sample: value, timeout=2.0)
                self.assertFalse(result.complete)
                self.assertEqual(result.error, wait.TIMEOUT_ERROR)

    def test_missing_cooldown_means_no_hold(self) -> None:
        self.assertIsNone(wait._read_rate_limit_until(self.root))

    def test_invalid_or_unsafe_cooldown_files_fail_closed(self) -> None:
        invalid = (
            (b'{"version":1,"until_unix":123} trailing', 0o600),
            (b'{"version":1,"version":1,"until_unix":123}', 0o600),
            (b'{"version":1,"until_unix":123,"extra":true}', 0o600),
            (b'{"version":1,"until_unix":true}', 0o600),
            (
                f'{{"version":1,"until_unix":{wait.MAX_GO_UNIX_TIME_SECONDS + 1}}}'.encode(),
                0o600,
            ),
            (b'{"version":1,"until_unix":123}', 0o644),
        )
        for contents, mode in invalid:
            with self.subTest(contents=contents, mode=mode):
                self.write_private(self.rate_limit_path, contents, mode)
                with self.assertRaises(wait.WaitError):
                    wait._read_rate_limit_until(self.root)
                self.rate_limit_path.unlink()

        target = self.root / "private-target"
        self.write_private(target, '{"version":1,"until_unix":123}')
        self.rate_limit_path.symlink_to(target)
        with self.assertRaises(wait.WaitError):
            wait._read_rate_limit_until(self.root)

        self.rate_limit_path.unlink()
        self.write_private(
            self.rate_limit_path,
            f'{{"version":1,"until_unix":{wait.MAX_GO_UNIX_TIME_SECONDS}}}',
        )
        self.assertEqual(wait._read_rate_limit_until(self.root), wait.MAX_GO_UNIX_TIME_SECONDS)

    def test_unavailable_mount_stats_returns_error_without_false_completion(self) -> None:
        result = self.run_wait(
            lambda _root, *, timeout: (_ for _ in ()).throw(stats.StatsError("private failure details"))
        )

        self.assertEqual(result.error, wait.STATS_ERROR)
        self.assertFalse(result.complete)
        self.assertNotIn("private failure details", result.error or "")

    def test_known_local_authentication_failure_skips_stats_request(self) -> None:
        request = mock.Mock(side_effect=AssertionError("must not request stats after auth failure"))
        with mock.patch.object(wait.local_info, "authentication_error", return_value="login required"):
            result = wait.wait_for_completion(
                self.root,
                10,
                request_stats=request,
                monotonic=self.fake_time.monotonic,
                wall_clock=self.fake_time.time,
                sleep=self.fake_time.sleep,
            )

        self.assertEqual(result.error, wait.AUTH_ERROR)
        request.assert_not_called()

    def test_json_success_and_stats_failure_are_machine_readable(self) -> None:
        stdout = io.StringIO()
        with (
            mock.patch.object(wait.local_info, "authentication_error", return_value=None),
            mock.patch.object(wait.local_stats, "request_stats", return_value=sample_stats()),
            mock.patch.object(wait.time, "monotonic", side_effect=self.fake_time.monotonic),
            mock.patch.object(wait.time, "time", side_effect=self.fake_time.time),
            mock.patch.object(wait.time, "sleep", side_effect=self.fake_time.sleep),
            contextlib.redirect_stdout(stdout),
        ):
            code = wait.main(["--root", str(self.root), "--timeout", "4", "--json"])

        self.assertEqual(code, 0)
        success = json.loads(stdout.getvalue())
        self.assertEqual(set(success), {"complete", "cooldown_observed", "waited_seconds"})
        self.assertTrue(success["complete"])
        self.assertEqual(success["waited_seconds"], 1.0)

        stdout = io.StringIO()
        self.fake_time = FakeTime()
        with (
            mock.patch.object(wait.local_info, "authentication_error", return_value=None),
            mock.patch.object(wait.local_stats, "request_stats", side_effect=stats.StatsError("details")),
            mock.patch.object(wait.time, "monotonic", side_effect=self.fake_time.monotonic),
            mock.patch.object(wait.time, "time", side_effect=self.fake_time.time),
            contextlib.redirect_stdout(stdout),
        ):
            code = wait.main(["--root", str(self.root), "--timeout", "4", "--json"])

        self.assertEqual(code, 2)
        failure = json.loads(stdout.getvalue())
        self.assertFalse(failure["complete"])
        self.assertEqual(failure["error"], wait.STATS_ERROR)
        self.assertNotIn("details", stdout.getvalue())

    def test_cancel_returns_130_and_json_error_without_exposing_state(self) -> None:
        stdout = io.StringIO()
        with (
            mock.patch.object(wait.local_info, "authentication_error", return_value=None),
            mock.patch.object(wait.local_stats, "request_stats", side_effect=KeyboardInterrupt),
            mock.patch.object(wait.time, "monotonic", side_effect=self.fake_time.monotonic),
            mock.patch.object(wait.time, "time", side_effect=self.fake_time.time),
            contextlib.redirect_stdout(stdout),
        ):
            code = wait.main(["--root", str(self.root), "--json"])

        self.assertEqual(code, 130)
        self.assertEqual(json.loads(stdout.getvalue())["complete"], False)
        self.assertIn("cancel", json.loads(stdout.getvalue())["error"].lower())

    def test_timeout_argument_requires_a_finite_positive_value_at_most_one_day(self) -> None:
        for invalid in ("0", "-1", "nan", "inf", "86400.1", "nope"):
            with self.subTest(timeout=invalid), contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                wait.build_parser().parse_args(["--timeout", invalid])


if __name__ == "__main__":
    unittest.main()
