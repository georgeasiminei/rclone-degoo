#!/usr/bin/env python3
"""Safely inventory or remove the isolated local Degoo vault installation."""

from __future__ import annotations

import argparse
import errno
import fcntl
import hashlib
import json
import os
import shlex
import re
import shutil
import stat
import struct
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

import install as installer


REMOTE_NAME = "degoo-live-test"
ROOT_NAME = "rclone-degoo"
MAX_MANIFEST_SIZE = 64 * 1024
MAX_VAULT_SIZE = 1024 * 1024
MAX_STATUS_SIZE = 4096
MAX_METADATA_CACHE_FILE_SIZE = (256 << 10) + 1024
MAX_METADATA_CACHE_BYTES = 64 << 20
MAX_METADATA_CACHE_ENTRIES = 8192
MAX_VFS_METADATA_SIZE = 64 << 10
STATUS_PATTERN = re.compile(r"Pending local uploads: ([0-9]+) files, ([0-9]+) bytes")
METADATA_CACHE_FILE = re.compile(r"[0-9a-f]{64}\.cache\Z")
EXPECTED_MANIFEST_PATHS = {
    "binary": "bin/rclone",
    "vault_config": "config/vault.json",
    "rclone_config": "config/rclone.conf",
    "cache_dir": "cache",
    "pidfile": "mount.pid",
}
FUSERMOUNT_NAMES = {"fusermount", "fusermount2", "fusermount2.9", "fusermount3"}
FS_IMMUTABLE_FL = 0x00000010
FS_IOC_GETFLAGS = (2 << 30) | (struct.calcsize("@L") << 16) | (ord("f") << 8) | 1


class CleanupError(Exception):
    """A safe, user-facing cleanup refusal without secret-bearing details."""


def home_directory() -> Path:
    """Return the current user's home directory without resolving symlinks."""
    home = Path(os.path.expanduser("~"))
    if not home.is_absolute():
        raise CleanupError("could not determine an absolute home directory")
    return Path(os.path.normpath(str(home)))


def expected_root() -> Path:
    """Return the single supported isolated install location."""
    return home_directory() / ".local" / "share" / ROOT_NAME


def expected_path_link() -> Path:
    """Return the only supported user-level command link location."""
    return home_directory() / ".local" / "bin" / "degoo-rc"


def _check_components(path: Path, *, missing_ok: bool = False) -> None:
    """Reject symlink components and non-directory path ancestors."""
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current = current / part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            if missing_ok:
                return
            raise CleanupError("a required path is missing") from None
        if stat.S_ISLNK(info.st_mode):
            raise CleanupError("symlink path components are not allowed")
        if current != path and not stat.S_ISDIR(info.st_mode):
            raise CleanupError("a path ancestor is not a directory")


def _check_directory(path: Path, *, private: bool = False) -> os.stat_result:
    """Validate an owned directory and return its lstat result."""
    _check_components(path)
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise CleanupError("a required install directory is unsafe")
    if info.st_uid != os.getuid():
        raise CleanupError("an install directory is not owned by the current user")
    if info.st_mode & 0o022:
        raise CleanupError("an install directory is writable by another user")
    if private and info.st_mode & 0o077:
        raise CleanupError("a private install directory has unsafe permissions")
    return info


def _read_private_file(path: Path, maximum: int, *, missing_ok: bool = False) -> bytes:
    """Read a bounded owner-private regular file without following symlinks."""
    _check_components(path, missing_ok=missing_ok)
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        if missing_ok:
            raise
        raise CleanupError("a required private file is missing") from None
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise CleanupError("a required private file is not a regular file")
    if before.st_uid != os.getuid() or before.st_mode & 0o077:
        raise CleanupError("a private file has unsafe ownership or permissions")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise CleanupError("a private file could not be opened safely") from None
    try:
        opened = os.fstat(fd)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("a private file changed while being opened")
        if opened.st_uid != os.getuid() or opened.st_mode & 0o077:
            raise CleanupError("a private file has unsafe ownership or permissions")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining > 0:
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
            raise CleanupError("a private file changed while being read")
    finally:
        os.close(fd)
    data = b"".join(chunks)
    if len(data) > maximum:
        raise CleanupError("a private file is larger than the supported limit")
    return data


def _decode_json(data: bytes) -> dict[str, Any]:
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise CleanupError("a private configuration file is invalid") from None
    if not isinstance(value, dict):
        raise CleanupError("a private configuration file is invalid")
    return value


def _validate_manifest(root: Path) -> dict[str, Any]:
    manifest = _decode_json(_read_private_file(root / "install.json", MAX_MANIFEST_SIZE))
    if type(manifest.get("version")) is not int or manifest["version"] != 1:
        raise CleanupError("the install manifest version is unsupported")
    if manifest.get("remote_name") != REMOTE_NAME:
        raise CleanupError("the install manifest does not identify this isolated vault")
    created = manifest.get("remote_created_by_setup")
    folder_id = manifest.get("root_folder_id")
    folder_name = manifest.get("remote_folder_name")
    if not isinstance(created, bool) or not isinstance(folder_id, str) or not isinstance(folder_name, str):
        raise CleanupError("the install manifest is incomplete")
    if folder_id == "0" or (created and not folder_id.strip()):
        raise CleanupError("the install manifest has an unsafe remote-root identity")
    for field, expected in EXPECTED_MANIFEST_PATHS.items():
        if manifest.get(field) != expected:
            raise CleanupError("the install manifest contains an unexpected local path")
    try:
        installer.configured_mountpoint(root, manifest)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    if created and not folder_name.strip():
        raise CleanupError("the install manifest is incomplete")
    service_config_home = manifest.get("service_config_home", "")
    service_links = manifest.get("service_links", [])
    path_link = manifest.get("path_link", "")
    if not isinstance(path_link, str) or (path_link and path_link != str(expected_path_link())):
        raise CleanupError("the install manifest contains an unexpected PATH link")
    if not isinstance(service_config_home, str) or not isinstance(service_links, list):
        raise CleanupError("the install manifest has an invalid service ownership record")
    if service_config_home:
        config_home = Path(service_config_home)
        if not config_home.is_absolute() or ".." in config_home.parts:
            raise CleanupError("the install manifest has an unsafe service config path")
        config_home = Path(os.path.normpath(str(config_home)))
        try:
            if os.path.commonpath((str(root), str(config_home))) == str(root):
                raise CleanupError("the service config path must be outside the install")
        except ValueError:
            raise CleanupError("the install manifest has an unsafe service config path") from None
    else:
        config_home = Path(os.environ.get("XDG_CONFIG_HOME", str(home_directory() / ".config")))
        if not config_home.is_absolute():
            raise CleanupError("the user service config path is invalid")
        config_home = Path(os.path.normpath(str(config_home)))
    unit_dir = config_home / "systemd" / "user"
    allowed_service_links = {
        str(unit_dir / "rclone-degoo.service"),
        str(unit_dir / "default.target.wants/rclone-degoo.service"),
    }
    if any(not isinstance(item, str) or item not in allowed_service_links for item in service_links) or len(set(service_links)) != len(service_links):
        raise CleanupError("the install manifest contains an unexpected service link path")
    return manifest


def _validate_layout(root: Path) -> dict[str, Any]:
    """Check the fixed install layout, private configs, and manifest."""
    _check_directory(root, private=True)
    for name in ("bin", "config", "mount", "tools", "tmp", "logs"):
        _check_directory(root / name)

    manifest = _validate_manifest(root)
    for rel, maximum in (("config/vault.json", MAX_VAULT_SIZE), ("config/auth.json", MAX_MANIFEST_SIZE), ("config/rclone.conf", MAX_MANIFEST_SIZE), ("config/mount.json", MAX_MANIFEST_SIZE), ("config/login.json", MAX_MANIFEST_SIZE)):
        path = root / rel
        try:
            _read_private_file(path, maximum, missing_ok=True)
        except FileNotFoundError:
            if rel == "config/vault.json":
                continue
            if rel == "config/rclone.conf":
                continue
            # Login state may be absent after logout.
            if rel == "config/auth.json":
                continue
            if rel in ("config/mount.json", "config/login.json"):
                continue
            raise

    vault_path = root / "config/vault.json"
    try:
        vault_bytes = _read_private_file(vault_path, MAX_VAULT_SIZE, missing_ok=True)
    except FileNotFoundError:
        vault_bytes = None
    vault: dict[str, Any] | None = _decode_json(vault_bytes) if vault_bytes is not None else None
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    contained_cache = root / "cache"
    if cache_dir == contained_cache or os.path.lexists(contained_cache):
        _check_directory(contained_cache)
    if vault is not None:
        if type(vault.get("version")) is not int or vault["version"] != 1 or vault.get("remote") != f"{REMOTE_NAME}:":
            raise CleanupError("the vault configuration does not match this install")
        if not isinstance(vault.get("password"), str) or not vault["password"]:
            raise CleanupError("the vault configuration is incomplete")
    if os.path.lexists(cache_dir):
        _check_directory(cache_dir, private=True)
        if cache_dir != root / "cache":
            _cache_mountpoints(cache_dir)

    try:
        mount_settings_bytes = _read_private_file(root / "config/mount.json", MAX_MANIFEST_SIZE, missing_ok=True)
    except FileNotFoundError:
        mount_settings_bytes = None
    if mount_settings_bytes is not None:
        try:
            installer.validate_mount_settings(mount_settings_bytes)
        except installer.InstallError:
            raise CleanupError("the mount settings are invalid")

    rclone_config = root / "config/rclone.conf"
    try:
        _read_private_file(rclone_config, MAX_MANIFEST_SIZE, missing_ok=True)
        has_rclone_config = True
    except FileNotFoundError:
        has_rclone_config = False
    binary = root / "bin/rclone"
    if binary.exists():
        _check_regular_executable(binary)
    elif has_rclone_config:
        raise CleanupError("the isolated rclone executable is missing")

    return {"manifest": manifest, "vault_bytes": vault_bytes, "vault": vault, "cache_dir": cache_dir}


def _check_regular_executable(path: Path) -> os.stat_result:
    _check_components(path)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise CleanupError("the isolated rclone executable is missing") from None
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise CleanupError("the isolated rclone executable is unsafe")
    if info.st_uid != os.getuid() or info.st_mode & 0o022 or info.st_mode & 0o6000 or not info.st_mode & 0o100:
        raise CleanupError("the isolated rclone executable has unsafe ownership or permissions")
    return info


def _validate_backup(path_arg: str, root: Path, vault_bytes: bytes | None) -> None:
    path = Path(path_arg)
    if not path.is_absolute() or ".." in path.parts:
        raise CleanupError("the vault backup path must be absolute and contain no parent traversal")
    path = Path(os.path.normpath(str(path)))
    if vault_bytes is None:
        raise CleanupError("there is no local vault secret to verify; use the explicit discard option")
    try:
        if os.path.commonpath((str(root), str(path))) == str(root):
            raise CleanupError("the vault backup must be outside the install root")
    except ValueError:
        raise CleanupError("the vault backup path is invalid") from None
    _check_components(path)
    _check_directory(path.parent)
    parent_info = os.lstat(path.parent)
    if parent_info.st_uid != os.getuid() or parent_info.st_mode & 0o022:
        raise CleanupError("the vault backup directory must be private to the current user")
    backup_bytes = _read_private_file(path, MAX_VAULT_SIZE)
    backup_vault = _decode_json(backup_bytes)
    installed_vault = _decode_json(vault_bytes)
    backup_vault.pop("account_credentials", None)
    installed_vault.pop("account_credentials", None)
    backup_settings = json.dumps(backup_vault, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
    installed_settings = json.dumps(installed_vault, ensure_ascii=True, sort_keys=True, separators=(",", ":"))
    if backup_settings != installed_settings:
        raise CleanupError("the vault backup does not match the installed vault configuration")
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    if cache_dir == path.parent or cache_dir in path.parents:
        raise CleanupError("the vault backup must be outside the configured cache")


def _default_retention_directory() -> Path:
    """Return a new timestamped private retention path beside the install."""
    parent = expected_root().parent
    timestamp = time.strftime("%Y%m%d-%H%M%S", time.localtime())
    base = parent / f"rclone-degoo-backup-{timestamp}"
    candidate = base
    suffix = 1
    while os.path.lexists(candidate):
        candidate = base.with_name(f"{base.name}-{suffix}")
        suffix += 1
    return candidate


def _validate_retention_directory(path_arg: str | Path, root: Path) -> Path:
    """Validate a new external retention directory without creating it."""
    path = Path(path_arg)
    if not path.is_absolute() or ".." in path.parts:
        raise CleanupError("the retention directory must be absolute and contain no parent traversal")
    path = Path(os.path.normpath(str(path)))
    try:
        common = os.path.commonpath((str(root), str(path)))
    except ValueError:
        raise CleanupError("the retention directory is invalid") from None
    if common == str(root) or common == str(path):
        raise CleanupError("the retention directory must be outside the install root")
    _check_components(path.parent)
    _check_directory(path.parent)
    parent_info = os.lstat(path.parent)
    if parent_info.st_uid != os.getuid() or parent_info.st_mode & 0o022:
        raise CleanupError("the retention parent must be owned by the current user and not writable by others")
    if os.path.lexists(path):
        raise CleanupError("the retention directory already exists")
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    if cache_dir == path or cache_dir in path.parents or path in cache_dir.parents:
        raise CleanupError("the retention directory must not overlap the configured cache")
    return path


def _open_checked_directory(path: Path) -> int:
    """Open an owned directory without following a symlink."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise CleanupError("a retained directory could not be opened safely") from None
    info = os.fstat(fd)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        os.close(fd)
        raise CleanupError("a retained directory has unsafe ownership or permissions")
    return fd


def _fsync_directory(fd: int) -> None:
    """Make directory entries durable before their source is removed."""
    try:
        os.fsync(fd)
    except OSError:
        raise CleanupError("retained state could not be flushed safely") from None


def _copy_file_at(source_fd: int, destination_fd: int, name: str, *, executable: bool = False) -> None:
    """Copy one regular file between checked directory descriptors."""
    try:
        before = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
    except OSError:
        raise CleanupError("a retained file changed while it was being inspected") from None
    if not stat.S_ISREG(before.st_mode) or before.st_uid != os.getuid() or before.st_mode & 0o022 or before.st_mode & 0o6000:
        raise CleanupError("a retained file has unsafe ownership or permissions")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        source_file = os.open(name, flags, dir_fd=source_fd)
    except OSError:
        raise CleanupError("a retained file could not be opened safely") from None
    try:
        opened = os.fstat(source_file)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("a retained file changed while being opened")
        output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        try:
            destination_file = os.open(name, output_flags, 0o700 if executable else 0o600, dir_fd=destination_fd)
        except OSError:
            raise CleanupError("a retained file could not be created safely") from None
        try:
            while True:
                chunk = os.read(source_file, 65536)
                if not chunk:
                    break
                view = memoryview(chunk)
                while view:
                    written = os.write(destination_file, view)
                    if written <= 0:
                        raise CleanupError("a retained file could not be written completely")
                    view = view[written:]
            after = os.fstat(source_file)
            if (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (
                after.st_dev,
                after.st_ino,
                after.st_size,
                after.st_mtime_ns,
            ):
                raise CleanupError("a retained file changed while it was being copied")
            os.fchmod(destination_file, 0o700 if executable else 0o600)
            os.fsync(destination_file)
        finally:
            os.close(destination_file)
    finally:
        os.close(source_file)


def _read_retained_file_at(source_fd: int, name: str, maximum: int) -> bytes:
    """Read one checked private file through an already-open directory."""
    try:
        before = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
    except OSError:
        raise CleanupError("a retained file changed while it was being inspected") from None
    if not stat.S_ISREG(before.st_mode) or before.st_uid != os.getuid() or before.st_mode & 0o077 or before.st_mode & 0o6000:
        raise CleanupError("a retained file has unsafe ownership or permissions")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        source_file = os.open(name, flags, dir_fd=source_fd)
    except OSError:
        raise CleanupError("a retained file could not be opened safely") from None
    try:
        opened = os.fstat(source_file)
        if not stat.S_ISREG(opened.st_mode) or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("a retained file changed while being opened")
        chunks: list[bytes] = []
        remaining = maximum + 1
        while remaining > 0:
            chunk = os.read(source_file, min(65536, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        after = os.fstat(source_file)
        if (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise CleanupError("a retained file changed while it was being read")
    finally:
        os.close(source_file)
    data = b"".join(chunks)
    if len(data) > maximum:
        raise CleanupError("a retained file is larger than the supported limit")
    return data


def _write_retained_file_at(destination_fd: int, name: str, data: bytes) -> None:
    """Write one new owner-private file and flush its contents."""
    output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        destination_file = os.open(name, output_flags, 0o600, dir_fd=destination_fd)
    except OSError:
        raise CleanupError("a retained file could not be created safely") from None
    try:
        view = memoryview(data)
        while view:
            written = os.write(destination_file, view)
            if written <= 0:
                raise CleanupError("a retained file could not be written completely")
            view = view[written:]
        os.fchmod(destination_file, 0o600)
        os.fsync(destination_file)
    finally:
        os.close(destination_file)


def _copy_sanitized_vault_at(source_fd: int, destination_fd: int, name: str) -> None:
    """Copy a vault configuration without remembered account credentials."""
    raw = _read_retained_file_at(source_fd, name, MAX_VAULT_SIZE)
    vault = _decode_json(raw)
    if "account_credentials" not in vault:
        _write_retained_file_at(destination_fd, name, raw)
        return
    del vault["account_credentials"]
    sanitized = (json.dumps(vault, ensure_ascii=True, sort_keys=True) + "\n").encode("utf-8")
    _write_retained_file_at(destination_fd, name, sanitized)


def _copy_directory_contents(source_fd: int, destination_fd: int, *, exclude_account_auth: bool = False) -> None:
    """Copy a checked directory tree without retaining symbolic links."""
    try:
        with os.scandir(source_fd) as iterator:
            children = sorted(iterator, key=lambda entry: entry.name)
    except OSError:
        raise CleanupError("a retained directory could not be inspected safely") from None
    for child in children:
        if exclude_account_auth and child.name in {"auth.json", "auth.json.lock"}:
            continue
        try:
            info = child.stat(follow_symlinks=False)
        except OSError:
            raise CleanupError("a retained directory changed while it was being inspected") from None
        if stat.S_ISDIR(info.st_mode):
            if info.st_uid != os.getuid() or info.st_mode & 0o022:
                raise CleanupError("a retained directory has unsafe ownership or permissions")
            source_child = -1
            destination_child = -1
            try:
                os.mkdir(child.name, 0o700, dir_fd=destination_fd)
                source_child = os.open(
                    child.name,
                    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0),
                    dir_fd=source_fd,
                )
                destination_child = os.open(
                    child.name,
                    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0),
                    dir_fd=destination_fd,
                )
            except OSError:
                if source_child >= 0:
                    os.close(source_child)
                if destination_child >= 0:
                    os.close(destination_child)
                raise CleanupError("a retained directory could not be copied safely") from None
            try:
                opened = os.fstat(source_child)
                if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
                    raise CleanupError("a retained directory changed while being opened")
                _copy_directory_contents(source_child, destination_child, exclude_account_auth=exclude_account_auth)
            finally:
                if source_child >= 0:
                    os.close(source_child)
                if destination_child >= 0:
                    os.close(destination_child)
        elif stat.S_ISREG(info.st_mode):
            if exclude_account_auth and child.name == "vault.json":
                _copy_sanitized_vault_at(source_fd, destination_fd, child.name)
            else:
                _copy_file_at(source_fd, destination_fd, child.name)
        else:
            raise CleanupError("symbolic links and special files cannot be retained")
    _fsync_directory(destination_fd)


def _copy_retained_directory(source: Path, destination: Path) -> None:
    """Copy an audited install directory to a new owner-private destination."""
    try:
        os.mkdir(destination, 0o700)
    except OSError:
        raise CleanupError("a retained directory could not be created safely") from None
    os.chmod(destination, 0o700)
    source_fd = _open_checked_directory(source)
    destination_fd = _open_checked_directory(destination)
    try:
        _copy_directory_contents(source_fd, destination_fd)
        parent_fd = _open_checked_directory(destination.parent)
        try:
            _fsync_directory(parent_fd)
        finally:
            os.close(parent_fd)
    finally:
        os.close(source_fd)
        os.close(destination_fd)


def _copy_retained_config_directory(source: Path, destination: Path) -> None:
    """Copy config while omitting account tokens and saved account passwords."""
    try:
        os.mkdir(destination, 0o700)
    except OSError:
        raise CleanupError("a retained directory could not be created safely") from None
    os.chmod(destination, 0o700)
    source_fd = _open_checked_directory(source)
    destination_fd = _open_checked_directory(destination)
    try:
        _copy_directory_contents(source_fd, destination_fd, exclude_account_auth=True)
        parent_fd = _open_checked_directory(destination.parent)
        try:
            _fsync_directory(parent_fd)
        finally:
            os.close(parent_fd)
    finally:
        os.close(source_fd)
        os.close(destination_fd)


def _copy_retained_file(source: Path, destination_directory: Path, *, executable: bool = False) -> None:
    """Copy one install file into an owner-private retention directory."""
    source_fd = _open_checked_directory(source.parent)
    destination_fd = _open_checked_directory(destination_directory)
    try:
        _copy_file_at(source_fd, destination_fd, source.name, executable=executable)
        _fsync_directory(destination_fd)
    finally:
        os.close(source_fd)
        os.close(destination_fd)


def _create_retention_directory(path: Path) -> None:
    """Create a new external directory suitable for private retained state."""
    try:
        os.mkdir(path, 0o700)
        os.chmod(path, 0o700)
    except OSError:
        raise CleanupError("the private retention directory could not be created safely") from None
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        raise CleanupError("the private retention directory has unsafe ownership or permissions")
    parent_fd = _open_checked_directory(path.parent)
    try:
        _fsync_directory(parent_fd)
    finally:
        os.close(parent_fd)
    retained_fd = _open_checked_directory(path)
    try:
        _fsync_directory(retained_fd)
    finally:
        os.close(retained_fd)


def _retain_state(
    root: Path,
    destination: Path,
    *,
    keep_config: bool,
    keep_cache: bool,
    keep_logs: bool,
    pending: bool,
) -> None:
    """Copy selected local state to the private external retention directory."""
    _validate_retention_directory(destination, root)
    _create_retention_directory(destination)
    if keep_config:
        _copy_retained_config_directory(root / "config", destination / "config")
    if keep_cache:
        _copy_retained_directory(installer.configured_cache_dir(root), destination / "cache")
    if keep_logs:
        _copy_retained_directory(root / "logs", destination / "logs")
    _copy_retained_file(root / "install.json", destination)
    if pending:
        try:
            os.mkdir(destination / "bin", 0o700)
        except OSError:
            raise CleanupError("the recovery binary directory could not be created safely") from None
        destination_fd = _open_checked_directory(destination)
        try:
            _fsync_directory(destination_fd)
        finally:
            os.close(destination_fd)
        _copy_retained_file(root / "bin/rclone", destination / "bin", executable=True)


def _unescape_mountinfo(text: str) -> str:
    """Decode octal escapes in a mountinfo field."""
    return re.sub(r"\\([0-7]{3})", lambda match: chr(int(match.group(1), 8)), text)


def read_mountpoints() -> set[str]:
    """Return mountpoint paths reported by the current Linux mount namespace."""
    try:
        lines = Path("/proc/self/mountinfo").read_text(encoding="ascii").splitlines()
    except OSError:
        raise CleanupError("the current mount table could not be read") from None
    result: set[str] = set()
    for line in lines:
        fields = line.split(" - ", 1)[0].split()
        if len(fields) < 5:
            raise CleanupError("the current mount table is malformed")
        result.add(os.path.normpath(_unescape_mountinfo(fields[4])))
    return result


def _mount_state(root: Path, points: set[str]) -> bool:
    root_text = os.path.normpath(str(root))
    mount_text = os.path.normpath(str(installer.configured_mountpoint(root)))
    if root_text in points:
        raise CleanupError("refusing to remove an install root that is itself a mountpoint")
    active = mount_text in points
    prefix = root_text.rstrip(os.sep) + os.sep
    mount_prefix = mount_text.rstrip(os.sep) + os.sep
    for point in points:
        if point.startswith(mount_prefix):
            raise CleanupError("refusing cleanup while a nested mount exists")
        if point.startswith(prefix) and point != mount_text:
            raise CleanupError("refusing cleanup while an unexpected mount exists inside the install")
    return active


def _check_mount_directory_removable(path: Path) -> None:
    """Refuse cleanup while the install-owned mount directory is immutable."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise CleanupError("the isolated mountpoint attributes could not be inspected safely") from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
            raise CleanupError("the isolated mountpoint changed before cleanup")
        value = bytearray(struct.calcsize("@L"))
        try:
            fcntl.ioctl(fd, FS_IOC_GETFLAGS, value, True)
        except OSError as exc:
            if exc.errno in {errno.ENOTTY, errno.EOPNOTSUPP, errno.ENOSYS}:
                return
            raise CleanupError(
                "the isolated mountpoint immutable flag could not be checked safely; "
                f"if `lsattr -d -- {shlex.quote(str(path))}` shows +i, clear it with "
                f"`sudo chattr -i -- {shlex.quote(str(path))}` and retry"
            ) from None
        if struct.unpack_from("@L", value)[0] & FS_IMMUTABLE_FL:
            raise CleanupError(
                "the isolated legacy mountpoint is immutable; clear it and retry with "
                f"`sudo chattr -i -- {shlex.quote(str(path))}`"
            )
    finally:
        os.close(fd)


def _service_paths(root: Path, manifest: dict[str, Any]) -> tuple[Path, Path]:
    config_text = manifest.get("service_config_home") or ""
    if not config_text:
        config_text = os.environ.get("XDG_CONFIG_HOME") or ""
        if config_text:
            candidate = Path(config_text)
            if not candidate.is_absolute() or ".." in candidate.parts:
                raise CleanupError("the user service config path is invalid")
            candidate = Path(os.path.normpath(str(candidate)))
            try:
                wrapper_override = os.path.commonpath((str(root), str(candidate))) == str(root)
            except ValueError:
                raise CleanupError("the user service config path is invalid") from None
            # Install wrappers redirect XDG paths into the root; an unset
            # service path in the manifest still refers to the user's config.
            if wrapper_override:
                config_text = str(home_directory() / ".config")
            else:
                config_text = str(candidate)
        else:
            config_text = str(home_directory() / ".config")
    config_home = Path(config_text)
    if not config_home.is_absolute() or ".." in config_home.parts:
        raise CleanupError("the user service config path is invalid")
    directory = Path(os.path.normpath(str(config_home))) / "systemd/user"
    return directory / "rclone-degoo.service", directory / "default.target.wants/rclone-degoo.service"


def _service_link_target(path: Path, unit: Path) -> bool:
    current = Path(path.anchor)
    components = path.parts[1:]
    for index, part in enumerate(components[:-1]):
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            return False
        except OSError:
            raise CleanupError("a user service directory could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise CleanupError("symlink or non-directory components in the user service path are not allowed")
        final_directory = index == len(components) - 2
        if final_directory:
            if info.st_uid != os.getuid() or info.st_mode & 0o022:
                raise CleanupError("the user service directory has unsafe ownership or permissions")
        else:
            system_shared = info.st_uid == 0 and not (info.st_mode & 0o022)
            sticky_temp = info.st_uid == 0 and bool(info.st_mode & stat.S_ISVTX)
            if not (info.st_uid == os.getuid() or system_shared or sticky_temp) or (info.st_mode & 0o022 and not sticky_temp):
                raise CleanupError("a user service path ancestor has unsafe ownership or permissions")
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    except OSError:
        raise CleanupError("a user service link could not be inspected safely") from None
    if not stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid():
        raise CleanupError("an expected user service path is occupied by an unowned file")
    try:
        target = Path(os.readlink(path))
    except OSError:
        raise CleanupError("a user service link could not be read") from None
    if not target.is_absolute():
        target = path.parent / target
    return Path(os.path.normpath(str(target))) == unit


def _path_link_target(path: Path, root: Path) -> bool:
    """Report whether a safe user-owned command link points to this install."""
    if not os.path.lexists(path):
        return False
    _check_directory(path.parent)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    except OSError:
        raise CleanupError("the PATH command link could not be inspected safely") from None
    if not stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid():
        return False
    try:
        target_text = os.readlink(path)
    except OSError:
        raise CleanupError("the PATH command link could not be read safely") from None
    target = Path(target_text)
    if not target.is_absolute():
        target = path.parent / target
    return Path(os.path.normpath(str(target))) == root / "degoo-rc"


def _remove_owned_path_link(root: Path, manifest: dict[str, Any]) -> bool:
    """Remove a recorded or exact-target command link owned by this install."""
    link = expected_path_link()
    recorded = manifest.get("path_link") == str(link)
    if not os.path.lexists(link):
        return False
    if not _path_link_target(link, root):
        if not recorded:
            return False
        raise CleanupError("the recorded PATH command link changed before cleanup")
    parent_fd = -1
    try:
        parent_fd = os.open(
            link.parent,
            os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
        )
        before = os.stat(link.name, dir_fd=parent_fd, follow_symlinks=False)
        if not stat.S_ISLNK(before.st_mode) or before.st_uid != os.getuid():
            raise CleanupError("the recorded PATH command link changed before cleanup")
        target_text = os.readlink(link.name, dir_fd=parent_fd)
        target = Path(target_text)
        if not target.is_absolute():
            target = link.parent / target
        if Path(os.path.normpath(str(target))) != root / "degoo-rc":
            raise CleanupError("the recorded PATH command link changed before cleanup")
        current = os.stat(link.name, dir_fd=parent_fd, follow_symlinks=False)
        if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("the PATH command link changed during cleanup")
        os.unlink(link.name, dir_fd=parent_fd)
        try:
            os.stat(link.name, dir_fd=parent_fd, follow_symlinks=False)
        except FileNotFoundError:
            return True
        except OSError:
            raise CleanupError("the owned PATH command link could not be verified after cleanup") from None
        raise CleanupError("the owned PATH command link remains after cleanup")
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the owned PATH command link could not be removed safely") from None
    finally:
        if parent_fd >= 0:
            os.close(parent_fd)


def _validate_recorded_path_link(root: Path, manifest: dict[str, Any]) -> None:
    """Reject a changed recorded link before service or mount cleanup starts."""
    link = expected_path_link()
    if manifest.get("path_link") == str(link) and os.path.lexists(link) and not _path_link_target(link, root):
        raise CleanupError("the recorded PATH command link changed before cleanup")


def _owned_service_links(root: Path, manifest: dict[str, Any]) -> tuple[list[Path], set[str]]:
    direct, wanted = _service_paths(root, manifest)
    unit = root / "rclone-degoo.service"
    recorded = set(manifest.get("service_links", []))
    links: list[Path] = []
    for path in (direct, wanted):
        matches = _service_link_target(path, unit)
        if str(path) in recorded and os.path.lexists(path) and not matches:
            raise CleanupError("a recorded user service link changed before cleanup")
        if matches:
            links.append(path)
    return links, recorded


def _systemctl_user(*arguments: str) -> subprocess.CompletedProcess[bytes]:
    env = {
        key: value
        for key, value in os.environ.items()
        if key in {"HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "LANG", "LC_ALL"}
    }
    env.setdefault("HOME", str(home_directory()))
    env["PATH"] = "/usr/bin:/bin"
    try:
        return subprocess.run(
            ["systemctl", "--user", *arguments],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=45,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise CleanupError("the owned user service could not be stopped safely") from None


def _systemctl_service_state() -> dict[str, str]:
    result = _systemctl_user(
        "show",
        "--property=LoadState",
        "--property=ActiveState",
        "--property=FragmentPath",
        "rclone-degoo.service",
    )
    if result.returncode != 0:
        raise CleanupError("the owned user service could not be identified safely")
    try:
        output = result.stdout.decode("utf-8")
    except UnicodeDecodeError:
        raise CleanupError("systemd returned invalid service state") from None
    state: dict[str, str] = {}
    for line in output.splitlines():
        key, separator, value = line.partition("=")
        if separator and key in {"LoadState", "ActiveState", "FragmentPath"}:
            state[key] = value
    if set(state) != {"LoadState", "ActiveState", "FragmentPath"}:
        raise CleanupError("systemd returned incomplete service state")
    return state


def _check_service_fragment(state: dict[str, str], allowed: set[str]) -> None:
    fragment = state["FragmentPath"]
    load_state = state["LoadState"]
    active_state = state["ActiveState"]
    if fragment:
        if not Path(fragment).is_absolute() or os.path.normpath(fragment) not in allowed:
            raise CleanupError("systemd resolved the service name to another unit")
        if load_state == "loaded":
            raise CleanupError("systemd still has the owned service loaded after cleanup")
        raise CleanupError("systemd returned an unexpected state for the owned service")
    if load_state != "not-found":
        raise CleanupError("systemd could not verify ownership of the loaded service")
    if active_state in {"active", "activating", "deactivating", "reloading"}:
        raise CleanupError("the owned user service is still active after cleanup")
    if active_state != "inactive":
        raise CleanupError("systemd could not verify that the owned service is inactive")


def _stop_owned_service(root: Path, manifest: dict[str, Any], *, disable: bool = True) -> set[str]:
    links, recorded = _owned_service_links(root, manifest)
    if not links and not recorded:
        return set()
    unit = root / "rclone-degoo.service"
    allowed_fragments = {str(unit), *recorded}
    for path in links:
        allowed_fragments.add(str(path))
    state = _systemctl_service_state()
    fragment = state["FragmentPath"]
    if fragment:
        if not Path(fragment).is_absolute() or os.path.normpath(fragment) not in allowed_fragments:
            raise CleanupError("systemd resolved the service name to another unit")
        if state["LoadState"] != "loaded":
            raise CleanupError("systemd returned an unexpected state for the owned service")
        if state["ActiveState"] not in {"active", "activating", "deactivating", "reloading", "inactive", "failed"}:
            raise CleanupError("systemd could not identify the state of the owned service")
        result = _systemctl_user("stop", "rclone-degoo.service")
        if result.returncode != 0:
            raise CleanupError("the owned user service could not be stopped safely")
        if links and disable:
            result = _systemctl_user("disable", "rclone-degoo.service")
            if result.returncode != 0:
                raise CleanupError("the owned user service could not be disabled safely")
    elif state["LoadState"] != "not-found" or state["ActiveState"] not in {"inactive", "failed"}:
        raise CleanupError("systemd could not verify that the owned service is inactive")
    return allowed_fragments


def _remove_owned_service_links(root: Path, manifest: dict[str, Any], previously_owned: set[str]) -> bool:
    direct, wanted = _service_paths(root, manifest)
    unit = root / "rclone-degoo.service"
    links, recorded = _owned_service_links(root, manifest)
    if not links and not recorded and not previously_owned:
        return False
    # Validate every candidate before unlinking any of them. Exact owned links
    # also cover a crash after link creation but before the manifest update.
    for path in (wanted, direct):
        if path in links:
            try:
                os.unlink(path)
            except OSError:
                raise CleanupError("an owned user service link could not be removed") from None
    for path in links:
        if os.path.lexists(path):
            raise CleanupError("an owned user service link remains after cleanup")

    result = _systemctl_user("daemon-reload")
    if result.returncode != 0:
        raise CleanupError("systemd could not reload after removing the owned service links")
    state = _systemctl_service_state()
    allowed_fragments = {str(unit), *recorded, *previously_owned, *(str(path) for path in links)}
    _check_service_fragment(state, allowed_fragments)
    return True


def _validate_helper_link(path: Path, *, require_executable: bool) -> Path:
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise CleanupError("the contained FUSE unmount helper is missing") from None
    if not stat.S_ISLNK(info.st_mode):
        raise CleanupError("the contained FUSE unmount helper is not the expected symlink")
    target_text = os.readlink(path)
    target = Path(target_text)
    if not target.is_absolute() or ".." in target.parts or target.parent not in (Path("/bin"), Path("/usr/bin")) or target.name not in FUSERMOUNT_NAMES:
        raise CleanupError("the contained FUSE unmount helper points to an untrusted location")
    if require_executable:
        try:
            real = Path(os.path.realpath(target))
            target_info = os.stat(target)
        except OSError:
            raise CleanupError("the system FUSE unmount helper is unavailable") from None
        if real.parent not in (Path("/bin"), Path("/usr/bin")) or real.name not in FUSERMOUNT_NAMES:
            raise CleanupError("the system FUSE unmount helper resolves to an untrusted location")
        if not stat.S_ISREG(target_info.st_mode) or target_info.st_uid != 0 or target_info.st_mode & 0o022 or not target_info.st_mode & 0o111:
            raise CleanupError("the system FUSE unmount helper is unsafe")
    return path


def _unmount(root: Path) -> None:
    helper = _validate_helper_link(root / "tools/fusermount3", require_executable=True)
    try:
        result = subprocess.run(
            [str(helper), "-u", str(installer.configured_mountpoint(root))],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env={"HOME": str(home_directory()), "PATH": "/usr/bin:/bin", "LANG": "C"},
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise CleanupError("the isolated mount could not be unmounted") from None
    if result.returncode != 0:
        raise CleanupError("the isolated mount could not be unmounted")
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        if not _mount_state(root, read_mountpoints()):
            return
        time.sleep(0.1)
    raise CleanupError("the isolated mount remains active after unmount")


def _cache_lock_path(root: Path) -> Path:
    digest = hashlib.sha256(f"{REMOTE_NAME}:".encode("utf-8")).hexdigest()[:24]
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    return cache_dir / f"degoo-vault-{digest}.lock"


def _ensure_cache_directory(cache_dir: Path) -> None:
    """Create a missing private cache path component by component."""
    missing: list[Path] = []
    current = Path(cache_dir.anchor)
    for part in cache_dir.parts[1:]:
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            missing.append(current)
            continue
        except OSError:
            raise CleanupError("the configured cache path could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise CleanupError("the configured cache path has an unsafe parent")
        if missing:
            raise CleanupError("the configured cache path changed while being prepared")
    for path in missing:
        try:
            os.mkdir(path, 0o700)
        except FileExistsError:
            pass
        except OSError:
            raise CleanupError("the configured cache directory could not be prepared safely") from None
        try:
            info = os.lstat(path)
        except OSError:
            raise CleanupError("the configured cache directory could not be verified") from None
        if (
            stat.S_ISLNK(info.st_mode)
            or not stat.S_ISDIR(info.st_mode)
            or info.st_uid != os.getuid()
            or stat.S_IMODE(info.st_mode) != 0o700
        ):
            raise CleanupError("the configured cache directory has unsafe ownership or permissions")
    _check_directory(cache_dir, private=True)


def _cache_name() -> str:
    """Return the VFS namespace bound to the installed Degoo remote."""
    digest = hashlib.sha256(f"{REMOTE_NAME}:".encode("utf-8")).hexdigest()[:24]
    return f"degoo-vault-{digest}"


def _cache_mountpoints(cache_dir: Path) -> None:
    """Reject a separate mountpoint nested inside the dedicated cache root."""
    for raw in read_mountpoints():
        mountpoint = Path(raw)
        if mountpoint != cache_dir and cache_dir in mountpoint.parents:
            raise CleanupError("an unrelated mount exists inside the configured cache")


def _check_cache_file(path: Path, *, maximum: int | None = None) -> os.stat_result:
    """Validate a regular private cache file without following links."""
    try:
        info = os.lstat(path)
    except OSError:
        raise CleanupError("a configured cache file could not be inspected safely") from None
    if (
        not stat.S_ISREG(info.st_mode)
        or stat.S_ISLNK(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
        or info.st_mode & 0o6000
        or info.st_nlink != 1
        or (maximum is not None and info.st_size > maximum)
    ):
        raise CleanupError("a configured cache file has unsafe ownership, permissions, or size")
    return info


def _audit_cache_tree(path: Path, cache_dir: Path, *, metadata: bool = False) -> bool:
    """Inspect a known cache subtree and report whether it contains data."""
    _check_directory(path, private=True)
    _cache_mountpoints(cache_dir)
    has_data = False
    if metadata:
        total_bytes = 0
        metadata_locks = {".lock", ".lock.lock"}
        entries = sorted(os.scandir(path), key=lambda entry: entry.name)
        if len(entries) > MAX_METADATA_CACHE_ENTRIES + len(metadata_locks):
            raise CleanupError("the persistent metadata cache exceeds its bounded entry count")
        if sum(entry.name not in metadata_locks for entry in entries) > MAX_METADATA_CACHE_ENTRIES:
            raise CleanupError("the persistent metadata cache exceeds its bounded entry count")
        for entry in entries:
            entry_path = path / entry.name
            if entry.name in metadata_locks:
                _check_cache_file(entry_path, maximum=4096)
                continue
            if not METADATA_CACHE_FILE.fullmatch(entry.name):
                raise CleanupError("the persistent metadata cache contains an unknown file")
            info = _check_cache_file(entry_path, maximum=MAX_METADATA_CACHE_FILE_SIZE)
            total_bytes += info.st_size
            if total_bytes > MAX_METADATA_CACHE_BYTES:
                raise CleanupError("the persistent metadata cache exceeds its bounded size")
            has_data = True
        return has_data

    try:
        children = sorted(os.scandir(path), key=lambda entry: entry.name)
    except OSError:
        raise CleanupError("the configured cache could not be inspected safely") from None
    for child in children:
        child_path = path / child.name
        try:
            info = child.stat(follow_symlinks=False)
        except OSError:
            raise CleanupError("the configured cache changed while being inspected") from None
        if stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_mode & 0o6000:
            raise CleanupError("the configured cache contains an unsafe entry")
        if stat.S_ISDIR(info.st_mode):
            has_data = _audit_cache_tree(child_path, cache_dir) or has_data
        elif stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
            has_data = True
        else:
            raise CleanupError("the configured cache contains a special or shared file")
    return has_data


def _audit_cache_namespace(root: Path, cache_dir: Path | None = None) -> bool:
    """Validate only the vault's owned cache namespace and report stored data."""
    if cache_dir is None:
        try:
            cache_dir = installer.configured_cache_dir(root)
        except installer.InstallError as exc:
            raise CleanupError(str(exc)) from None
    try:
        _check_components(cache_dir, missing_ok=True)
        os.lstat(cache_dir)
    except FileNotFoundError:
        return False
    except (OSError, CleanupError):
        raise CleanupError("the configured cache path is unsafe") from None
    _check_directory(cache_dir, private=True)
    _cache_mountpoints(cache_dir)
    cache_name = _cache_name()
    lock = _cache_lock_path(root)
    identity = cache_dir / f"{cache_name}.identity"
    known = {lock.name, identity.name, "vfs", "vfsMeta", "metadata"}
    try:
        children = sorted(os.scandir(cache_dir), key=lambda entry: entry.name)
    except OSError:
        raise CleanupError("the configured cache could not be inspected safely") from None
    has_data = False
    identity_exists = False
    for child in children:
        path = cache_dir / child.name
        try:
            info = child.stat(follow_symlinks=False)
        except OSError:
            raise CleanupError("the configured cache changed while being inspected") from None
        if child.name not in known:
            raise CleanupError("the configured cache contains an unknown path")
        if child.name in {lock.name, identity.name}:
            _check_cache_file(path, maximum=4096 if child.name == lock.name else 65)
            if child.name == identity.name:
                raw = _read_private_file(path, 65)
                if raw is None or not re.fullmatch(rb"[0-9a-f]{64}\n", raw):
                    raise CleanupError("the configured cache identity is invalid")
                identity_exists = True
                has_data = True
            continue
        if not stat.S_ISDIR(info.st_mode):
            raise CleanupError("the configured cache contains an unsafe namespace")
        _check_directory(path, private=True)
        try:
            namespaces = sorted(os.scandir(path), key=lambda entry: entry.name)
        except OSError:
            raise CleanupError("the configured cache namespace could not be inspected") from None
        if any(entry.name != cache_name for entry in namespaces):
            raise CleanupError("the configured cache contains another vault namespace")
        if not namespaces:
            continue
        namespace = path / cache_name
        if child.name == "metadata":
            has_data = _audit_cache_tree(namespace, cache_dir, metadata=True) or has_data
        else:
            has_data = _audit_cache_tree(namespace, cache_dir) or has_data
    if has_data and not identity_exists:
        raise CleanupError("the configured cache data or its vault identity is incomplete")
    return has_data


def _validate_vfs_cache_recovery(cache_dir: Path) -> None:
    """Require each cached data file to have valid, matching recovery metadata."""
    cache_name = _cache_name()
    data_root = cache_dir / "vfs" / cache_name
    metadata_root = cache_dir / "vfsMeta" / cache_name
    data_files = {
        path.relative_to(data_root): path
        for path in data_root.rglob("*")
        if path.is_file()
    } if data_root.is_dir() else {}
    metadata_files = {
        path.relative_to(metadata_root): path
        for path in metadata_root.rglob("*")
        if path.is_file()
    } if metadata_root.is_dir() else {}
    if data_files.keys() != metadata_files.keys():
        raise CleanupError("cache data or its recovery metadata is incomplete; uninstall was refused")
    for relative, data_path in data_files.items():
        metadata_path = metadata_files[relative]
        data_info = _check_cache_file(data_path)
        try:
            raw = installer.read_private_file(metadata_path, MAX_VFS_METADATA_SIZE)
            metadata = json.loads(raw or b"")
        except (installer.InstallError, UnicodeDecodeError, json.JSONDecodeError, RecursionError):
            raise CleanupError("cache recovery metadata is malformed; uninstall was refused") from None
        if (
            not isinstance(metadata, dict)
            or type(metadata.get("Dirty")) is not bool
            or type(metadata.get("Size")) is not int
            or metadata["Size"] < 0
            or metadata["Size"] != data_info.st_size
        ):
            raise CleanupError("cache recovery metadata does not match its data; uninstall was refused")
        fingerprint = metadata.get("Fingerprint")
        if fingerprint is not None and (not isinstance(fingerprint, str) or len(fingerprint) > 4096):
            raise CleanupError("cache recovery metadata has an invalid fingerprint; uninstall was refused")
        if metadata["Dirty"] is False and not fingerprint:
            raise CleanupError("clean cache data has no recovery fingerprint; uninstall was refused")


def _verify_cache_identity(root: Path, cache_dir: Path) -> None:
    """Ask the offline rclone command to verify this cache's vault identity."""
    manifest = _validate_manifest(root)
    folder_id = manifest.get("root_folder_id")
    if not isinstance(folder_id, str) or not folder_id:
        raise CleanupError("the configured cache has no matching vault folder identity")
    binary = root / "bin/rclone"
    _check_regular_executable(binary)
    env = {
        "HOME": str(home_directory()),
        "XDG_CONFIG_HOME": str(root / "config"),
        "XDG_CACHE_HOME": str(cache_dir),
        "TMPDIR": str(root / "logs"),
        "PATH": "/usr/bin:/bin",
        "LANG": "C",
    }
    try:
        result = subprocess.run(
            [str(binary), "--config", str(root / "config/rclone.conf"), "degoo", "verify-cache",
             str(root / "config/vault.json"), "--folder-id", folder_id],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=env,
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise CleanupError("the configured cache identity could not be verified offline") from None
    if result.returncode != 0:
        raise CleanupError("the configured cache identity does not match the vault")


def _remove_owned_cache(root: Path, cache_dir: Path, lock_path: Path) -> None:
    """Remove verified vault cache entries while retaining the held lock inode."""
    has_data = _audit_cache_namespace(root, cache_dir)
    if has_data and (root / "config/vault.json").is_file():
        _verify_cache_identity(root, cache_dir)
    _cache_mountpoints(cache_dir)
    try:
        children = sorted(os.scandir(cache_dir), key=lambda entry: entry.name)
    except OSError:
        raise CleanupError("the configured cache could not be inspected before cleanup") from None
    known = {lock_path.name, f"{_cache_name()}.identity", "vfs", "vfsMeta", "metadata"}
    for child in children:
        if child.name not in known:
            raise CleanupError("an unknown path appeared in the configured cache before cleanup")
        if child.name == lock_path.name:
            continue
        path = cache_dir / child.name
        try:
            info = child.stat(follow_symlinks=False)
        except OSError:
            raise CleanupError("the configured cache changed before cleanup") from None
        if stat.S_ISDIR(info.st_mode):
            try:
                shutil.rmtree(path)
            except OSError:
                raise CleanupError("the owned cache namespace could not be removed safely") from None
        elif stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode):
            try:
                path.unlink()
            except OSError:
                raise CleanupError("the owned cache entry could not be removed safely") from None
        else:
            raise CleanupError("the configured cache changed to an unsafe entry before cleanup")


def _acquire_cache_lock(root: Path, wait_seconds: float) -> int:
    path = _cache_lock_path(root)
    _ensure_cache_directory(path.parent)
    _cache_mountpoints(path.parent)
    flags = os.O_CREAT | os.O_RDWR | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags, 0o600)
    except OSError:
        raise CleanupError("the vault cache lock file is unsafe") from None
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        raise CleanupError("the vault cache lock file has unsafe ownership or permissions")
    deadline = time.monotonic() + wait_seconds
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return fd
        except OSError as exc:
            if exc.errno not in (errno.EACCES, errno.EAGAIN):
                os.close(fd)
                raise CleanupError("the vault cache lock could not be acquired") from None
            if time.monotonic() >= deadline:
                os.close(fd)
                raise CleanupError("the vault cache is active in another process") from None
            time.sleep(0.1)


def _audit_tree(root: Path) -> list[dict[str, str]]:
    """Reject unexpected links/special files and collect safe inventory names."""
    entries: list[dict[str, str]] = []
    helper_rel = Path("tools/fusermount3")

    def visit(directory: Path, relative: Path) -> None:
        try:
            children = sorted(os.scandir(directory), key=lambda entry: entry.name)
        except OSError:
            raise CleanupError("the install tree could not be inspected safely") from None
        for child in children:
            rel = relative / child.name
            try:
                info = child.stat(follow_symlinks=False)
            except OSError:
                raise CleanupError("the install tree changed while being inspected") from None
            if stat.S_ISLNK(info.st_mode):
                if rel != helper_rel:
                    raise CleanupError("unexpected symlinks inside the install are not allowed")
                _validate_helper_link(root / rel, require_executable=False)
                kind = "symlink"
            elif stat.S_ISDIR(info.st_mode):
                if info.st_uid != os.getuid() or info.st_mode & 0o022:
                    raise CleanupError("an install directory has unsafe ownership or permissions")
                kind = "directory"
                entries.append({"path": rel.as_posix(), "type": kind})
                # A mounted vault can contain arbitrary remote names and is not
                # enumerated during dry-run inventory.
                if rel != Path("mount"):
                    visit(Path(child.path), rel)
                continue
            elif stat.S_ISREG(info.st_mode):
                if info.st_uid != os.getuid() or info.st_mode & 0o022 or info.st_mode & 0o6000:
                    raise CleanupError("an install file has unsafe ownership or permissions")
                kind = "file"
            elif stat.S_ISSOCK(info.st_mode) and rel == Path("tmp/vfs-sync.sock"):
                if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
                    raise CleanupError("the mount refresh socket has unsafe ownership or permissions")
                kind = "socket"
            else:
                raise CleanupError("special files inside the install are not allowed")
            entries.append({"path": rel.as_posix(), "type": kind})

    visit(root, Path())
    entries.sort(key=lambda item: item["path"])
    return entries


def _cache_has_data(root: Path, lock_path: Path) -> bool:
    """Return whether the validated vault cache contains more than its lock."""
    expected_lock = _cache_lock_path(root)
    if lock_path != expected_lock:
        raise CleanupError("the configured cache lock path changed")
    try:
        return _audit_cache_namespace(root)
    except CleanupError as exc:
        raise CleanupError(f"the cache is not empty or safe: {exc}") from None


def _legacy_cache_has_contents(root: Path) -> bool:
    """Report files left in the install cache after a configured path moved."""
    cache = root / "cache"
    try:
        for directory, dirnames, filenames in os.walk(cache, followlinks=False):
            for name in dirnames:
                path = Path(directory) / name
                info = os.lstat(path)
                if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
                    raise CleanupError("the legacy install cache contains an unsafe entry")
                if os.path.ismount(path):
                    raise CleanupError("a mount exists inside the legacy install cache")
            for name in filenames:
                path = Path(directory) / name
                info = os.lstat(path)
                if not stat.S_ISREG(info.st_mode):
                    raise CleanupError("the legacy install cache contains an unsafe entry")
                if stat.S_ISREG(info.st_mode):
                    return True
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the legacy install cache could not be inspected safely") from None
    return False


def _pending_uploads(root: Path) -> tuple[int, int]:
    binary = root / "bin/rclone"
    config = root / "config/rclone.conf"
    vault = root / "config/vault.json"
    _check_regular_executable(binary)
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise CleanupError(str(exc)) from None
    try:
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(
                [str(binary), "--config", str(config), "--log-file", os.devnull, "--log-level", "ERROR", "degoo", "status", str(vault)],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.DEVNULL,
                env={
                    "HOME": str(home_directory()),
                    "XDG_CONFIG_HOME": str(root / "config"),
                    "XDG_CACHE_HOME": str(cache_dir),
                    "TMPDIR": str(root / "logs"),
                    "PATH": "/usr/bin:/bin",
                    "LANG": "C",
                },
                timeout=30,
                check=False,
            )
            if result.returncode != 0:
                raise CleanupError("pending-upload status could not be verified")
            output.seek(0)
            data = output.read(MAX_STATUS_SIZE + 1)
    except CleanupError:
        raise
    except (OSError, subprocess.TimeoutExpired):
        raise CleanupError("pending-upload status could not be verified") from None
    if len(data) > MAX_STATUS_SIZE:
        raise CleanupError("pending-upload status response was too large")
    try:
        text = data.decode("ascii").strip()
    except UnicodeDecodeError:
        raise CleanupError("pending-upload status response was invalid") from None
    match = STATUS_PATTERN.fullmatch(text)
    if not match:
        raise CleanupError("pending-upload status response was invalid")
    try:
        return int(match.group(1)), int(match.group(2))
    except ValueError:
        raise CleanupError("pending-upload status response was invalid") from None


def _require_empty_mount_directory(root: Path) -> None:
    try:
        with os.scandir(installer.configured_mountpoint(root)) as iterator:
            if next(iterator, None) is not None:
                raise CleanupError("the unmounted mount directory contains local files")
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the isolated mount directory could not be inspected") from None


def _require_empty_contained_mount_directory(root: Path) -> None:
    """Refuse cleanup when the unused install-contained mount directory has data."""
    mount_directory = root / "mount"
    _check_directory(mount_directory)
    try:
        with os.scandir(mount_directory) as iterator:
            if next(iterator, None) is not None:
                raise CleanupError("the unused contained mount directory contains local files")
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the unused contained mount directory could not be inspected") from None


def _remove_root(root: Path) -> None:
    """Remove only the validated install root using Python's safe rmtree path."""
    if not getattr(shutil.rmtree, "avoids_symlink_attacks", False):
        raise CleanupError("this Python does not provide symlink-safe tree removal")
    parent = root.parent
    _check_directory(parent)
    parent_fd = os.open(parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0))
    try:
        before = os.stat(root.name, dir_fd=parent_fd, follow_symlinks=False)
        if not stat.S_ISDIR(before.st_mode) or stat.S_ISLNK(before.st_mode) or before.st_uid != os.getuid():
            raise CleanupError("the install root changed before removal")
        current = os.lstat(root)
        if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("the install root changed before removal")
        root_fd = os.open(
            root.name,
            os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
            dir_fd=parent_fd,
        )
        try:
            opened = os.fstat(root_fd)
            if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
                raise CleanupError("the install root changed before removal")
            _make_tree_directories_writable(root_fd)
        finally:
            os.close(root_fd)
        current = os.stat(root.name, dir_fd=parent_fd, follow_symlinks=False)
        if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("the install root changed before removal")
        shutil.rmtree(root)
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the isolated local install could not be removed safely") from None
    finally:
        os.close(parent_fd)


def _make_install_tree_writable(root: Path) -> None:
    """Prepare only the validated install tree for safe recursive removal."""
    _check_directory(root.parent)
    parent_fd = os.open(root.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0))
    root_fd = -1
    try:
        before = os.stat(root.name, dir_fd=parent_fd, follow_symlinks=False)
        if not stat.S_ISDIR(before.st_mode) or before.st_uid != os.getuid() or before.st_mode & 0o022:
            raise CleanupError("the install root changed before removal")
        root_fd = os.open(
            root.name,
            os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
            dir_fd=parent_fd,
        )
        opened = os.fstat(root_fd)
        if (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("the install root changed before removal")
        _make_tree_directories_writable(root_fd)
        current = os.stat(root.name, dir_fd=parent_fd, follow_symlinks=False)
        if (current.st_dev, current.st_ino) != (before.st_dev, before.st_ino):
            raise CleanupError("the install root changed before removal")
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("read-only install directories could not be prepared for removal") from None
    finally:
        if root_fd >= 0:
            os.close(root_fd)
        os.close(parent_fd)


def _make_tree_directories_writable(directory_fd: int) -> None:
    """Add owner write permission to validated install directories before removal."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        directory = os.fstat(directory_fd)
        if not stat.S_ISDIR(directory.st_mode) or directory.st_uid != os.getuid() or directory.st_mode & 0o022:
            raise CleanupError("an install directory changed before removal")
        mode = stat.S_IMODE(directory.st_mode) | stat.S_IWUSR
        if mode != stat.S_IMODE(directory.st_mode):
            os.fchmod(directory_fd, mode)
        with os.scandir(directory_fd) as entries:
            for entry in entries:
                info = entry.stat(follow_symlinks=False)
                if not stat.S_ISDIR(info.st_mode):
                    continue
                if info.st_uid != os.getuid() or info.st_mode & 0o022:
                    raise CleanupError("an install directory changed before removal")
                child_fd = os.open(entry.name, flags, dir_fd=directory_fd)
                try:
                    current = os.fstat(child_fd)
                    if (
                        not stat.S_ISDIR(current.st_mode)
                        or current.st_uid != os.getuid()
                        or current.st_mode & 0o022
                        or (current.st_dev, current.st_ino) != (info.st_dev, info.st_ino)
                    ):
                        raise CleanupError("an install directory changed before removal")
                    mode = stat.S_IMODE(current.st_mode) | stat.S_IWUSR
                    if mode != stat.S_IMODE(current.st_mode):
                        os.fchmod(child_fd, mode)
                    _make_tree_directories_writable(child_fd)
                finally:
                    os.close(child_fd)
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("read-only install directories could not be prepared for removal") from None


def _verify_cleanup(
    root: Path,
    owned_service_links: list[Path],
    owned_path_link: bool,
) -> None:
    """Verify that the install root and its owned external links are absent."""
    if _entry_exists(root):
        raise CleanupError("the install root remains after cleanup")
    for path in owned_service_links:
        if _entry_exists(path):
            raise CleanupError("an owned user service link remains after cleanup")
    if owned_path_link and _entry_exists(expected_path_link()):
        raise CleanupError("the owned PATH command link remains after cleanup")


def _entry_exists(path: Path) -> bool:
    """Check an entry without treating inspection errors as absence."""
    try:
        os.lstat(path)
    except FileNotFoundError:
        return False
    except OSError:
        raise CleanupError("cleanup results could not be verified safely") from None
    return True


def _service_inventory(root: Path, manifest: dict[str, Any]) -> list[dict[str, Any]]:
    direct, wanted = _service_paths(root, manifest)
    unit = root / "rclone-degoo.service"
    recorded = set(manifest.get("service_links", []))
    result: list[dict[str, Any]] = []
    for path in (direct, wanted):
        if str(path) not in recorded and not os.path.lexists(path):
            continue
        try:
            info = os.lstat(path)
        except FileNotFoundError:
            present = False
            kind = "missing"
            matches = False
        except OSError:
            present = False
            kind = "unavailable"
            matches = False
        else:
            present = True
            kind = "symlink" if stat.S_ISLNK(info.st_mode) else "other"
            matches = _service_link_target(path, unit) if kind == "symlink" else False
        result.append({
            "path": str(path),
            "owned": str(path) in recorded and matches,
            "present": present,
            "type": kind,
        })
    return result


def _path_link_inventory(root: Path, manifest: dict[str, Any]) -> dict[str, Any] | None:
    path = expected_path_link()
    recorded = manifest.get("path_link", "")
    present = os.path.lexists(path)
    if not present and not recorded:
        return None
    if not present:
        kind = "missing"
        matches = False
    else:
        _check_directory(path.parent)
        try:
            info = os.lstat(path)
        except OSError:
            raise CleanupError("the PATH command link could not be inspected safely") from None
        kind = "symlink" if stat.S_ISLNK(info.st_mode) else "other"
        matches = _path_link_target(path, root) if kind == "symlink" else False
    return {
        "path": str(path),
        "owned": recorded == str(path) and matches,
        "present": present,
        "type": kind,
    }


def _inventory(root: Path, manifest: dict[str, Any], entries: list[dict[str, str]], mounted: bool) -> None:
    result = {
        "mode": "inventory",
        "root": str(root),
        "mounted": mounted,
        "files": entries,
        "service_links": _service_inventory(root, manifest),
        "path_link": _path_link_inventory(root, manifest),
        "remote_data": "left untouched",
    }
    json.dump(result, sys.stdout, ensure_ascii=True, sort_keys=True)
    sys.stdout.write("\n")


def _inspect_install(root_arg: str) -> tuple[Path, dict[str, Any], bool, list[dict[str, str]]]:
    """Validate and inventory the fixed local install before any action."""
    expected = expected_root()
    supplied = Path(root_arg)
    if not supplied.is_absolute() or ".." in supplied.parts:
        raise CleanupError("--root must be an absolute path without parent traversal")
    root = Path(os.path.normpath(str(supplied)))
    if root != expected:
        raise CleanupError("--root must be the isolated ~/.local/share/rclone-degoo directory")
    _check_components(root)
    layout = _validate_layout(root)
    mounted = _mount_state(root, read_mountpoints())
    entries = _audit_tree(root)
    return root, layout, mounted, entries


def _prompt_yes_no(prompt: str, *, default: bool = False) -> bool:
    """Ask a terminal yes/no question with an explicit default."""
    suffix = "[Y/n]" if default else "[y/N]"
    while True:
        answer = input(f"{prompt} {suffix} ").strip().lower()
        if not answer:
            return default
        if answer in ("y", "yes"):
            return True
        if answer in ("n", "no"):
            return False
        print("Please answer yes or no.")


def _wizard(args: argparse.Namespace) -> int:
    """Guide the user through retaining selected local state and cleanup."""
    if not sys.stdin.isatty() or not sys.stdout.isatty():
        raise CleanupError("--wizard requires an interactive terminal; use --inventory or --delete-local")

    root, layout, _, _ = _inspect_install(args.root)
    print("This removes local Degoo mount files only. Degoo remote data will be left untouched.")
    print("A new owner-only backup outside the install is created only if at least one keep option is answered Yes.")
    print(
        "Configuration includes the vault encryption key, remembered email, and mount settings. "
        "Account passwords and authentication tokens are excluded; a fresh Degoo login is required."
    )
    print("The local cache may contain pending uploads; keep both configuration and cache to recover them.")
    keep_config = _prompt_yes_no("Keep vault, remembered email, and mount configuration?")
    keep_cache = _prompt_yes_no("Keep the local cache?")
    keep_logs = _prompt_yes_no("Keep local logs?")

    retention_directory: Path | None = None
    if keep_config or keep_cache or keep_logs:
        default_directory = _default_retention_directory()
        while retention_directory is None:
            typed_directory = input(
                f"Backup directory path [{default_directory}] "
                "(press Enter for the default; this is not a yes/no question): "
            ).strip()
            try:
                retention_directory = _validate_retention_directory(typed_directory or default_directory, root)
            except CleanupError as exc:
                print(f"That backup path cannot be used: {exc}")
                print(
                    "Your selected retention still needs a valid private backup path. "
                    "Enter another path, or press Ctrl+C to cancel and restart the wizard to change what is kept."
                )
        print(f"Selected local state will be copied to {retention_directory}.")

    discard_secret = False
    if layout["vault_bytes"] is not None and not keep_config and not args.vault_backup:
        answer = input("To delete the only local vault secret, type DISCARD: ").strip()
        if answer != "DISCARD":
            print("Cancelled; the local install was left unchanged.")
            return 1
        discard_secret = True
    answer = input("To remove the local Degoo install, type REMOVE: ").strip()
    if answer != "REMOVE":
        print("Cancelled; the local install was left unchanged.")
        return 1

    args.delete_local = True
    args.keep_config = keep_config
    args.keep_cache = keep_cache
    args.keep_logs = keep_logs
    args.keep_directory = str(retention_directory) if retention_directory is not None else None
    args.discard_vault_secret = discard_secret
    return uninstall(args)


def uninstall(args: argparse.Namespace) -> int:
    root, layout, mounted, entries = _inspect_install(args.root)

    if not args.delete_local:
        _inventory(root, layout["manifest"], entries, mounted)
        return 0

    if layout["cache_dir"] != root / "cache" and _legacy_cache_has_contents(root):
        raise CleanupError(
            "the legacy install cache still contains local files; recover or move that cache before uninstalling"
        )

    _validate_recorded_path_link(root, layout["manifest"])
    direct_service_link, wanted_service_link = _service_paths(root, layout["manifest"])
    service_links, recorded_service_links = _owned_service_links(root, layout["manifest"])
    owned_service_links = [
        path
        for path in (direct_service_link, wanted_service_link)
        if path in service_links or str(path) in recorded_service_links
    ]
    expected_link = expected_path_link()
    owned_path_link = (
        layout["manifest"].get("path_link") == str(expected_link)
        or _path_link_target(expected_link, root)
    )

    keep_config = args.keep_config
    keep_cache = args.keep_cache
    keep_logs = args.keep_logs
    retain_state = keep_config or keep_cache or keep_logs
    retention_directory = None
    if retain_state:
        retention_directory = _validate_retention_directory(args.keep_directory or _default_retention_directory(), root)

    vault_exists = layout["vault_bytes"] is not None
    if args.vault_backup:
        _validate_backup(args.vault_backup, root, layout["vault_bytes"])
    elif vault_exists and not (args.discard_vault_secret or keep_config):
        raise CleanupError("local deletion requires --vault-backup or --discard-vault-secret")

    config_exists = (root / "config/rclone.conf").exists()
    binary_exists = (root / "bin/rclone").exists()
    pending_files = 0
    pending_bytes = 0
    pending_state = False
    if vault_exists and config_exists and binary_exists:
        pending_files, pending_bytes = _pending_uploads(root)
        pending_state = pending_files != 0 or pending_bytes != 0
        if pending_state and not (keep_config and keep_cache):
            raise CleanupError("pending local uploads remain; the install and cache were preserved")
        cache_has_data = _audit_cache_namespace(root, layout["cache_dir"])
        if cache_has_data:
            _verify_cache_identity(root, layout["cache_dir"])
        _validate_vfs_cache_recovery(layout["cache_dir"])

    legacy_mountpoint = installer.configured_mountpoint(root, layout["manifest"]) == root / "mount"
    if not mounted:
        _require_empty_mount_directory(root)
    if not legacy_mountpoint:
        # The external mount is managed separately; root/mount remains part of
        # the install and must be empty before stopping services or removing links.
        _require_empty_contained_mount_directory(root)

    defer_mountpoint_check = mounted and legacy_mountpoint
    if not defer_mountpoint_check:
        _check_mount_directory_removable(root / "mount")

    # Check once before stopping a service, then check again under the cache lock.
    previously_owned_service = _stop_owned_service(
        root,
        layout["manifest"],
        disable=not defer_mountpoint_check,
    )
    if _mount_state(root, read_mountpoints()):
        _unmount(root)
    # Recheck the whole mount namespace after unmounting and before touching the cache.
    if _mount_state(root, read_mountpoints()):
        raise CleanupError("the isolated mount remains active")
    _require_empty_mount_directory(root)
    if defer_mountpoint_check:
        _check_mount_directory_removable(root / "mount")
        previously_owned_service.update(_stop_owned_service(root, layout["manifest"]))

    # The isolated install always binds this cache to the fixed remote name.
    # Holding rclone's flock prevents a new mount from racing the status check.
    lock_fd = _acquire_cache_lock(root, 10 if mounted else 0)
    try:
        pending_files = 0
        pending_bytes = 0
        pending_state = False
        if vault_exists and config_exists and binary_exists:
            pending_files, pending_bytes = _pending_uploads(root)
            pending_state = pending_files != 0 or pending_bytes != 0
            if pending_state and not (keep_config and keep_cache):
                raise CleanupError("pending local uploads remain; the install and cache were preserved")
        elif _cache_has_data(root, _cache_lock_path(root)):
            raise CleanupError("the rclone configuration is incomplete and the cache is not empty")

        # Revalidate protected inputs after unmounting and while holding the VFS lock.
        latest = _validate_layout(root)
        if latest["vault_bytes"] != layout["vault_bytes"]:
            raise CleanupError("the vault configuration changed during cleanup")
        cache_has_data = _audit_cache_namespace(root, latest["cache_dir"])
        if cache_has_data and latest["vault_bytes"] is not None:
            _verify_cache_identity(root, latest["cache_dir"])
        if latest["vault_bytes"] is not None:
            _validate_vfs_cache_recovery(latest["cache_dir"])
        if args.vault_backup:
            _validate_backup(args.vault_backup, root, latest["vault_bytes"])
        final_points = read_mountpoints()
        if _mount_state(root, final_points):
            raise CleanupError("the isolated mount became active during cleanup")
        _require_empty_mount_directory(root)
        _audit_tree(root)
        if retention_directory is not None:
            _retain_state(
                root,
                retention_directory,
                keep_config=keep_config,
                keep_cache=keep_cache,
                keep_logs=keep_logs,
                pending=pending_state,
            )
        _remove_owned_cache(root, latest["cache_dir"], _cache_lock_path(root))
        _make_install_tree_writable(root)
        # Reload and verify systemd while the install layout is still retryable.
        service_cleanup = _remove_owned_service_links(root, latest["manifest"], previously_owned_service)
        path_link_cleanup = _remove_owned_path_link(root, latest["manifest"])
        owned_path_link = owned_path_link or path_link_cleanup
        # Removing the empty mountpoint first makes a concurrent re-mount fail
        # before recursive deletion can traverse a mounted filesystem.
        try:
            os.rmdir(root / "mount")
        except OSError:
            raise CleanupError("the isolated mountpoint could not be safely removed") from None
        if _mount_state(root, read_mountpoints()):
            raise CleanupError("a mount became active during cleanup")
        _remove_root(root)
        _verify_cleanup(root, owned_service_links, owned_path_link)
    finally:
        os.close(lock_fd)

    if pending_state:
        print(
            "Removed the isolated local Degoo install. Pending local uploads "
            f"({pending_files} files, {pending_bytes} bytes) were preserved in {retention_directory}. "
            "To resume them, run `./contrib/degoo/degoo-rc install --wizard "
            f"--restore-directory {shlex.quote(str(retention_directory))}` from the project checkout "
            "or use the extracted package's degoo-rc installer. "
            "A fresh Degoo login is required after restoration; only the remembered email is retained. "
            "Degoo remote data was left untouched."
        )
    else:
        print("Removed the isolated local Degoo install. Remote encrypted files were left untouched.")
    if retention_directory is not None:
        retained: list[str] = []
        if keep_config:
            retained.append("configuration (vault key, remembered email, and mount settings; log in again)")
        if keep_cache:
            retained.append("local cache")
        if keep_logs:
            retained.append("local logs")
        print(f"Private backup created at {retention_directory}; retained: {', '.join(retained)} plus install metadata.")
        removed = [name for name, keep in (("configuration", keep_config), ("cache", keep_cache), ("logs", keep_logs)) if not keep]
        if removed:
            print(f"Unselected local state removed: {', '.join(removed)}.")
    else:
        print(
            "No retention directory was created. The local configuration, cache, and logs were removed."
        )
    if service_cleanup:
        print("The owned user service is confirmed inactive; its links were removed and systemd was reloaded.")
    else:
        print("No owned user service links were configured.")
    if path_link_cleanup:
        print("The owned ~/.local/bin/degoo-rc command link was removed.")
    else:
        print("No owned ~/.local/bin/degoo-rc command link was configured.")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(expected_root()), help="isolated install root (defaults to ~/.local/share/rclone-degoo)")
    actions = parser.add_mutually_exclusive_group()
    actions.add_argument("--delete-local", action="store_true", help="unmount safely and remove the isolated local install")
    actions.add_argument("--inventory", "--dry-run", dest="inventory", action="store_true", help="inventory the install without changing it")
    actions.add_argument("--wizard", action="store_true", help="interactively choose local state to retain and remove the install")
    gate = parser.add_mutually_exclusive_group()
    gate.add_argument("--vault-backup", help="private backup file that exactly matches config/vault.json")
    gate.add_argument("--discard-vault-secret", action="store_true", help="explicitly allow deleting the only local vault secret")
    parser.add_argument("--keep-config", action="store_true", help="copy vault, Degoo login, and mount configuration to a private retention directory")
    parser.add_argument("--keep-cache", action="store_true", help="copy the local cache to a private retention directory")
    parser.add_argument("--keep-logs", action="store_true", help="copy local logs to a private retention directory")
    parser.add_argument("--keep-directory", help="new external private directory for retained state (defaults to a timestamped path)")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if (args.vault_backup or args.discard_vault_secret) and not args.delete_local:
        parser.error("--vault-backup and --discard-vault-secret require --delete-local")
    keep_selected = args.keep_config or args.keep_cache or args.keep_logs
    if args.keep_directory and not keep_selected:
        parser.error("--keep-directory requires at least one --keep-* option")
    if (keep_selected or args.keep_directory) and not args.delete_local and not args.wizard:
        parser.error("--keep-* options require --delete-local or --wizard")
    if args.discard_vault_secret and args.keep_config:
        parser.error("--discard-vault-secret cannot be combined with --keep-config")
    if args.wizard or (not args.delete_local and not args.inventory and sys.stdin.isatty() and sys.stdout.isatty()):
        try:
            return _wizard(args)
        except CleanupError as exc:
            print(_format_failure(args, str(exc)), file=sys.stderr)
            return 2
        except OSError:
            print(_format_failure(args, "filesystem access failed safely"), file=sys.stderr)
            return 2
        except (EOFError, KeyboardInterrupt):
            if args.delete_local:
                print(_format_failure(args, "cleanup was interrupted"), file=sys.stderr)
                return 2
            print("Cancelled; the local install was left unchanged.")
            return 1
    try:
        return uninstall(args)
    except CleanupError as exc:
        print(_format_failure(args, str(exc)), file=sys.stderr)
        return 2
    except OSError:
        print(_format_failure(args, "filesystem access failed safely"), file=sys.stderr)
        return 2


def _format_failure(args: argparse.Namespace, reason: str) -> str:
    """Describe whether a requested removal left its local install behind."""
    if not args.delete_local:
        return f"uninstall refused: {reason}"
    try:
        root_remains = _entry_exists(Path(args.root))
    except CleanupError:
        return f"uninstall incomplete: {reason}; the local install root could not be verified"
    if root_remains:
        return f"uninstall incomplete: {reason}; local install remains at {args.root}"
    return f"uninstall incomplete: {reason}; the install root is absent"


if __name__ == "__main__":
    raise SystemExit(main())
