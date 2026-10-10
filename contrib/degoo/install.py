#!/usr/bin/env python3
"""Install or upgrade a contained Linux Degoo vault client."""

from __future__ import annotations

import argparse
import configparser
import errno
import fcntl
import hashlib
import json
import os
import secrets
import shlex
import shutil
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any


ROOT_NAME = "rclone-degoo"
REMOTE_NAME = "degoo-live-test"
ROOT_REL = Path(".local/share") / ROOT_NAME
SOURCE_FILES = ("install.py", "login.py", "service.py", "setup.py", "uninstall.py")
MAX_CONFIG_SIZE = 1024 * 1024
DEFAULT_MOUNT_SETTINGS = {
    "cache_max_size": "20G",
    "cache_min_free_space": "20G",
    "cache_max_age": "10m",
    "cache_poll_interval": "5s",
    "verify_upload": True,
}
FUSERMOUNT_NAMES = {"fusermount", "fusermount2", "fusermount2.9", "fusermount3"}


class InstallError(Exception):
    """A safe, user-facing refusal from the contained installer."""


def home_directory() -> Path:
    home = Path(os.path.expanduser("~"))
    if not home.is_absolute():
        raise InstallError("could not determine an absolute home directory")
    return Path(os.path.normpath(str(home)))


def expected_root() -> Path:
    return home_directory() / ROOT_REL


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
    if manifest.get("mountpoint") != "mount" or manifest.get("cache_dir") != "cache":
        raise InstallError("the existing install manifest contains unexpected paths")
    folder_id = manifest.get("root_folder_id")
    if not isinstance(folder_id, str) or folder_id == "0":
        raise InstallError("the existing install manifest has an unsafe folder ID")
    return manifest


def read_vault(raw: bytes, root: Path, *, imported: bool) -> tuple[dict[str, Any], bytes]:
    config = parse_json_object(raw, "the vault configuration is invalid")
    allowed = {"version", "remote", "password", "password_salt", "cache_dir", "chunk_size", "max_object_size"}
    if set(config) - allowed:
        raise InstallError("the vault configuration has unsupported fields")
    if type(config.get("version")) is not int or config["version"] != 1:
        raise InstallError("the vault configuration version must be 1")
    if not isinstance(config.get("password"), str) or not config["password"]:
        raise InstallError("the vault configuration does not contain a password")
    if "password_salt" in config and not isinstance(config["password_salt"], str):
        raise InstallError("the vault configuration password salt is invalid")
    if not imported:
        if config.get("remote") != f"{REMOTE_NAME}:":
            raise InstallError("the installed vault configuration points to another remote")
        if config.get("cache_dir") != str(root / "cache"):
            raise InstallError("the installed vault configuration points outside this cache")
    else:
        remote = config.get("remote")
        if not isinstance(remote, str) or not remote.endswith(":") or not remote[:-1]:
            raise InstallError("the imported vault configuration has an invalid remote")
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


def create_vault_config(root: Path, imported: bytes | None) -> bytes:
    if imported is not None:
        return imported
    config = {
        "version": 1,
        "remote": f"{REMOTE_NAME}:",
        "password": secrets.token_urlsafe(32),
        "cache_dir": str(root / "cache"),
        "chunk_size": "1Gi",
        "max_object_size": "19Gi",
    }
    return (json.dumps(config, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_mount_settings(raw: bytes) -> None:
    settings = parse_json_object(raw, "config/mount.json is invalid")
    expected_fields = set(DEFAULT_MOUNT_SETTINGS)
    if set(settings) != expected_fields:
        raise InstallError("config/mount.json must contain the supported cache and upload settings")
    for field in ("cache_max_size", "cache_min_free_space", "cache_max_age", "cache_poll_interval"):
        if not isinstance(settings[field], str) or not settings[field] or any(character in settings[field] for character in "\r\n\x00"):
            raise InstallError(f"config/mount.json has an invalid {field}")
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


def rclone_config(root: Path, folder_id: str) -> bytes:
    return (
        f"[{REMOTE_NAME}]\n"
        "type = degoo\n"
        f"root_folder_id = {folder_id}\n"
        f"auth_file = {root / 'config/auth.json'}\n"
    ).encode("utf-8")


def shell_quote(value: str | Path) -> str:
    return shlex.quote(str(value))


def shell_wrapper(name: str, root: Path) -> bytes:
    root_text = shell_quote(root)
    if name == "service.sh":
        content = (
            "#!/bin/sh\nset -eu\n"
            f"ROOT={root_text}\n"
            "export PYTHONDONTWRITEBYTECODE=1\n"
            "exec python3 \"$ROOT/service.py\" --root \"$ROOT\" \"$@\"\n"
        )
        return content.encode("utf-8")
    if name == "login.sh":
        command = f"exec python3 {shell_quote(root / 'login.py')} --root \"$ROOT\" \"$@\""
    elif name == "setup.sh":
        command = f"exec python3 {shell_quote(root / 'setup.py')} --root \"$ROOT\" \"$@\""
    elif name == "mount.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/mount.log\" --log-level INFO degoo mount "
            f"--settings \"$ROOT/config/mount.json\" \"$ROOT/config/vault.json\" \"$ROOT/mount\" \"$@\""
        )
    elif name == "status.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"--log-file \"$ROOT/logs/status.log\" --log-level ERROR degoo status \"$ROOT/config/vault.json\" \"$@\""
        )
    elif name == "location.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"degoo location \"$ROOT/config/vault.json\" \"$@\""
        )
    elif name == "folders.sh":
        command = (
            f"exec \"$ROOT/bin/rclone\" --config \"$ROOT/config/rclone.conf\" "
            f"degoo folders \"$ROOT/config/auth.json\" \"$@\""
        )
    elif name == "stop.sh":
        command = (
            "if command -v mountpoint >/dev/null 2>&1 && ! mountpoint -q \"$ROOT/mount\"; then\n"
            "  exit 0\n"
            "fi\n"
            "if command -v fusermount3 >/dev/null 2>&1; then\n"
            "  if fusermount3 -u \"$ROOT/mount\"; then exit 0; fi\n"
            "elif command -v fusermount >/dev/null 2>&1; then\n"
            "  if fusermount -u \"$ROOT/mount\"; then exit 0; fi\n"
            "else\n"
            "  if command -v mountpoint >/dev/null 2>&1 && ! mountpoint -q \"$ROOT/mount\"; then exit 0; fi\n"
            "  echo 'FUSE unmount helper was not found (install fuse3 or fuse).' >&2\n"
            "  exit 1\n"
            "fi\n"
            "if command -v mountpoint >/dev/null 2>&1 && ! mountpoint -q \"$ROOT/mount\"; then exit 0; fi\n"
            "exit 1"
        )
    else:
        raise InstallError("an internal wrapper name was invalid")
    path = '"$ROOT/tools:/usr/bin:/bin"' if name in {"mount.sh", "stop.sh"} else "/usr/bin:/bin"
    content = f"#!/bin/sh\nset -eu\nROOT={root_text}\numask 077\nexport PYTHONDONTWRITEBYTECODE=1\nexport XDG_CONFIG_HOME=\"$ROOT/config\"\nexport XDG_CACHE_HOME=\"$ROOT/cache\"\nexport TMPDIR=\"$ROOT/tmp\"\nexport PATH={path}\n{command}\n"
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
        f"ExecStart={unit_quote(str(root / 'mount.sh'))}\n"
        f"ExecStop={unit_quote(str(root / 'stop.sh'))}\n"
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
        result = subprocess.run(
            [str(binary), "degoo", "--help"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            env={"HOME": str(home_directory()), "PATH": "/usr/bin:/bin", "LANG": "C"},
            timeout=30,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise InstallError("the supplied rclone binary could not verify its Degoo command") from None
    if result.returncode != 0 or b"mount" not in result.stdout or b"login" not in result.stdout:
        raise InstallError("--binary must be an rclone build with the Degoo mount and login commands")


def _install(args: argparse.Namespace) -> int:
    root = expected_root()
    ensure_parent_directories(root)
    root_exists = os.path.lexists(root)
    if args.upgrade:
        if not root_exists:
            raise InstallError("--upgrade requires an existing contained install")
        check_directory(root, private=True)
        manifest = load_manifest(root)
    else:
        if root_exists:
            raise InstallError("the contained install already exists; use --upgrade to preserve it")
        os.mkdir(root, 0o700)
        check_directory(root, private=True)
        manifest = None

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
    if manifest is None:
        manifest = {
            "version": 1,
            "root_folder_id": "",
            "remote_folder_name": "",
            "remote_created_by_setup": False,
            "remote_name": REMOTE_NAME,
            "mountpoint": "mount",
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
        vault, _ = read_vault(existing_vault, root, imported=False)
        vault_bytes = existing_vault
    elif folder_id or imported_vault is not None:
        vault_bytes = create_vault_config(root, imported_vault)
        vault, _ = read_vault(vault_bytes, root, imported=False)
    else:
        vault_bytes = None
        vault = None

    existing_settings = read_private_file(root / "config/mount.json", missing_ok=True)
    if existing_settings is not None:
        validate_mount_settings(existing_settings)
        settings_bytes = existing_settings
    else:
        settings_bytes = (json.dumps(DEFAULT_MOUNT_SETTINGS, indent=2, sort_keys=True) + "\n").encode("utf-8")

    copy_binary(args.binary, root / "bin/rclone")
    validate_binary(root / "bin/rclone")
    for source_name in SOURCE_FILES:
        source = Path(__file__).absolute().parent / source_name
        if not source.is_file() or source.is_symlink():
            raise InstallError("the contained installer source files are incomplete")
        destination = root / source_name
        data = source.read_bytes()
        write_private_file(destination, data, replace=os.path.lexists(destination), mode=0o700)

    if existing_vault is None and vault_bytes is not None:
        write_private_file(root / "config/vault.json", vault_bytes)
    if existing_settings is None:
        write_private_file(root / "config/mount.json", settings_bytes)
    if folder_id:
        if existing_rclone_config is None:
            write_private_file(root / "config/rclone.conf", rclone_config(root, folder_id))
        else:
            validate_rclone_config(existing_rclone_config, root, folder_id)

    for wrapper in ("login.sh", "setup.sh", "mount.sh", "status.sh", "location.sh", "folders.sh", "stop.sh", "service.sh"):
        write_private_file(root / wrapper, shell_wrapper(wrapper, root), replace=os.path.lexists(root / wrapper), mode=0o700)
    write_private_file(root / "rclone-degoo.service", service_unit(root), replace=os.path.lexists(root / "rclone-degoo.service"), mode=0o600)
    install_fuse_helper(root)

    if supplied_folder_id:
        manifest["root_folder_id"] = folder_id
    if folder_name:
        manifest["remote_folder_name"] = folder_name
    if args.folder_created_by_setup:
        manifest["remote_created_by_setup"] = True
    manifest.setdefault("service_links", [])
    manifest_data = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
    write_private_file(root / "install.json", manifest_data, replace=True)

    if folder_id:
        print(f"Installed the contained Degoo vault at {root}.")
        print(f"Account login: {root / 'login.sh'}")
        print(f"Mount: {root / 'mount.sh'}")
        print(f"User service: {root / 'service.sh'} enable")
    else:
        print(f"Installed the contained Degoo client at {root}; no vault folder is configured yet.")
        print(f"Log in with {root / 'login.sh'}, then run {root / 'setup.sh'} --parent-id DEVICE_ID --name VAULT_NAME.")
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


def _acquire_upgrade_lock(root: Path) -> int:
    if os.path.ismount(root) or os.path.ismount(root / "mount"):
        raise InstallError("stop the contained mount before upgrading")
    cache = root / "cache"
    check_directory(cache, private=True)
    digest = hashlib.sha256(f"{REMOTE_NAME}:".encode("utf-8")).hexdigest()[:24]
    path = cache / f"degoo-vault-{digest}.lock"
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


def install(args: argparse.Namespace) -> int:
    supplied_folder_id = validate_folder_id(args.folder_id) if args.folder_id else ""
    folder_name = validate_folder_name(args.folder_name) if args.folder_name else ""
    if folder_name and not supplied_folder_id and not args.upgrade:
        raise InstallError("--folder-name requires --folder-id")
    if args.folder_created_by_setup and not supplied_folder_id:
        raise InstallError("the setup-created folder marker requires --folder-id")
    args.binary = str(_binary_source(args.binary))
    validate_binary(Path(args.binary))
    if not args.upgrade:
        return _install(args)
    root = expected_root()
    if not os.path.lexists(root):
        raise InstallError("--upgrade requires an existing contained install")
    check_directory(root, private=True)
    load_manifest(root)
    lock = _acquire_upgrade_lock(root)
    try:
        return _install(args)
    finally:
        os.close(lock)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, help="path to a Linux rclone executable built with the Degoo command")
    parser.add_argument("--folder-id", help="ID of an existing dedicated Degoo folder (account root 0 is refused)")
    parser.add_argument("--folder-name", help="optional human-readable name for the dedicated folder")
    parser.add_argument("--vault-config", help="private existing vault configuration to import on another machine")
    parser.add_argument("--upgrade", action="store_true", help="update the contained client while preserving its vault, login, settings, and cache")
    parser.add_argument("--folder-created-by-setup", action="store_true", help=argparse.SUPPRESS)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return install(args)
    except InstallError as exc:
        print(f"install refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("install refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
