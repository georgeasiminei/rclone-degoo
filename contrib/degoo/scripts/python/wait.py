#!/usr/bin/env python3
"""Wait until local Degoo writeback remains idle across two samples."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import json
import math
import sys
import time
from pathlib import Path
from typing import Any, Callable

import info as local_info
import install as installer
import stats as local_stats


DEFAULT_TIMEOUT_SECONDS = 7200.0
MAX_TIMEOUT_SECONDS = 86400.0
SAMPLE_INTERVAL_SECONDS = 1.0
MAX_RATE_LIMIT_FILE_SIZE = 4096
MAX_GO_UNIX_TIME_SECONDS = (1 << 63) - 1 - 62_135_596_800

AUTH_ERROR = "Account authentication needs attention; sign in with degoo-rc login."
COOLDOWN_ERROR = "Persisted cooldown state is invalid or unsafe; local writeback cannot be confirmed."
STATS_ERROR = "Local mount statistics are unavailable or invalid; check the mount and retry."
TIMEOUT_ERROR = "Timed out before local writeback was confirmed."
CANCEL_ERROR = "Wait cancelled before local writeback was confirmed."


class WaitError(Exception):
    """A safe, user-facing local writeback wait error."""


@dataclass(frozen=True)
class WaitResult:
    """Describe whether local writeback stayed idle across consecutive samples."""

    complete: bool
    cooldown_observed: bool
    waited_seconds: float
    error: str | None = None
    interrupted: bool = False


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def _reject_json_constant(value: str) -> None:
    raise ValueError(f"invalid JSON number: {value}")


def _read_rate_limit_until(root: Path) -> int | None:
    """Read a valid provider cooldown deadline from the owner-private sidecar."""
    path = root / "config" / "auth.json.rate-limit.json"
    try:
        raw = installer.read_private_file(path, MAX_RATE_LIMIT_FILE_SIZE, missing_ok=True)
    except installer.InstallError:
        raise WaitError(COOLDOWN_ERROR) from None
    if raw is None:
        return None
    try:
        state = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_reject_duplicate_keys,
            parse_constant=_reject_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError, ValueError, TypeError):
        raise WaitError(COOLDOWN_ERROR) from None
    if (
        not isinstance(state, dict)
        or set(state) != {"version", "until_unix"}
        or type(state["version"]) is not int
        or state["version"] != 1
        or type(state["until_unix"]) is not int
        or not 0 < state["until_unix"] <= MAX_GO_UNIX_TIME_SECONDS
    ):
        raise WaitError(COOLDOWN_ERROR)
    return state["until_unix"]


def _writeback_is_idle(value: Any) -> bool:
    """Validate the relevant local counters and report whether all are idle."""
    if not isinstance(value, dict) or not isinstance(value.get("runtime"), dict):
        raise WaitError(STATS_ERROR)
    runtime = value["runtime"]
    cache = runtime.get("cache")
    uploads = runtime.get("uploads")
    transfer = runtime.get("transfer")
    if not isinstance(cache, dict) or not isinstance(uploads, dict) or not isinstance(transfer, dict):
        raise WaitError(STATS_ERROR)
    if any(type(cache.get(field)) is not int or cache[field] < 0 for field in ("bytes_dirty", "errored_files")):
        raise WaitError(STATS_ERROR)
    if "files_dirty" in cache and (type(cache["files_dirty"]) is not int or cache["files_dirty"] < 0):
        raise WaitError(STATS_ERROR)
    if type(cache.get("out_of_space")) is not bool:
        raise WaitError(STATS_ERROR)
    if set(uploads) != {"queued", "uploading", "retrying"} or any(
        type(uploads[field]) is not int or uploads[field] < 0 for field in uploads
    ):
        raise WaitError(STATS_ERROR)
    if type(transfer.get("active_uploads")) is not int or transfer["active_uploads"] < 0:
        raise WaitError(STATS_ERROR)

    return (
        not any(uploads.values())
        and cache["bytes_dirty"] == 0
        and cache.get("files_dirty", 0) == 0
        and transfer["active_uploads"] == 0
        and not cache["out_of_space"]
        and cache["errored_files"] == 0
    )


def wait_for_completion(
    root: Path,
    timeout: float,
    *,
    interval: float = SAMPLE_INTERVAL_SECONDS,
    request_stats: Callable[..., dict[str, Any]] | None = None,
    monotonic: Callable[[], float] | None = None,
    wall_clock: Callable[[], float] | None = None,
    sleep: Callable[[float], None] | None = None,
) -> WaitResult:
    """Wait within a monotonic deadline for two idle samples and expired cooldown."""
    stats_request = local_stats.request_stats if request_stats is None else request_stats
    monotonic_clock = time.monotonic if monotonic is None else monotonic
    current_time = time.time if wall_clock is None else wall_clock
    sleep_for = time.sleep if sleep is None else sleep
    started = monotonic_clock()
    deadline = started + timeout
    cooldown_observed = False
    preceding_sample_was_idle = False

    def result(*, complete: bool = False, error: str | None = None, interrupted: bool = False) -> WaitResult:
        return WaitResult(
            complete=complete,
            cooldown_observed=cooldown_observed,
            waited_seconds=max(0.0, monotonic_clock() - started),
            error=error,
            interrupted=interrupted,
        )

    try:
        while True:
            remaining = deadline - monotonic_clock()
            if remaining <= 0:
                return result(error=TIMEOUT_ERROR)

            try:
                auth_error = local_info.authentication_error(root / "config" / "auth.json", logs_dir=root / "logs")
            except Exception:
                return result(error=AUTH_ERROR)
            if auth_error is not None:
                return result(error=AUTH_ERROR)

            try:
                rate_limit_until = _read_rate_limit_until(root)
            except WaitError as exc:
                return result(error=str(exc))
            now = current_time()
            cooldown_active = rate_limit_until is not None and rate_limit_until > now
            cooldown_observed = cooldown_observed or cooldown_active

            remaining = deadline - monotonic_clock()
            if remaining <= 0:
                return result(error=TIMEOUT_ERROR)
            try:
                current_stats = stats_request(root, timeout=min(local_stats.STATS_TIMEOUT, remaining))
            except Exception:
                return result(error=STATS_ERROR)
            if deadline - monotonic_clock() <= 0:
                return result(error=TIMEOUT_ERROR)

            try:
                sample_is_idle = _writeback_is_idle(current_stats)
            except WaitError as exc:
                return result(error=str(exc))
            try:
                rate_limit_until = _read_rate_limit_until(root)
            except WaitError as exc:
                return result(error=str(exc))
            cooldown_active = rate_limit_until is not None and rate_limit_until > current_time()
            cooldown_observed = cooldown_observed or cooldown_active
            cooldown_expired = not cooldown_active
            sample_is_idle = sample_is_idle and cooldown_expired
            if sample_is_idle and preceding_sample_was_idle:
                return result(complete=True)
            preceding_sample_was_idle = sample_is_idle

            remaining = deadline - monotonic_clock()
            if remaining <= 0:
                return result(error=TIMEOUT_ERROR)
            sleep_for(min(interval, remaining))
    except KeyboardInterrupt:
        return result(error=CANCEL_ERROR, interrupted=True)


def _timeout(value: str) -> float:
    try:
        seconds = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError("must be a positive number no greater than 86400 seconds") from None
    if not math.isfinite(seconds) or seconds <= 0 or seconds > MAX_TIMEOUT_SECONDS:
        raise argparse.ArgumentTypeError("must be a positive number no greater than 86400 seconds")
    return seconds


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Wait for two consecutive idle samples from the local mount and for any persisted provider cooldown to expire. "
            "This does not make cloud requests or independently verify remote integrity."
        )
    )
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument(
        "--timeout",
        type=_timeout,
        default=DEFAULT_TIMEOUT_SECONDS,
        metavar="SECONDS",
        help="maximum wait duration (default: 7200 seconds; maximum: 86400 seconds)",
    )
    parser.add_argument("--json", action="store_true", help="print machine-readable completion status")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    result = wait_for_completion(Path(args.root), args.timeout)
    if args.json:
        if result.complete:
            print(
                json.dumps(
                    {
                        "complete": True,
                        "cooldown_observed": result.cooldown_observed,
                        "waited_seconds": result.waited_seconds,
                    },
                    separators=(",", ":"),
                )
            )
        else:
            print(
                json.dumps(
                    {
                        "complete": False,
                        "cooldown_observed": result.cooldown_observed,
                        "error": result.error or STATS_ERROR,
                    },
                    separators=(",", ":"),
                )
            )
    elif result.complete:
        print(
            f"Local writeback appears complete after {result.waited_seconds:.1f}s. "
            "This uses local queue and cache evidence; it is not an independent remote integrity check."
        )
    else:
        print(f"degoo-rc wait: {result.error or STATS_ERROR}", file=sys.stderr)

    if result.interrupted:
        return 130
    if result.complete:
        return 0
    if result.error == TIMEOUT_ERROR:
        return 1
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
