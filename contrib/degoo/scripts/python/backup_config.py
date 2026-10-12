#!/usr/bin/env python3
"""Create a private portable Degoo configuration backup without account credentials."""

from __future__ import annotations

import argparse
import ctypes
import errno
import json
import os
from pathlib import Path
import secrets
import shutil
import stat
import sys
import time

import install as installer
import restore


MAX_MANIFEST_SIZE = 64 * 1024
CONFIG_FILES = ("login.json", "mount.json", "rclone.conf", "vault.json")
SOURCE_CONFIG_FILES = ("auth.json", "auth.json.lock", *CONFIG_FILES)
REQUIRED_CONFIG_FILES = frozenset({"mount.json", "rclone.conf", "vault.json"})
RENAME_NOREPLACE = 1


class BackupConfigError(Exception):
    """A safe refusal to create a portable Degoo configuration backup."""


def _absolute_path(value: str | os.PathLike[str], label: str) -> Path:
    """Return a normalized absolute path without traversal components."""
    path = Path(os.path.expanduser(os.fspath(value)))
    if not path.is_absolute() or ".." in path.parts:
        raise BackupConfigError(f"the {label} must be an absolute path without parent traversal")
    if any(ord(character) < 0x20 or ord(character) == 0x7F for character in str(path)):
        raise BackupConfigError(f"the {label} contains unsupported control characters")
    return Path(os.path.normpath(str(path)))


def _stamp(info: os.stat_result) -> tuple[int, ...]:
    """Return the file identity and timestamps used to detect source changes."""
    return (
        info.st_dev,
        info.st_ino,
        info.st_mode,
        info.st_uid,
        info.st_nlink,
        info.st_size,
        info.st_mtime_ns,
        info.st_ctime_ns,
    )


def _safe_private_file(path: Path) -> os.stat_result:
    """Validate an owner-private regular file with no alternate hard links."""
    try:
        installer.check_components(path)
        info = os.lstat(path)
    except (OSError, installer.InstallError):
        raise BackupConfigError("a required configuration file could not be inspected safely") from None
    if (
        not stat.S_ISREG(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
        or info.st_mode & 0o6000
        or info.st_nlink != 1
    ):
        raise BackupConfigError("a configuration file has unsafe ownership or permissions")
    return info


def _config_snapshot(config_dir: Path) -> tuple[os.stat_result, tuple[str, ...], dict[str, os.stat_result]]:
    """Validate the supported config entries and capture their initial identities."""
    try:
        info = installer.check_directory(config_dir, private=True)
        names = tuple(sorted(os.listdir(config_dir)))
    except (OSError, installer.InstallError):
        raise BackupConfigError("the configuration directory could not be inspected safely") from None
    if any(name not in SOURCE_CONFIG_FILES for name in names):
        raise BackupConfigError("the configuration directory contains unsupported state")
    files = {name: _safe_private_file(config_dir / name) for name in names}
    if not REQUIRED_CONFIG_FILES.issubset(files):
        raise BackupConfigError("the Degoo vault is not fully configured; run setup before backing it up")
    return info, names, files


def _assert_source_unchanged(
    root: Path,
    manifest_info: os.stat_result,
    config_dir_info: os.stat_result,
    config_names: tuple[str, ...],
    config_files: dict[str, os.stat_result],
) -> None:
    """Reject a backup assembled from configuration files that changed mid-read."""
    current_manifest = _safe_private_file(root / "install.json")
    try:
        current_config_dir = installer.check_directory(root / "config", private=True)
        current_names = tuple(sorted(os.listdir(root / "config")))
    except (OSError, installer.InstallError):
        raise BackupConfigError("configuration changed while the backup was being created; retry") from None
    if (
        _stamp(current_manifest) != _stamp(manifest_info)
        or _stamp(current_config_dir) != _stamp(config_dir_info)
        or current_names != config_names
    ):
        raise BackupConfigError("configuration changed while the backup was being created; retry")
    for name, before in config_files.items():
        try:
            after = _safe_private_file(root / "config" / name)
        except BackupConfigError:
            raise BackupConfigError("configuration changed while the backup was being created; retry") from None
        if _stamp(after) != _stamp(before):
            raise BackupConfigError("configuration changed while the backup was being created; retry")


def _read_source(root: Path) -> dict[str, bytes]:
    """Read and validate one consistent configuration snapshot without touching cache data."""
    try:
        installer.check_directory(root, private=True)
    except installer.InstallError as exc:
        raise BackupConfigError(str(exc)) from None
    manifest_path = root / "install.json"
    manifest_info = _safe_private_file(manifest_path)
    config_dir_info, config_names, config_files = _config_snapshot(root / "config")

    try:
        manifest_bytes = installer.read_private_file(manifest_path, MAX_MANIFEST_SIZE)
        if manifest_bytes is None:
            raise BackupConfigError("the install metadata is missing")
        manifest = installer.load_manifest(root)
        saved_manifest = installer.parse_json_object(manifest_bytes, "the install metadata is invalid")
        folder_id = manifest.get("root_folder_id")
        if saved_manifest != manifest or not isinstance(folder_id, str) or not folder_id:
            raise BackupConfigError("the Degoo vault is not fully configured; run setup before backing it up")
        try:
            if installer.validate_folder_id(folder_id) != folder_id:
                raise BackupConfigError("the Degoo vault is not fully configured; run setup before backing it up")
        except installer.InstallError:
            raise BackupConfigError("the Degoo vault is not fully configured; run setup before backing it up") from None
    except installer.InstallError as exc:
        raise BackupConfigError(str(exc)) from None

    files: dict[str, bytes] = {"install.json": manifest_bytes}
    for name in CONFIG_FILES:
        if name not in config_files:
            continue
        try:
            data = installer.read_private_file(root / "config" / name, installer.MAX_CONFIG_SIZE)
        except installer.InstallError as exc:
            raise BackupConfigError(str(exc)) from None
        if data is None:
            raise BackupConfigError("a configuration file disappeared while the backup was being created")
        files[name] = data

    try:
        vault, _ = installer.read_vault(files["vault.json"], root, imported=False)
        installer.validate_rclone_config(files["rclone.conf"], root, folder_id)
        installer.validate_mount_settings(files["mount.json"])
    except installer.InstallError as exc:
        raise BackupConfigError(str(exc)) from None
    if vault.get("remote") != f"{installer.REMOTE_NAME}:":
        raise BackupConfigError("the vault secret is not bound to the configured Degoo remote")

    _assert_source_unchanged(root, manifest_info, config_dir_info, config_names, config_files)
    if "account_credentials" in vault:
        del vault["account_credentials"]
        files["vault.json"] = (json.dumps(vault, indent=2, sort_keys=True) + "\n").encode("utf-8")
    return files


def _default_directory(root: Path) -> Path:
    """Return a new timestamped backup path beside the contained install."""
    parent = root.parent
    timestamp = time.strftime("%Y%m%d-%H%M%S", time.localtime())
    base = parent / f"rclone-degoo-backup-{timestamp}"
    candidate = base
    suffix = 1
    while os.path.lexists(candidate):
        candidate = base.with_name(f"{base.name}-{suffix}")
        suffix += 1
    return candidate


def _remove_empty_directories(created: list[tuple[Path, tuple[int, int]]]) -> None:
    """Remove only newly created, still-identical empty parent directories."""
    for path, identity in reversed(created):
        try:
            info = os.lstat(path)
            if stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode) and (info.st_dev, info.st_ino) == identity:
                os.rmdir(path)
        except OSError:
            pass


def _prepare_parent(parent: Path) -> list[tuple[Path, tuple[int, int]]]:
    """Create missing private parent directories and return their identities."""
    try:
        installer.check_components(parent, missing_ok=True)
    except installer.InstallError as exc:
        raise BackupConfigError(str(exc)) from None

    missing: list[Path] = []
    current = Path(parent.anchor)
    for part in parent.parts[1:]:
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            missing.append(current)
            continue
        except OSError:
            raise BackupConfigError("the backup parent could not be inspected safely") from None
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise BackupConfigError("the backup parent contains an unsafe path component")
        if missing:
            raise BackupConfigError("the backup parent changed while it was being checked")

    existing_parent = missing[0].parent if missing else parent
    try:
        installer.check_directory(existing_parent)
    except installer.InstallError as exc:
        raise BackupConfigError(str(exc)) from None

    created: list[tuple[Path, tuple[int, int]]] = []
    try:
        for directory in missing:
            try:
                os.mkdir(directory, 0o700)
                info = os.lstat(directory)
            except OSError:
                raise BackupConfigError("a private backup parent directory could not be created safely") from None
            identity = (info.st_dev, info.st_ino)
            created.append((directory, identity))
            installer.check_directory(directory, private=True)
    except BaseException:
        _remove_empty_directories(created)
        raise
    try:
        installer.check_directory(parent)
    except installer.InstallError as exc:
        _remove_empty_directories(created)
        raise BackupConfigError(str(exc)) from None
    return created


def _validate_destination(root: Path, destination: Path) -> None:
    """Ensure a new destination is outside the install and does not exist."""
    try:
        common = os.path.commonpath((str(root), str(destination)))
    except ValueError:
        raise BackupConfigError("the backup directory is invalid") from None
    if common in {str(root), str(destination)}:
        raise BackupConfigError("the backup directory must be separate from the install")
    if os.path.lexists(destination):
        raise BackupConfigError("the backup directory already exists")


def _open_parent(parent: Path) -> int:
    """Open the checked backup parent without following its final component."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(parent, flags)
    except OSError:
        raise BackupConfigError("the backup parent could not be opened safely") from None
    info = os.fstat(fd)
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022:
        os.close(fd)
        raise BackupConfigError("the backup parent has unsafe ownership or permissions")
    return fd


def _fsync_directory(path: Path) -> None:
    """Flush a checked directory entry set to stable storage."""
    flags = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(path, flags)
    except OSError:
        raise BackupConfigError("the private backup could not be flushed safely") from None
    try:
        os.fsync(fd)
    except OSError:
        raise BackupConfigError("the private backup could not be flushed safely") from None
    finally:
        os.close(fd)


def _rename_noreplace(parent_fd: int, source_name: str, destination_name: str) -> None:
    """Publish a staged directory atomically without replacing an existing path."""
    try:
        function = ctypes.CDLL(None, use_errno=True).renameat2
    except AttributeError:
        raise BackupConfigError("this system cannot publish a backup without replacing existing paths") from None
    function.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    function.restype = ctypes.c_int
    result = function(parent_fd, os.fsencode(source_name), parent_fd, os.fsencode(destination_name), RENAME_NOREPLACE)
    if result == 0:
        return
    if ctypes.get_errno() == errno.EEXIST:
        raise BackupConfigError("the backup directory already exists")
    raise BackupConfigError("the private backup could not be published safely") from None


def _remove_stage(parent: Path, stage_name: str, identity: tuple[int, int]) -> None:
    """Remove only the private staging directory created by this invocation."""
    stage = parent / stage_name
    try:
        info = os.lstat(stage)
    except FileNotFoundError:
        return
    except OSError:
        raise BackupConfigError("the temporary private backup could not be cleaned up safely") from None
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or (info.st_dev, info.st_ino) != identity:
        raise BackupConfigError("the temporary private backup changed before cleanup")
    try:
        shutil.rmtree(stage)
    except OSError:
        raise BackupConfigError("the temporary private backup could not be cleaned up safely") from None


def _stage_backup(parent_fd: int, destination: Path, files: dict[str, bytes]) -> tuple[str, tuple[int, int]]:
    """Write a validated config-only snapshot into a new private staging directory."""
    for _ in range(16):
        stage_name = f".degoo-backup-config-{os.getpid()}-{secrets.token_hex(8)}"
        try:
            os.mkdir(stage_name, 0o700, dir_fd=parent_fd)
            break
        except FileExistsError:
            continue
        except OSError:
            raise BackupConfigError("a private staging directory could not be created") from None
    else:
        raise BackupConfigError("a private staging directory could not be created")

    stage_path = destination.parent / stage_name
    try:
        stage_info = os.stat(stage_name, dir_fd=parent_fd, follow_symlinks=False)
    except OSError:
        try:
            os.rmdir(stage_name, dir_fd=parent_fd)
        except OSError:
            pass
        raise BackupConfigError("the private staging directory could not be inspected safely") from None
    if not stat.S_ISDIR(stage_info.st_mode) or stat.S_ISLNK(stage_info.st_mode):
        raise BackupConfigError("the private staging path is unsafe")
    identity = (stage_info.st_dev, stage_info.st_ino)
    try:
        os.chmod(stage_path, 0o700)
        config_dir = stage_path / "config"
        os.mkdir(config_dir, 0o700)
        os.chmod(config_dir, 0o700)
        installer.check_directory(stage_path, private=True)
        installer.check_directory(config_dir, private=True)
        installer.write_private_file(stage_path / "install.json", files["install.json"])
        for name in CONFIG_FILES:
            if name in files:
                installer.write_private_file(config_dir / name, files[name])

        _fsync_directory(config_dir)
        _fsync_directory(stage_path)
        restore.inspect_backup(stage_path, destination.parent / ".degoo-backup-validation-target")
        return stage_name, identity
    except BaseException:
        _remove_stage(destination.parent, stage_name, identity)
        raise


def run(root: Path, directory: Path | None = None) -> Path:
    """Create and return a private configuration backup outside the install root."""
    install_root = _absolute_path(root, "install root")
    destination = _absolute_path(directory, "backup directory") if directory is not None else _default_directory(install_root)
    _validate_destination(install_root, destination)

    created_parents = _prepare_parent(destination.parent)
    parent_fd = -1
    stage_name: str | None = None
    stage_identity: tuple[int, int] | None = None
    published = False
    try:
        files = _read_source(install_root)
        _validate_destination(install_root, destination)
        parent_fd = _open_parent(destination.parent)
        stage_name, stage_identity = _stage_backup(parent_fd, destination, files)
        _rename_noreplace(parent_fd, stage_name, destination.name)
        published = True
        try:
            os.fsync(parent_fd)
        except OSError:
            raise BackupConfigError(f"backup was created at {destination}, but its directory could not be flushed safely") from None
        return destination
    except BaseException:
        if stage_name is not None and stage_identity is not None and not published:
            _remove_stage(destination.parent, stage_name, stage_identity)
        if created_parents and not published:
            _remove_empty_directories(created_parents)
        raise
    finally:
        if parent_fd >= 0:
            os.close(parent_fd)


def build_parser() -> argparse.ArgumentParser:
    """Build the noninteractive backup-config command parser."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument("--directory", help="new private backup directory (defaults beside the install)")
    return parser


def main(argv: list[str] | None = None) -> int:
    """Run the backup-config command and print only its destination path."""
    args = build_parser().parse_args(argv)
    try:
        directory = run(_absolute_path(args.root, "install root"), Path(args.directory) if args.directory else None)
        print(f"Configuration backup created at: {directory}")
        print("Vault configuration only. Saved account passwords and authentication tokens are excluded; log in after restoring.")
        print("File data, cache, and pending uploads are excluded.")
        return 0
    except (BackupConfigError, installer.InstallError, restore.RestoreError) as exc:
        print(f"backup-config refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("backup-config refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
