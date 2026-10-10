#!/usr/bin/env python3
"""Report local state for a contained Linux Degoo vault install."""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import shlex
import stat
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import install as installer
import logs as log_reader
import service as service_manager


MAX_JSON_SIZE = 1024 * 1024
MAX_STATUS_OUTPUT = 4096
MAX_REMOTE_OUTPUT = 64 * 1024
MAX_REMOTE_ERROR = 16 * 1024
STATUS_PATTERN = re.compile(rb"Pending local uploads: ([0-9]+) files, ([0-9]+) bytes\s*\Z")
SIZE_PATTERN = re.compile(r"(?:[0-9]+(?:\.[0-9]+)?)(?:[KMGTPE](?:iB?|B)?|B)?\Z", re.IGNORECASE)
DURATION_PATTERN = re.compile(r"(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+\Z")
MOUNT_ESCAPE = re.compile(r"\\([0-7]{3})")


class InfoError(Exception):
    """A safe, user-facing status reporting error."""


def _safe_text(value: str, maximum: int = 512) -> str:
    """Return bounded printable text without terminal or line controls."""
    return "".join(char for char in value if char.isprintable())[:maximum]


def _display_path(path: Path) -> str:
    text = _safe_text(str(path))
    try:
        home = str(Path.home())
    except RuntimeError:
        home = ""
    if home and (text == home or text.startswith(home + os.sep)):
        text = "~" + text[len(home) :]
    return text


def _read_json(path: Path, maximum: int = MAX_JSON_SIZE, *, missing_ok: bool = True) -> dict[str, Any] | None:
    try:
        raw = installer.read_private_file(path, maximum, missing_ok=missing_ok)
    except installer.InstallError:
        return None
    if raw is None:
        return None
    try:
        value = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    return value if isinstance(value, dict) else None


def _mountpoints() -> set[Path]:
    """Read mounted paths from proc without resolving or traversing them."""
    result: set[Path] = set()
    try:
        with open("/proc/self/mountinfo", encoding="utf-8", errors="replace") as stream:
            for line in stream:
                fields = line.split()
                if len(fields) > 4:
                    decoded = MOUNT_ESCAPE.sub(lambda match: chr(int(match.group(1), 8)), fields[4])
                    if decoded.startswith("/"):
                        result.add(Path(os.path.normpath(decoded)))
    except OSError:
        return set()
    return result


def mounted_state(mountpoint: Path, mountinfo: Path = Path("/proc/self/mountinfo")) -> str:
    """Return mounted, not mounted, or unknown using the kernel mount table."""
    target = Path(os.path.normpath(str(mountpoint)))
    try:
        with mountinfo.open(encoding="utf-8", errors="replace") as stream:
            for line in stream:
                fields = line.split()
                if len(fields) > 4:
                    decoded = MOUNT_ESCAPE.sub(lambda match: chr(int(match.group(1), 8)), fields[4])
                    if Path(os.path.normpath(decoded)) == target:
                        return "mounted"
    except OSError:
        return "unknown (mount table unavailable)"
    return "not mounted"


def scan_install_tree(
    root: Path,
    cache_dir: Path,
    mountpoint: Path,
    *,
    max_entries: int = 5000,
    max_seconds: float = 0.5,
    mountpoints: set[Path] | None = None,
) -> dict[str, int | bool]:
    """Estimate regular-file allocation without following links or entering mounts."""
    root = Path(os.path.normpath(str(root)))
    cache_dir = Path(os.path.normpath(str(cache_dir)))
    mountpoint = Path(os.path.normpath(str(mountpoint)))
    known_mounts = _mountpoints() if mountpoints is None else mountpoints
    if mountpoints is None and Path("/") not in known_mounts:
        return {
            "allocated": 0,
            "apparent": 0,
            "cache_allocated": 0,
            "cache_fs_allocated": 0,
            "entries": 0,
            "partial": True,
        }
    try:
        root_dev = root.stat().st_dev
        cache_dev = cache_dir.stat().st_dev
    except OSError:
        return {
            "allocated": 0,
            "apparent": 0,
            "cache_allocated": 0,
            "cache_fs_allocated": 0,
            "entries": 0,
            "partial": True,
        }

    allocated = 0
    apparent = 0
    cache_allocated = 0
    cache_fs_allocated = 0
    entries = 0
    partial = False
    started = time.monotonic()
    stack = [root]
    while stack:
        directory = stack.pop()
        if directory == mountpoint or (directory in known_mounts and directory != cache_dir):
            continue
        if directory != root and directory != cache_dir:
            try:
                directory_info = directory.stat(follow_symlinks=False)
            except OSError:
                partial = True
                continue
            if directory_info.st_dev not in {root_dev, cache_dev}:
                continue
        try:
            with os.scandir(directory) as children:
                for child in children:
                    entries += 1
                    if entries > max_entries or time.monotonic() - started > max_seconds:
                        partial = True
                        stack.clear()
                        break
                    path = Path(child.path)
                    if path == mountpoint or (path in known_mounts and path != cache_dir):
                        continue
                    try:
                        info = child.stat(follow_symlinks=False)
                    except OSError:
                        partial = True
                        continue
                    if info.st_dev not in {root_dev, cache_dev}:
                        continue
                    try:
                        if child.is_dir(follow_symlinks=False):
                            stack.append(path)
                        elif child.is_file(follow_symlinks=False):
                            blocks = getattr(info, "st_blocks", 0) * 512
                            allocated += blocks
                            apparent += info.st_size
                            if info.st_dev == cache_dev:
                                cache_fs_allocated += blocks
                            if path == cache_dir or cache_dir in path.parents:
                                cache_allocated += blocks
                    except OSError:
                        partial = True
        except OSError:
            partial = True
            continue
    return {
        "allocated": allocated,
        "apparent": apparent,
        "cache_allocated": cache_allocated,
        "cache_fs_allocated": cache_fs_allocated,
        "entries": entries,
        "partial": partial,
    }


def filesystem_stats(path: Path) -> dict[str, int] | None:
    """Return df-style byte counts for the filesystem containing path."""
    try:
        values = os.statvfs(path)
    except OSError:
        return None
    block = values.f_frsize or values.f_bsize
    total = values.f_blocks * block
    free = values.f_bfree * block
    available = values.f_bavail * block
    return {"total": total, "used": max(0, total - free), "available": available}


def _format_bytes(value: int) -> str:
    units = ("B", "KiB", "MiB", "GiB", "TiB", "PiB")
    amount = float(max(0, value))
    for unit in units:
        if amount < 1024 or unit == units[-1]:
            if unit == "B":
                return f"{int(amount)} {unit}"
            return f"{amount:.1f} {unit}"
        amount /= 1024
    return f"{value} B"


def _safe_setting(value: Any, *, duration: bool = False) -> str:
    if not isinstance(value, str):
        return "unknown"
    pattern = DURATION_PATTERN if duration else SIZE_PATTERN
    return value if len(value) <= 32 and pattern.fullmatch(value) else "unknown"


def _expiry_claim(token: str) -> tuple[str, float | None]:
    """Decode an unsigned JWT expiry hint; this is not token validation."""
    pieces = token.split(".")
    if len(pieces) != 3 or len(pieces[1]) > 8192:
        return "opaque or unreadable token", None
    try:
        payload = base64.urlsafe_b64decode(pieces[1] + "=" * (-len(pieces[1]) % 4))
        value = json.loads(payload)
    except (ValueError, json.JSONDecodeError, RecursionError):
        return "opaque or unreadable token", None
    if not isinstance(value, dict):
        return "opaque or unreadable token", None
    expiry = value.get("exp")
    if isinstance(expiry, bool) or not isinstance(expiry, (int, float)):
        return "JWT expiry claim unavailable", None
    try:
        return "JWT expiry claim", float(expiry)
    except (OverflowError, ValueError):
        return "JWT expiry claim unavailable", None


def _login_command(auth_path: Path) -> str:
    root = auth_path.parent.parent
    return " ".join(shlex.quote(value) for value in (str(root / "degoo-rc"), "login"))


def authentication_error(
    auth_path: Path,
    *,
    now: float | None = None,
    logs_dir: Path | None = None,
) -> str | None:
    """Return an actionable auth error when local evidence requires a login."""
    auth = _read_json(auth_path, 64 * 1024)
    command = _login_command(auth_path)
    if auth is None:
        if not os.path.lexists(auth_path):
            return f"No saved account token; log in before cloud operations by running {command}."
        return None
    current = time.time() if now is None else now
    try:
        auth_info = os.lstat(auth_path)
    except OSError:
        auth_info = None
    if (
        logs_dir is not None
        and auth_info is not None
        and stat.S_ISREG(auth_info.st_mode)
        and auth_info.st_uid == os.getuid()
        and not auth_info.st_mode & 0o077
        and not auth_info.st_mode & 0o6000
        and log_reader.auth_required_after(logs_dir, auth_info.st_mtime, now=current)
    ):
        return f"A recent Degoo request was rejected as requiring login. Run {command} to sign in again."
    token = auth.get("token")
    refresh = auth.get("refresh_token")
    has_refresh = isinstance(refresh, str) and bool(refresh)
    if (not isinstance(token, str) or not token) and not has_refresh:
        return f"No saved account token; log in before cloud operations by running {command}."
    if isinstance(token, str) and token and not has_refresh:
        _, expiry = _expiry_claim(token)
        if expiry is not None and expiry <= current:
            return (
                "The saved access token has expired and no refresh token is available; "
                f"log in before the next cloud request with {command}."
            )
    return None


def authentication_status(auth_path: Path, *, now: float | None = None, logs_dir: Path | None = None) -> str:
    """Describe cached token availability without exposing token contents."""
    error = authentication_error(auth_path, now=now, logs_dir=logs_dir)
    if error is not None:
        return error
    auth = _read_json(auth_path, 64 * 1024)
    if auth is None:
        return "Saved account state is unavailable or unreadable; token details are hidden."
    current = time.time() if now is None else now
    token = auth.get("token")
    refresh = auth.get("refresh_token")
    has_refresh = isinstance(refresh, str) and bool(refresh)
    if not isinstance(token, str) or not token:
        if has_refresh:
            return "No cached access token; a saved refresh token may renew it on the next cloud request."
        return "No saved account token; log in before cloud operations."
    hint, expiry = _expiry_claim(token)
    if expiry is None:
        if has_refresh:
            return "Access-token expiry is unknown; a saved refresh token may renew it on the next cloud request."
        return "Access-token expiry is unknown; saved state has no refresh token."
    try:
        expires_at = datetime.fromtimestamp(expiry, tz=timezone.utc).strftime("%Y-%m-%d %H:%M UTC")
    except (OverflowError, OSError, ValueError):
        return "JWT expiry claim is outside the supported date range; token status is unknown."
    if expiry <= current:
        if has_refresh:
            return f"{hint} expired at {expires_at}; a saved refresh token may renew it on the next cloud request."
        return f"{hint} expired at {expires_at}; log in before the next cloud request."
    suffix = " A refresh token is also saved." if has_refresh else ""
    return f"{hint} is {expires_at} (local claim only; not server-validated).{suffix}"


def pending_uploads(root: Path, timeout: float = 5.0) -> tuple[int, int] | None:
    """Read only the expected local pending-count line from the packaged helper."""
    helper = root / "scripts" / "status.sh"
    if not _owned_executable(helper):
        return None
    try:
        with tempfile.TemporaryFile(dir=root / "tmp") as output:
            result = subprocess.run(
                [str(helper)],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.DEVNULL,
                timeout=timeout,
                check=False,
            )
            output.seek(0)
            data = output.read(MAX_STATUS_OUTPUT + 1)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0 or len(data) > MAX_STATUS_OUTPUT:
        return None
    match = STATUS_PATTERN.fullmatch(data)
    if not match:
        return None
    try:
        return int(match.group(1)), int(match.group(2))
    except ValueError:
        return None


def _systemctl(*args: str, timeout: float = 2.0) -> subprocess.CompletedProcess[bytes] | None:
    env = dict(os.environ)
    env.setdefault("HOME", str(installer.home_directory()))
    env["PATH"] = "/usr/bin:/bin"
    try:
        return subprocess.run(
            ["systemctl", "--user", *args],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return None


def linger_status(timeout: float = 2.0) -> str:
    """Read whether the current user's systemd manager is configured to linger."""
    env = dict(os.environ)
    env.setdefault("HOME", str(installer.home_directory()))
    env["PATH"] = "/usr/bin:/bin"
    try:
        result = subprocess.run(
            ["loginctl", "show-user", str(os.getuid()), "--property=Linger", "--value"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=timeout,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return "unknown"
    if result.returncode != 0 or len(result.stdout) > 128:
        return "unknown"
    value = result.stdout.decode("utf-8", errors="ignore").strip().lower()
    if value in {"yes", "true", "1"}:
        return "enabled"
    if value in {"no", "false", "0"}:
        return "disabled"
    return "unknown"


def _recorded_service_links(root: Path) -> list[str]:
    try:
        manifest = service_manager._load_manifest(root)
        unit = service_manager._unit_path(root)
        direct, wanted = service_manager._owned_paths(root, manifest)
        return [
            str(path)
            for path in (direct, wanted)
            if str(path) in manifest.get("service_links", [])
            and service_manager._validate_owned_link(path, unit)
        ]
    except (service_manager.ServiceError, installer.InstallError):
        return []


def service_status(root: Path, manifest: dict[str, Any]) -> str:
    """Report systemd state only when an owned install link resolves the unit."""
    try:
        checked_manifest = service_manager._load_manifest(root)
        links = checked_manifest.get("service_links", [])
        if not links:
            return "not configured for this install"
        unit = service_manager._unit_path(root)
        direct, wanted = service_manager._owned_paths(root, checked_manifest)
        allowed = {str(unit)}
        for path in (direct, wanted):
            if str(path) in links and service_manager._validate_owned_link(path, unit):
                allowed.add(str(path))
        if len(allowed) == 1:
            return "unknown (owned service link is missing)"
    except (service_manager.ServiceError, installer.InstallError):
        return "unknown (service ownership could not be verified)"

    loaded = _systemctl("show", "--property=FragmentPath", "--value", service_manager.UNIT_NAME)
    if loaded is None or loaded.returncode != 0 or len(loaded.stdout) > 4096:
        return "unknown (user systemd manager is unavailable)"
    try:
        fragment = loaded.stdout.decode("utf-8").strip()
    except UnicodeDecodeError:
        return "unknown (service path could not be read)"
    if fragment not in allowed:
        return "unknown (systemd resolved a different service unit)"

    enabled_result = _systemctl("is-enabled", service_manager.UNIT_NAME)
    active_result = _systemctl("is-active", service_manager.UNIT_NAME)
    enabled = "unknown"
    active = "unknown"
    if enabled_result is not None and len(enabled_result.stdout) <= 256:
        value = enabled_result.stdout.decode("utf-8", errors="ignore").strip()
        if value in {"enabled", "enabled-runtime", "linked", "linked-runtime"}:
            enabled = "enabled"
        elif value in {"disabled", "static", "indirect", "masked", "generated", "transient", "alias"}:
            enabled = "disabled"
    if active_result is not None and len(active_result.stdout) <= 256:
        value = active_result.stdout.decode("utf-8", errors="ignore").strip()
        if value in {"active", "inactive", "failed", "activating", "deactivating", "reloading"}:
            active = value
    return f"{enabled}; {active}"


def _remote_names(data: bytes) -> list[str] | None:
    if len(data) > MAX_REMOTE_OUTPUT:
        return None
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    if not isinstance(value, list) or len(value) > 128:
        return None
    names: list[str] = []
    for entry in value:
        if not isinstance(entry, dict) or not isinstance(entry.get("name"), str):
            return None
        name = _safe_text(entry["name"], 255)
        if not name:
            return None
        names.append(name)
    return names


def remote_location(root: Path, *, timeout: float = 12.0) -> tuple[str, list[str] | None]:
    """Optionally query remote ancestry while discarding raw provider output."""
    binary = root / "bin" / "rclone"
    vault = root / "config" / "vault.json"
    if not _owned_executable(binary):
        return "remote check could not start", None
    env = dict(os.environ)
    env.setdefault("HOME", str(installer.home_directory()))
    env["XDG_CACHE_HOME"] = str(root / "cache")
    env["TMPDIR"] = str(root / "tmp")
    env["PATH"] = "/usr/bin:/bin"
    try:
        with tempfile.TemporaryFile(dir=root / "tmp") as output, tempfile.TemporaryFile(dir=root / "tmp") as error:
            result = subprocess.run(
                [
                    str(binary),
                    "--config",
                    str(root / "config" / "rclone.conf"),
                    "--log-file",
                    "/dev/null",
                    "--log-level",
                    "ERROR",
                    "degoo",
                    "location",
                    str(vault),
                ],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=error,
                env=env,
                timeout=timeout,
                check=False,
            )
            output.seek(0)
            data = output.read(MAX_REMOTE_OUTPUT + 1)
            error.seek(0)
            error_data = error.read(MAX_REMOTE_ERROR + 1)
    except subprocess.TimeoutExpired:
        return "remote check timed out", None
    except OSError:
        return "remote check could not start", None
    if len(data) > MAX_REMOTE_OUTPUT or len(error_data) > MAX_REMOTE_ERROR:
        return "remote check failed", None
    if result.returncode == 0:
        names = _remote_names(data)
        return ("checked", names) if names is not None else ("remote response was not recognized", None)
    safe_error = error_data.decode("utf-8", errors="ignore")
    if re.search(r"(?:^|\b)degoo login required(?:\b|$)", safe_error, re.IGNORECASE):
        return "login required for remote check", None
    return "remote check failed", None


def _cached_remote_path(manifest: dict[str, Any]) -> str:
    path = manifest.get("remote_path")
    if isinstance(path, list) and path and all(isinstance(item, str) for item in path):
        names = [_safe_text(item, 255) for item in path]
        if all(names):
            return " / ".join(names)
    name = manifest.get("remote_folder_name")
    if isinstance(name, str) and name:
        parent = manifest.get("parent_folder_id")
        if isinstance(parent, str) and parent:
            return f"{_safe_text(name, 255)} (parent ID { _safe_text(parent, 128) })"
        return _safe_text(name, 255)
    return "not recorded in local install metadata"


def _settings_summary(root: Path) -> list[str]:
    settings = _read_json(root / "config" / "mount.json", 64 * 1024)
    if settings is None:
        return ["Mount settings: unavailable or unreadable."]
    maximum = _safe_setting(settings.get("cache_max_size"))
    minimum = _safe_setting(settings.get("cache_min_free_space"))
    age = _safe_setting(settings.get("cache_max_age"), duration=True)
    poll = _safe_setting(settings.get("cache_poll_interval"), duration=True)
    write_back = _safe_setting(settings.get("write_back"), duration=True)
    verify = settings.get("verify_upload")
    verify_text = "enabled" if verify is True else "disabled" if verify is False else "unknown"
    return [
        f"  Cache maximum size: {maximum} (soft target; dirty or open files cannot be evicted).",
        f"  Minimum free-space target: {minimum} (soft target; dirty or open files are retained).",
        f"  Cache maximum age: {age} (eligible closed, clean files use last-access age; cleanup runs on polling cadence).",
        f"  Cache polling interval: {poll}.",
        f"  Delay before uploading closed files: {write_back}.",
        f"  Upload verification: {verify_text}.",
    ]


def _encryption_summary(root: Path) -> str:
    vault = _read_json(root / "config" / "vault.json", MAX_JSON_SIZE)
    if vault is None:
        return "Vault encryption settings: not configured or unavailable."
    chunk = _safe_setting(vault.get("chunk_size"))
    maximum = _safe_setting(vault.get("max_object_size"))
    return f"Vault format: encrypted chunks up to {chunk}; remote object limit configured as {maximum}."


def _owned_executable(path: Path) -> bool:
    try:
        info = os.lstat(path)
    except OSError:
        return False
    return (
        stat.S_ISREG(info.st_mode)
        and info.st_uid == os.getuid()
        and not info.st_mode & 0o022
        and not info.st_mode & 0o6000
        and bool(info.st_mode & 0o100)
    )


def _validate_root(value: str) -> tuple[Path, dict[str, Any]]:
    root = Path(os.path.expanduser(value))
    if not root.is_absolute() or ".." in root.parts:
        raise InfoError("--root must be an absolute path without parent traversal")
    root = Path(os.path.normpath(str(root)))
    try:
        installer.check_directory(root, private=True)
        for name in ("bin", "config", "cache", "mount", "tools", "tmp", "logs"):
            installer.check_directory(root / name, private=True)
        for name in ("auth.json", "login.json", "mount.json", "rclone.conf", "vault.json"):
            installer.read_private_file(root / "config" / name, missing_ok=True)
        manifest = installer.load_manifest(root)
    except installer.InstallError as exc:
        raise InfoError(str(exc)) from None
    return root, manifest


def report(root: Path, manifest: dict[str, Any], *, check_remote: bool = False) -> str:
    """Build a bounded human-readable report without printing private state."""
    lines = ["Contained Degoo vault status", f"Install: {_display_path(root)}"]
    auth_path = root / "config" / "auth.json"
    auth_error = authentication_error(auth_path, logs_dir=root / "logs")
    if auth_error:
        lines.insert(2, f"AUTHENTICATION ERROR: {auth_error}")
    folder_id = manifest.get("root_folder_id")
    if isinstance(folder_id, str) and folder_id and folder_id != "0":
        lines.append(f"Vault folder ID: {_safe_text(folder_id, 128)}")
    parent_id = manifest.get("parent_folder_id")
    if isinstance(parent_id, str) and parent_id:
        lines.append(f"Parent folder ID (cached): {_safe_text(parent_id, 128)}")
    lines.append(f"Remote path (cached local metadata): {_cached_remote_path(manifest)}")
    lines.append(f"Mount point: {_display_path(root / 'mount')} — {mounted_state(root / 'mount')}")
    lines.append(f"User service: {service_status(root, manifest)}")
    lines.append(
        f"User lingering: {linger_status()} "
        "(keeps the user manager available across logout; the service must also be enabled)."
    )

    config_paths = (
        ("Vault config", root / "config" / "vault.json", "vault password and encryption settings"),
        ("Account auth", root / "config" / "auth.json", "account tokens; contents are never displayed"),
        (
            "Login email",
            root / "config" / "login.json",
            f"remembered email only; {_login_command(root / 'config' / 'auth.json')} --clear-email forgets it",
        ),
        ("Mount settings", root / "config" / "mount.json", "cache and upload behavior"),
        ("Rclone config", root / "config" / "rclone.conf", "contained remote and auth-file path"),
        ("Install manifest", root / "install.json", "local vault identity and owned systemd-link records"),
        ("User service unit", root / service_manager.UNIT_NAME, "mount startup, restart policy, and stop command"),
        ("Service command", root / "degoo-rc", "service enable, disable, start, stop, and inspect the user service"),
        ("Cache", root / "cache", "local cached file data and pending writes"),
        ("Logs", root / "logs", "contained command logs"),
    )
    lines.append("Local paths:")
    for label, path, description in config_paths:
        lines.append(f"  {label}: {_display_path(path)} ({description})")
    links = _recorded_service_links(root)
    if links:
        lines.append("  Owned external user-service links:")
        lines.extend(f"    {_display_path(Path(path))}" for path in links)

    lines.append("Saved mount and cache settings (CLI overrides on a running mount are not confirmed):")
    lines.extend(_settings_summary(root))
    lines.append(_encryption_summary(root))

    counts = pending_uploads(root) if (root / "config" / "vault.json").is_file() else None
    if counts is None:
        lines.append("Pending uploads: unavailable (the local status check did not complete).")
    else:
        lines.append(f"Pending uploads: {counts[0]} files, {_format_bytes(counts[1])} (local cache status).")
    lines.append("Pending counts describe local dirty uploads; they do not audit cloud contents or confirm remote integrity.")

    filesystem = filesystem_stats(root / "cache")
    scan = scan_install_tree(root, root / "cache", root / "mount")
    lines.append(f"Cache backing filesystem: {_display_path(root / 'cache')}")
    if filesystem is None:
        lines.append("  Disk usage: unavailable.")
    else:
        lines.append(
            "  Disk: "
            f"{_format_bytes(filesystem['used'])} used of {_format_bytes(filesystem['total'])}; "
            f"{_format_bytes(filesystem['available'])} available."
        )
        other = max(0, filesystem["used"] - int(scan["cache_fs_allocated"]))
        lines.append(
            "  Other data on this filesystem: approximately "
            f"{_format_bytes(other)} after subtracting install files found on this filesystem."
        )
    qualifier = " (partial bounded scan)" if scan["partial"] else ""
    lines.append(
        "  Approximate install allocation: "
        f"{_format_bytes(int(scan['allocated']))} allocated, {_format_bytes(int(scan['apparent']))} apparent "
        f"across {scan['entries']} entries{qualifier}; symlinks and the mounted tree are skipped."
    )
    lines.append(f"  Approximate cache allocation: {_format_bytes(int(scan['cache_allocated']))}.")
    if filesystem is not None:
        lines.append(
            "  Install allocation found on this filesystem: "
            f"{_format_bytes(int(scan['cache_fs_allocated']))}."
        )

    lines.append("Saved account authentication (running mount credentials are not inspected):")
    lines.append(f"  {authentication_status(auth_path, logs_dir=root / 'logs')}")
    lines.append("  This summary reads saved auth.json; a running mount may hold different in-memory token state.")
    lines.append("  This local check does not contact Degoo or validate the token with the server.")
    if check_remote:
        state, names = remote_location(root)
        if state == "checked" and names is not None:
            location = " / ".join(names) if names else "account root"
            lines.append(f"Remote check: succeeded; current folder ancestry: {location}.")
        else:
            lines.append(f"Remote check: {state}; no provider error details were printed.")
            if state == "login required for remote check" and not auth_error:
                command = _login_command(auth_path)
                lines.insert(
                    2,
                    f"AUTHENTICATION ERROR: Degoo rejected the current account authentication. Run {command} to sign in again.",
                )
    else:
        lines.append(
            "Remote ancestry is cached local metadata; use "
            f"{shlex.quote(str(root / 'degoo-rc'))} info --check-remote to query Degoo."
        )
    return "\n".join(lines) + "\n"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument(
        "--check-remote",
        action="store_true",
        help="query Degoo for current folder ancestry (may refresh account authentication)",
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        root, manifest = _validate_root(args.root)
        sys.stdout.write(report(root, manifest, check_remote=args.check_remote))
        return 0
    except InfoError as exc:
        print(f"info refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("info refused: local status could not be read safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
