#!/usr/bin/env python3
"""Finish a staged Degoo install by selecting or creating its dedicated folder."""

from __future__ import annotations

import argparse
import os
import stat
import subprocess
import sys
from pathlib import Path

import install as installer


class SetupError(Exception):
    """A safe error from the contained vault setup helper."""


def _root(value: str) -> Path:
    try:
        root = installer.Path(os.path.expanduser(value))
        if not root.is_absolute() or ".." in root.parts:
            raise installer.InstallError("--root must be an absolute path without parent traversal")
        root = installer.Path(os.path.normpath(str(root)))
        installer.check_directory(root, private=True)
        installer.check_directory(root / "config", private=True)
        installer.check_directory(root / "logs", private=True)
        installer.check_directory(root / "tmp", private=True)
        installer.check_directory(root / "cache", private=True)
        installer.load_manifest(root)
        return root
    except installer.InstallError as exc:
        raise SetupError(str(exc)) from None


def _validate_identifier(value: str, label: str) -> str:
    identifier = value.strip()
    if not identifier or identifier == "0" or any(ord(character) < 0x21 or ord(character) == 0x7F for character in identifier):
        raise SetupError(f"{label} must identify a dedicated Degoo folder and cannot be 0")
    return identifier


def _validate_name(value: str) -> str:
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
        raise SetupError("--name must be a safe single folder name (not a path or an internal .rclone name)")
    return name


def _binary(root: Path) -> Path:
    path = root / "bin/rclone"
    installer.check_components(path)
    try:
        info = os.lstat(path)
    except OSError:
        raise SetupError("the contained rclone executable is unavailable") from None
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o022 or not info.st_mode & 0o100:
        raise SetupError("the contained rclone executable is unsafe")
    return path


def _run_create(root: Path, parent_id: str, name: str) -> str:
    binary = _binary(root)
    env = {
        "HOME": str(installer.home_directory()),
        "XDG_CONFIG_HOME": str(root / "config"),
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
                str(root / "config/rclone.conf"),
                "--log-file",
                str(root / "logs/setup.log"),
                "--log-level",
                "ERROR",
                "degoo",
                "create-vault",
                str(root / "config/auth.json"),
                "--parent-id",
                parent_id,
                "--name",
                name,
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=600,
            check=False,
            umask=0o077,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise SetupError("Degoo could not create the dedicated vault folder") from None
    if result.returncode != 0:
        # Do not include provider output, which may contain account metadata.
        raise SetupError("Degoo rejected vault creation; check login.sh and the parent folder ID")
    try:
        text = result.stdout.decode("ascii").strip()
    except UnicodeDecodeError:
        raise SetupError("Degoo returned an invalid vault folder ID") from None
    if not text or "\n" in text or "\r" in text:
        raise SetupError("Degoo returned an invalid vault folder ID")
    return _validate_identifier(text, "created folder ID")


def _finish_install(root: Path, folder_id: str, name: str, *, created: bool, vault_config: str | None) -> int:
    command = [
        sys.executable,
        str(root / "scripts/python/install.py"),
        "--binary",
        str(root / "bin/rclone"),
        "--folder-id",
        folder_id,
        "--folder-name",
        name,
        "--upgrade",
    ]
    if created:
        command.append("--folder-created-by-setup")
    if vault_config:
        command.extend(("--vault-config", vault_config))
    try:
        result = subprocess.run(command, stdin=subprocess.DEVNULL, check=False, env={**os.environ, "PATH": "/usr/bin:/bin"})
    except OSError:
        raise SetupError("the contained install could not be completed") from None
    return result.returncode


def run(args: argparse.Namespace) -> int:
    root = _root(args.root)
    if bool(args.folder_id) == bool(args.parent_id):
        raise SetupError("choose either --folder-id for an existing folder or --parent-id with --name to create one")
    if args.folder_id:
        folder_id = _validate_identifier(args.folder_id, "--folder-id")
        name = _validate_name(args.name) if args.name else ""
        return _finish_install(root, folder_id, name, created=False, vault_config=args.vault_config)
    parent_id = _validate_identifier(args.parent_id, "--parent-id")
    name = _validate_name(args.name or "")
    folder_id = _run_create(root, parent_id, name)
    code = _finish_install(root, folder_id, name, created=True, vault_config=args.vault_config)
    if code != 0:
        print(f"The folder was created, but local setup did not finish. Re-run with --folder-id {folder_id} --name {name!r}.", file=sys.stderr)
        return code
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="contained install root")
    choice = parser.add_mutually_exclusive_group(required=True)
    choice.add_argument("--folder-id", help="use an existing dedicated folder")
    choice.add_argument("--parent-id", help="create a new dedicated folder under this device folder")
    parser.add_argument("--name", help="human-readable name for the folder; required with --parent-id")
    parser.add_argument("--vault-config", help="private existing vault configuration to import")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(args)
    except (SetupError, installer.InstallError) as exc:
        print(f"setup refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("setup refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
