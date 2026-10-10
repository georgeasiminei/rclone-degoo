#!/usr/bin/env python3
"""Safely inventory or remove the isolated local Degoo vault installation."""

from __future__ import annotations

import argparse
import errno
import fcntl
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any


REMOTE_NAME = "degoo-live-test"
ROOT_NAME = "rclone-degoo"
MAX_MANIFEST_SIZE = 64 * 1024
MAX_VAULT_SIZE = 1024 * 1024
MAX_STATUS_SIZE = 4096
STATUS_PATTERN = re.compile(r"Pending local uploads: ([0-9]+) files, ([0-9]+) bytes")
EXPECTED_MANIFEST_PATHS = {
    "binary": "bin/rclone",
    "vault_config": "config/vault.json",
    "rclone_config": "config/rclone.conf",
    "cache_dir": "cache",
    "mountpoint": "mount",
    "pidfile": "mount.pid",
}
FUSERMOUNT_NAMES = {"fusermount", "fusermount2", "fusermount3"}


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
    if created and not folder_name.strip():
        raise CleanupError("the install manifest is incomplete")
    return manifest


def _validate_layout(root: Path) -> dict[str, Any]:
    """Check the fixed install layout, private configs, and manifest."""
    _check_directory(root, private=True)
    for name in ("bin", "config", "cache", "mount", "tools", "logs"):
        _check_directory(root / name)

    manifest = _validate_manifest(root)
    for rel, maximum in (("config/vault.json", MAX_VAULT_SIZE), ("config/auth.json", MAX_MANIFEST_SIZE), ("config/rclone.conf", MAX_MANIFEST_SIZE)):
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
            raise

    vault_path = root / "config/vault.json"
    try:
        vault_bytes = _read_private_file(vault_path, MAX_VAULT_SIZE, missing_ok=True)
    except FileNotFoundError:
        vault_bytes = None
    vault: dict[str, Any] | None = _decode_json(vault_bytes) if vault_bytes is not None else None
    if vault is not None:
        if type(vault.get("version")) is not int or vault["version"] != 1 or vault.get("remote") != f"{REMOTE_NAME}:":
            raise CleanupError("the vault configuration does not match this install")
        if not isinstance(vault.get("password"), str) or not vault["password"]:
            raise CleanupError("the vault configuration is incomplete")
        if vault.get("cache_dir") != str(root / "cache"):
            raise CleanupError("the vault configuration points outside the isolated cache")

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

    return {"manifest": manifest, "vault_bytes": vault_bytes, "vault": vault}


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
    if backup_bytes != vault_bytes:
        raise CleanupError("the vault backup does not match the installed vault configuration")


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
    mount_text = os.path.normpath(str(root / "mount"))
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
            [str(helper), "-u", str(root / "mount")],
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
    return root / "cache" / f"degoo-vault-{digest}.lock"


def _acquire_cache_lock(root: Path, wait_seconds: float) -> int:
    path = _cache_lock_path(root)
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
            else:
                raise CleanupError("special files inside the install are not allowed")
            entries.append({"path": rel.as_posix(), "type": kind})

    visit(root, Path())
    entries.sort(key=lambda item: item["path"])
    return entries


def _cache_has_data(root: Path, lock_path: Path) -> bool:
    """Return whether cache contains anything beyond the held lock file."""
    for directory, dirnames, filenames in os.walk(root / "cache", followlinks=False):
        for name in dirnames:
            path = Path(directory) / name
            info = os.lstat(path)
            if stat.S_ISLNK(info.st_mode):
                raise CleanupError("symlinks inside the cache are not allowed")
        for name in filenames:
            path = Path(directory) / name
            if path == lock_path:
                continue
            info = os.lstat(path)
            if not stat.S_ISREG(info.st_mode):
                raise CleanupError("unexpected files inside the cache are not allowed")
            return True
    return False


def _pending_uploads(root: Path) -> tuple[int, int]:
    binary = root / "bin/rclone"
    config = root / "config/rclone.conf"
    vault = root / "config/vault.json"
    _check_regular_executable(binary)
    try:
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(
                [str(binary), "--config", str(config), "--log-file", os.devnull, "--log-level", "ERROR", "degoo", "status", str(vault)],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.DEVNULL,
                env={
                    "HOME": str(root),
                    "XDG_CONFIG_HOME": str(root / "config"),
                    "XDG_CACHE_HOME": str(root / "cache"),
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
        with os.scandir(root / "mount") as iterator:
            if next(iterator, None) is not None:
                raise CleanupError("the unmounted mount directory contains local files")
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the isolated mount directory could not be inspected") from None


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
        shutil.rmtree(root)
    except CleanupError:
        raise
    except OSError:
        raise CleanupError("the isolated local install could not be removed safely") from None
    finally:
        os.close(parent_fd)


def _inventory(root: Path, manifest: dict[str, Any], entries: list[dict[str, str]], mounted: bool) -> None:
    result = {
        "mode": "inventory",
        "root": str(root),
        "mounted": mounted,
        "files": entries,
        "remote_data": "left untouched",
    }
    json.dump(result, sys.stdout, ensure_ascii=True, sort_keys=True)
    sys.stdout.write("\n")


def uninstall(args: argparse.Namespace) -> int:
    expected = expected_root()
    supplied = Path(args.root)
    if not supplied.is_absolute() or ".." in supplied.parts:
        raise CleanupError("--root must be an absolute path without parent traversal")
    root = Path(os.path.normpath(str(supplied)))
    if root != expected:
        raise CleanupError("--root must be the isolated ~/.local/share/rclone-degoo directory")
    _check_components(root)
    layout = _validate_layout(root)
    points = read_mountpoints()
    mounted = _mount_state(root, points)
    entries = _audit_tree(root)

    if not args.delete_local:
        _inventory(root, layout["manifest"], entries, mounted)
        return 0

    if args.vault_backup:
        _validate_backup(args.vault_backup, root, layout["vault_bytes"])
    elif not args.discard_vault_secret:
        raise CleanupError("local deletion requires --vault-backup or --discard-vault-secret")

    if mounted:
        _unmount(root)
    # Recheck the whole mount namespace after unmounting and before touching the cache.
    if _mount_state(root, read_mountpoints()):
        raise CleanupError("the isolated mount remains active")
    _require_empty_mount_directory(root)

    # The isolated install always binds this cache to the fixed remote name.
    # Holding rclone's flock prevents a new mount from racing the status check.
    lock_fd = _acquire_cache_lock(root, 10 if mounted else 0)
    try:
        vault_exists = layout["vault_bytes"] is not None
        config_exists = (root / "config/rclone.conf").exists()
        binary_exists = (root / "bin/rclone").exists()
        if vault_exists and config_exists and binary_exists:
            files, size = _pending_uploads(root)
            if files != 0 or size != 0:
                raise CleanupError("pending local uploads remain; the install and cache were preserved")
        elif _cache_has_data(root, _cache_lock_path(root)):
            raise CleanupError("the rclone configuration is incomplete and the cache is not empty")

        # Revalidate protected inputs after unmounting and while holding the VFS lock.
        latest = _validate_layout(root)
        if latest["vault_bytes"] != layout["vault_bytes"]:
            raise CleanupError("the vault configuration changed during cleanup")
        if args.vault_backup:
            _validate_backup(args.vault_backup, root, latest["vault_bytes"])
        final_points = read_mountpoints()
        if _mount_state(root, final_points):
            raise CleanupError("the isolated mount became active during cleanup")
        _require_empty_mount_directory(root)
        _audit_tree(root)
        # Removing the empty mountpoint first makes a concurrent re-mount fail
        # before recursive deletion can traverse a mounted filesystem.
        try:
            os.rmdir(root / "mount")
        except OSError:
            raise CleanupError("the isolated mountpoint could not be safely removed") from None
        if _mount_state(root, read_mountpoints()):
            raise CleanupError("a mount became active during cleanup")
        _remove_root(root)
    finally:
        os.close(lock_fd)

    print("Removed the isolated local Degoo install. Remote encrypted files were left untouched.")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="isolated install root (must be ~/.local/share/rclone-degoo)")
    actions = parser.add_mutually_exclusive_group()
    actions.add_argument("--delete-local", action="store_true", help="unmount safely and remove the isolated local install")
    actions.add_argument("--dry-run", action="store_true", help="inventory the install without changing it (the default)")
    gate = parser.add_mutually_exclusive_group()
    gate.add_argument("--vault-backup", help="private backup file that exactly matches config/vault.json")
    gate.add_argument("--discard-vault-secret", action="store_true", help="explicitly allow deleting the only local vault secret")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if (args.vault_backup or args.discard_vault_secret) and not args.delete_local:
        parser.error("--vault-backup and --discard-vault-secret require --delete-local")
    try:
        return uninstall(args)
    except CleanupError as exc:
        print(f"uninstall refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("uninstall refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
