#!/usr/bin/env python3
"""Refresh remote metadata for a directory in the mounted Degoo vault."""

from __future__ import annotations

import argparse
import http.client
import json
import os
import socket
import stat
import sys
from pathlib import Path
from typing import Any

import info as local_info
import install as installer


SOCKET_NAME = "vfs-sync.sock"
SOCKET_TIMEOUT = 300.0
MAX_RESPONSE_BYTES = 64 * 1024
MAX_UNIX_SOCKET_PATH_BYTES = 107


class SyncError(Exception):
    """A safe, user-facing error from a local vault metadata refresh."""


class UnixHTTPConnection(http.client.HTTPConnection):
    """Send one HTTP request over an owner-private Unix-domain socket."""

    def __init__(self, socket_path: Path, timeout: float = SOCKET_TIMEOUT):
        super().__init__("localhost", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:
        connection = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        connection.settimeout(self.timeout)
        try:
            connection.connect(str(self.socket_path))
        except OSError:
            connection.close()
            raise
        self.sock = connection


def _validate_install(root: Path) -> Path:
    """Validate the private install manifest and return its mounted vault path."""
    if not root.is_absolute() or any(
        ord(character) < 0x20 or ord(character) == 0x7F for character in str(root)
    ):
        raise SyncError("the install root must be an absolute path without control characters")
    root = Path(os.path.normpath(str(root)))
    try:
        installer.check_directory(root, private=True)
        manifest = installer.load_manifest(root)
        mountpoint = root / str(manifest["mountpoint"])
        installer.check_components(mountpoint)
        mount_info = os.lstat(mountpoint)
        if not stat.S_ISDIR(mount_info.st_mode) or stat.S_ISLNK(mount_info.st_mode):
            raise installer.InstallError("the mount path is not a directory")
        installer.check_directory(root / "tmp", private=True)
    except (installer.InstallError, KeyError, OSError):
        raise SyncError("the Degoo install is missing or unsafe; run degoo-rc install and retry") from None

    state = local_info.mounted_state(mountpoint)
    if state == "not mounted":
        raise SyncError("the Degoo vault is not mounted; mount it and retry")
    if state != "mounted":
        raise SyncError("the Degoo mount state could not be verified; check the mount and retry")
    return mountpoint


def _inside_mount(path: Path, mount_root: Path) -> Path:
    """Return a mount-relative path or refuse a target outside the vault."""
    try:
        return path.relative_to(mount_root)
    except ValueError:
        raise SyncError("the requested path is outside the mounted Degoo vault") from None


def resolve_target(
    mount_root: Path, local_path: str | Path, *, cwd: Path | None = None
) -> tuple[Path, str]:
    """Resolve a file or directory to a mounted directory for metadata refresh.

    Files refresh their containing directory. A missing target refreshes its
    nearest existing parent, which also lets a deleted directory be refreshed.
    """
    try:
        resolved_mount = mount_root.resolve(strict=True)
        requested = Path(local_path)
        if not requested.is_absolute():
            requested = (Path.cwd() if cwd is None else cwd) / requested
        target = requested.resolve(strict=False)
    except (OSError, RuntimeError, ValueError):
        raise SyncError("the requested local path could not be resolved safely") from None

    _inside_mount(target, resolved_mount)
    try:
        target_info = target.stat()
    except FileNotFoundError:
        target_info = None
    except NotADirectoryError:
        target_info = None
    except OSError:
        raise SyncError("the requested local path could not be inspected") from None

    if target_info is not None:
        if stat.S_ISDIR(target_info.st_mode):
            directory = target
        elif stat.S_ISREG(target_info.st_mode):
            directory = target.parent
        else:
            raise SyncError("the requested path must be a directory or regular file")
    else:
        directory = target.parent
        while True:
            _inside_mount(directory, resolved_mount)
            try:
                parent_info = directory.stat()
            except (FileNotFoundError, NotADirectoryError):
                if directory == resolved_mount:
                    raise SyncError("the requested path has no existing directory inside the vault")
                directory = directory.parent
                continue
            except OSError:
                raise SyncError("the requested local path could not be inspected") from None
            if stat.S_ISDIR(parent_info.st_mode):
                break
            if stat.S_ISREG(parent_info.st_mode):
                directory = directory.parent
                continue
            raise SyncError("the nearest existing parent is not a directory")

    relative = _inside_mount(directory, resolved_mount)
    return directory, relative.as_posix() if relative.parts else ""


def _socket_path(root: Path) -> Path:
    """Return the active mount's socket after checking its owner and mode."""
    path = root / "tmp" / SOCKET_NAME
    if len(os.fsencode(path)) > MAX_UNIX_SOCKET_PATH_BYTES:
        raise SyncError("the install path is too long for the local sync socket")
    try:
        installer.check_components(path, missing_ok=True)
        info = os.lstat(path)
    except FileNotFoundError:
        raise SyncError("this running mount has no sync socket; upgrade the client and restart the mount") from None
    except (installer.InstallError, OSError):
        raise SyncError("the mounted vault sync socket could not be inspected safely") from None
    if (
        not stat.S_ISSOCK(info.st_mode)
        or info.st_uid != os.getuid()
        or stat.S_IMODE(info.st_mode) != 0o600
    ):
        raise SyncError("the mounted vault sync socket has unsafe ownership or permissions")
    return path


def _request(socket_path: Path, relative_path: str) -> None:
    """Ask the mounted VFS to refresh one directory and validate its reply."""
    try:
        body = json.dumps(
            {"path": relative_path, "recursive": True},
            ensure_ascii=False,
            separators=(",", ":"),
        ).encode("utf-8")
    except (UnicodeEncodeError, ValueError):
        raise SyncError("the requested path cannot be represented by the local sync service") from None

    connection = UnixHTTPConnection(socket_path)
    try:
        connection.request("POST", "/sync", body=body, headers={"Content-Type": "application/json"})
        response = connection.getresponse()
        payload = response.read(MAX_RESPONSE_BYTES + 1)
    except (OSError, http.client.HTTPException, TimeoutError):
        raise SyncError("the mounted vault sync request failed; check the mount and retry") from None
    finally:
        connection.close()

    if not 200 <= response.status < 300:
        raise SyncError("the mounted vault could not refresh directory metadata; check the mount logs")
    if len(payload) > MAX_RESPONSE_BYTES:
        raise SyncError("the mounted vault returned an invalid sync response")
    try:
        result: Any = json.loads(payload)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise SyncError("the mounted vault returned an invalid sync response") from None
    if not isinstance(result, dict) or result.get("path") != relative_path or result.get("refreshed") is not True:
        raise SyncError("the mounted vault returned an invalid sync response")


def refresh_directory(root: Path, local_path: str | Path, *, cwd: Path | None = None) -> Path:
    """Refresh metadata for a file's parent or selected directory in the vault."""
    mountpoint = _validate_install(root)
    directory, relative = resolve_target(mountpoint, local_path, cwd=cwd)
    socket_path = _socket_path(root)
    _request(socket_path, relative)
    return directory


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description=(
            "Refresh remote directory metadata for a local vault path. Files refresh their parent; "
            "missing paths refresh their nearest existing parent. Pending local writes are preserved."
        )
    )
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument(
        "path",
        nargs="?",
        default=".",
        help="local file or directory inside the mounted vault (default: .)",
    )
    return parser


def _display_path(path: Path) -> str:
    """Return printable local path text suitable for a terminal line."""
    return "".join(character if character.isprintable() else "?" for character in str(path))


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        directory = refresh_directory(Path(args.root), args.path)
    except SyncError as error:
        print(f"degoo-rc sync: {error}", file=sys.stderr)
        return 2
    print(
        f"Refreshed remote directory metadata for {_display_path(directory)}. "
        "Pending local writes preserved; reopen files to see changed content."
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
