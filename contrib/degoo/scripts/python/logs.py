#!/usr/bin/env python3
"""Show recent mount, login, setup, and status logs for a contained Degoo vault."""

from __future__ import annotations

import argparse
import os
import re
import stat
import sys
import time
from datetime import datetime
from pathlib import Path
from urllib.parse import urlsplit, urlunsplit

import install as installer


LOG_NAMES = ("mount.log", "login.log", "setup.log", "status.log")
AUTH_LOG_NAMES = ("mount.log", "login.log")
DEFAULT_LINES = 100
MAX_LOG_BYTES = 4 * 1024 * 1024
MAX_LINE_BYTES = 16 * 1024
MAX_FOLLOW_BYTES_PER_POLL = 256 * 1024
AUTH_MARKER_AGE = 7 * 24 * 60 * 60
AUTH_MARKER_FUTURE_SKEW = 5 * 60
LOG_STAMP = re.compile(
    r"^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})\s+"
    r"(?:ERROR|CRITICAL|WARNING|NOTICE|INFO|DEBUG)\s*:\s+.*\bdegoo login required\b",
    re.IGNORECASE,
)
URL_PATTERN = re.compile(r"(?i)\bhttps?://[^\s\"'<>]+")
BEARER_PATTERN = re.compile(r"(?i)\bBearer\s+[A-Za-z0-9._~+/-]+=*")
JWT_PATTERN = re.compile(
    r"(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\."
    r"[A-Za-z0-9_-]{8,}(?![A-Za-z0-9_-])"
)
CREDENTIAL_VALUE = re.compile(
    r"(?P<prefix>(?<![A-Za-z0-9_.-])(?:access[_-]?token|refresh[_-]?token|auth[_-]?token|"
    r"session[_-]?token|id[_-]?token|token|"
    r"password|passwd|secret|authorization|proxy[_-]?authorization|x-api-key|api[_-]?key|"
    r"client[_-]?secret|signature|sig|credential|credentials|set-cookie|cookie)[\"']?\s*[:=]\s*)"
    r"(?P<value>\"[^\"]*\"|'[^']*'|[^\s,;}&\]]+)",
    re.IGNORECASE,
)


class LogsError(Exception):
    """A safe, user-facing error while inspecting contained logs."""


def validate_root(value: str) -> Path:
    """Validate the private install root and its fixed log directory."""
    root = Path(os.path.expanduser(value))
    if not root.is_absolute() or ".." in root.parts:
        raise LogsError("--root must be an absolute path without parent traversal")
    root = Path(os.path.normpath(str(root)))
    try:
        installer.check_directory(root, private=True)
        installer.check_directory(root / "logs", private=True)
        installer.load_manifest(root)
    except installer.InstallError as exc:
        raise LogsError(str(exc)) from None
    return root


def _open_log(path: Path) -> int | None:
    """Open an owner-private regular log without following links."""
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        return None
    except OSError:
        raise LogsError(f"{path.name} could not be inspected safely") from None
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise LogsError(f"{path.name} is not a regular log file")
    if before.st_uid != os.getuid() or before.st_mode & 0o077 or before.st_mode & 0o6000:
        raise LogsError(f"{path.name} does not have owner-only permissions")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise LogsError(f"{path.name} could not be opened safely") from None
    try:
        opened = os.fstat(fd)
        if (
            not stat.S_ISREG(opened.st_mode)
            or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)
            or opened.st_uid != os.getuid()
            or opened.st_mode & 0o077
            or opened.st_mode & 0o6000
        ):
            raise LogsError(f"{path.name} changed while being opened")
    except BaseException:
        os.close(fd)
        raise
    return fd


def _clean_line(raw: bytes) -> str:
    """Bound and strip terminal controls from one log line."""
    shortened = len(raw) > MAX_LINE_BYTES
    text = raw[:MAX_LINE_BYTES].decode("utf-8", errors="replace")
    visible = "".join(character for character in text if character.isprintable() or character == "\t")
    visible = _redact_sensitive(visible)
    return visible + (" … [line truncated]" if shortened else "")


def _redact_url(match: re.Match[str]) -> str:
    """Hide URL credentials, query strings, and fragments from displayed logs."""
    value = match.group(0)
    trailing = ""
    while value and value[-1] in ").,;!?":
        trailing = value[-1] + trailing
        value = value[:-1]
    try:
        parts = urlsplit(value)
    except ValueError:
        return "[URL redacted]" + trailing
    host = parts.netloc.rsplit("@", 1)[-1]
    safe_url = urlunsplit((parts.scheme, host, parts.path, "", ""))
    if parts.query:
        safe_url += "?[query redacted]"
    if parts.fragment:
        safe_url += "#[fragment redacted]"
    return safe_url + trailing


def _redact_sensitive(text: str) -> str:
    """Remove common credential fields, bearer/JWT values, and URL secrets."""
    text = URL_PATTERN.sub(_redact_url, text)
    text = BEARER_PATTERN.sub("Bearer [redacted]", text)
    text = CREDENTIAL_VALUE.sub(lambda match: f"{match.group('prefix')}[redacted]", text)
    return JWT_PATTERN.sub("[JWT redacted]", text)


def _tail_fd(fd: int, count: int) -> list[str]:
    """Return the last lines and preserve any concurrent append for follow mode."""
    size = os.fstat(fd).st_size
    start = max(0, size - MAX_LOG_BYTES)
    os.lseek(fd, start, os.SEEK_SET)
    remaining = size - start
    chunks: list[bytes] = []
    while remaining > 0:
        chunk = os.read(fd, min(65536, remaining))
        if not chunk:
            break
        chunks.append(chunk)
        remaining -= len(chunk)
    data = b"".join(chunks)
    offset = start + len(data)
    current_size = os.fstat(fd).st_size
    if current_size < offset:
        offset = current_size
    os.lseek(fd, offset, os.SEEK_SET)
    lines = data.splitlines()
    if start and lines:
        lines[0] = b"[older log content omitted] " + lines[0]
    return [_clean_line(line) for line in lines[-count:]]


def _read_log_lines(log_dir: Path, name: str, count: int = DEFAULT_LINES) -> list[str] | None:
    """Read a bounded tail from one private log; return None when it is absent."""
    fd = _open_log(log_dir / name)
    if fd is None:
        return None
    try:
        return _tail_fd(fd, count)
    except OSError:
        raise LogsError(f"{name} could not be read safely") from None
    finally:
        os.close(fd)


def auth_required_after(log_dir: Path, auth_mtime: float, *, now: float | None = None) -> bool:
    """Find a recent timestamped auth rejection newer than saved auth state."""
    try:
        installer.check_directory(log_dir, private=True)
    except installer.InstallError:
        return False
    current = time.time() if now is None else now
    for name in AUTH_LOG_NAMES:
        try:
            lines = _read_log_lines(log_dir, name)
        except LogsError:
            continue
        if lines is None:
            continue
        for line in lines:
            match = LOG_STAMP.match(line)
            if match is None:
                continue
            try:
                event_time = datetime.strptime(match.group(1), "%Y/%m/%d %H:%M:%S").timestamp()
            except (OverflowError, OSError, ValueError):
                continue
            if auth_mtime < event_time <= current + AUTH_MARKER_FUTURE_SKEW and current - event_time <= AUTH_MARKER_AGE:
                return True
    return False


def _prepare(root: Path) -> tuple[str, dict[str, int]]:
    """Read initial tails and leave existing descriptors at their append point."""
    output: list[str] = []
    descriptors: dict[str, int] = {}
    log_dir = root / "logs"
    try:
        for name in LOG_NAMES:
            output.append(f"== {name} (last {DEFAULT_LINES} lines) ==")
            fd = _open_log(log_dir / name)
            if fd is None:
                output.append(f"No {name} yet.")
                continue
            descriptors[name] = fd
            lines = _tail_fd(fd, DEFAULT_LINES)
            output.extend(lines if lines else ["(empty)"])
    except (LogsError, OSError):
        for fd in descriptors.values():
            os.close(fd)
        raise
    output.append("")
    output.append("Service startup messages: journalctl --user -u rclone-degoo.service -n 100 --no-pager")
    return "\n".join(output) + "\n", descriptors


def report(root: Path) -> str:
    """Return recent contained mount, login, setup, and status log lines."""
    root = validate_root(str(root))
    output, descriptors = _prepare(root)
    for fd in descriptors.values():
        os.close(fd)
    return output


def _follow(root: Path, descriptors: dict[str, int]) -> None:
    """Print appended lines, waiting for log files that have not been created yet."""
    pending = {name: b"" for name in descriptors}
    offsets = {name: os.lseek(fd, 0, os.SEEK_CUR) for name, fd in descriptors.items()}
    waiting = set(LOG_NAMES) - set(descriptors)
    log_dir = root / "logs"
    try:
        while descriptors or waiting:
            emitted = False
            for name in tuple(waiting):
                try:
                    fd = _open_log(log_dir / name)
                except LogsError:
                    print(f"[{name}] is unsafe; it will not be followed.", flush=True)
                    waiting.remove(name)
                    continue
                if fd is None:
                    continue
                descriptors[name] = fd
                pending[name] = b""
                offsets[name] = 0
                waiting.remove(name)
                print(f"[{name}] is available; following new lines.", flush=True)
                emitted = True
            for name, fd in tuple(descriptors.items()):
                try:
                    size = os.fstat(fd).st_size
                    if size < offsets[name]:
                        os.lseek(fd, 0, os.SEEK_SET)
                        offsets[name] = 0
                        pending[name] = b""
                    chunk = os.read(fd, MAX_FOLLOW_BYTES_PER_POLL)
                except OSError:
                    chunk = b""
                if not chunk:
                    continue
                emitted = True
                offsets[name] += len(chunk)
                data = pending[name] + chunk
                split = data.split(b"\n")
                pending[name] = split.pop()
                for raw in split:
                    print(f"[{name}] {_clean_line(raw)}", flush=True)
                if len(pending[name]) > MAX_LINE_BYTES:
                    print(f"[{name}] {_clean_line(pending[name])}", flush=True)
                    pending[name] = b""
            if not emitted:
                time.sleep(0.25)
    finally:
        for fd in descriptors.values():
            os.close(fd)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Show the last 100 lines of the contained mount, login, setup, and status logs."
    )
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument(
        "--follow",
        action="store_true",
        help=(
            "also print appended lines; wait for logs created later if needed "
            "(Ctrl-C stops)"
        ),
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        root = validate_root(args.root)
        output, descriptors = _prepare(root)
        sys.stdout.write(output)
        sys.stdout.flush()
        if args.follow and descriptors:
            print(
                "Following log files; service messages can be followed with "
                "journalctl --user -fu rclone-degoo.service.",
                flush=True,
            )
            _follow(root, descriptors)
        elif args.follow:
            print(
                "Waiting for mount.log, login.log, setup.log, or status.log; service "
                "messages can be followed with journalctl --user -fu rclone-degoo.service.",
                flush=True,
            )
            _follow(root, descriptors)
        else:
            for fd in descriptors.values():
                os.close(fd)
        return 0
    except KeyboardInterrupt:
        return 130
    except (LogsError, OSError) as exc:
        message = str(exc) if isinstance(exc, LogsError) else "local logs could not be read safely"
        print(f"logs refused: {message}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
