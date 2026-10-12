#!/usr/bin/env python3
"""Measure mounted file reads and writes, including VFS upload completion."""

from __future__ import annotations

import argparse
import math
import os
import re
import stat
import subprocess
import sys
import time
from pathlib import Path

import install as installer
import status as local_status
import sync as local_sync


class BenchmarkError(Exception):
    """A benchmark failure that leaves the selected file available for recovery."""


def parse_size(value: str) -> int:
    """Parse a positive byte count or a binary K/M/G/T size."""
    match = re.fullmatch(r"([0-9]+)([KMGT]?)(?:i?B)?", value, re.IGNORECASE)
    if match is None:
        raise argparse.ArgumentTypeError("use positive bytes or a size such as 16MiB or 2GiB")
    number = int(match[1]) * 1024 ** (" KMGT".index(match[2].upper()))
    if not 0 < number <= (1 << 63) - 1:
        raise argparse.ArgumentTypeError("size must be positive and fit in 63 bits")
    return number


def positive_seconds(value: str) -> float:
    """Parse a finite positive number of seconds."""
    try:
        number = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError("use a positive number of seconds") from None
    if not math.isfinite(number) or number <= 0:
        raise argparse.ArgumentTypeError("use a finite positive number of seconds")
    return number


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="degoo-rc benchmark", description=__doc__, epilog=(
        "Successful tests delete the selected file unless --no-delete is supplied. "
        "Failures preserve it. Direct reads bypass the kernel page cache but may "
        "still use rclone's disk cache; this is not a cold cloud-read guarantee. "
        "Use degoo-rc benchmark write help or degoo-rc benchmark read help for options."
    ))
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    commands = parser.add_subparsers(dest="mode", required=True)
    for mode in ("write", "read"):
        command = commands.add_parser(mode, description=(
            "Write zeros, fsync, close, then wait for this file's VFS uploaded state."
            if mode == "write" else "Read an existing benchmark file to /dev/null."
        ))
        command.add_argument("filename", nargs="?", help="file path (default: benchmark.bin under the mount; relative paths use the current directory)")
        command.add_argument("--block-size", type=parse_size, default=16 * 1024 * 1024, help="dd block size (default: 16MiB; K/M/G/T suffixes are binary)")
        command.add_argument("--timeout", type=positive_seconds, default=3600.0, help="total I/O and upload wait limit in seconds (default: 3600)")
        command.add_argument("--no-delete", action="store_true", help="keep the file after a successful test")
        if mode == "write":
            command.add_argument("--size", type=parse_size, default=2 * 1024 ** 3, help="exact bytes to write (default: 2GiB)")
            command.add_argument("--poll-interval", type=positive_seconds, default=1.0, help="seconds between file upload-state checks (default: 1)")
        else:
            command.add_argument("--buffered", action="store_true", help="allow kernel page-cache reads instead of direct I/O")
            command.add_argument("--drop-caches", action="store_true", help="explicitly run sudo to drop system caches before timing; rclone's disk cache remains")
    return parser


def wait_for_upload(path: Path, timeout: float, poll_interval: float, started: float) -> None:
    """Wait for this file's current VFS version to be uploaded within the time limit."""
    while True:
        remaining = timeout - (time.monotonic() - started)
        if remaining <= 0:
            raise BenchmarkError("upload wait timed out; file preserved; inspect degoo-rc status and logs")
        try:
            state = os.getxattr(path, local_status.UPLOAD_STATUS_XATTR, follow_symlinks=False)
        except OSError:
            raise BenchmarkError("file upload state is unavailable; file preserved; check the mount and degoo-rc status") from None
        if state == b"uploaded":
            return
        if state not in (b"pending", b"uploading"):
            raise BenchmarkError("file upload state is unknown; file preserved; inspect degoo-rc status and logs")
        time.sleep(min(poll_interval, remaining))


def verify_written_data(path: Path, identity: os.stat_result, started: float, timeout: float) -> None:
    """Confirm generated zero data when writeback changes the reported timestamp."""
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        current = os.fstat(descriptor)
        if (
            not stat.S_ISREG(current.st_mode)
            or (current.st_dev, current.st_ino, current.st_size)
            != (identity.st_dev, identity.st_ino, identity.st_size)
        ):
            raise BenchmarkError("the benchmark file changed during the test; cleanup refused")
        zeros = bytes(1024 * 1024)
        remaining = identity.st_size
        while remaining:
            if time.monotonic() - started >= timeout:
                raise BenchmarkError("content check timed out; file preserved")
            data = os.read(descriptor, min(remaining, len(zeros)))
            if not data or data != zeros[:len(data)]:
                raise BenchmarkError("the benchmark file changed during the test; cleanup refused")
            remaining -= len(data)
        if time.monotonic() - started >= timeout:
            raise BenchmarkError("content check timed out; file preserved")
        after = os.fstat(descriptor)
        if (after.st_size, after.st_mtime_ns) != (current.st_size, current.st_mtime_ns):
            raise BenchmarkError("the benchmark file changed during the content check; cleanup refused")
    finally:
        os.close(descriptor)


def run(args: argparse.Namespace) -> int:
    """Benchmark a regular mounted file, preserving it on failure."""
    mountpoint = local_sync._validate_install(Path(args.root)).resolve(strict=True)
    requested = Path(args.filename) if args.filename else mountpoint / "benchmark.bin"
    # Resolve only the parent so the final component is opened without following a link.
    path = requested.absolute().parent.resolve(strict=True) / requested.name
    local_sync._inside_mount(path, mountpoint)
    if path == mountpoint:
        raise BenchmarkError("select a regular file inside the mounted vault")
    if args.mode == "read" and args.drop_caches:
        result = subprocess.run(["sudo", "sh", "-c", "echo 3 > /proc/sys/vm/drop_caches"], check=False)
        if result.returncode:
            raise BenchmarkError("could not drop system caches; read did not run")
    flags = os.O_NOFOLLOW | (os.O_WRONLY | os.O_CREAT | os.O_EXCL if args.mode == "write" else os.O_RDONLY | os.O_NONBLOCK)
    try:
        descriptor = os.open(path, flags, 0o600)
    except FileExistsError:
        raise BenchmarkError("write refuses to overwrite an existing file; select a new benchmark filename") from None
    try:
        identity = os.fstat(descriptor)
        if not stat.S_ISREG(identity.st_mode):
            raise BenchmarkError("the benchmark path must be a regular file")
        size = args.size if args.mode == "write" else identity.st_size
        if size <= 0:
            raise BenchmarkError("the read benchmark needs a nonempty file")
        command = ["dd", f"bs={args.block_size}", "status=progress"]
        if args.mode == "write":
            command += ["if=/dev/zero", f"count={size}", "iflag=count_bytes", "conv=fsync"]
            streams = {"stdout": descriptor, "stdin": subprocess.DEVNULL}
        else:
            command += ["of=/dev/null"]
            if not args.buffered:
                command += ["iflag=direct"]
            streams = {"stdin": descriptor}
        print(f"Benchmark {args.mode}: {path} ({size} bytes)", flush=True)
        started = time.monotonic()
        result = subprocess.run(command, **streams, check=False, timeout=args.timeout)
        if result.returncode:
            raise BenchmarkError("dd failed; file preserved; direct reads may require --buffered on this mount")
        if args.mode == "write":
            identity = os.fstat(descriptor)
            if identity.st_size != size:
                raise BenchmarkError("the written file has an unexpected size; file preserved")
    finally:
        os.close(descriptor)
    if args.mode == "write":
        print("Saved locally; waiting for this file's VFS upload completion...", flush=True)
        wait_for_upload(path, args.timeout, args.poll_interval, started)
    elapsed = time.monotonic() - started
    if elapsed <= 0:
        raise BenchmarkError("could not measure elapsed time; file preserved")
    current = path.lstat()
    if (
        (current.st_dev, current.st_ino, current.st_size)
        != (identity.st_dev, identity.st_ino, identity.st_size)
        or not stat.S_ISREG(current.st_mode)
    ):
        raise BenchmarkError("the benchmark file changed during the test; cleanup refused")
    if current.st_mtime_ns != identity.st_mtime_ns:
        if args.mode != "write":
            raise BenchmarkError("the benchmark file changed during the test; cleanup refused")
        # FUSE/cache close and upload can change mtime without changing the data.
        print("Writeback timestamp changed; checking generated zero data (excluded from throughput timing)...", flush=True)
        verify_written_data(path, identity, started, args.timeout)
        after = path.lstat()
        if (
            (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
            != (current.st_dev, current.st_ino, current.st_size, current.st_mtime_ns)
            or not stat.S_ISREG(after.st_mode)
        ):
            raise BenchmarkError("the benchmark file changed during the content check; cleanup refused")
    print(f"Elapsed: {elapsed:.3f} seconds\nAverage speed: {size / elapsed / 1_000_000:.2f} MB/s", flush=True)
    if args.mode == "write":
        print("Includes local write, fsync, close, writeback delay, and VFS upload completion; no independent cloud integrity check.")
    else:
        print("Mounted read throughput; rclone's local disk cache may supply the data.")
    if args.no_delete:
        print(f"Kept: {path}")
    else:
        path.unlink()
        print(f"Deleted: {path}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(args)
    except (BenchmarkError, local_sync.SyncError) as exc:
        print(f"benchmark refused: {exc}", file=sys.stderr)
    except subprocess.TimeoutExpired:
        print("benchmark timed out; file preserved", file=sys.stderr)
    except KeyboardInterrupt:
        print("benchmark interrupted; file preserved", file=sys.stderr)
        return 130
    except OSError:
        print("benchmark failed: could not access the mounted file or run dd; file preserved", file=sys.stderr)
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
