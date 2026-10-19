#!/usr/bin/env python3
"""Show local Degoo mount, cache, and transfer statistics."""

from __future__ import annotations

import argparse
import http.client
import json
import math
import re
import sys
import textwrap
import time
from pathlib import Path
from typing import Any

import info as local_info
import install as installer
import sync as local_sync


STATS_TIMEOUT = 5.0
MAX_STATS_RESPONSE = 64 * 1024
MIN_INTERVAL_SECONDS = 0.1
MAX_COUNTER = (1 << 64) - 1
MAX_RUNTIME_INTEGER = (1 << 63) - 1
BAR_WIDTH = 36
OPERATION_NAME = re.compile(r"[A-Za-z][A-Za-z0-9]*(?: [A-Za-z][A-Za-z0-9]*){0,3}\Z")

API_COUNTERS = {
    "metadata_cache_hits",
    "metadata_cache_misses",
    "metadata_cache_coalesced",
    "metadata_pages",
    "metadata_mutation_invalidations",
    "metadata_parent_invalidations",
    "metadata_global_invalidations",
    "http_requests",
    "read_http_requests",
    "write_http_requests",
    "requests_by_operation",
}
METADATA_CACHE_COUNTERS = {"hits", "misses", "fills", "evictions"}
CACHE_INTEGER_FIELDS = {
    "bytes_used",
    "bytes_dirty",
    "bytes_clean",
    "bytes_evictable",
    "bytes_pinned",
    "files",
    "min_free_bytes",
}
CACHE_OPTIONAL_INTEGER_FIELDS = {
    "files_dirty",
    "files_clean",
    "files_evictable",
    "files_pinned",
    "errored_files",
    "disk_total_bytes",
    "disk_free_bytes",
}
CACHE_OPTIONAL_DURATION_FIELDS = {"max_age_seconds", "poll_interval_seconds"}
TRANSFER_INTEGER_FIELDS = {"upload_bytes", "active_uploads"}
TRANSFER_OPTIONAL_INTEGER_FIELDS = {"download_bytes", "active_downloads"}
PROCESS_INTEGER_FIELDS = {"goroutines", "heap_alloc_bytes", "heap_sys_bytes", "sys_bytes"}


class StatsError(Exception):
    """A safe, user-facing error from the local mount statistics service."""


def _reject_duplicate_keys(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


def _reject_json_constant(value: str) -> None:
    raise ValueError(f"invalid JSON number: {value}")


def _counter(value: Any, name: str, *, maximum: int = MAX_COUNTER) -> None:
    if type(value) is not int or not 0 <= value <= maximum:
        raise ValueError(f"invalid {name}")


def _finite_nonnegative(value: Any, name: str) -> None:
    if type(value) is int:
        if not 0 <= value <= MAX_RUNTIME_INTEGER:
            raise ValueError(f"invalid {name}")
        return
    if type(value) is not float or not math.isfinite(value) or value < 0:
        raise ValueError(f"invalid {name}")


def _validate_counter_group(value: Any, allowed: set[str], name: str) -> None:
    if (
        not isinstance(value, dict)
        or any(key not in allowed for key in value)
        or not allowed.issubset(value)
    ):
        raise ValueError(f"invalid {name}")
    for key, item in value.items():
        if key == "requests_by_operation":
            if not isinstance(item, dict) or len(item) > 256:
                raise ValueError("invalid request operation counters")
            for operation, count in item.items():
                if not isinstance(operation, str) or OPERATION_NAME.fullmatch(operation) is None:
                    raise ValueError("invalid request operation counter")
                _counter(count, "request operation count")
        else:
            _counter(item, name)


def _validate_runtime(runtime: Any) -> None:
    if not isinstance(runtime, dict) or set(runtime) - {"cache", "uploads", "transfer", "process"}:
        raise ValueError("invalid runtime statistics")
    if not {"cache", "uploads", "transfer"}.issubset(runtime):
        raise ValueError("incomplete runtime statistics")

    cache = runtime["cache"]
    cache_fields = CACHE_INTEGER_FIELDS | CACHE_OPTIONAL_INTEGER_FIELDS | CACHE_OPTIONAL_DURATION_FIELDS | {
        "max_bytes",
        "out_of_space",
    }
    if not isinstance(cache, dict) or set(cache) - cache_fields:
        raise ValueError("invalid cache statistics")
    if not CACHE_INTEGER_FIELDS.issubset(cache) or not {"max_bytes", "out_of_space"}.issubset(cache):
        raise ValueError("incomplete cache statistics")
    for field in CACHE_INTEGER_FIELDS | CACHE_OPTIONAL_INTEGER_FIELDS:
        if field in cache:
            _counter(cache[field], field, maximum=MAX_RUNTIME_INTEGER)
    if type(cache["max_bytes"]) is not int or not -1 <= cache["max_bytes"] <= MAX_RUNTIME_INTEGER:
        raise ValueError("invalid cache max_bytes")
    if type(cache["out_of_space"]) is not bool:
        raise ValueError("invalid cache out_of_space")
    for field in CACHE_OPTIONAL_DURATION_FIELDS:
        if field in cache:
            _finite_nonnegative(cache[field], field)

    uploads = runtime["uploads"]
    if not isinstance(uploads, dict) or set(uploads) != {"queued", "uploading", "retrying"}:
        raise ValueError("invalid upload queue statistics")
    for field, value in uploads.items():
        _counter(value, field, maximum=MAX_RUNTIME_INTEGER)

    transfer = runtime["transfer"]
    transfer_fields = TRANSFER_INTEGER_FIELDS | TRANSFER_OPTIONAL_INTEGER_FIELDS | {
        "upload_bytes_per_second",
        "download_bytes_per_second",
    }
    if not isinstance(transfer, dict) or set(transfer) - transfer_fields:
        raise ValueError("invalid transfer statistics")
    if not TRANSFER_INTEGER_FIELDS.issubset(transfer) or "upload_bytes_per_second" not in transfer:
        raise ValueError("incomplete transfer statistics")
    for field in TRANSFER_INTEGER_FIELDS | TRANSFER_OPTIONAL_INTEGER_FIELDS:
        if field in transfer:
            _counter(transfer[field], field, maximum=MAX_RUNTIME_INTEGER)
    for field in ("upload_bytes_per_second", "download_bytes_per_second"):
        if field in transfer:
            _finite_nonnegative(transfer[field], field)

    if "process" in runtime:
        process = runtime["process"]
        process_fields = PROCESS_INTEGER_FIELDS | {"uptime_seconds"}
        if not isinstance(process, dict) or set(process) - process_fields:
            raise ValueError("invalid process statistics")
        for field in PROCESS_INTEGER_FIELDS:
            if field in process:
                _counter(process[field], field, maximum=MAX_RUNTIME_INTEGER)
        if "uptime_seconds" in process:
            _finite_nonnegative(process["uptime_seconds"], "uptime_seconds")


def _validate_stats(value: Any) -> dict[str, Any]:
    """Validate aggregate counters while allowing optional newer metrics."""
    if not isinstance(value, dict) or set(value) - {"api", "metadata_cache", "runtime"}:
        raise ValueError("invalid statistics response")
    if "runtime" not in value:
        raise StatsError(
            "this mount does not expose runtime cache statistics; update the client and restart the mount"
        )
    if not {"api", "metadata_cache"}.issubset(value):
        raise ValueError("incomplete statistics response")
    _validate_counter_group(value["api"], API_COUNTERS, "API counters")
    _validate_counter_group(value["metadata_cache"], METADATA_CACHE_COUNTERS, "metadata cache counters")
    _validate_runtime(value["runtime"])
    return value


def request_stats(root: Path) -> dict[str, Any]:
    """Read aggregate statistics from the private local mount socket only."""
    try:
        local_sync._validate_install(root)
        socket_path = local_sync._socket_path(root)
    except local_sync.SyncError as exc:
        raise StatsError(str(exc)) from None

    connection = local_sync.UnixHTTPConnection(socket_path, timeout=STATS_TIMEOUT)
    try:
        connection.request("GET", "/stats", headers={"Accept": "application/json"})
        response = connection.getresponse()
        payload = response.read(MAX_STATS_RESPONSE + 1)
    except (OSError, http.client.HTTPException, TimeoutError):
        raise StatsError("the mounted vault statistics request failed; check the mount and retry") from None
    finally:
        connection.close()

    if response.status in {404, 405, 503}:
        raise StatsError(
            "this mount does not expose runtime cache statistics; update the client and restart the mount"
        )
    if response.status != 200:
        raise StatsError("the mounted vault could not return statistics; check the mount logs")
    if len(payload) > MAX_STATS_RESPONSE:
        raise StatsError("the mounted vault returned an invalid statistics response")
    try:
        value = json.loads(
            payload,
            object_pairs_hook=_reject_duplicate_keys,
            parse_constant=_reject_json_constant,
        )
        return _validate_stats(value)
    except StatsError:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError, ValueError, TypeError, OverflowError):
        raise StatsError("the mounted vault returned an invalid statistics response") from None


def _format_bytes(value: int) -> str:
    return local_info._format_bytes(max(0, value))


def _format_duration(seconds: float) -> str:
    remaining = int(seconds)
    days, remaining = divmod(remaining, 86400)
    hours, remaining = divmod(remaining, 3600)
    minutes, seconds_left = divmod(remaining, 60)
    parts: list[str] = []
    if days:
        parts.append(f"{days}d")
    if hours or days:
        parts.append(f"{hours}h")
    if minutes or hours or days:
        parts.append(f"{minutes}m")
    parts.append(f"{seconds_left}s")
    return " ".join(parts)


def _bar(cache: dict[str, Any]) -> str:
    used = cache["bytes_used"]
    maximum = cache["max_bytes"]
    categories = [
        ("D", cache["bytes_dirty"]),
        ("E", cache["bytes_evictable"]),
        ("P", cache["bytes_pinned"]),
    ]
    classified = sum(value for _, value in categories)
    if used == 0 and classified == 0 and maximum <= 0:
        return "." * BAR_WIDTH
    if used > classified:
        categories.append(("?", used - classified))
    classified = sum(value for _, value in categories)
    if maximum <= 0:
        denominator = max(used, classified, 1)
        portions = categories
    else:
        denominator = max(maximum, used, classified, 1)
        portions = categories + [(".", max(0, maximum - max(used, classified)))]

    exact = [(label, value * BAR_WIDTH / denominator) for label, value in portions]
    allocated = [(label, int(amount)) for label, amount in exact]
    remaining = BAR_WIDTH - sum(count for _, count in allocated)
    order = sorted(range(len(exact)), key=lambda index: exact[index][1] - int(exact[index][1]), reverse=True)
    for index in order[: max(0, remaining)]:
        label, count = allocated[index]
        allocated[index] = label, count + 1
    bar = "".join(label * count for label, count in allocated)
    if len(bar) < BAR_WIDTH:
        bar += "." * (BAR_WIDTH - len(bar))
    return bar[:BAR_WIDTH]


def format_stats(stats: dict[str, Any]) -> str:
    """Format runtime and request counters for an ordinary terminal."""
    runtime = stats["runtime"]
    cache = runtime["cache"]
    uploads = runtime["uploads"]
    transfer = runtime["transfer"]
    lines: list[str] = []
    api_lines: list[str] = []

    if "api" in stats:
        api = stats["api"]
        if any(field in api for field in ("http_requests", "read_http_requests", "write_http_requests")):
            total = api.get("http_requests", 0)
            reads = api.get("read_http_requests", 0)
            writes = api.get("write_http_requests", 0)
            api_lines.append(f"Degoo API: {total} HTTP requests ({reads} reads, {writes} writes)")
        metadata = [
            f"{api[field]} {label}"
            for field, label in (
                ("metadata_cache_hits", "listing hits"),
                ("metadata_cache_misses", "listing misses"),
                ("metadata_cache_coalesced", "coalesced listings"),
                ("metadata_pages", "listing requests"),
            )
            if field in api
        ]
        if metadata:
            api_lines.append("Directory metadata: " + ", ".join(metadata))
        invalidations = [
            f"{api[field]} {label}"
            for field, label in (
                ("metadata_mutation_invalidations", "mutation"),
                ("metadata_parent_invalidations", "parent"),
                ("metadata_global_invalidations", "global"),
            )
            if field in api
        ]
        if invalidations:
            api_lines.append("Metadata invalidations: " + ", ".join(invalidations))
        operations = api.get("requests_by_operation", {})
        if operations:
            operation_counts = ", ".join(f"{name} {operations[name]}" for name in sorted(operations))
            api_lines.extend(
                textwrap.wrap(
                    f"API operations: {operation_counts}",
                    width=88,
                    subsequent_indent="  ",
                    break_long_words=False,
                    break_on_hyphens=False,
                )
            )

    if "metadata_cache" in stats:
        metadata_cache = stats["metadata_cache"]
        labels = ("hits", "misses", "fills", "evictions")
        counts = [f"{metadata_cache[field]} {field}" for field in labels if field in metadata_cache]
        if counts:
            api_lines.append("Persistent metadata cache: " + ", ".join(counts))

    if cache["max_bytes"] <= 0:
        target = "unlimited target"
    else:
        target = f"{_format_bytes(cache['max_bytes'])} soft target"
    usage = f"Cache: {_format_bytes(cache['bytes_used'])} used / {target}"
    if cache["max_bytes"] > 0 and cache["bytes_used"] > cache["max_bytes"]:
        usage += f" ({_format_bytes(cache['bytes_used'] - cache['max_bytes'])} above target)"
    lines.append(usage)
    lines.append(f"[{_bar(cache)}]")
    legend = "  D dirty  E clean and evictable  P clean and pinned"
    if cache["max_bytes"] > 0:
        legend += "  . target headroom"
    if cache["bytes_used"] > cache["bytes_dirty"] + cache["bytes_evictable"] + cache["bytes_pinned"]:
        legend += "  ? unclassified"
    lines.append(legend)
    lines.append("  VFS cached ranges; excludes filesystem and metadata overhead.")
    lines.append(
        f"  Clean: {_format_bytes(cache['bytes_clean'])} "
        f"(evictable {_format_bytes(cache['bytes_evictable'])}, pinned {_format_bytes(cache['bytes_pinned'])})"
    )
    lines.append(f"  Dirty: {_format_bytes(cache['bytes_dirty'])}")
    lines.append("  Dirty includes open writes that have not been sent.")
    lines.append("  Evictable clean data can be removed; pinned data is open or within its grace period.")
    if all(field in cache for field in ("files_dirty", "files_clean", "files_evictable", "files_pinned")):
        lines.append(
            f"  Files: {cache['files']} total ({cache['files_dirty']} dirty, {cache['files_clean']} clean; "
            f"{cache['files_evictable']} evictable, {cache['files_pinned']} pinned)"
        )
    else:
        lines.append(f"  Files: {cache['files']}")
    if "errored_files" in cache:
        lines.append(f"  Files with cache errors: {cache['errored_files']}")
    lines.append(
        f"Uploads: {uploads['queued']} queued, {uploads['uploading']} uploading, {uploads['retrying']} retrying"
    )
    speed = transfer["upload_bytes_per_second"] / (1024 * 1024)
    lines.append(
        f"Upload: {_format_bytes(transfer['upload_bytes'])} encrypted HTTP bodies (retries included); "
        f"{speed:.2f} MiB/s over ~5s; {transfer['active_uploads']} active"
    )
    if "download_bytes" in transfer:
        active = transfer.get("active_downloads", 0)
        download_speed = transfer.get("download_bytes_per_second", 0.0) / (1024 * 1024)
        lines.append(
            f"Download: {_format_bytes(transfer['download_bytes'])} received; "
            f"{download_speed:.2f} MiB/s over ~5s; {active} active"
        )
    lines.append(f"  Minimum free space: {_format_bytes(cache['min_free_bytes'])}")
    if cache["out_of_space"]:
        lines.append("  OUT OF SPACE reported: cache may be blocked by disk or quota pressure.")
    if "disk_total_bytes" in cache and "disk_free_bytes" in cache:
        lines.append(
            f"  Cache filesystem: {_format_bytes(cache['disk_free_bytes'])} free / "
            f"{_format_bytes(cache['disk_total_bytes'])} total"
        )
    if "max_age_seconds" in cache or "poll_interval_seconds" in cache:
        settings = []
        if "max_age_seconds" in cache:
            settings.append(f"clean max age {_format_duration(cache['max_age_seconds'])}")
        if "poll_interval_seconds" in cache:
            settings.append(f"cleaner checks every {_format_duration(cache['poll_interval_seconds'])}")
        lines.append("  Cache cleanup: " + ", ".join(settings))

    if "process" in runtime:
        process = runtime["process"]
        details = []
        if "uptime_seconds" in process:
            details.append(f"up {_format_duration(process['uptime_seconds'])}")
        if "goroutines" in process:
            details.append(f"{process['goroutines']} goroutines")
        if "heap_alloc_bytes" in process:
            details.append(f"heap {_format_bytes(process['heap_alloc_bytes'])}")
        if "heap_sys_bytes" in process:
            details.append(f"heap reserved {_format_bytes(process['heap_sys_bytes'])}")
        if "sys_bytes" in process:
            details.append(f"runtime memory {_format_bytes(process['sys_bytes'])}")
        if details:
            lines.append("Mount process: " + ", ".join(details))
    lines.extend(api_lines)
    return "\n".join(lines)


def _interval(value: str) -> float:
    try:
        seconds = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError("interval must be a finite number of seconds") from None
    if not math.isfinite(seconds) or seconds < MIN_INTERVAL_SECONDS:
        raise argparse.ArgumentTypeError(f"interval must be finite and at least {MIN_INTERVAL_SECONDS:g} seconds")
    return seconds


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Show aggregate cache, upload, transfer, and Degoo API statistics from the local mount. "
            "This reads the private Unix socket and makes no requests to Degoo."
        )
    )
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument("--json", action="store_true", help="print the full structured statistics object")
    parser.add_argument("--watch", action="store_true", help="refresh the local statistics until interrupted")
    parser.add_argument(
        "--interval",
        type=_interval,
        default=2.0,
        metavar="SECONDS",
        help=f"watch refresh period, at least {MIN_INTERVAL_SECONDS:g}s (default: 2s)",
    )
    return parser


def _stdout_is_tty() -> bool:
    isatty = getattr(sys.stdout, "isatty", None)
    return bool(isatty()) if callable(isatty) else False


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if args.json and args.watch:
        parser.error("--json cannot be combined with --watch")

    try:
        while True:
            stats = request_stats(Path(args.root))
            if args.json:
                print(json.dumps(stats, indent=2), flush=args.watch)
            else:
                if args.watch and _stdout_is_tty():
                    print("\033[2J\033[H", end="")
                print(format_stats(stats), flush=args.watch)
            if not args.watch:
                break
            time.sleep(args.interval)
    except KeyboardInterrupt:
        return 0
    except StatsError as error:
        print(f"degoo-rc stats: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
