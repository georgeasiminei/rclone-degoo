#!/usr/bin/env python3
"""Validate and restore private state retained by the Degoo uninstaller."""

from __future__ import annotations

import configparser
from dataclasses import dataclass, field
import errno
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile
from typing import Any

import install as installer


MAX_MANIFEST_SIZE = 64 * 1024
MAX_PRIVATE_CONFIG_SIZE = 1024 * 1024
MAX_IDENTITY_SIZE = 128
MAX_LOCK_SIZE = 4096
IDENTITY_PATTERN = re.compile(rb"[0-9a-f]{64}\n?")
CONFIG_FILES = {"auth.json", "login.json", "mount.json", "rclone.conf", "vault.json"}
SPARSE_SEEK_UNSUPPORTED = {
    errno.EINVAL,
    errno.ENOSYS,
    getattr(errno, "ENOTSUP", errno.EINVAL),
    getattr(errno, "EOPNOTSUPP", errno.EINVAL),
    getattr(errno, "ENOTTY", errno.EINVAL),
}
MANIFEST_PATHS = {
    "binary": "bin/rclone",
    "vault_config": "config/vault.json",
    "rclone_config": "config/rclone.conf",
    "cache_dir": "cache",
    "pidfile": "mount.pid",
}


class RestoreError(Exception):
    """A safe refusal to inspect or restore retained Degoo state."""


@dataclass(frozen=True)
class Backup:
    """Validated settings and private state available in a retained backup."""

    directory: Path
    folder_id: str
    folder_name: str
    parent_id: str
    mount_settings: dict[str, Any]
    vault_config_path: Path | None
    vault_settings: dict[str, Any] | None = field(repr=False)
    auth_bytes: bytes | None = field(repr=False)
    login_bytes: bytes | None = field(repr=False)
    cache_path: Path | None
    cache_has_data: bool
    _cache_snapshot: tuple[tuple[Any, ...], ...] = field(default=(), repr=False, compare=False)


def _absolute_path(value: str | os.PathLike[str], label: str) -> Path:
    """Return a normalized absolute path without traversal components."""
    path = Path(os.path.expanduser(os.fspath(value)))
    if not path.is_absolute() or ".." in path.parts:
        raise RestoreError(f"the {label} must be an absolute path without parent traversal")
    return Path(os.path.normpath(str(path)))


def _check_components(path: Path, *, missing_ok: bool = False) -> None:
    """Reject symlink components and non-directory path ancestors."""
    current = Path(path.anchor)
    for index, part in enumerate(path.parts[1:]):
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            if missing_ok:
                return
            raise RestoreError("a required backup path is missing") from None
        except OSError:
            raise RestoreError("a backup path could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode):
            raise RestoreError("symlink path components are not allowed")
        if index < len(path.parts[1:]) - 1 and not stat.S_ISDIR(info.st_mode):
            raise RestoreError("a backup path ancestor is not a directory")


def _check_directory(path: Path, *, private: bool = False) -> os.stat_result:
    """Validate an owned real directory and return its lstat result."""
    _check_components(path)
    try:
        info = os.lstat(path)
    except OSError:
        raise RestoreError("a required backup directory is unavailable") from None
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise RestoreError("a required backup path is not a directory")
    if info.st_uid != os.getuid() or info.st_mode & 0o022:
        raise RestoreError("a backup directory has unsafe ownership or permissions")
    if private and info.st_mode & 0o077:
        raise RestoreError("a private backup directory must use mode 0700")
    return info


def _read_private_file(path: Path, maximum: int, *, missing_ok: bool = False) -> bytes | None:
    """Read a bounded private regular file through the install safety checks."""
    try:
        return installer.read_private_file(path, maximum, missing_ok=missing_ok)
    except installer.InstallError:
        raise RestoreError("a retained private file is missing, invalid, or unsafe") from None


def _parse_object(data: bytes, message: str) -> dict[str, Any]:
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise RestoreError(message) from None
    if not isinstance(value, dict):
        raise RestoreError(message)
    return value


def _validate_manifest(root: Path) -> tuple[dict[str, Any], str, str, str]:
    """Read only the remote defaults needed for a fresh install."""
    raw = _read_private_file(root / "install.json", MAX_MANIFEST_SIZE)
    assert raw is not None
    manifest = _parse_object(raw, "the retained install metadata is invalid")
    if type(manifest.get("version")) is not int or manifest["version"] != 1:
        raise RestoreError("the retained install metadata version is unsupported")
    if manifest.get("remote_name") != installer.REMOTE_NAME:
        raise RestoreError("the retained metadata does not identify this Degoo install")
    for field_name, expected in MANIFEST_PATHS.items():
        if manifest.get(field_name) != expected:
            raise RestoreError("the retained metadata contains an unexpected local path")
    try:
        installer.configured_mountpoint(root, manifest)
    except installer.InstallError as exc:
        raise RestoreError(str(exc)) from None
    if not isinstance(manifest.get("remote_created_by_setup"), bool):
        raise RestoreError("the retained install metadata is incomplete")

    folder_id = manifest.get("root_folder_id", "")
    folder_name = manifest.get("remote_folder_name", "")
    parent_id = manifest.get("parent_folder_id", "")
    if not isinstance(folder_id, str) or not isinstance(folder_name, str) or not isinstance(parent_id, str):
        raise RestoreError("the retained remote folder defaults are invalid")
    if folder_id:
        try:
            if installer.validate_folder_id(folder_id) != folder_id:
                raise RestoreError("the retained remote folder ID is invalid")
        except installer.InstallError:
            raise RestoreError("the retained remote folder ID is invalid") from None
    if folder_name:
        try:
            if installer.validate_folder_name(folder_name) != folder_name:
                raise RestoreError("the retained remote folder name is invalid")
        except installer.InstallError:
            raise RestoreError("the retained remote folder name is invalid") from None
    if parent_id:
        try:
            if installer.validate_folder_id(parent_id) != parent_id:
                raise RestoreError("the retained parent folder ID is invalid")
        except installer.InstallError:
            raise RestoreError("the retained parent folder ID is invalid") from None
    if folder_name and not folder_id:
        raise RestoreError("the retained folder name has no matching folder ID")
    return manifest, folder_id, folder_name, parent_id


def _validate_config_directory(path: Path) -> None:
    """Reject unrecognized config entries, links, and insecure secret files."""
    _check_directory(path, private=True)
    try:
        entries = sorted(path.iterdir(), key=lambda item: item.name)
    except OSError:
        raise RestoreError("the retained config directory could not be inspected") from None
    if any(entry.name not in CONFIG_FILES for entry in entries):
        raise RestoreError("the retained config directory contains unsupported state")
    for entry in entries:
        _check_components(entry)
        try:
            info = os.lstat(entry)
        except OSError:
            raise RestoreError("a retained config file could not be inspected safely") from None
        if (
            not stat.S_ISREG(info.st_mode)
            or info.st_uid != os.getuid()
            or info.st_mode & 0o077
            or info.st_mode & 0o6000
            or info.st_nlink != 1
        ):
            raise RestoreError("a retained config file has unsafe ownership or permissions")


def _validate_rclone_config(raw: bytes, folder_id: str) -> None:
    """Check the saved remote without carrying its old auth path forward."""
    parser = configparser.ConfigParser(interpolation=None, strict=True)
    try:
        parser.read_string(raw.decode("utf-8"))
    except (UnicodeDecodeError, configparser.Error):
        raise RestoreError("the retained Degoo remote config is invalid") from None
    if parser.sections() != [installer.REMOTE_NAME]:
        raise RestoreError("the retained Degoo remote config is incomplete")
    section = parser[installer.REMOTE_NAME]
    if set(section) != {"type", "root_folder_id", "auth_file"}:
        raise RestoreError("the retained Degoo remote config contains unsupported settings")
    auth_path_text = section.get("auth_file", "")
    auth_path = Path(auth_path_text)
    if (
        section.get("type") != "degoo"
        or section.get("root_folder_id") != folder_id
        or not auth_path.is_absolute()
        or ".." in auth_path.parts
        or auth_path.name != "auth.json"
        or auth_path.parent.name != "config"
        or any(ord(character) < 0x20 or ord(character) == 0x7F for character in auth_path_text)
    ):
        raise RestoreError("the retained Degoo remote config points to another folder or unsafe token path")


def _validate_auth(data: bytes) -> bytes:
    auth = _parse_object(data, "the retained account authentication state is invalid")
    if set(auth) - {"token", "refresh_token"}:
        raise RestoreError("the retained account authentication state has unsupported fields")
    if any(not isinstance(auth.get(key, ""), str) for key in ("token", "refresh_token")):
        raise RestoreError("the retained account authentication state is invalid")
    if not any(auth.get(key, "") for key in ("token", "refresh_token")):
        raise RestoreError("the retained account authentication state is empty")
    return data


def _validate_login(data: bytes) -> bytes:
    login = _parse_object(data, "the retained account email is invalid")
    email = login.get("email")
    if set(login) != {"email"} or not isinstance(email, str) or not email.strip() or any(
        character in email for character in "\r\n\x00"
    ):
        raise RestoreError("the retained account email is invalid")
    return data


def _cache_name() -> str:
    digest = hashlib.sha256(f"{installer.REMOTE_NAME}:".encode()).hexdigest()[:24]
    return f"degoo-vault-{digest}"


def _stamp(info: os.stat_result) -> tuple[int, ...]:
    return (
        info.st_dev,
        info.st_ino,
        info.st_mode,
        info.st_size,
        info.st_mtime_ns,
        info.st_ctime_ns,
    )


def _private_cache_directory(path: Path) -> os.stat_result:
    """Validate a cache directory that can contain decrypted local files."""
    info = _check_directory(path, private=True)
    if info.st_mode & 0o6000:
        raise RestoreError("a retained cache directory has unsafe special permissions")
    return info


def _private_cache_file(path: Path) -> os.stat_result:
    """Validate a private regular cache file and return its lstat result."""
    _check_components(path)
    try:
        info = os.lstat(path)
    except OSError:
        raise RestoreError("a retained cache file could not be inspected safely") from None
    if (
        not stat.S_ISREG(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
        or info.st_mode & 0o6000
        or info.st_nlink != 1
    ):
        raise RestoreError("a retained cache file has unsafe ownership or permissions")
    return info


def _scan_cache_data(path: Path, relative: str, snapshot: dict[str, tuple[int, ...]]) -> bool:
    """Audit an allowed VFS cache subtree and report whether it contains entries."""
    info = _private_cache_directory(path)
    snapshot[relative] = _stamp(info)
    try:
        entries = sorted(path.iterdir(), key=lambda item: item.name)
    except OSError:
        raise RestoreError("the retained cache directory could not be inspected") from None
    has_entries = bool(entries)
    for entry in entries:
        child_relative = f"{relative}/{entry.name}"
        _check_components(entry)
        try:
            child_info = os.lstat(entry)
        except OSError:
            raise RestoreError("a retained cache entry could not be inspected safely") from None
        if stat.S_ISDIR(child_info.st_mode):
            has_entries = _scan_cache_data(entry, child_relative, snapshot) or has_entries
        elif stat.S_ISREG(child_info.st_mode):
            if _private_cache_file(entry) != child_info:
                raise RestoreError("a retained cache file changed while being inspected")
            snapshot[child_relative] = _stamp(child_info)
        else:
            raise RestoreError("symbolic links and special cache files are not allowed")
    return has_entries


def _scan_cache(path: Path) -> tuple[bool, tuple[tuple[Any, ...], ...]]:
    """Audit the rclone cache layout and capture a race-check snapshot."""
    root_info = _private_cache_directory(path)
    snapshot: dict[str, tuple[int, ...]] = {"": _stamp(root_info)}
    cache_name = _cache_name()
    identity_name = f"{cache_name}.identity"
    lock_name = f"{cache_name}.lock"
    try:
        entries = sorted(path.iterdir(), key=lambda item: item.name)
    except OSError:
        raise RestoreError("the retained cache could not be inspected") from None
    has_data = False
    for entry in entries:
        if entry.name == identity_name:
            info = _private_cache_file(entry)
            raw = _read_private_file(entry, MAX_IDENTITY_SIZE)
            assert raw is not None
            if not IDENTITY_PATTERN.fullmatch(raw):
                raise RestoreError("the retained cache identity file is invalid")
            snapshot[entry.name] = _stamp(info)
        elif entry.name == lock_name:
            info = _private_cache_file(entry)
            if info.st_size > MAX_LOCK_SIZE:
                raise RestoreError("the retained cache lock file is invalid")
            snapshot[entry.name] = _stamp(info)
        elif entry.name in {"vfs", "vfsMeta"}:
            top_info = _private_cache_directory(entry)
            snapshot[entry.name] = _stamp(top_info)
            try:
                children = sorted(entry.iterdir(), key=lambda item: item.name)
            except OSError:
                raise RestoreError("the retained cache directory could not be inspected") from None
            if any(child.name != cache_name for child in children):
                raise RestoreError("the retained cache contains an unknown remote path")
            if children:
                has_data = _scan_cache_data(children[0], f"{entry.name}/{cache_name}", snapshot) or has_data
        else:
            raise RestoreError("the retained cache contains an unknown path")
    if has_data and identity_name not in {entry.name for entry in entries}:
        raise RestoreError("the retained cache data has no matching identity file")
    return has_data, tuple(sorted((name, *value) for name, value in snapshot.items()))


def _validate_target_path(path: Path, label: str) -> Path:
    normalized = _absolute_path(path, label)
    _check_components(normalized, missing_ok=True)
    return normalized


def inspect_backup(path: str | os.PathLike[str], target_root: str | os.PathLike[str]) -> Backup:
    """Inspect a retained directory or private install root without changing it."""
    root = _absolute_path(path, "backup directory")
    target = _validate_target_path(Path(target_root), "install root")
    _check_directory(root, private=True)
    _, folder_id, folder_name, parent_id = _validate_manifest(root)

    config_dir = root / "config"
    config_exists = os.path.lexists(config_dir)
    if config_exists:
        _validate_config_directory(config_dir)
    elif not folder_id:
        # A partial backup containing only logs or metadata remains useful as
        # folder-selection defaults, even if it has no portable vault secret.
        pass

    vault_config_path: Path | None = None
    vault_settings: dict[str, Any] | None = None
    auth_bytes: bytes | None = None
    login_bytes: bytes | None = None
    mount_settings = dict(installer.DEFAULT_MOUNT_SETTINGS)
    rclone_bytes: bytes | None = None
    if config_exists:
        vault_raw = _read_private_file(config_dir / "vault.json", installer.MAX_CONFIG_SIZE, missing_ok=True)
        if vault_raw is not None:
            vault_config_path = config_dir / "vault.json"
            try:
                source_vault = installer.parse_json_object(vault_raw, "invalid")
                source_cache_dir = source_vault.get("cache_dir")
                if (
                    not isinstance(source_cache_dir, str)
                    or not Path(source_cache_dir).is_absolute()
                    or ".." in Path(source_cache_dir).parts
                    or any(character in source_cache_dir for character in "\r\n\x00")
                ):
                    raise RestoreError("the retained vault config has an unsafe cache path")
                normalized = installer.import_vault_config(str(vault_config_path), target)
                vault_settings = installer.parse_json_object(normalized, "invalid")
            except installer.InstallError:
                raise RestoreError("the retained vault configuration is invalid or unsafe") from None
            except RestoreError:
                raise
            if source_vault.get("remote") != f"{installer.REMOTE_NAME}:":
                raise RestoreError("the retained vault config does not use the contained Degoo remote")
        auth_raw = _read_private_file(config_dir / "auth.json", installer.MAX_CONFIG_SIZE, missing_ok=True)
        if auth_raw is not None:
            auth_bytes = _validate_auth(auth_raw)
        login_raw = _read_private_file(config_dir / "login.json", installer.MAX_CONFIG_SIZE, missing_ok=True)
        if login_raw is not None:
            login_bytes = _validate_login(login_raw)
        mount_raw = _read_private_file(config_dir / "mount.json", installer.MAX_CONFIG_SIZE, missing_ok=True)
        if mount_raw is not None:
            try:
                installer.validate_mount_settings(mount_raw)
            except installer.InstallError:
                raise RestoreError("the retained mount settings are invalid") from None
            mount_settings = installer.parse_json_object(mount_raw, "the retained mount settings are invalid")
        rclone_bytes = _read_private_file(config_dir / "rclone.conf", installer.MAX_CONFIG_SIZE, missing_ok=True)
        if rclone_bytes is not None:
            if not folder_id:
                raise RestoreError("the retained remote config has no matching folder ID")
            _validate_rclone_config(rclone_bytes, folder_id)

    cache_path = root / "cache"
    cache_exists = os.path.lexists(cache_path)
    cache_has_data = False
    cache_snapshot: tuple[tuple[Any, ...], ...] = ()
    if cache_exists:
        cache_has_data, cache_snapshot = _scan_cache(cache_path)
    if cache_has_data:
        if vault_config_path is None or vault_settings is None:
            raise RestoreError("nonempty retained cache data requires its matching vault configuration")
        if not folder_id or rclone_bytes is None:
            raise RestoreError("nonempty retained cache data requires its matching Degoo folder configuration")
        if vault_settings.get("remote") != f"{installer.REMOTE_NAME}:":
            raise RestoreError("the retained cache has no matching contained remote")

    return Backup(
        directory=root,
        folder_id=folder_id,
        folder_name=folder_name,
        parent_id=parent_id,
        mount_settings=mount_settings,
        vault_config_path=vault_config_path,
        vault_settings=vault_settings,
        auth_bytes=auth_bytes,
        login_bytes=login_bytes,
        cache_path=cache_path if cache_exists else None,
        cache_has_data=cache_has_data,
        _cache_snapshot=cache_snapshot,
    )


def _same_inspection(left: Backup, right: Backup) -> bool:
    """Report whether a previously shown backup still matches disk state."""
    return (
        left.directory == right.directory
        and left.folder_id == right.folder_id
        and left.folder_name == right.folder_name
        and left.parent_id == right.parent_id
        and left.mount_settings == right.mount_settings
        and left.vault_config_path == right.vault_config_path
        and left.vault_settings == right.vault_settings
        and left.auth_bytes == right.auth_bytes
        and left.login_bytes == right.login_bytes
        and left.cache_path == right.cache_path
        and left.cache_has_data == right.cache_has_data
        and left._cache_snapshot == right._cache_snapshot
    )


def _open_directory(path: Path) -> int:
    """Open a private directory without following a symlink."""
    _check_components(path)
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise RestoreError("a retained cache directory could not be opened safely") from None
    info = os.fstat(fd)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
        os.close(fd)
        raise RestoreError("a retained cache directory has unsafe ownership or permissions")
    return fd


def _lock_source_cache(cache_path: Path) -> int | None:
    """Lock an existing source cache lock file without creating source state."""
    root_fd = _open_directory(cache_path)
    lock_name = f"{_cache_name()}.lock"
    try:
        try:
            before = os.stat(lock_name, dir_fd=root_fd, follow_symlinks=False)
        except FileNotFoundError:
            return None
        except OSError:
            raise RestoreError("the retained cache lock could not be inspected safely") from None
        if (
            not stat.S_ISREG(before.st_mode)
            or before.st_uid != os.getuid()
            or before.st_mode & 0o077
            or before.st_mode & 0o6000
            or before.st_nlink != 1
            or before.st_size > MAX_LOCK_SIZE
        ):
            raise RestoreError("the retained cache lock file has unsafe ownership or permissions")
        flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        try:
            lock_fd = os.open(lock_name, flags, dir_fd=root_fd)
        except OSError:
            raise RestoreError("the retained cache lock could not be opened safely") from None
        try:
            opened = os.fstat(lock_fd)
            if _stamp(opened) != _stamp(before) or not stat.S_ISREG(opened.st_mode):
                raise RestoreError("the retained cache lock changed while being opened")
            try:
                fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise RestoreError("the retained cache is active and cannot be restored safely") from None
            except OSError:
                raise RestoreError("the retained cache could not be locked safely") from None
            return lock_fd
        except BaseException:
            os.close(lock_fd)
            raise
    finally:
        os.close(root_fd)


def _copy_file_at(source_fd: int, destination_fd: int, name: str) -> None:
    """Copy one regular file to an exclusive owner-private destination."""
    try:
        before = os.stat(name, dir_fd=source_fd, follow_symlinks=False)
    except OSError:
        raise RestoreError("a retained cache file changed while being inspected") from None
    if (
        not stat.S_ISREG(before.st_mode)
        or before.st_uid != os.getuid()
        or before.st_mode & 0o077
        or before.st_mode & 0o6000
        or before.st_nlink != 1
    ):
        raise RestoreError("a retained cache file has unsafe ownership or permissions")
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        source_file = os.open(name, flags, dir_fd=source_fd)
    except OSError:
        raise RestoreError("a retained cache file could not be opened safely") from None
    try:
        opened = os.fstat(source_file)
        if not stat.S_ISREG(opened.st_mode) or _stamp(opened) != _stamp(before):
            raise RestoreError("a retained cache file changed while being opened")
        output_flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
        try:
            destination_file = os.open(name, output_flags, 0o600, dir_fd=destination_fd)
        except OSError:
            raise RestoreError("a cache file could not be created safely") from None
        try:
            if not _copy_sparse_file(source_file, destination_file, opened.st_size):
                _copy_dense_file(source_file, destination_file, opened.st_size)
            after = os.fstat(source_file)
            if _stamp(opened) != _stamp(after):
                raise RestoreError("a retained cache file changed while being copied")
            os.fchmod(destination_file, 0o600)
            os.fsync(destination_file)
        finally:
            os.close(destination_file)
    finally:
        os.close(source_file)


def _write_file_region(source_fd: int, destination_fd: int, start: int, end: int) -> None:
    """Copy a bounded file region, handling short writes until it is complete."""
    os.lseek(source_fd, start, os.SEEK_SET)
    os.lseek(destination_fd, start, os.SEEK_SET)
    remaining = end - start
    while remaining:
        chunk = os.read(source_fd, min(1024 * 1024, remaining))
        if not chunk:
            raise RestoreError("a retained cache file changed while being copied")
        view = memoryview(chunk)
        while view:
            written = os.write(destination_fd, view)
            if written <= 0:
                raise RestoreError("a retained cache file could not be copied completely")
            view = view[written:]
        remaining -= len(chunk)


def _copy_sparse_file(source_fd: int, destination_fd: int, size: int) -> bool:
    """Copy allocated extents and leave holes sparse; return false if unsupported."""
    seek_data = getattr(os, "SEEK_DATA", None)
    seek_hole = getattr(os, "SEEK_HOLE", None)
    if seek_data is None or seek_hole is None:
        return False
    offset = 0
    while offset < size:
        try:
            data = os.lseek(source_fd, offset, seek_data)
        except OSError as exc:
            if exc.errno == errno.ENXIO:
                break
            if exc.errno in SPARSE_SEEK_UNSUPPORTED:
                return False
            raise RestoreError("a retained cache file could not be inspected for sparse extents") from None
        if data < offset or data >= size:
            if data >= size:
                break
            return False
        try:
            hole = os.lseek(source_fd, data, seek_hole)
        except OSError as exc:
            if exc.errno in SPARSE_SEEK_UNSUPPORTED or exc.errno == errno.ENXIO:
                return False
            raise RestoreError("a retained cache file could not be inspected for sparse extents") from None
        if hole <= data:
            return False
        end = min(hole, size)
        _write_file_region(source_fd, destination_fd, data, end)
        offset = end
    try:
        os.ftruncate(destination_fd, size)
    except OSError:
        raise RestoreError("a retained cache file could not be sized safely") from None
    return True


def _copy_dense_file(source_fd: int, destination_fd: int, size: int) -> None:
    """Copy a complete file densely when sparse extent seeking is unavailable."""
    try:
        os.ftruncate(destination_fd, 0)
        os.lseek(source_fd, 0, os.SEEK_SET)
        os.lseek(destination_fd, 0, os.SEEK_SET)
    except OSError:
        raise RestoreError("a retained cache file could not be prepared for copying") from None
    remaining = size
    while remaining:
        chunk = os.read(source_fd, min(1024 * 1024, remaining))
        if not chunk:
            raise RestoreError("a retained cache file changed while being copied")
        view = memoryview(chunk)
        while view:
            written = os.write(destination_fd, view)
            if written <= 0:
                raise RestoreError("a retained cache file could not be copied completely")
            view = view[written:]
        remaining -= len(chunk)
    try:
        os.ftruncate(destination_fd, size)
    except OSError:
        raise RestoreError("a retained cache file could not be sized safely") from None


def _copy_directory_contents(source_fd: int, destination_fd: int) -> None:
    """Copy a checked cache directory tree with no-follow opens."""
    try:
        with os.scandir(source_fd) as iterator:
            children = sorted(iterator, key=lambda entry: entry.name)
    except OSError:
        raise RestoreError("a retained cache directory could not be inspected") from None
    for child in children:
        try:
            info = child.stat(follow_symlinks=False)
        except OSError:
            raise RestoreError("a retained cache entry changed while being inspected") from None
        if stat.S_ISDIR(info.st_mode):
            if info.st_uid != os.getuid() or info.st_mode & 0o077 or info.st_mode & 0o6000:
                raise RestoreError("a retained cache directory has unsafe ownership or permissions")
            source_child = -1
            destination_child = -1
            try:
                source_child = os.open(
                    child.name,
                    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
                    dir_fd=source_fd,
                )
                opened = os.fstat(source_child)
                if _stamp(opened) != _stamp(info):
                    raise RestoreError("a retained cache directory changed while being opened")
                os.mkdir(child.name, 0o700, dir_fd=destination_fd)
                destination_child = os.open(
                    child.name,
                    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
                    dir_fd=destination_fd,
                )
                _copy_directory_contents(source_child, destination_child)
            except OSError:
                raise RestoreError("a retained cache directory could not be copied safely") from None
            finally:
                if source_child >= 0:
                    os.close(source_child)
                if destination_child >= 0:
                    os.close(destination_child)
        elif stat.S_ISREG(info.st_mode):
            _copy_file_at(source_fd, destination_fd, child.name)
        else:
            raise RestoreError("symbolic links and special cache files are not allowed")
    try:
        os.fsync(destination_fd)
    except OSError:
        raise RestoreError("the staged cache could not be flushed safely") from None


def _copy_cache(cache_path: Path, destination: Path) -> None:
    """Copy the audited cache data and identity, omitting the stale lock file."""
    source_fd = _open_directory(cache_path)
    destination_fd = _open_directory(destination)
    try:
        with os.scandir(source_fd) as iterator:
            entries = sorted(iterator, key=lambda entry: entry.name)
        for entry in entries:
            if entry.name.endswith(".lock"):
                continue
            info = entry.stat(follow_symlinks=False)
            if stat.S_ISDIR(info.st_mode):
                source_child = os.open(
                    entry.name,
                    os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
                    dir_fd=source_fd,
                )
                try:
                    if _stamp(os.fstat(source_child)) != _stamp(info):
                        raise RestoreError("a retained cache directory changed while being copied")
                    os.mkdir(entry.name, 0o700, dir_fd=destination_fd)
                    destination_child = os.open(
                        entry.name,
                        os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
                        dir_fd=destination_fd,
                    )
                    try:
                        _copy_directory_contents(source_child, destination_child)
                    finally:
                        os.close(destination_child)
                finally:
                    os.close(source_child)
            elif stat.S_ISREG(info.st_mode):
                _copy_file_at(source_fd, destination_fd, entry.name)
            else:
                raise RestoreError("symbolic links and special cache files are not allowed")
        try:
            os.fsync(destination_fd)
        except OSError:
            raise RestoreError("the staged cache could not be flushed safely") from None
    except RestoreError:
        raise
    except OSError:
        raise RestoreError("the retained cache could not be copied safely") from None
    finally:
        os.close(source_fd)
        os.close(destination_fd)


def _restore_cache_binding(backup: Backup, root: Path) -> None:
    """Ask the bundled rclone command to verify and rebind the copied identity."""
    binary = root / "bin" / "rclone"
    _check_components(binary)
    try:
        info = os.lstat(binary)
    except OSError:
        raise RestoreError("the contained rclone executable is unavailable for cache recovery") from None
    if (
        not stat.S_ISREG(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o022
        or info.st_mode & 0o6000
        or not info.st_mode & 0o100
    ):
        raise RestoreError("the contained rclone executable is unsafe for cache recovery")
    assert backup.vault_config_path is not None
    command = [
        str(binary),
        "--config",
        str(root / "config" / "rclone.conf"),
        "degoo",
        "restore-cache",
        str(backup.vault_config_path),
        str(root / "config" / "vault.json"),
        "--folder-id",
        backup.folder_id,
    ]
    env = {
        "HOME": os.path.expanduser("~"),
        "XDG_CONFIG_HOME": str(root / "config"),
        "XDG_CACHE_HOME": str(root / "cache"),
        "TMPDIR": str(root / "tmp"),
        "PATH": "/usr/bin:/bin",
        "LANG": "C.UTF-8",
    }
    try:
        result = subprocess.run(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=60,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise RestoreError(
            f"the cache was copied to {root / 'cache'} with its saved identity, but local cache rebinding did not finish; keep the backup and retry recovery before mounting"
        ) from None
    if result.returncode != 0:
        raise RestoreError(
            f"the cache was copied to {root / 'cache'} with its saved identity, but local cache rebinding was refused; keep the backup and retry recovery before mounting"
        )


def restore_to_install(backup: Backup, root: str | os.PathLike[str]) -> None:
    """Restore saved login state and, when present, rebind a cache into a fresh install."""
    target = _absolute_path(root, "install root")
    try:
        common = os.path.commonpath((str(backup.directory), str(target)))
    except ValueError:
        raise RestoreError("the retained backup and install paths are incompatible") from None
    if common in {str(target), str(backup.directory)}:
        raise RestoreError("the retained backup must be outside the install root")

    source_lock: int | None = None
    if backup.cache_has_data:
        source_lock = _lock_source_cache(backup.directory / "cache")
    try:
        _restore_to_install_locked(backup, target)
    finally:
        if source_lock is not None:
            os.close(source_lock)


def _restore_to_install_locked(backup: Backup, target: Path) -> None:
    """Restore while holding any existing source cache lock."""
    try:
        current = inspect_backup(backup.directory, target)
    except RestoreError:
        raise
    if not _same_inspection(backup, current):
        raise RestoreError("the retained backup changed after it was selected; inspect it again before restoring")

    _check_directory(target, private=True)
    config_dir = target / "config"
    cache_dir = target / "cache"
    _check_directory(config_dir, private=True)
    _check_directory(cache_dir, private=True)

    if backup.vault_settings is not None:
        target_vault_bytes = _read_private_file(config_dir / "vault.json", installer.MAX_CONFIG_SIZE)
        assert target_vault_bytes is not None
        try:
            target_vault, _ = installer.read_vault(target_vault_bytes, target, imported=False)
        except installer.InstallError:
            raise RestoreError("the staged vault configuration is invalid") from None
        if target_vault != backup.vault_settings:
            raise RestoreError("the staged vault configuration does not match the selected backup")
    if backup.folder_id:
        target_rclone_bytes = _read_private_file(config_dir / "rclone.conf", installer.MAX_CONFIG_SIZE)
        assert target_rclone_bytes is not None
        try:
            installer.validate_rclone_config(target_rclone_bytes, target, backup.folder_id)
        except installer.InstallError:
            raise RestoreError("the staged Degoo remote config does not match this install") from None

    destinations: list[tuple[Path, bytes]] = []
    if backup.auth_bytes is not None:
        destinations.append((config_dir / "auth.json", backup.auth_bytes))
    if backup.login_bytes is not None:
        destinations.append((config_dir / "login.json", backup.login_bytes))
    missing_account_files: list[tuple[Path, bytes]] = []
    for path, data in destinations:
        if not os.path.lexists(path):
            missing_account_files.append((path, data))
            continue
        existing = _read_private_file(path, installer.MAX_CONFIG_SIZE)
        if existing != data:
            raise RestoreError("refusing to replace different account state already present in the install")
    if backup.cache_has_data:
        try:
            existing_cache_entries = list(cache_dir.iterdir())
        except OSError:
            raise RestoreError("the staged cache could not be inspected safely") from None
        if existing_cache_entries:
            raise RestoreError("refusing to merge retained data into a nonempty install cache")

    try:
        for path, data in missing_account_files:
            installer.write_private_file(path, data)
    except installer.InstallError:
        raise RestoreError("saved account state could not be restored safely") from None

    if not backup.cache_has_data:
        return
    assert backup.cache_path is not None
    stage = Path(tempfile.mkdtemp(prefix=".restore-cache-", dir=target))
    os.chmod(stage, 0o700)
    try:
        try:
            _copy_cache(backup.cache_path, stage)
            copied_has_data, _ = _scan_cache(stage)
        except RestoreError as exc:
            raise RestoreError(f"{exc}; partial cache data is staged at {stage} and the original backup remains intact") from None
        if not copied_has_data:
            raise RestoreError(f"the staged cache at {stage} is missing retained data; the original backup remains intact")
        if not _same_inspection(backup, inspect_backup(backup.directory, target)):
            raise RestoreError(f"the backup changed during copying; staged data remains at {stage} and the backup is intact")
        try:
            os.replace(stage, cache_dir)
        except OSError:
            raise RestoreError(f"the retained cache is staged at {stage}; the original backup remains intact") from None
        try:
            root_fd = _open_directory(target)
            try:
                os.fsync(root_fd)
            finally:
                os.close(root_fd)
        except RestoreError:
            raise RestoreError(
                f"the cache was copied to {cache_dir} with its saved identity, but the install directory could not be flushed; keep the backup"
            ) from None
        _restore_cache_binding(backup, target)
    except RestoreError:
        # An incomplete staging directory is private and may contain recovery
        # data, so leave it in place for inspection instead of deleting it.
        raise
    except OSError:
        raise RestoreError(f"cache restoration stopped; any staged files remain under {target}") from None
