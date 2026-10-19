#!/usr/bin/env python3
"""Install or upgrade a contained Linux Degoo vault client."""

from __future__ import annotations

import argparse
import configparser
from decimal import Decimal, InvalidOperation
import errno
import fcntl
import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any


ROOT_NAME = "rclone-degoo"
REMOTE_NAME = "degoo-live-test"
ROOT_REL = Path(".local/share") / ROOT_NAME
SOURCE_FILES = ("cli.py", "install.py", "info.py", "logs.py", "login.py", "service.py", "setup.py", "uninstall.py", "wizard.py", "mount_prerequisites.py", "mount_setup.py", "connection.py", "restore.py", "backup_config.py", "sync.py", "status.py", "stats.py", "benchmark.py", "update.py", "update_preflight.py")
DEFAULT_MOUNTPOINT = "/mnt/degoo"
SOURCE_SHELL_FILES = ("install.sh", "trash.sh", "uninstall.sh")
GENERATED_SHELL_FILES = ("info.sh", "logs.sh", "login.sh", "setup.sh", "mount.sh", "status.sh", "location.sh", "folders.sh", "stop.sh", "service.sh")
IMMUTABLE_MOUNT_FILES = ("helper.c", "README.md")
COMPATIBILITY_SHELL_FILES = (*SOURCE_SHELL_FILES, *GENERATED_SHELL_FILES)
LEGACY_GENERATED_SHELL_FILES = COMPATIBILITY_SHELL_FILES
MAX_CONFIG_SIZE = 1024 * 1024
MAX_BUILD_METADATA_SIZE = 16 * 1024
BUILD_COMMIT_PATTERN = re.compile(r"(?:[0-9a-f]{40}|[0-9a-f]{64})\Z")
SHA256_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
DEFAULT_MOUNT_SETTINGS = {
    "cache_max_size": "60G",
    "cache_min_free_space": "60G",
    "cache_max_age": "750h",
    "cache_poll_interval": "5s",
    "write_back": "5s",
    "verify_upload": False,
    "buffer_size": "16Mi",
    "read_ahead": "1Mi",
    "read_chunk_size": "128Mi",
    "read_chunk_size_limit": "off",
    "read_chunk_streams": 8,
    "dir_cache_time": "30m",
    "attr_timeout": "1s",
    "transfers": 4,
    "bwlimit": "off",
    "timeout": "5m",
}
REQUIRED_MOUNT_SETTINGS = frozenset(
    {"cache_max_size", "cache_min_free_space", "cache_max_age", "cache_poll_interval", "verify_upload"}
)
MOUNT_SIZE_SETTINGS = frozenset(
    {"cache_max_size", "cache_min_free_space", "buffer_size", "read_ahead", "read_chunk_size", "read_chunk_size_limit"}
)
MOUNT_SIZE_OFF_FIELDS = frozenset({"cache_max_size", "cache_min_free_space", "read_chunk_size", "read_chunk_size_limit"})
MOUNT_DURATION_SETTINGS = frozenset(
    {"cache_max_age", "cache_poll_interval", "dir_cache_time", "attr_timeout", "timeout"}
)
MOUNT_COUNT_SETTINGS = {"read_chunk_streams": (0, (1 << 31) - 1), "transfers": (1, (1 << 31) - 1)}
MAX_WRITE_BACK_DURATION_NS = (1 << 63) - 1
SIZE_NUMBER = r"(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)"
SIZE_SUFFIX = r"(?:b|[kmgtpe](?:ib?)?)?"
MOUNT_SIZE_VALUE = re.compile(rf"(?:off|{SIZE_NUMBER}{SIZE_SUFFIX})\Z", re.IGNORECASE)
MOUNT_DURATION_COMPONENT = re.compile(r"((?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+))(ns|us|µs|μs|ms|s|m|h|d|w|M|y)")
MOUNT_DURATION_NUMBER = re.compile(rf"{SIZE_NUMBER}\Z")
BW_RATE = re.compile(rf"(?P<tx>off|{SIZE_NUMBER}{SIZE_SUFFIX})(?::(?P<rx>off|{SIZE_NUMBER}{SIZE_SUFFIX}))?\Z", re.IGNORECASE)
FUSERMOUNT_NAMES = {"fusermount", "fusermount2", "fusermount2.9", "fusermount3"}
LEGACY_LOG_FILES = ("mount.log", "login.log", "setup.log", "status.log", "wizard-folders.log", "diagnostics.log")


class InstallError(Exception):
    """A safe, user-facing refusal from the contained installer."""


def is_valid_write_back_duration(value: Any) -> bool:
    """Report whether value is a positive delay other than Go's off sentinel."""
    total_ns = _mount_duration_ns(value)
    return total_ns is not None and int(total_ns) > 0


def _mount_duration_ns(value: Any) -> Decimal | None:
    """Parse a nonnegative rclone duration to nanoseconds, or return None."""
    if (
        not isinstance(value, str)
        or not value
        or len(value) > 128
        or any(character in value for character in "\r\n\x00")
        or value == "off"
    ):
        return None
    units_ns = {
        "ns": Decimal(1),
        "us": Decimal(1_000),
        "µs": Decimal(1_000),
        "μs": Decimal(1_000),
        "ms": Decimal(1_000_000),
        "s": Decimal(1_000_000_000),
        "m": Decimal(60_000_000_000),
        "h": Decimal(3_600_000_000_000),
        "d": Decimal(86_400_000_000_000),
        "w": Decimal(604_800_000_000_000),
        "M": Decimal(2_592_000_000_000_000),
        "y": Decimal(31_536_000_000_000_000),
    }
    if MOUNT_DURATION_NUMBER.fullmatch(value):
        try:
            total_ns = Decimal(value) * units_ns["s"]
        except InvalidOperation:
            return None
        return total_ns if total_ns < MAX_WRITE_BACK_DURATION_NS else None
    position = 0
    total_ns = Decimal(0)
    for match in MOUNT_DURATION_COMPONENT.finditer(value):
        if match.start() != position:
            return None
        amount, unit = match.groups()
        try:
            total_ns += Decimal(amount) * units_ns[unit]
        except InvalidOperation:
            return None
        if total_ns >= MAX_WRITE_BACK_DURATION_NS:
            return None
        position = match.end()
    return total_ns if position == len(value) and position > 0 else None


def is_valid_mount_duration(value: Any) -> bool:
    """Report whether value can be parsed as a nonnegative rclone duration."""
    if isinstance(value, str) and value == "off":
        return True
    return _mount_duration_ns(value) is not None


def is_valid_mount_size(value: Any, *, allow_off: bool = True) -> bool:
    """Report whether value can be parsed as an rclone size setting."""
    if (
        not isinstance(value, str)
        or not value
        or len(value) > 64
        or any(character in value for character in "\r\n\x00")
    ):
        return False
    if not MOUNT_SIZE_VALUE.fullmatch(value):
        return False
    if value.casefold() == "off":
        return allow_off
    number = re.match(SIZE_NUMBER, value)
    assert number is not None
    try:
        amount = Decimal(number.group())
    except InvalidOperation:
        return False
    suffix = value[number.end() :]
    if suffix.casefold() == "b":
        multiplier = 1
    elif suffix:
        power = "kmgtpe".find(suffix[0].casefold()) + 1
        if power < 1:
            return False
        multiplier = 1024**power
    else:
        multiplier = 1024 if amount else 1
    try:
        return amount >= 0 and amount * multiplier < MAX_WRITE_BACK_DURATION_NS
    except InvalidOperation:
        return False


def is_valid_bwlimit(value: Any) -> bool:
    """Report whether value follows rclone's bandwidth value or timetable syntax."""
    if (
        not isinstance(value, str)
        or not value
        or len(value) > 512
        or any(character in value for character in "\r\n\x00")
    ):
        return False
    slots = value.replace(";", " ").split()
    if not slots:
        return False
    for slot in slots:
        if "," not in slot:
            if len(slots) != 1:
                return False
            rates = [slot]
        else:
            time_spec, rate = slot.split(",", 1)
            if "," in rate:
                return False
            day, separator, clock = time_spec.partition("-")
            if separator:
                weekdays = {
                    "mon", "monday", "tue", "tuesday", "wed", "wednesday", "thu", "thursday",
                    "fri", "friday", "sat", "saturday", "sun", "sunday",
                }
                if day.casefold() not in weekdays:
                    return False
            else:
                clock = time_spec
            if not re.fullmatch(r"[0-9]{2}:[0-9]{2}", clock):
                return False
            hour, minute = (int(part) for part in clock.split(":"))
            if hour > 23 or minute > 59:
                return False
            rates = [rate]
        for rate in rates:
            match = BW_RATE.fullmatch(rate)
            if match is None:
                return False
            if not is_valid_mount_size(match.group("tx")):
                return False
            if match.group("rx") is not None and not is_valid_mount_size(match.group("rx")):
                return False
    return True


def home_directory() -> Path:
    home = Path(os.path.expanduser("~"))
    if not home.is_absolute():
        raise InstallError("could not determine an absolute home directory")
    return Path(os.path.normpath("/" + str(home).lstrip("/")))


def expected_root() -> Path:
    return home_directory() / ROOT_REL


def expected_path_link() -> Path:
    """Return the user-level command link for this contained install."""
    return home_directory() / ".local" / "bin" / "degoo-rc"


def _path_link_matches(link: Path, root: Path) -> bool:
    """Report whether a safe user-owned link points to this install entrypoint."""
    if not os.path.lexists(link):
        return False
    check_directory(link.parent)
    try:
        info = os.lstat(link)
    except OSError:
        raise InstallError("the PATH command link could not be inspected safely") from None
    if not stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid():
        return False
    try:
        target_text = os.readlink(link)
    except OSError:
        raise InstallError("the PATH command link could not be read safely") from None
    target = Path(target_text)
    if not target.is_absolute():
        target = link.parent / target
    return Path(os.path.normpath(str(target))) == root / "degoo-rc"


def _ensure_path_link_directory() -> Path:
    """Create and validate ~/.local/bin without following symlinked paths."""
    local = home_directory() / ".local"
    check_directory(local)
    directory = local / "bin"
    try:
        os.mkdir(directory, 0o700)
    except FileExistsError:
        pass
    except OSError:
        raise InstallError("the command directory ~/.local/bin could not be created safely") from None
    check_directory(directory)
    return directory


def _preflight_path_link(root: Path) -> Path:
    """Refuse an unrelated command before creating or upgrading the install."""
    directory = _ensure_path_link_directory()
    link = directory / "degoo-rc"
    if os.path.lexists(link) and not _path_link_matches(link, root):
        raise InstallError("~/.local/bin/degoo-rc already exists and is not owned by this install")
    return link


def _path_contains_local_bin() -> bool:
    """Report whether ~/.local/bin is already present in the current PATH."""
    expected = home_directory() / ".local" / "bin"
    for entry in os.environ.get("PATH", "").split(os.pathsep):
        expanded = os.path.expandvars(os.path.expanduser(entry))
        candidate = Path(os.path.abspath(expanded or os.curdir))
        if candidate == expected:
            return True
    return False


def _install_path_link(root: Path, manifest: dict[str, Any]) -> bool:
    """Create or verify the command link and record its ownership."""
    link = _preflight_path_link(root)
    created = False
    if not os.path.lexists(link):
        try:
            os.symlink(str(root / "degoo-rc"), link)
        except OSError:
            raise InstallError("the unified Degoo command could not be linked into ~/.local/bin") from None
        created = True
    if not _path_link_matches(link, root):
        raise InstallError("~/.local/bin/degoo-rc is not a verified link to this install")
    manifest["path_link"] = str(link)
    return created


def _print_path_hint() -> None:
    if not _path_contains_local_bin():
        print('Add the unified command to PATH in this shell with:')
        print('  export PATH="$HOME/.local/bin:$PATH"')


def check_components(path: Path, *, missing_ok: bool = False) -> None:
    current = Path(path.anchor)
    for index, part in enumerate(path.parts[1:]):
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            if missing_ok:
                return
            raise InstallError("a required path is missing") from None
        except OSError:
            raise InstallError("a path could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode):
            raise InstallError("symlink path components are not allowed")
        if index != len(path.parts[1:]) - 1 and not stat.S_ISDIR(info.st_mode):
            raise InstallError("a path ancestor is not a directory")


def check_directory(path: Path, *, private: bool = False) -> os.stat_result:
    check_components(path)
    try:
        info = os.lstat(path)
    except OSError:
        raise InstallError("a required install directory is unavailable") from None
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise InstallError("a required install path is not a directory")
    if info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise InstallError("an install directory has unsafe ownership or permissions")
    if private and info.st_mode & 0o077:
        raise InstallError("a private install directory must use mode 0700")
    return info


def ensure_private_directory(path: Path) -> None:
    try:
        os.mkdir(path, 0o700)
    except FileExistsError:
        check_directory(path, private=True)
    except OSError:
        raise InstallError("a private install directory could not be created") from None
    else:
        check_directory(path, private=True)


def ensure_parent_directories(root: Path) -> None:
    home = home_directory()
    check_components(home)
    home_info = check_directory(home)
    if home_info.st_uid != os.getuid() or home_info.st_mode & 0o022:
        raise InstallError("the home directory has unsafe ownership or permissions")
    for path in (home / ".local", home / ".local/share"):
        if not path.exists():
            try:
                os.mkdir(path, 0o700)
            except OSError:
                raise InstallError("the install parent directory could not be created") from None
        check_components(path)
        try:
            info = os.lstat(path)
        except OSError:
            raise InstallError(f"the install parent directory {path} could not be inspected safely") from None
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise InstallError(f"the install parent directory {path} is not a safe directory")
        if info.st_uid != os.getuid() or info.st_mode & 0o022:
            raise InstallError(
                f"{path} must be owned by the current user and not writable by others; "
                f"review it and run chmod go-w {shlex.quote(str(path))} if appropriate"
            )


def preflight_install_safety() -> None:
    """Fail early if the install's ancestor directories are unsafe.

    The installer creates missing ~/.local and ~/.local/share directories,
    but it cannot safely use existing directories that are writable by group
    or other, or not owned by the current user. ~/.local/bin is created by
    the installer if missing; if it already exists it must also be safe.
    """
    home = home_directory()
    paths = [home, home / ".local", home / ".local/share", home / ".local/bin"]
    for candidate in paths:
        if not candidate.exists():
            continue
        try:
            info = os.lstat(candidate)
        except OSError:
            raise InstallError(f"{candidate} could not be inspected for safe install") from None
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise InstallError(f"{candidate} is not a safe directory and cannot be used for install")
        if info.st_uid != os.getuid():
            raise InstallError(
                f"{candidate} is not owned by the current user; review ownership before installing"
            )
        if info.st_mode & 0o022:
            raise InstallError(
                f"{candidate} is writable by group or other and cannot be used for a private install; "
                f"run chmod go-w {shlex.quote(str(candidate))}"
            )


def read_private_file(path: Path, maximum: int = MAX_CONFIG_SIZE, *, missing_ok: bool = False) -> bytes | None:
    check_components(path, missing_ok=missing_ok)
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        if missing_ok:
            return None
        raise InstallError("a required private file is missing") from None
    except OSError:
        raise InstallError("a private file could not be inspected safely") from None
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise InstallError("a private path is not a regular file")
    if before.st_uid != os.getuid() or before.st_mode & 0o077 or before.st_mode & 0o6000:
        raise InstallError("a private file has unsafe ownership or permissions")
    if before.st_size > maximum:
        raise InstallError("a private file is larger than the supported limit")
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        raise InstallError("a private file could not be opened safely") from None
    try:
        opened = os.fstat(fd)
        if (
            not stat.S_ISREG(opened.st_mode)
            or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)
            or opened.st_uid != os.getuid()
            or opened.st_mode & 0o077
            or opened.st_mode & 0o6000
        ):
            raise InstallError("a private file changed while being opened")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining:
            chunk = os.read(fd, min(65536, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        after = os.fstat(fd)
        if (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise InstallError("a private file changed while being read")
    finally:
        os.close(fd)
    data = b"".join(chunks)
    if len(data) > maximum:
        raise InstallError("a private file is larger than the supported limit")
    return data


def normalize_legacy_logs(log_dir: Path) -> None:
    """Set known legacy log files to owner-only mode through validated file descriptors."""
    check_directory(log_dir, private=True)
    opened: list[tuple[str, Path, int, os.stat_result]] = []
    try:
        for name in LEGACY_LOG_FILES:
            path = log_dir / name
            try:
                before = os.lstat(path)
            except FileNotFoundError:
                continue
            except OSError:
                raise InstallError(f"{name} could not be inspected safely") from None
            if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
                raise InstallError(f"{name} is not a regular log file")
            if before.st_uid != os.getuid():
                raise InstallError(f"{name} is not owned by the current user")
            if before.st_nlink != 1:
                raise InstallError(f"{name} has unexpected hard links")
            flags = (
                os.O_RDONLY
                | getattr(os, "O_NOFOLLOW", 0)
                | getattr(os, "O_CLOEXEC", 0)
                | getattr(os, "O_NONBLOCK", 0)
            )
            try:
                fd = os.open(path, flags)
            except OSError:
                raise InstallError(f"{name} could not be opened safely") from None
            opened.append((name, path, fd, before))
            opened_info = os.fstat(fd)
            if (
                not stat.S_ISREG(opened_info.st_mode)
                or (opened_info.st_dev, opened_info.st_ino) != (before.st_dev, before.st_ino)
                or opened_info.st_uid != os.getuid()
                or opened_info.st_nlink != 1
            ):
                raise InstallError(f"{name} changed while being opened")

        for name, path, fd, before in opened:
            try:
                current = os.lstat(path)
            except OSError:
                raise InstallError(f"{name} changed before its permissions could be fixed") from None
            opened_info = os.fstat(fd)
            if (
                not stat.S_ISREG(current.st_mode)
                or (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino)
                or (opened_info.st_dev, opened_info.st_ino) != (before.st_dev, before.st_ino)
                or current.st_uid != os.getuid()
                or opened_info.st_uid != os.getuid()
            ):
                raise InstallError(f"{name} changed before its permissions could be fixed")

        for name, _, fd, _ in opened:
            try:
                os.fchmod(fd, 0o600)
            except OSError:
                raise InstallError(f"{name} could not be made owner-private") from None
            if stat.S_IMODE(os.fstat(fd).st_mode) != 0o600:
                raise InstallError(f"{name} could not be made owner-private")
    finally:
        for _, _, fd, _ in opened:
            os.close(fd)


def write_private_file(path: Path, data: bytes, *, replace: bool = False, mode: int = 0o600) -> None:
    check_directory(path.parent, private=True)
    existing = read_private_file(path, missing_ok=True)
    if existing is not None and not replace:
        raise InstallError(f"refusing to overwrite existing {path.name}")
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}-{secrets.token_hex(4)}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(temporary, flags, mode)
    except OSError:
        raise InstallError("a private file could not be written safely") from None
    try:
        with os.fdopen(fd, "wb", closefd=True) as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    except OSError:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise InstallError("a private file could not be written safely") from None


def parse_json_object(data: bytes, message: str) -> dict[str, Any]:
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise InstallError(message) from None
    if not isinstance(value, dict):
        raise InstallError(message)
    return value


def validate_folder_id(value: str) -> str:
    folder_id = value.strip()
    if not folder_id or folder_id == "0" or any(ord(character) < 0x21 or ord(character) == 0x7F for character in folder_id):
        raise InstallError("--folder-id must identify a dedicated folder and cannot be 0")
    return folder_id


def validate_folder_name(value: str) -> str:
    name = value.strip()
    if (
        not name
        or name in {".", ".."}
        or name.casefold().startswith(".rclone")
        or "/" in name
        or "\\" in name
        or len(name.encode("utf-8")) > 255
        or any(ord(character) < 0x20 or ord(character) == 0x7F for character in name)
    ):
        raise InstallError("--folder-name must be a safe single folder name (not a path or an internal .rclone name)")
    return name


def load_manifest(root: Path) -> dict[str, Any]:
    raw = read_private_file(root / "install.json", 64 * 1024)
    manifest = parse_json_object(raw or b"", "the install manifest is invalid")
    if type(manifest.get("version")) is not int or manifest["version"] != 1 or manifest.get("remote_name") != REMOTE_NAME:
        raise InstallError("the existing install manifest is unsupported")
    mountpoint = manifest.get("mountpoint")
    if not isinstance(mountpoint, str) or validate_mountpoint(mountpoint, root) != mountpoint or manifest.get("cache_dir") != "cache":
        raise InstallError("the existing install manifest contains unexpected paths")
    linked_path = manifest.get("path_link", "")
    if not isinstance(linked_path, str) or (linked_path and linked_path != str(expected_path_link())):
        raise InstallError("the existing install manifest contains an unexpected PATH link")
    folder_id = manifest.get("root_folder_id")
    if not isinstance(folder_id, str) or folder_id == "0":
        raise InstallError("the existing install manifest has an unsafe folder ID")
    return manifest


def configured_mountpoint(root: Path, manifest: dict[str, Any] | None = None) -> Path:
    """Return the normalized configured mountpoint."""
    root = Path(root)
    if not root.is_absolute() or ".." in root.parts:
        raise InstallError("the install root must be absolute without parent traversal")
    root = Path(os.path.normpath("/" + str(root).lstrip("/")))
    if manifest is None:
        raw = read_private_file(root / "install.json", 64 * 1024, missing_ok=True)
        manifest = parse_json_object(raw, "the install manifest is invalid") if raw else {}
    value = manifest.get("mountpoint", "mount")
    if not isinstance(value, str):
        raise InstallError("the install manifest contains an unexpected mountpoint")
    normalized = validate_mountpoint(value, root)
    if normalized != value:
        raise InstallError("the install manifest contains an unexpected mountpoint")
    return root / "mount" if normalized == "mount" else Path(normalized)


def _paths_overlap(left: Path, right: Path) -> bool:
    """Report whether either path contains the other after lexical normalization."""
    return left == right or left in right.parents or right in left.parents


def validate_mountpoint(value: str, root: Path) -> str:
    """Normalize a safe mountpoint while preserving the legacy manifest token."""
    if (
        not isinstance(value, str)
        or not value
        or any(ord(character) < 0x20 or 0x7F <= ord(character) < 0xA0 for character in value)
    ):
        raise InstallError("the mountpoint path is invalid")
    root = Path(root)
    if not root.is_absolute() or ".." in root.parts:
        raise InstallError("the install root must be absolute without parent traversal")
    root = Path(os.path.normpath("/" + str(root).lstrip("/")))
    if value == "mount":
        return "mount"
    expanded = os.path.expanduser(value)
    path = Path(expanded)
    if not path.is_absolute() or ".." in path.parts:
        raise InstallError("the mountpoint path must be absolute and cannot contain parent traversal")
    path = Path(os.path.normpath("/" + str(path).lstrip("/")))
    if path == Path(path.anchor):
        raise InstallError("the filesystem root cannot be used as a mountpoint")
    legacy_mount = Path(os.path.normpath(str(root / "mount")))
    if path == legacy_mount:
        return "mount"
    if _paths_overlap(path, root):
        raise InstallError("the mountpoint path cannot overlap the contained install")
    default_cache = Path(os.path.normpath(str(root / "cache")))
    if _paths_overlap(path, default_cache):
        raise InstallError("the mountpoint path cannot overlap the default vault cache")
    return str(path)


def _validate_configured_cache_dir(value: Any, root: Path, *, mountpoint: Path | None = None) -> Path:
    """Validate a vault cache path without creating it."""
    if (
        not isinstance(value, str)
        or not value
        or any(ord(character) < 0x20 or 0x7F <= ord(character) < 0xA0 for character in value)
    ):
        raise InstallError("the vault cache path is invalid")
    cache = Path(value)
    if not cache.is_absolute() or ".." in cache.parts:
        raise InstallError("the vault cache path must be absolute and cannot contain parent traversal")
    cache = Path(os.path.normpath("/" + value.lstrip("/")))
    root = Path(os.path.normpath(str(root)))
    home = home_directory()
    if cache == Path(cache.anchor) or cache in home.parents or cache == home or cache == root or cache in root.parents:
        raise InstallError("the vault cache path overlaps a protected root directory")

    default_cache = root / "cache"
    if root in cache.parents:
        protected = (
            root / "bin",
            root / "config",
            root / "logs",
            root / "mount",
            root / "tmp",
            root / "tools",
            root / "scripts",
            root / "immutable-mount",
            root / "degoo-rc",
            root / "install.json",
            root / "rclone-degoo.service",
        )
        if cache != default_cache and any(_paths_overlap(cache, path) for path in protected):
            raise InstallError("the vault cache path overlaps protected install files")
    selected_mountpoint = configured_mountpoint(root) if mountpoint is None else Path(os.path.normpath(str(mountpoint)))
    if _paths_overlap(cache, selected_mountpoint):
        raise InstallError("the vault cache path overlaps the mountpoint")

    try:
        check_components(cache, missing_ok=True)
        for parent in reversed(cache.parents):
            try:
                info = os.lstat(parent)
            except FileNotFoundError:
                continue
            except OSError:
                raise InstallError("the vault cache path could not be inspected safely") from None
            if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
                raise InstallError("the vault cache path has an unsafe parent directory")
        try:
            info = os.lstat(cache)
        except FileNotFoundError:
            return cache
        except OSError:
            raise InstallError("the vault cache path could not be inspected safely") from None
    except InstallError:
        raise

    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise InstallError("the vault cache path is not a regular directory")
    if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
        raise InstallError("the vault cache directory must be owned by this user with mode 0700")
    return cache


def configured_cache_dir(root: Path, *, mountpoint: Path | None = None) -> Path:
    """Return the normalized, validated cache directory saved in vault.json."""
    root = Path(os.path.normpath(str(root)))
    raw = read_private_file(root / "config/vault.json", missing_ok=True)
    if raw is None:
        return _validate_configured_cache_dir(str(root / "cache"), root, mountpoint=mountpoint)
    config, _ = read_vault(raw, root, imported=False, mountpoint=mountpoint)
    return Path(os.path.normpath(config["cache_dir"]))


def mountpoint_instructions(path: Path) -> str:
    """Return commands to create an owner-private immutable mount directory."""
    quoted = shlex.quote(str(path))
    return (
        f"sudo mkdir -p -- {quoted}\n"
        f"sudo chown -- {os.getuid()}:{os.getgid()} {quoted}\n"
        f"sudo chmod 700 -- {quoted}\n"
        f"sudo chattr +i -- {quoted}"
    )


def _create_mountpoint_path(path: Path) -> None:
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            current.mkdir(mode=0o700)
        except FileExistsError:
            pass
        except OSError:
            raise InstallError("a mountpoint directory could not be created safely") from None
        try:
            info = os.lstat(current)
        except OSError:
            raise InstallError("a mountpoint path component could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise InstallError("symlink or non-directory mountpoint components are not allowed")


def prepare_mountpoint(path: Path) -> None:
    """Create or validate an empty, owner-private, unmounted mount directory."""
    path = Path(os.path.normpath(str(path)))
    is_default = path == Path(os.path.normpath(str(DEFAULT_MOUNTPOINT)))
    try:
        check_components(path, missing_ok=True)
        if os.path.ismount(path):
            raise InstallError("the selected mountpoint is already mounted; unmount it before installing")
        if not os.path.lexists(path):
            _create_mountpoint_path(path)
        check_components(path)
        try:
            info = os.lstat(path)
        except OSError:
            raise InstallError("the mountpoint could not be inspected safely") from None
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise InstallError("the mountpoint must be a real directory")
        if info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_mode & 0o500 != 0o500:
            raise InstallError("the mountpoint must be owned by this user, private, and owner-readable")
        try:
            with os.scandir(path) as entries:
                if next(entries, None) is not None:
                    raise InstallError(
                        "the mountpoint must be empty; it contains ordinary local files that are not queued for upload"
                    )
        except OSError:
            raise InstallError("the mountpoint contents could not be inspected safely") from None
        if os.path.ismount(path):
            raise InstallError("the selected mountpoint is already mounted; unmount it before installing")
    except InstallError as exc:
        if "already mounted" in str(exc) or "must be empty" in str(exc):
            raise
        if is_default:
            raise InstallError(
                f"mount directory {path} could not be created or is not owner-private. "
                "Run these commands, then retry installation:\n" + mountpoint_instructions(path)
            ) from None
        raise InstallError(f"mount directory {path} is unsafe: {exc}") from None
    except OSError:
        if is_default:
            raise InstallError(
                f"mount directory {path} could not be created or is not owner-private. "
                "Run these commands, then retry installation:\n" + mountpoint_instructions(path)
            ) from None
        raise InstallError(f"mount directory {path} could not be created safely") from None


def read_vault(raw: bytes, root: Path, *, imported: bool, mountpoint: Path | None = None) -> tuple[dict[str, Any], bytes]:
    config = parse_json_object(raw, "the vault configuration is invalid")
    allowed = {"version", "remote", "password", "password_salt", "cache_dir", "chunk_size", "max_object_size", "account_credentials"}
    if set(config) - allowed:
        raise InstallError("the vault configuration has unsupported fields")
    if type(config.get("version")) is not int or config["version"] != 1:
        raise InstallError("the vault configuration version must be 1")
    if not isinstance(config.get("password"), str) or not config["password"]:
        raise InstallError("the vault configuration does not contain a password")
    if "password_salt" in config and not isinstance(config["password_salt"], str):
        raise InstallError("the vault configuration password salt is invalid")
    if "account_credentials" in config and not isinstance(config["account_credentials"], str):
        raise InstallError("the encrypted account credentials are invalid")
    if not imported:
        if config.get("remote") != f"{REMOTE_NAME}:":
            raise InstallError("the installed vault configuration points to another remote")
        _validate_configured_cache_dir(config.get("cache_dir"), root, mountpoint=mountpoint)
    else:
        remote = config.get("remote")
        if not isinstance(remote, str) or not remote.endswith(":") or not remote[:-1]:
            raise InstallError("the imported vault configuration has an invalid remote")
        config.pop("account_credentials", None)
        config["remote"] = f"{REMOTE_NAME}:"
        config["cache_dir"] = str(root / "cache")
    return config, (json.dumps(config, indent=2, sort_keys=True) + "\n").encode("utf-8")


def import_vault_config(path_arg: str, root: Path) -> bytes:
    path = Path(os.path.expanduser(path_arg))
    if not path.is_absolute() or ".." in path.parts:
        raise InstallError("--vault-config must be an absolute path without parent traversal")
    path = Path(os.path.normpath(str(path)))
    check_components(path)
    check_directory(path.parent)
    parent = os.lstat(path.parent)
    if parent.st_uid != os.getuid() or parent.st_mode & 0o022:
        raise InstallError("the imported vault config directory has unsafe permissions")
    raw = read_private_file(path)
    assert raw is not None
    _, rewritten = read_vault(raw, root, imported=True)
    return rewritten


def _validate_vault_password_inputs(password: Any, password_salt: Any) -> tuple[str | None, str | None]:
    """Validate an internally supplied vault password and optional exact salt."""
    if password is not None and (not isinstance(password, str) or password == ""):
        raise InstallError("the chosen vault password must be a nonempty string")
    if password_salt is not None and not isinstance(password_salt, str):
        raise InstallError("the chosen vault password salt must be a string")
    if password is None and password_salt not in (None, ""):
        raise InstallError("a vault password salt requires a chosen vault password")
    return password, password_salt or None


def _install_vault_password_inputs(args: argparse.Namespace) -> tuple[str | None, str | None]:
    """Validate the wizard-only vault secret handoff without exposing it in CLI options."""
    password, password_salt = _validate_vault_password_inputs(
        getattr(args, "vault_password", None),
        getattr(args, "vault_password_salt", None),
    )
    if password is not None and getattr(args, "vault_config", None):
        raise InstallError("a chosen vault password cannot be combined with an imported vault configuration")
    return password, password_salt


def _validate_upgrade_vault_state(
    root: Path,
    manifest: dict[str, Any],
    args: argparse.Namespace,
    password: str | None,
) -> None:
    """Allow a first key only on an unbound staged install with no prior vault files."""
    existing_vault = read_private_file(root / "config/vault.json", missing_ok=True)
    if password is not None and existing_vault is not None:
        raise InstallError("a chosen vault password cannot replace an existing vault configuration")

    folder_id = manifest.get("root_folder_id", "")
    existing_rclone_config = read_private_file(root / "config/rclone.conf", missing_ok=True)
    if folder_id and existing_vault is None and not getattr(args, "vault_config", None):
        raise InstallError("the bound vault configuration is missing; import the matching private vault configuration")
    if password is not None and (folder_id or existing_rclone_config is not None):
        raise InstallError("a chosen vault password is allowed only before the staged install has been bound")


def create_vault_config(
    root: Path,
    imported: bytes | None,
    *,
    password: str | None = None,
    password_salt: str | None = None,
) -> bytes:
    password, password_salt = _validate_vault_password_inputs(password, password_salt)
    if imported is not None:
        if password is not None or password_salt is not None:
            raise InstallError("a chosen vault password cannot replace an imported vault configuration")
        return imported
    config = {
        "version": 1,
        "remote": f"{REMOTE_NAME}:",
        "password": password if password is not None else secrets.token_urlsafe(32),
        "cache_dir": str(root / "cache"),
        "chunk_size": "128Mi",
        "max_object_size": "19Gi",
    }
    if password_salt is not None:
        config["password_salt"] = password_salt
    return (json.dumps(config, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_mount_settings(raw: bytes) -> None:
    settings = parse_json_object(raw, "config/mount.json is invalid")
    supported_fields = set(DEFAULT_MOUNT_SETTINGS)
    if REQUIRED_MOUNT_SETTINGS - set(settings) or set(settings) - supported_fields:
        raise InstallError("config/mount.json must contain the supported cache and upload settings")
    for field in MOUNT_SIZE_SETTINGS:
        if field in settings and not is_valid_mount_size(settings[field], allow_off=field in MOUNT_SIZE_OFF_FIELDS):
            raise InstallError(f"config/mount.json has an invalid {field}")
    for field in MOUNT_DURATION_SETTINGS:
        if field in settings and not is_valid_mount_duration(settings[field]):
            raise InstallError(f"config/mount.json has an invalid {field}")
    for field, (minimum, maximum) in MOUNT_COUNT_SETTINGS.items():
        value = settings.get(field)
        if field in settings and (type(value) is not int or value < minimum or value > maximum):
            raise InstallError(f"config/mount.json has an invalid {field}")
    if "write_back" in settings and not is_valid_write_back_duration(settings["write_back"]):
        raise InstallError("config/mount.json has an invalid write_back setting")
    if "bwlimit" in settings and not is_valid_bwlimit(settings["bwlimit"]):
        raise InstallError("config/mount.json has an invalid bwlimit setting")
    if type(settings["verify_upload"]) is not bool:
        raise InstallError("config/mount.json has an invalid verify_upload setting")


def validate_rclone_config(raw: bytes, root: Path, folder_id: str) -> None:
    parser = configparser.ConfigParser(interpolation=None, strict=True)
    try:
        parser.read_string(raw.decode("utf-8"))
    except (UnicodeDecodeError, configparser.Error):
        raise InstallError("config/rclone.conf is invalid") from None
    if parser.sections() != [REMOTE_NAME] or not parser.has_section(REMOTE_NAME):
        raise InstallError("config/rclone.conf does not contain the contained Degoo remote")
    section = parser[REMOTE_NAME]
    if section.get("type") != "degoo" or section.get("root_folder_id") != folder_id or section.get("auth_file") != str(root / "config/auth.json"):
        raise InstallError("config/rclone.conf points to another remote or account-token path")
    if "credentials_file" in section and section["credentials_file"] != str(root / "config/vault.json"):
        raise InstallError("config/rclone.conf points to another vault-credential path")


def rclone_config(root: Path, folder_id: str) -> bytes:
    return (
        f"[{REMOTE_NAME}]\n"
        "type = degoo\n"
        f"root_folder_id = {folder_id}\n"
        f"auth_file = {root / 'config/auth.json'}\n"
        f"credentials_file = {root / 'config/vault.json'}\n"
    ).encode("utf-8")


def shell_quote(value: str | Path) -> str:
    return shlex.quote(str(value))


def shell_wrapper(name: str, root: Path, *, mountpoint: Path | None = None) -> bytes:
    root_text = shell_quote(root)
    mountpoint = configured_mountpoint(root) if mountpoint is None else mountpoint
    if name == "info.sh":
        content = (
            "#!/bin/sh\nset -eu\n"
            f"ROOT={root_text}\n"
            "export PYTHONDONTWRITEBYTECODE=1\n"
            "exec python3 \"$ROOT/scripts/python/info.py\" --root \"$ROOT\" \"$@\"\n"
        )
        return content.encode("utf-8")
    if name == "logs.sh":
        content = (
            "#!/bin/sh\nset -eu\n"
            f"ROOT={root_text}\n"
            "export PYTHONDONTWRITEBYTECODE=1\n"
            "exec python3 \"$ROOT/scripts/python/logs.py\" --root \"$ROOT\" \"$@\"\n"
        )
        return content.encode("utf-8")
    if name == "service.sh":
        content = (
            "#!/bin/sh\nset -eu\n"
            f"ROOT={root_text}\n"
            "export PYTHONDONTWRITEBYTECODE=1\n"
            "exec python3 \"$ROOT/scripts/python/service.py\" --root \"$ROOT\" \"$@\"\n"
        )
        return content.encode("utf-8")
    if name == "login.sh":
        command = f"exec python3 {shell_quote(root / 'scripts/python/login.py')} --root \"$ROOT\" \"$@\""
    elif name == "setup.sh":
        command = f"exec python3 {shell_quote(root / 'scripts/python/setup.py')} --root \"$ROOT\" \"$@\""
    elif name == "install.sh":
        command = (
            "if [ \"$#\" -eq 0 ]; then set -- --wizard; fi\n"
            f"exec python3 {shell_quote(root / 'scripts/python/install.py')} \"$@\""
        )
    elif name == "uninstall.sh":
        command = f"exec python3 {shell_quote(root / 'scripts/python/uninstall.py')} --root \"$ROOT\" \"$@\""
    elif name == "mount.sh":
        command = (
            "case $(\"$ROOT/bin/rclone\" degoo mount --help) in\n"
            "  *--sync-socket*) set -- --sync-socket \"$ROOT/tmp/vfs-sync.sock\" \"$@\";;\n"
            "esac\n"
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/mount.log\" --log-level NOTICE degoo mount "
            f"--settings \"$ROOT/config/mount.json\" \"$ROOT/config/vault.json\" \"$MOUNTPOINT\" \"$@\""
        )
    elif name == "status.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/status.log\" --log-level ERROR degoo status \"$ROOT/config/vault.json\" \"$@\""
        )
    elif name == "location.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/diagnostics.log\" --log-level NOTICE "
            f"degoo location \"$ROOT/config/vault.json\" \"$@\""
        )
    elif name == "folders.sh":
        command = (
            "if [ -f \"$ROOT/config/vault.json\" ]; then\n"
            "  set -- --vault-config \"$ROOT/config/vault.json\" \"$@\"\n"
            "fi\n"
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/diagnostics.log\" --log-level NOTICE "
            f"degoo folders \"$ROOT/config/auth.json\" \"$@\""
        )
    elif name == "stop.sh":
        command = (
            "if command -v mountpoint >/dev/null 2>&1; then\n"
            "  if mountpoint -q \"$MOUNTPOINT\"; then :; else\n"
            "    mount_status=$?\n"
            "    if [ \"$mount_status\" -eq 32 ]; then exit 0; fi\n"
            "  fi\n"
            "fi\n"
            "if command -v fusermount3 >/dev/null 2>&1; then\n"
            "  if fusermount3 -u \"$MOUNTPOINT\"; then exit 0; fi\n"
            "elif command -v fusermount >/dev/null 2>&1; then\n"
            "  if fusermount -u \"$MOUNTPOINT\"; then exit 0; fi\n"
            "else\n"
            "  echo 'FUSE unmount helper was not found (install fuse3 or fuse).' >&2\n"
            "  exit 1\n"
            "fi\n"
            "if command -v mountpoint >/dev/null 2>&1; then\n"
            "  if mountpoint -q \"$MOUNTPOINT\"; then :; else\n"
            "    mount_status=$?\n"
            "    if [ \"$mount_status\" -eq 32 ]; then exit 0; fi\n"
            "  fi\n"
            "fi\n"
            "exit 1"
        )
    else:
        raise InstallError("an internal wrapper name was invalid")
    path = '"$ROOT/tools:/usr/bin:/bin"' if name in {"mount.sh", "stop.sh"} else "/usr/bin:/bin"
    cache_setup = (
        "DEGOO_CACHE_DIR=$(python3 -I -B -c "
        "'import pathlib, sys; sys.path.insert(0, sys.argv[1]); import install; "
        "print(install.configured_cache_dir(pathlib.Path(sys.argv[2])))' \"$ROOT/scripts/python\" \"$ROOT\")\n"
        "export XDG_CACHE_HOME=\"$DEGOO_CACHE_DIR\"\n"
    )
    content = f"#!/bin/sh\nset -eu\nROOT={root_text}\nMOUNTPOINT={shell_quote(mountpoint)}\numask 077\nexport PYTHONDONTWRITEBYTECODE=1\nexport XDG_CONFIG_HOME=\"$ROOT/config\"\nexport TMPDIR=\"$ROOT/tmp\"\nexport PATH={path}\n{cache_setup}{command}\n"
    return content.encode("utf-8")


def compatibility_wrapper(name: str) -> bytes:
    """Keep an old root-level helper name forwarding to the scripts directory."""
    if name not in COMPATIBILITY_SHELL_FILES:
        raise InstallError("an internal compatibility wrapper name was invalid")
    content = (
        "#!/bin/sh\nset -eu\n"
        'BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\n'
        f'exec "$BASE/scripts/{name}" "$@"\n'
    )
    return content.encode("utf-8")


def unit_quote(value: str) -> str:
    if "\x00" in value or "\n" in value or "\r" in value:
        raise InstallError("the install path cannot be represented in a user service")
    escaped = value.replace("\\", "\\\\").replace('"', '\\"').replace("%", "%%").replace("$", "$$")
    return f'"{escaped}"'


def service_unit(root: Path) -> bytes:
    content = (
        "[Unit]\n"
        "Description=Private encrypted Degoo vault mount\n"
        "Wants=network-online.target\n"
        "After=network-online.target\n\n"
        "StartLimitIntervalSec=0\n\n"
        "[Service]\n"
        "Type=simple\n"
        f"ExecStart={unit_quote(str(root / 'scripts/mount.sh'))}\n"
        f"ExecStop={unit_quote(str(root / 'scripts/stop.sh'))}\n"
        "Restart=on-failure\n"
        "RestartSec=30s\n"
        "TimeoutStopSec=60s\n\n"
        "[Install]\n"
        "WantedBy=default.target\n"
    )
    return content.encode("utf-8")


def copy_binary(source_arg: str, destination: Path) -> None:
    source = Path(os.path.expanduser(source_arg))
    if not source.is_absolute():
        source = Path.cwd() / source
    source = Path(os.path.normpath(str(source)))
    check_components(source)
    try:
        before = os.lstat(source)
        if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode) or not before.st_mode & 0o111:
            raise InstallError("--binary must name a regular executable file")
        fd = os.open(source, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
    except InstallError:
        raise
    except OSError:
        raise InstallError("--binary could not be opened safely") from None
    temporary = destination.with_name(f".rclone.tmp-{os.getpid()}-{secrets.token_hex(4)}")
    out_fd: int | None = None
    try:
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino) or not stat.S_ISREG(opened.st_mode):
            raise InstallError("--binary changed while being opened")
        out_fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o700)
        with os.fdopen(fd, "rb", closefd=True) as src, os.fdopen(out_fd, "wb", closefd=True) as dst:
            fd = -1
            out_fd = None
            shutil.copyfileobj(src, dst, length=1024 * 1024)
            dst.flush()
            os.fsync(dst.fileno())
        os.chmod(temporary, 0o700)
        os.replace(temporary, destination)
    except InstallError:
        for open_fd in (fd, out_fd):
            if open_fd is not None and open_fd >= 0:
                try:
                    os.close(open_fd)
                except OSError:
                    pass
        try:
            temporary.unlink()
        except OSError:
            pass
        raise
    except OSError:
        for open_fd in (fd, out_fd):
            if open_fd is not None and open_fd >= 0:
                try:
                    os.close(open_fd)
                except OSError:
                    pass
        try:
            temporary.unlink()
        except OSError:
            pass
        raise InstallError("the rclone executable could not be installed safely") from None


def install_fuse_helper(root: Path) -> None:
    alias = root / "tools/fusermount3"
    existing = os.path.lexists(alias)
    if existing:
        try:
            info = os.lstat(alias)
            target = Path(os.readlink(alias))
        except OSError:
            raise InstallError("tools/fusermount3 is not a safe system-helper alias") from None
        if not stat.S_ISLNK(info.st_mode) or not target.is_absolute() or ".." in target.parts or target.parent not in (Path("/bin"), Path("/usr/bin")) or target.name not in FUSERMOUNT_NAMES:
            raise InstallError("tools/fusermount3 is not a safe system-helper alias")
        return
    for name in ("fusermount3", "fusermount", "fusermount2", "fusermount2.9"):
        candidate = shutil.which(name, path="/usr/bin:/bin")
        if not candidate:
            continue
        target = Path(candidate)
        if target.parent not in (Path("/bin"), Path("/usr/bin")) or target.name not in FUSERMOUNT_NAMES:
            continue
        try:
            os.symlink(str(target), alias)
        except OSError:
            raise InstallError("a compatible FUSE unmount helper could not be linked safely") from None
        return


def validate_binary(binary: Path) -> None:
    try:
        with tempfile.TemporaryDirectory(prefix="degoo-binary-check-") as temporary:
            isolated = Path(temporary)
            result = subprocess.run(
                [str(binary), "degoo", "--help"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                env={"HOME": temporary, "PATH": "/usr/bin:/bin", "LANG": "C",
                     "XDG_CONFIG_HOME": str(isolated / "config"),
                     "XDG_CACHE_HOME": str(isolated / "cache"), "RCLONE_CONFIG": os.devnull},
                timeout=30,
                check=False,
            )
    except (OSError, subprocess.TimeoutExpired):
        raise InstallError("the supplied rclone binary could not verify its Degoo command") from None
    if result.returncode != 0 or b"mount" not in result.stdout or b"login" not in result.stdout:
        raise InstallError("--binary must be an rclone build with the Degoo mount and login commands")


def _file_sha256(path: Path) -> str:
    """Hash one regular file without following links or accepting a changing file."""
    check_components(path)
    try:
        before = os.lstat(path)
        if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
            raise InstallError("an installer-managed package file is not a regular file")
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
    except InstallError:
        raise
    except OSError:
        raise InstallError("an installer-managed package file could not be read safely") from None
    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise InstallError("an installer-managed package file changed while being opened")
        digest = hashlib.sha256()
        while chunk := os.read(fd, 1024 * 1024):
            digest.update(chunk)
        after = os.fstat(fd)
        if (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise InstallError("an installer-managed package file changed while being read")
        return digest.hexdigest()
    except OSError:
        raise InstallError("an installer-managed package file could not be read safely") from None
    finally:
        os.close(fd)


def _build_commit(source_root: Path, binary_sha256: str) -> str | None:
    """Return verified clean-build provenance when the package includes it."""
    path = source_root / "docs" / "BUILD.json"
    try:
        check_components(path)
        before = os.lstat(path)
        if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode) or before.st_size > MAX_BUILD_METADATA_SIZE:
            return None
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
    except (InstallError, OSError):
        return None
    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            return None
        data = bytearray()
        while len(data) <= MAX_BUILD_METADATA_SIZE:
            chunk = os.read(fd, min(4096, MAX_BUILD_METADATA_SIZE + 1 - len(data)))
            if not chunk:
                break
            data.extend(chunk)
        after = os.fstat(fd)
        if len(data) > MAX_BUILD_METADATA_SIZE or (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            return None
    except OSError:
        return None
    finally:
        os.close(fd)
    try:
        metadata = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    if (
        not isinstance(metadata, dict)
        or type(metadata.get("version")) is not int
        or metadata["version"] != 1
        or metadata.get("binary_sha256") != binary_sha256
        or metadata.get("source_dirty") is not False
    ):
        return None
    commit = metadata.get("commit")
    return commit if isinstance(commit, str) and BUILD_COMMIT_PATTERN.fullmatch(commit) else None


def package_identity(binary: Path, source_root: Path) -> dict[str, str]:
    """Return stable hashes for an installed package payload and its binary."""
    files = {
        Path("bin/rclone"): _file_sha256(binary),
        Path("degoo-rc"): _file_sha256(source_root / "degoo-rc"),
        **{
            Path("scripts") / name: _file_sha256(source_root / "scripts" / name)
            for name in SOURCE_SHELL_FILES
        },
        **{
            Path("scripts/python") / name: _file_sha256(source_root / "scripts" / "python" / name)
            for name in SOURCE_FILES
        },
        **{
            Path("immutable-mount") / name: _file_sha256(source_root / "immutable-mount" / name)
            for name in IMMUTABLE_MOUNT_FILES
        },
    }
    payload = hashlib.sha256()
    for relative, file_digest in sorted(files.items(), key=lambda item: item[0].as_posix()):
        payload.update(relative.as_posix().encode("utf-8"))
        payload.update(b"\0")
        payload.update(bytes.fromhex(file_digest))
    binary_sha256 = files[Path("bin/rclone")]
    identity = {
        "binary_sha256": binary_sha256,
        "payload_sha256": payload.hexdigest(),
    }
    commit = _build_commit(source_root, binary_sha256)
    if commit is not None:
        identity["commit"] = commit
    return identity


def _install(args: argparse.Namespace) -> int:
    vault_password, vault_password_salt = _install_vault_password_inputs(args)
    root = expected_root()
    ensure_parent_directories(root)
    _preflight_path_link(root)
    root_exists = os.path.lexists(root)
    if args.upgrade:
        if not root_exists:
            raise InstallError("--upgrade requires an existing contained install")
        check_directory(root, private=True)
        manifest = load_manifest(root)
        _validate_upgrade_vault_state(root, manifest, args, vault_password)
        legacy_wrappers = tuple(
            name
            for name in LEGACY_GENERATED_SHELL_FILES
            if os.path.lexists(root / name)
            and read_private_file(root / name, 64 * 1024) is not None
        )
    else:
        if root_exists:
            raise InstallError("the contained install already exists; use --upgrade to preserve it")
        os.mkdir(root, 0o700)
        check_directory(root, private=True)
        manifest = None
        legacy_wrappers = ()

    if manifest is not None and getattr(args, "mountpoint", None):
        previous_mountpoint = configured_mountpoint(root, manifest)
        if manifest["mountpoint"] != args.mountpoint:
            try:
                with os.scandir(previous_mountpoint) as entries:
                    if next(entries, None) is not None:
                        raise InstallError("recover local files from the old mountpoint before changing it")
            except FileNotFoundError:
                pass
            manifest["mountpoint"] = args.mountpoint
    initial_folder_id = manifest.get("root_folder_id", "") if manifest else ""
    supplied_folder_id = validate_folder_id(args.folder_id) if args.folder_id else ""
    if initial_folder_id and supplied_folder_id and supplied_folder_id != initial_folder_id:
        raise InstallError("the requested folder ID differs from this cache's existing vault")
    folder_id = supplied_folder_id or initial_folder_id
    folder_name = validate_folder_name(args.folder_name) if args.folder_name else ""
    if not folder_name and manifest:
        folder_name = manifest.get("remote_folder_name", "")
    if folder_name and not folder_id:
        raise InstallError("--folder-name requires --folder-id")
    if args.folder_created_by_setup and not supplied_folder_id:
        raise InstallError("the setup-created folder marker requires --folder-id")

    for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs"):
        ensure_private_directory(root / name)
    ensure_private_directory(root / "scripts")
    ensure_private_directory(root / "scripts/python")
    ensure_private_directory(root / "immutable-mount")
    if manifest is None:
        manifest = {
            "version": 1,
            "root_folder_id": "",
            "remote_folder_name": "",
            "parent_folder_id": "",
            "remote_created_by_setup": False,
            "remote_name": REMOTE_NAME,
            "mountpoint": getattr(args, "mountpoint", None) or DEFAULT_MOUNTPOINT,
            "binary": "bin/rclone",
            "vault_config": "config/vault.json",
            "rclone_config": "config/rclone.conf",
            "cache_dir": "cache",
            "pidfile": "mount.pid",
            "service_links": [],
            "service_config_home": "",
        }
        data = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
        write_private_file(root / "install.json", data)
    for path in (root / "install.json", root / "config/rclone.conf", root / "config/vault.json", root / "config/mount.json", root / "config/auth.json", root / "config/login.json"):
        read_private_file(path, missing_ok=True)

    existing_rclone_config = read_private_file(root / "config/rclone.conf", missing_ok=True)
    if existing_rclone_config is not None:
        old_id = initial_folder_id
        if not old_id:
            old_id = manifest.get("root_folder_id", "") if manifest else ""
        if not old_id:
            raise InstallError("an unmanifested rclone config exists in the contained install")
        validate_rclone_config(existing_rclone_config, root, old_id)
        if supplied_folder_id and supplied_folder_id != old_id:
            raise InstallError("config/rclone.conf is bound to another folder ID")
        folder_id = old_id

    if args.vault_config and args.upgrade and read_private_file(root / "config/vault.json", missing_ok=True) is not None:
        raise InstallError("--vault-config cannot replace an existing vault secret during upgrade")
    imported_vault = import_vault_config(args.vault_config, root) if args.vault_config else None
    existing_vault = read_private_file(root / "config/vault.json", missing_ok=True)
    if existing_vault is not None:
        vault, _ = read_vault(
            existing_vault,
            root,
            imported=False,
            mountpoint=configured_mountpoint(root, manifest),
        )
        vault_bytes = existing_vault
    elif folder_id or imported_vault is not None or vault_password is not None:
        vault_bytes = create_vault_config(
            root,
            imported_vault,
            password=vault_password,
            password_salt=vault_password_salt,
        )
        vault, _ = read_vault(
            vault_bytes,
            root,
            imported=False,
            mountpoint=configured_mountpoint(root, manifest),
        )
    else:
        vault_bytes = None
        vault = None

    existing_settings = read_private_file(root / "config/mount.json", missing_ok=True)
    if existing_settings is not None:
        validate_mount_settings(existing_settings)
        settings_bytes = existing_settings
    else:
        settings_bytes = (json.dumps(DEFAULT_MOUNT_SETTINGS, indent=2, sort_keys=True) + "\n").encode("utf-8")

    if args.upgrade:
        normalize_legacy_logs(root / "logs")
    copy_binary(args.binary, root / "bin/rclone")
    validate_binary(root / "bin/rclone")
    source_root = Path(__file__).absolute().parent.parent.parent
    for source_name in IMMUTABLE_MOUNT_FILES:
        source = source_root / "immutable-mount" / source_name
        try:
            source_info = os.lstat(source)
        except OSError:
            raise InstallError("the immutable mount helper sources are missing") from None
        if not stat.S_ISREG(source_info.st_mode) or stat.S_ISLNK(source_info.st_mode):
            raise InstallError("the immutable mount helper sources are unsafe")
        destination = root / "immutable-mount" / source_name
        write_private_file(destination, source.read_bytes(), replace=os.path.lexists(destination))
    for source_name in SOURCE_FILES:
        source = Path(__file__).absolute().parent / source_name
        try:
            source_info = os.lstat(source)
        except OSError:
            raise InstallError("the contained installer source files are incomplete") from None
        if not stat.S_ISREG(source_info.st_mode) or stat.S_ISLNK(source_info.st_mode):
            raise InstallError("the contained installer source files are incomplete")
        destination = root / "scripts/python" / source_name
        data = source.read_bytes()
        write_private_file(destination, data, replace=os.path.lexists(destination), mode=0o700)

    entrypoint_source = source_root / "degoo-rc"
    try:
        entrypoint_info = os.lstat(entrypoint_source)
    except OSError:
        raise InstallError("the unified Degoo command source is missing") from None
    if not stat.S_ISREG(entrypoint_info.st_mode) or stat.S_ISLNK(entrypoint_info.st_mode):
        raise InstallError("the unified Degoo command source is unsafe")
    write_private_file(root / "degoo-rc", entrypoint_source.read_bytes(), replace=os.path.lexists(root / "degoo-rc"), mode=0o700)

    for source_name in SOURCE_SHELL_FILES:
        source = source_root / "scripts" / source_name
        try:
            source_info = os.lstat(source)
        except OSError:
            raise InstallError("the contained shell helper source files are incomplete") from None
        if not stat.S_ISREG(source_info.st_mode) or stat.S_ISLNK(source_info.st_mode):
            raise InstallError("the contained shell helper source files are unsafe")
        destination = root / "scripts" / source_name
        write_private_file(destination, source.read_bytes(), replace=os.path.lexists(destination), mode=0o700)

    if existing_vault is None and vault_bytes is not None:
        write_private_file(root / "config/vault.json", vault_bytes)
    if existing_settings is None:
        write_private_file(root / "config/mount.json", settings_bytes)
    if folder_id:
        if existing_rclone_config is None:
            write_private_file(root / "config/rclone.conf", rclone_config(root, folder_id))
        else:
            validate_rclone_config(existing_rclone_config, root, folder_id)

    for wrapper in GENERATED_SHELL_FILES:
        destination = root / "scripts" / wrapper
        write_private_file(destination, shell_wrapper(wrapper, root, mountpoint=configured_mountpoint(root, manifest)), replace=os.path.lexists(destination), mode=0o700)
    for wrapper in legacy_wrappers:
        destination = root / wrapper
        write_private_file(destination, compatibility_wrapper(wrapper), replace=os.path.lexists(destination), mode=0o700)
    if args.upgrade:
        for source_name in SOURCE_FILES:
            legacy = root / source_name
            if read_private_file(legacy, missing_ok=True) is not None:
                legacy.unlink()
    write_private_file(root / "rclone-degoo.service", service_unit(root), replace=os.path.lexists(root / "rclone-degoo.service"), mode=0o600)
    install_fuse_helper(root)

    if supplied_folder_id:
        manifest["root_folder_id"] = folder_id
    if folder_name:
        manifest["remote_folder_name"] = folder_name
    if args.parent_folder_id:
        manifest["parent_folder_id"] = args.parent_folder_id
    if args.folder_created_by_setup:
        manifest["remote_created_by_setup"] = True
    manifest.setdefault("service_links", [])
    source_identity = package_identity(Path(args.binary), source_root)
    installed_identity = package_identity(root / "bin/rclone", root)
    if (
        source_identity["binary_sha256"] != installed_identity["binary_sha256"]
        or source_identity["payload_sha256"] != installed_identity["payload_sha256"]
    ):
        raise InstallError("the installed package payload does not match its staged source")
    manifest["build"] = {"version": 1, **installed_identity}
    if "commit" in source_identity:
        manifest["build"]["commit"] = source_identity["commit"]
    package_sha256 = getattr(args, "package_sha256", None)
    if package_sha256 is not None:
        manifest["build"]["package_sha256"] = package_sha256
    path_link_created = _install_path_link(root, manifest)
    manifest_data = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
    try:
        write_private_file(root / "install.json", manifest_data, replace=True)
    except (InstallError, OSError):
        if path_link_created and _path_link_matches(expected_path_link(), root):
            expected_path_link().unlink()
        raise

    _print_path_hint()

    if folder_id:
        print(f"Installed the contained Degoo vault at {root}.")
        print("Unified command: degoo-rc")
        print("Account login: degoo-rc login")
        print("Mount: degoo-rc mount")
        print("Logs: degoo-rc logs")
        print("User service: degoo-rc service enable")
    else:
        print(f"Installed the contained Degoo client at {root}; no vault folder is configured yet.")
        print("Unified command: degoo-rc")
        print("Log in with degoo-rc login, then run degoo-rc setup --parent-id DEVICE_ID --name VAULT_NAME.")
        print("Logs: degoo-rc logs")
    return 0


def _binary_source(path_arg: str) -> Path:
    source = Path(os.path.expanduser(path_arg))
    if not source.is_absolute():
        source = Path.cwd() / source
    source = Path(os.path.normpath(str(source)))
    check_components(source)
    try:
        info = os.lstat(source)
    except OSError:
        raise InstallError("--binary could not be inspected safely") from None
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or not info.st_mode & 0o111 or info.st_mode & 0o6000:
        raise InstallError("--binary must name a regular executable file")
    return source


def _user_service_active(root: Path) -> bool:
    """Report whether this install's user service is active or starting."""
    try:
        import info
    except ImportError:
        return False
    state = info.service_status(root, {})
    return state.rsplit(";", 1)[-1].strip() in {"active", "activating", "deactivating", "reloading"}


def _upgrade_lock_path(root: Path, *, cache_dir: Path | None = None) -> Path:
    digest = hashlib.sha256(f"{REMOTE_NAME}:".encode("utf-8")).hexdigest()[:24]
    cache = configured_cache_dir(root) if cache_dir is None else cache_dir
    return cache / f"degoo-vault-{digest}.lock"


def _check_upgrade_mount_stopped(root: Path) -> None:
    if os.path.ismount(root) or os.path.ismount(configured_mountpoint(root)):
        if _user_service_active(root):
            stop_command = f"{shlex.quote(str(root / 'scripts/service.sh'))} stop"
        else:
            stop_command = shlex.quote(str(root / "scripts/stop.sh"))
        raise InstallError(f"stop the contained mount before upgrading; run {stop_command}")


def _require_empty_old_mountpoint(path: Path) -> None:
    """Refuse a mountpoint migration that could hide local files or a mounted tree."""
    check_components(path, missing_ok=True)
    if not os.path.lexists(path):
        return
    try:
        info = os.lstat(path)
    except OSError:
        raise InstallError("the old mountpoint could not be inspected safely") from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise InstallError("the old mountpoint must be a real directory before it can be changed")
    if os.path.ismount(path):
        raise InstallError("stop the contained mount before changing its mountpoint")
    try:
        with os.scandir(path) as entries:
            if next(entries, None) is not None:
                raise InstallError("recover local files from the old mountpoint before changing it")
    except OSError:
        raise InstallError("the old mountpoint contents could not be inspected safely") from None


def _acquire_upgrade_lock(root: Path, *, mountpoint: Path | None = None) -> int:
    _check_upgrade_mount_stopped(root)
    cache = configured_cache_dir(root, mountpoint=mountpoint)
    ensure_private_directory(cache)
    path = _upgrade_lock_path(root, cache_dir=cache)
    flags = os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags, 0o600)
    except OSError:
        raise InstallError("the vault cache lock could not be opened safely") from None
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        raise InstallError("the vault cache lock has unsafe ownership or permissions")
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        os.close(fd)
        if exc.errno in (errno.EACCES, errno.EAGAIN):
            raise InstallError("the Degoo cache is active; stop the mount before upgrading") from None
        raise InstallError("the vault cache lock could not be acquired") from None
    return fd


def _validate_upgrade_lock_fd(root: Path, lock_fd: int, *, mountpoint: Path | None = None) -> None:
    """Validate and lock the inherited descriptor for this install's cache."""
    _check_upgrade_mount_stopped(root)
    if type(lock_fd) is not int or lock_fd < 0:
        raise InstallError("the inherited cache-lock descriptor is invalid")
    cache = configured_cache_dir(root, mountpoint=mountpoint)
    check_directory(cache, private=True)
    path = _upgrade_lock_path(root, cache_dir=cache)
    try:
        path_info = os.lstat(path)
        fd_info = os.fstat(lock_fd)
    except OSError:
        raise InstallError("the inherited cache-lock descriptor could not be verified") from None
    for info in (path_info, fd_info):
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid()
            or info.st_mode & 0o077
        ):
            raise InstallError("the inherited cache-lock file has unsafe ownership or permissions")
    if (path_info.st_dev, path_info.st_ino) != (fd_info.st_dev, fd_info.st_ino):
        raise InstallError("the inherited descriptor does not refer to this install's cache lock")
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        if exc.errno in (errno.EACCES, errno.EAGAIN):
            raise InstallError("the Degoo cache is active; stop the mount before upgrading") from None
        raise InstallError("the inherited cache-lock descriptor could not be locked") from None


def install(args: argparse.Namespace) -> int:
    _install_vault_password_inputs(args)
    upgrade_lock_fd = getattr(args, "upgrade_lock_fd", None)
    if upgrade_lock_fd is not None and not args.upgrade:
        raise InstallError("--upgrade-lock-fd can only be used with --upgrade")
    package_sha256 = getattr(args, "package_sha256", None)
    if package_sha256 is not None and (not isinstance(package_sha256, str) or not SHA256_PATTERN.fullmatch(package_sha256)):
        raise InstallError("--package-sha256 must be a lowercase SHA-256 digest")
    supplied_folder_id = validate_folder_id(args.folder_id) if args.folder_id else ""
    folder_name = validate_folder_name(args.folder_name) if args.folder_name else ""
    supplied_parent_folder_id = getattr(args, "parent_folder_id", None)
    parent_folder_id = validate_folder_id(supplied_parent_folder_id) if supplied_parent_folder_id else ""
    if folder_name and not supplied_folder_id and not args.upgrade:
        raise InstallError("--folder-name requires --folder-id")
    if args.folder_created_by_setup and not supplied_folder_id:
        raise InstallError("the setup-created folder marker requires --folder-id")
    if parent_folder_id and not args.folder_created_by_setup:
        raise InstallError("the parent folder ID can only be recorded for a setup-created folder")
    if parent_folder_id and not supplied_folder_id:
        raise InstallError("--parent-folder-id requires --folder-id")
    args.parent_folder_id = parent_folder_id
    args.binary = str(_binary_source(args.binary))
    validate_binary(Path(args.binary))
    preflight_install_safety()
    root = expected_root()
    supplied_mountpoint = getattr(args, "mountpoint", None)
    normalized_mountpoint = validate_mountpoint(supplied_mountpoint, root) if supplied_mountpoint is not None else None
    existing_manifest = load_manifest(root) if args.upgrade else None
    selected_mountpoint = normalized_mountpoint or (
        existing_manifest["mountpoint"] if existing_manifest else DEFAULT_MOUNTPOINT
    )
    selected_mountpoint = validate_mountpoint(selected_mountpoint, root)
    args.mountpoint = selected_mountpoint
    selected_path = root / "mount" if selected_mountpoint == "mount" else Path(selected_mountpoint)
    if not args.upgrade:
        if os.path.lexists(root):
            raise InstallError("the contained install already exists; use --upgrade to preserve it")
        configured_cache_dir(root, mountpoint=selected_path)
        if selected_mountpoint != "mount":
            prepare_mountpoint(selected_path)
        return _install(args)
    if not os.path.lexists(root):
        raise InstallError("--upgrade requires an existing contained install")
    check_directory(root, private=True)
    existing_manifest = load_manifest(root)
    vault_password, _ = _install_vault_password_inputs(args)
    _validate_upgrade_vault_state(root, existing_manifest, args, vault_password)
    if upgrade_lock_fd is None:
        lock = _acquire_upgrade_lock(root, mountpoint=selected_path)
    else:
        _validate_upgrade_lock_fd(root, upgrade_lock_fd, mountpoint=selected_path)
        lock = None
    try:
        previous_mountpoint = configured_mountpoint(root, existing_manifest)
        if selected_path != previous_mountpoint:
            if _paths_overlap(selected_path, previous_mountpoint):
                raise InstallError("the new mountpoint cannot overlap the current mountpoint")
            _require_empty_old_mountpoint(previous_mountpoint)
        if selected_mountpoint != "mount":
            prepare_mountpoint(selected_path)
        return _install(args)
    finally:
        if lock is not None:
            os.close(lock)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", help="path to a Linux rclone executable built with the Degoo command")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--wizard", action="store_true", help="ask for installation and vault settings step by step")
    mode.add_argument("--no-wizard", action="store_true", help="install without interactive setup (requires --binary)")
    parser.add_argument(
        "--mountpoint",
        help="absolute owner-private mount directory; new installs default to /mnt/degoo, upgrades keep the saved path, and mount selects the legacy contained directory",
    )
    parser.add_argument("--folder-id", help="ID of an existing dedicated Degoo folder (account root 0 is refused)")
    parser.add_argument("--folder-name", help="optional human-readable name for the dedicated folder")
    parser.add_argument("--vault-config", help="private existing vault configuration to import on another machine")
    parser.add_argument("--restore-directory", help="private retained backup directory to restore through the interactive wizard")
    parser.add_argument("--upgrade", action="store_true", help="update the contained client while preserving its vault, login, settings, and cache")
    parser.add_argument("--upgrade-lock-fd", type=int, help=argparse.SUPPRESS)
    parser.add_argument("--package-sha256", help=argparse.SUPPRESS)
    parser.add_argument("--folder-created-by-setup", action="store_true", help=argparse.SUPPRESS)
    parser.add_argument("--parent-folder-id", help=argparse.SUPPRESS)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    arguments = sys.argv[1:] if argv is None else argv
    args = parser.parse_args(arguments)
    try:
        if args.wizard or (not arguments and sys.stdin.isatty()):
            import wizard
            return wizard.run(args)
        if args.restore_directory:
            parser.error("--restore-directory requires --wizard")
        if not args.binary:
            parser.error("--binary is required for noninteractive installation; use --wizard for guided setup")
        return install(args)
    except InstallError as exc:
        print(f"install refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("install refused: filesystem access failed safely", file=sys.stderr)
        return 2
    except (EOFError, KeyboardInterrupt):
        print("Installation cancelled; any completed setup is preserved.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
