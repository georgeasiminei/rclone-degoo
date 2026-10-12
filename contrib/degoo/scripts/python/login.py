#!/usr/bin/env python3
"""Prompt for a Degoo account email and run the contained rclone login."""

from __future__ import annotations

import argparse
import json
import os
import secrets
import shlex
import stat
import subprocess
import sys
from pathlib import Path


class LoginError(Exception):
    """A safe error message for the contained account login helper."""


def _degoo_rc(root: Path, *arguments: str) -> str:
    """Return a copyable command through the contained user-facing CLI."""
    return " ".join(shlex.quote(value) for value in (str(root / "degoo-rc"), *arguments))


def _check_components(path: Path) -> None:
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            info = os.lstat(current)
        except OSError:
            raise LoginError("the contained install path is unavailable") from None
        if stat.S_ISLNK(info.st_mode):
            raise LoginError("symlink path components are not allowed")
        if current != path and not stat.S_ISDIR(info.st_mode):
            raise LoginError("an install path ancestor is not a directory")


def _private_directory(path: Path) -> None:
    _check_components(path)
    try:
        info = os.lstat(path)
    except OSError:
        raise LoginError("the contained config directory is unavailable") from None
    if (
        not stat.S_ISDIR(info.st_mode)
        or stat.S_ISLNK(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
    ):
        raise LoginError("the contained config directory must be private to the current user")


def _read_email(path: Path) -> str | None:
    try:
        before = os.lstat(path)
    except FileNotFoundError:
        return None
    except OSError:
        raise LoginError("the cached account email could not be inspected") from None
    if not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode):
        raise LoginError("the cached account email is not a regular file")
    if before.st_uid != os.getuid() or before.st_mode & 0o077 or before.st_size > 4096:
        raise LoginError("the cached account email has unsafe permissions")
    try:
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
    except OSError:
        raise LoginError("the cached account email could not be opened safely") from None
    try:
        opened = os.fstat(fd)
        if (
            (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)
            or opened.st_uid != os.getuid()
            or opened.st_mode & 0o077
        ):
            raise LoginError("the cached account email changed while being opened")
        raw = os.read(fd, 4097)
    finally:
        os.close(fd)
    if len(raw) > 4096:
        raise LoginError("the cached account email is too large")
    try:
        data = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise LoginError("the cached account email is invalid") from None
    if not isinstance(data, dict) or set(data) != {"email"} or not isinstance(data["email"], str):
        raise LoginError("the cached account email is invalid")
    email = data["email"].strip()
    return email or None


def _write_email(path: Path, email: str) -> None:
    payload = (json.dumps({"email": email}, ensure_ascii=True, sort_keys=True) + "\n").encode("ascii")
    temporary = path.with_name(f".{path.name}.tmp-{os.getpid()}-{secrets.token_hex(4)}")
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(temporary, flags, 0o600)
    except OSError:
        raise LoginError("the cached account email could not be saved safely") from None
    try:
        with os.fdopen(fd, "wb", closefd=True) as stream:
            stream.write(payload)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    except OSError:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise LoginError("the cached account email could not be saved safely") from None


def _remove_email(path: Path) -> bool:
    _read_email(path)
    try:
        path.unlink()
    except FileNotFoundError:
        return False
    except OSError:
        raise LoginError("the cached account email could not be removed safely") from None
    return True


def _root_path(value: str) -> Path:
    root = Path(os.path.expanduser(value))
    if not root.is_absolute() or ".." in root.parts:
        raise LoginError("--root must be an absolute path without parent traversal")
    return Path(os.path.normpath(str(root)))


def run(root: Path, *, clear_email: bool) -> int:
    account_home = Path.home()
    _private_directory(root)
    config_dir = root / "config"
    _private_directory(config_dir)
    email_path = config_dir / "login.json"
    if clear_email:
        removed = _remove_email(email_path)
        print("Forgot the cached Degoo account email." if removed else "No cached Degoo account email was stored.")
        return 0

    previous = _read_email(email_path)
    if previous:
        email = previous
    else:
        try:
            email = input("Degoo account email: ").strip()
        except EOFError:
            raise LoginError("no account email was entered") from None
    if not email or any(character in email for character in "\r\n\x00"):
        raise LoginError("a Degoo account email is required")

    binary = root / "bin" / "rclone"
    _check_components(binary)
    try:
        binary_info = os.lstat(binary)
    except OSError:
        raise LoginError("the contained rclone executable is unavailable") from None
    if (
        not stat.S_ISREG(binary_info.st_mode)
        or stat.S_ISLNK(binary_info.st_mode)
        or binary_info.st_uid != os.getuid()
        or binary_info.st_mode & 0o022
        or binary_info.st_mode & 0o6000
        or not binary_info.st_mode & 0o100
    ):
        raise LoginError("the contained rclone executable is unsafe")
    auth_path = config_dir / "auth.json"
    vault_path = config_dir / "vault.json"
    _check_components(vault_path.parent)
    credential_args = ["--vault-config", str(vault_path)] if os.path.lexists(vault_path) else []
    for required in (root / "logs", root / "tmp", root / "cache"):
        _check_components(required)
    _write_email(email_path, email)
    env = {
        "HOME": str(account_home),
        "XDG_CONFIG_HOME": str(config_dir),
        "XDG_CACHE_HOME": str(root / "cache"),
        "TMPDIR": str(root / "tmp"),
        "PATH": "/usr/bin:/bin",
        "LANG": "C.UTF-8",
    }
    try:
        result = subprocess.run(
            [
                str(binary),
                "--config",
                str(config_dir / "rclone.conf"),
                "--log-file",
                str(root / "logs/login.log"),
                "--log-level",
                "ERROR",
                "degoo",
                "login",
                str(auth_path),
                "--email",
                email,
                "--cached-email",
                *credential_args,
            ],
            stdin=None,
            check=False,
            env=env,
            umask=0o077,
        )
    except OSError:
        raise LoginError("the contained Degoo login command could not be started") from None
    if result.returncode != 0:
        print(
            f"Degoo login did not complete. Review {root / 'logs/login.log'} and try {_degoo_rc(root, 'login')} again.",
            file=sys.stderr,
        )
    elif credential_args:
        print("Account password saved encrypted with the vault key for automatic re-login.")
    else:
        print("Tokens saved. After vault setup, run degoo-rc login once to enable encrypted password fallback.")
    return result.returncode


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="contained install root")
    parser.add_argument("--clear-email", action="store_true", help="forget the cached email without logging out")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(_root_path(args.root), clear_email=args.clear_email)
    except LoginError as exc:
        print(f"login refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("login refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
