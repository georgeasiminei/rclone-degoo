#!/usr/bin/env python3
"""Read-only checks and manual setup commands for Degoo FUSE mounts."""

from __future__ import annotations

import errno
import fcntl
import os
import shlex
import shutil
import stat
import struct
from pathlib import Path


FS_IOC_GETFLAGS = (2 << 30) | (struct.calcsize("l") << 16) | (ord("f") << 8) | 1
FS_IMMUTABLE_FL = 0x00000010
ST_NOSUID = getattr(os, "ST_NOSUID", 2)
ST_NOEXEC = getattr(os, "ST_NOEXEC", 8)

IMMUTABLE_MOUNTPOINT = Path("/mnt/degoo")
IMMUTABLE_HELPER = Path("/usr/local/libexec/rclone-degoo/fusermount3")
HELPER_DIRECTORY = IMMUTABLE_HELPER.parent

_UNSUPPORTED_FLAG_IOCTL = {errno.ENOTTY, errno.EOPNOTSUPP}


def _absolute(path: Path) -> Path:
    """Return a normalized absolute path without following symlinks."""
    return Path(os.path.abspath(os.path.normpath(os.fspath(path))))


def _lstat(path: Path) -> os.stat_result:
    """Inspect a path component without following a final symlink."""
    return os.lstat(path)


def _ioctl_get_flags(fd: int) -> int:
    """Read Linux inode flags from an already-open descriptor."""
    buffer = bytearray(struct.pack("I", 0))
    fcntl.ioctl(fd, FS_IOC_GETFLAGS, buffer, True)
    return struct.unpack("I", buffer)[0]


def _helper_problem(path: Path) -> str | None:
    """Return why the installed immutable-mount helper cannot be trusted."""
    helper = _absolute(path)
    current = helper
    while True:
        try:
            info = _lstat(current)
        except FileNotFoundError:
            return f"{current} is missing"
        except OSError as exc:
            return f"{current} cannot be inspected ({exc.strerror or exc})"

        if stat.S_ISLNK(info.st_mode):
            return f"{current} is a symlink"
        if info.st_uid != 0:
            return f"{current} is not root-owned"
        if info.st_mode & 0o022:
            return f"{current} is writable by group or other users"
        if current == helper:
            if not stat.S_ISREG(info.st_mode):
                return f"{current} is not a regular file"
            if not (info.st_mode & stat.S_ISUID):
                return f"{current} is missing its setuid bit"
            if info.st_mode & 0o111 != 0o111:
                return f"{current} is not executable by owner, group, and other users"
        elif not stat.S_ISDIR(info.st_mode):
            return f"{current} is not a directory"

        parent = current.parent
        if parent == current:
            break
        current = parent

    try:
        filesystem = os.statvfs(helper)
    except OSError as exc:
        return f"the filesystem containing {helper} cannot be inspected ({exc.strerror or exc})"
    if filesystem.f_flag & ST_NOSUID:
        return f"the filesystem containing {helper} is mounted nosuid, so setuid is disabled"
    if filesystem.f_flag & ST_NOEXEC:
        return f"the filesystem containing {helper} is mounted noexec, so helper execution is disabled"
    return None


def _mountpoint_setup(path: Path) -> str:
    """Describe a missing target; the wizard prints its setup command block."""
    return f"mountpoint missing: {path} does not exist; create it as an owner-private directory before mounting"


def _helper_source_available(path: Path) -> bool:
    """Return whether the packaged helper source is a readable regular file."""
    try:
        info = _lstat(path)
    except OSError:
        return False
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        return False
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        return False
    try:
        return stat.S_ISREG(os.fstat(fd).st_mode)
    except OSError:
        return False
    finally:
        os.close(fd)


def _directory_issues(path: Path, info: os.stat_result, fd: int, *, prefix: str) -> list[str]:
    """Check ownership, access, and emptiness required before mounting."""
    issues: list[str] = []
    expected_owner = os.getuid()
    if info.st_uid != expected_owner:
        issues.append(f"{prefix}: {path} must be owned by the current user (uid {expected_owner})")
    if info.st_mode & 0o500 != 0o500:
        issues.append(f"{prefix}: {path} needs owner read and execute permission")
    try:
        names = os.listdir(fd)
    except OSError as exc:
        issues.append(f"{prefix}: {path} contents cannot be inspected ({exc.strerror or exc})")
    else:
        if names:
            issues.append(
                f"{prefix}: {path} must be empty; it contains ordinary local files that are not queued for upload; "
                "move them to a safe location before mounting"
            )
    return issues


def _immutable_target_issues(path: Path, info: os.stat_result, fd: int) -> list[str]:
    """Check the immutable helper's fixed target and its directory contract."""
    issues: list[str] = []
    target = _absolute(path)
    expected = _absolute(IMMUTABLE_MOUNTPOINT)
    if target != expected:
        issues.append(
            "immutable mountpoint unsupported: the administrator helper only supports /mnt/degoo; "
            "migrate the contained install with `degoo-rc install --mountpoint /mnt/degoo` while preserving its cache"
        )
    elif target == Path("/mnt/degoo"):
        parent = Path("/mnt")
        try:
            parent_info = _lstat(parent)
        except OSError as exc:
            issues.append(f"immutable mountpoint unsafe: /mnt cannot be inspected ({exc.strerror or exc})")
        else:
            if (
                stat.S_ISLNK(parent_info.st_mode)
                or not stat.S_ISDIR(parent_info.st_mode)
                or parent_info.st_uid != 0
                or parent_info.st_mode & 0o022
            ):
                issues.append(
                    "immutable mountpoint unsafe: /mnt must be a real root-owned directory without group/world write access"
                )

    issues.extend(_directory_issues(path, info, fd, prefix="immutable mountpoint unsafe"))
    if info.st_mode & 0o022:
        issues.append(f"immutable mountpoint unsafe: {path} must not have group/world write permission")
    return issues


def check(root: Path, mountpoint: Path) -> list[str]:
    """Return blocking mount prerequisites; an empty list means the path is ready."""
    target = _absolute(mountpoint)
    try:
        path_info = _lstat(target)
    except FileNotFoundError:
        return [_mountpoint_setup(target)]
    except OSError as exc:
        return [f"mountpoint unsafe: {target} cannot be inspected ({exc.strerror or exc})"]

    if stat.S_ISLNK(path_info.st_mode):
        return [f"mountpoint unsafe: {target} is a symlink; use a real directory as the mountpoint"]
    if not stat.S_ISDIR(path_info.st_mode):
        return [f"mountpoint unsafe: {target} is not a directory"]

    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(target, flags)
    except FileNotFoundError:
        return [_mountpoint_setup(target)]
    except OSError as exc:
        return [f"mountpoint unsafe: {target} cannot be opened safely ({exc.strerror or exc})"]

    try:
        info = os.fstat(fd)
        if not stat.S_ISDIR(info.st_mode):
            return [f"mountpoint unsafe: {target} is not a directory"]
        try:
            inode_flags = _ioctl_get_flags(fd)
        except OSError as exc:
            if exc.errno in _UNSUPPORTED_FLAG_IOCTL:
                return _directory_issues(target, info, fd, prefix="mountpoint unsafe")
            reason = exc.strerror or exc
            return [f"mountpoint unsafe: the immutable flag on {target} cannot be inspected ({reason})"]

        if inode_flags & FS_IMMUTABLE_FL == 0:
            return _directory_issues(target, info, fd, prefix="mountpoint unsafe")

        issues = _immutable_target_issues(target, info, fd)
        helper_problem = _helper_problem(IMMUTABLE_HELPER)
        if helper_problem is not None:
            source = Path(root) / "immutable-mount" / "helper.c"
            readme = Path(root) / "immutable-mount" / "README.md"
            issues.append(
                f"immutable mount helper invalid: {IMMUTABLE_HELPER}: {helper_problem}. "
                f"Build and install it manually using {readme}"
            )
            if not _helper_source_available(source):
                issues.append(
                    f"helper source missing: {source} is unavailable or unreadable; re-extract the complete archive, "
                    "change into its extracted rclone-degoo directory, and run `./degoo-rc install --upgrade` "
                    "to restore it while preserving the configuration and cache"
                )
            if shutil.which("gcc") is None:
                issues.append(f"GCC missing: install a C compiler to build {source}; packages do not install the privileged helper")
        return issues
    finally:
        os.close(fd)


def setup_commands(root: Path, *, start_service: bool = True) -> list[str]:
    """Return a copyable shell block to build and manually install the helper."""
    source = shlex.quote(str(Path(root) / "immutable-mount" / "helper.c"))
    commands = [
        "(",
        "set -eu",
        "command -v gcc >/dev/null 2>&1 || { printf '%s\\n' 'Install GCC using your distribution package manager, then retry.' >&2; exit 1; }",
        f"source={source}",
        "test -f \"$source\" || { printf '%s\\n' \"Helper source is missing: $source\" >&2; exit 1; }",
        'build_dir=$(mktemp -d "${TMPDIR:-/tmp}/rclone-degoo-fusermount3.XXXXXXXX")',
        "trap 'rm -rf -- \"$build_dir\"' EXIT",
        "trap 'exit 1' HUP INT TERM",
        "gcc -O2 -D_FORTIFY_SOURCE=2 -fstack-protector-strong -fPIE -pie \\",
        "  -Wl,-z,relro,-z,now -Wall -Wextra -Werror -std=c11 \\",
        '  -o "$build_dir/fusermount3" "$source"',
        f"sudo install -o root -g root -m 0755 -d -- {shlex.quote(str(HELPER_DIRECTORY))}",
        f"sudo install -o root -g root -m 4755 -- \"$build_dir/fusermount3\" {shlex.quote(str(IMMUTABLE_HELPER))}",
        f"stat -c '%U:%G %a %n' -- {shlex.quote(str(HELPER_DIRECTORY))} {shlex.quote(str(IMMUTABLE_HELPER))}",
        f"if command -v namei >/dev/null 2>&1; then namei -l {shlex.quote(str(IMMUTABLE_HELPER))}; fi",
    ]
    if start_service:
        launcher = shlex.quote(str(Path(root) / "degoo-rc"))
        commands.extend((f"{launcher} service enable", f"{launcher} info"))
    commands.append(")")
    return ["\n".join(commands)]
