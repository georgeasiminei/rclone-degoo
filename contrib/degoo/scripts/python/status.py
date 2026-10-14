#!/usr/bin/env python3
"""Report Degoo vault health and local upload state without querying cloud data."""

from __future__ import annotations

import argparse
import http.client
import hashlib
import json
import os
import re
import stat
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from typing import Any

import info as local_info
import install as installer
import sync as local_sync


MAX_STATUS_OUTPUT = 4096
MAX_CACHE_METADATA = 64 * 1024
MAX_SCAN_FILES = 1000
MAX_SCAN_ENTRIES = 5000
MAX_SCAN_SECONDS = 1.0
STATUS_TIMEOUT = 5.0
UPLOAD_STATUS_XATTR = "user.rclone.upload-status"
UPLOAD_STATES = {"pending", "uploading", "uploaded", "unknown"}
STATUS_PATTERN = re.compile(rb"Pending local uploads: ([0-9]{1,20}) files, ([0-9]{1,20}) bytes\r?\n?\Z")


class StatusError(Exception):
    """A safe refusal to inspect local vault status."""


def _safe_text(value: str, maximum: int = 512) -> str:
    """Return bounded printable text suitable for terminal and JSON output."""
    return "".join(character if character.isprintable() else "�" for character in value)[:maximum]


def _validate_root(value: str) -> tuple[Path, dict[str, Any]]:
    try:
        return local_info._validate_root(value)
    except local_info.InfoError as exc:
        raise StatusError(str(exc)) from None


def _parse_counts(data: bytes) -> tuple[int, int] | None:
    """Accept only the bounded local pending-count line emitted by rclone."""
    if len(data) > MAX_STATUS_OUTPUT:
        return None
    match = STATUS_PATTERN.fullmatch(data)
    if match is None:
        return None
    try:
        files, size = int(match.group(1)), int(match.group(2))
    except ValueError:
        return None
    if files > (1 << 63) - 1 or size > (1 << 63) - 1:
        return None
    return files, size


def pending_uploads(root: Path, timeout: float = STATUS_TIMEOUT) -> tuple[int, int] | None:
    """Read dirty local VFS totals using the contained legacy rclone command."""
    vault_path = root / "config" / "vault.json"
    try:
        vault_data = installer.read_private_file(vault_path, installer.MAX_CONFIG_SIZE, missing_ok=True)
    except installer.InstallError:
        return None
    if vault_data is None:
        return None
    try:
        vault = json.loads(vault_data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    if not isinstance(vault, dict) or not isinstance(vault.get("remote"), str) or not vault["remote"]:
        return None
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError:
        return None

    binary = root / "bin" / "rclone"
    if not local_info._owned_executable(binary):
        return None
    log_path = root / "logs" / "status.log"
    if not local_info._prepare_diagnostic_log(log_path):
        return None
    env = {
        "HOME": str(installer.home_directory()),
        "XDG_CONFIG_HOME": str(root / "config"),
        "XDG_CACHE_HOME": str(cache_dir),
        "TMPDIR": str(root / "tmp"),
        "PATH": "/usr/bin:/bin",
        "LANG": "C.UTF-8",
        "RCLONE_CONFIG": str(root / "config" / "rclone.conf"),
    }
    try:
        with tempfile.TemporaryFile(dir=root / "tmp") as output:
            result = subprocess.run(
                [
                    str(binary),
                    "--config",
                    str(root / "config" / "rclone.conf"),
                    "--log-file",
                    str(log_path),
                    "--log-level",
                    "ERROR",
                    "degoo",
                    "status",
                    str(vault_path),
                ],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.DEVNULL,
                env=env,
                timeout=timeout,
                check=False,
                umask=0o077,
            )
            output.seek(0)
            data = output.read(MAX_STATUS_OUTPUT + 1)
    except (OSError, subprocess.TimeoutExpired):
        return None
    if result.returncode != 0:
        return None
    return _parse_counts(data)


def live_uploads(root: Path) -> dict[str, int] | None:
    """Read only aggregate upload and retry counts from the private mount socket."""
    try:
        socket_path = local_sync._socket_path(root)
    except local_sync.SyncError:
        return None
    connection = local_sync.UnixHTTPConnection(socket_path, timeout=STATUS_TIMEOUT)
    try:
        connection.request("GET", "/status")
        response = connection.getresponse()
        data = response.read(MAX_STATUS_OUTPUT + 1)
        if response.status != 200 or len(data) > MAX_STATUS_OUTPUT:
            return None
        value = json.loads(data)
    except (OSError, http.client.HTTPException, UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    finally:
        connection.close()
    fields = {"queued", "uploading", "retrying"}
    if not isinstance(value, dict) or set(value) != fields:
        return None
    if any(type(value[name]) is not int or not 0 <= value[name] < (1 << 63) for name in fields):
        return None
    return value


def _cache_name(remote: str) -> str:
    """Return the VFS cache namespace used by the Degoo vault command."""
    digest = hashlib.sha256(remote.encode("utf-8")).hexdigest()[:24]
    return f"degoo-vault-{digest}"


def cache_name(remote: str) -> str:
    """Expose the deterministic local cache namespace for diagnostics and tests."""
    return _cache_name(remote)


def _cache_roots(root: Path) -> tuple[Path, Path] | None:
    """Return the local data and metadata roots for this vault, if configured."""
    try:
        raw = installer.read_private_file(root / "config" / "vault.json", installer.MAX_CONFIG_SIZE, missing_ok=True)
    except installer.InstallError:
        return None
    if raw is None:
        return None
    try:
        config = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    if not isinstance(config, dict):
        return None
    remote = config.get("remote")
    if not isinstance(remote, str) or not remote or len(remote) > 4096:
        return None
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError:
        return None
    name = _cache_name(remote)
    return cache_dir / "vfs" / name, cache_dir / "vfsMeta" / name


def _checked_private_directory(path: Path) -> bool:
    """Check an existing cache directory without following symlinks."""
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    except OSError:
        raise StatusError("a local cache directory could not be inspected safely") from None
    if (
        not stat.S_ISDIR(info.st_mode)
        or stat.S_ISLNK(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
        or info.st_mode & 0o6000
    ):
        raise StatusError("a local cache directory has unsafe ownership or permissions")
    return True


def _check_cache_directories(base: Path, directory: Path, cache_root: Path) -> bool:
    """Validate every existing directory from the private cache root to directory."""
    try:
        directory.relative_to(base)
        relative = directory.relative_to(cache_root)
    except ValueError:
        raise StatusError("a local cache path escaped the configured cache directory") from None
    current = cache_root
    if not _checked_private_directory(current):
        return False
    for component in relative.parts:
        current /= component
        if not _checked_private_directory(current):
            return False
    return True


def _safe_cache_file(path: Path, root: Path) -> os.stat_result | None:
    """Inspect a cache data file as private regular metadata-free data."""
    try:
        installer.check_components(path, missing_ok=True)
    except installer.InstallError:
        return None
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return None
    except OSError:
        return None
    if (
        not stat.S_ISREG(info.st_mode)
        or stat.S_ISLNK(info.st_mode)
        or info.st_uid != os.getuid()
        or info.st_mode & 0o077
        or info.st_mode & 0o6000
        or info.st_nlink != 1
    ):
        return None
    return info


def _cache_record(root: Path, relative: Path, *, displayed: str | None = None) -> dict[str, Any] | None:
    """Read one cache metadata item and classify only local evidence."""
    if not relative.parts or any(part in {".", ".."} for part in relative.parts):
        return None
    roots = _cache_roots(root)
    if roots is None:
        return None
    data_root, metadata_root = roots
    try:
        cache_root = installer.configured_cache_dir(root)
    except installer.InstallError:
        return None
    metadata_path = metadata_root.joinpath(*relative.parts)
    data_path = data_root.joinpath(*relative.parts)
    try:
        if not _check_cache_directories(metadata_root, metadata_path.parent, cache_root):
            return None
        if not _check_cache_directories(data_root, data_path.parent, cache_root):
            return None
        raw = installer.read_private_file(metadata_path, MAX_CACHE_METADATA, missing_ok=True)
    except installer.InstallError:
        return None
    if raw is None:
        return None
    data_info = _safe_cache_file(data_path, root)
    try:
        metadata = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        metadata = None
    filename = _safe_text(displayed or relative.as_posix())
    if not isinstance(metadata, dict) or type(metadata.get("Dirty")) is not bool:
        return {
            "path": filename,
            "state": "unknown",
            "evidence": "private local cache metadata",
            "detail": "Local cache metadata is unavailable or malformed.",
            "size_bytes": data_info.st_size if data_info else None,
            "live_cloud_verified": False,
        }

    dirty = metadata["Dirty"]
    fingerprint = metadata.get("Fingerprint")
    has_fingerprint = isinstance(fingerprint, str) and bool(fingerprint) and len(fingerprint) <= 4096
    size = metadata.get("Size")
    size_bytes = size if type(size) is int and 0 <= size <= (1 << 63) - 1 else None
    if size_bytes is None and data_info is not None:
        size_bytes = data_info.st_size
    if data_info is None:
        state = "unknown"
        detail = "Cache metadata exists, but the corresponding local cache data file is unavailable."
    elif dirty:
        state = "pending"
        detail = "Local cache metadata marks this file dirty; its cached data survives process restarts."
    elif has_fingerprint:
        state = "cached-uploaded"
        detail = (
            "A clean local cache entry has a previously observed remote fingerprint; the cache may be stale "
            "and this does not verify current cloud contents."
        )
    else:
        state = "unknown"
        detail = "The local cache entry is clean but has no recorded remote fingerprint."
    return {
        "path": filename,
        "state": state,
        "evidence": "private local cache metadata",
        "detail": detail,
        "size_bytes": size_bytes,
        "live_cloud_verified": False,
    }


def _local_record(path: Path, relative: Path) -> dict[str, Any]:
    """Describe an ordinary file below an unmounted mountpoint."""
    try:
        file_info = os.lstat(path)
    except OSError:
        file_info = None
    return {
        "path": _safe_text(relative.as_posix() or "."),
        "state": "local-only",
        "evidence": "ordinary file below an unmounted mountpoint",
        "detail": "This is a local file, not part of the vault and not a pending cloud upload.",
        "size_bytes": file_info.st_size if file_info and stat.S_ISREG(file_info.st_mode) else None,
        "live_cloud_verified": False,
    }


def _xattr_record(path: Path, relative: Path, size: int | None) -> dict[str, Any]:
    """Read the mounted VFS upload-state hint for one regular file."""
    try:
        raw = os.getxattr(path, UPLOAD_STATUS_XATTR, follow_symlinks=False)
        value = raw.decode("ascii") if len(raw) <= 32 else ""
    except (AttributeError, OSError, UnicodeDecodeError):
        value = ""
    state = value if value in UPLOAD_STATES else "unknown"
    details = {
        "pending": "The mounted VFS reports a local write awaiting upload.",
        "uploading": "The mounted VFS reports that the upload is in progress; local cache data remains available.",
        "uploaded": (
            "The mounted VFS reports a known remote representation after writeback; this is not an independent "
            "live cloud revalidation."
        ),
        "unknown": "The mounted VFS has no readable upload-state hint for this file.",
    }
    return {
        "path": _safe_text(relative.as_posix() or "."),
        "state": state,
        "evidence": "mounted VFS upload-status xattr",
        "detail": details[state],
        "size_bytes": size,
        "live_cloud_verified": False,
    }


def _resolve_local_path(
    mountpoint: Path, local_path: str | os.PathLike[str], *, cwd: Path | None = None
) -> tuple[Path, Path, os.stat_result | None]:
    """Resolve a requested path under the mountpoint without traversing symlinks."""
    try:
        requested = Path(os.fspath(local_path))
    except (TypeError, ValueError):
        raise StatusError("the selected path is invalid") from None
    if not str(requested) or "\x00" in str(requested) or ".." in requested.parts:
        raise StatusError("the selected path must not contain parent traversal")
    if not requested.is_absolute():
        base = Path.cwd() if cwd is None else cwd
        if base != mountpoint and mountpoint not in base.parents:
            base = mountpoint
        requested = base / requested
    target = Path(os.path.normpath(str(requested)))
    mount_root = Path(os.path.normpath(str(mountpoint)))
    try:
        relative = target.relative_to(mount_root)
    except ValueError:
        raise StatusError("the selected path is outside the contained mountpoint") from None

    try:
        mount_info = os.lstat(mount_root)
    except OSError:
        raise StatusError("the contained mountpoint could not be inspected safely") from None
    if not stat.S_ISDIR(mount_info.st_mode) or stat.S_ISLNK(mount_info.st_mode):
        raise StatusError("the contained mountpoint is not a real directory")

    current = mount_root
    target_info: os.stat_result | None = mount_info if not relative.parts else None
    for index, component in enumerate(relative.parts):
        current /= component
        try:
            component_info = os.lstat(current)
        except FileNotFoundError:
            target_info = None
            break
        except OSError:
            raise StatusError("the selected local path could not be inspected safely") from None
        if stat.S_ISLNK(component_info.st_mode):
            raise StatusError("symlink components are not allowed in a selected path")
        if index < len(relative.parts) - 1 and not stat.S_ISDIR(component_info.st_mode):
            raise StatusError("a selected path parent is not a directory")
        target_info = component_info
    return target, relative, target_info


def _scan_context() -> dict[str, Any]:
    return {"started": time.monotonic(), "entries": 0, "files": 0, "truncated": False}


def _scan_limit(context: dict[str, Any]) -> bool:
    if (
        context["entries"] >= MAX_SCAN_ENTRIES
        or context["files"] >= MAX_SCAN_FILES
        or time.monotonic() - context["started"] > MAX_SCAN_SECONDS
    ):
        context["truncated"] = True
        return True
    return False


def _walk_directory(
    root: Path,
    start: Path,
    relative_start: Path,
    *,
    mounted: bool,
    context: dict[str, Any],
) -> list[dict[str, Any]]:
    """Walk only a directory explicitly selected by the caller."""
    records: list[dict[str, Any]] = []
    stack = [(start, relative_start)]
    while stack and not context["truncated"]:
        directory, relative_directory = stack.pop()
        try:
            with os.scandir(directory) as children:
                for child in children:
                    if _scan_limit(context):
                        break
                    context["entries"] += 1
                    try:
                        child_info = child.stat(follow_symlinks=False)
                    except FileNotFoundError:
                        continue
                    except OSError:
                        raise StatusError("a selected directory entry could not be inspected") from None
                    if stat.S_ISLNK(child_info.st_mode):
                        continue
                    path = Path(child.path)
                    relative = relative_directory / child.name
                    if stat.S_ISDIR(child_info.st_mode):
                        stack.append((path, relative))
                    elif stat.S_ISREG(child_info.st_mode):
                        context["files"] += 1
                        if mounted:
                            records.append(_xattr_record(path, relative, child_info.st_size))
                        else:
                            records.append(_local_record(path, relative))
        except OSError:
            raise StatusError("the selected directory could not be read") from None
    return records


def _walk_cache_metadata(
    root: Path, relative_start: Path, *, context: dict[str, Any]
) -> list[dict[str, Any]]:
    """Read bounded cached VFS metadata below a selected offline directory."""
    roots = _cache_roots(root)
    if roots is None:
        return []
    data_root, metadata_root = roots
    try:
        cache_root = installer.configured_cache_dir(root)
    except installer.InstallError:
        return []
    start = metadata_root.joinpath(*relative_start.parts)
    if not _check_cache_directories(metadata_root, start, cache_root):
        return []
    records: list[dict[str, Any]] = []
    stack = [(start, relative_start)]
    while stack and not context["truncated"]:
        directory, relative_directory = stack.pop()
        try:
            with os.scandir(directory) as children:
                for child in children:
                    if _scan_limit(context):
                        break
                    context["entries"] += 1
                    try:
                        child_info = child.stat(follow_symlinks=False)
                    except FileNotFoundError:
                        continue
                    except OSError:
                        raise StatusError("a local cache entry could not be inspected") from None
                    if stat.S_ISLNK(child_info.st_mode):
                        continue
                    path = Path(child.path)
                    relative = relative_directory / child.name
                    if stat.S_ISDIR(child_info.st_mode):
                        if _check_cache_directories(metadata_root, path, cache_root):
                            stack.append((path, relative))
                    elif stat.S_ISREG(child_info.st_mode):
                        context["files"] += 1
                        item = _cache_record(root, relative, displayed=relative.as_posix())
                        if item is not None:
                            records.append(item)
        except OSError:
            raise StatusError("a local cache directory could not be read") from None
    return records


def _merge_offline_records(
    local_records: list[dict[str, Any]], cache_records: list[dict[str, Any]]
) -> list[dict[str, Any]]:
    """Keep local mountpoint files distinct from cached vault files with the same name."""
    result = {record["path"]: record for record in local_records}
    for cached in cache_records:
        existing = result.get(cached["path"])
        if existing is None:
            result[cached["path"]] = cached
            continue
        existing["cached_state"] = cached["state"]
        existing["cached_detail"] = cached["detail"]
        existing["cached_evidence"] = cached["evidence"]
    return list(result.values())


def inspect_path(
    root: Path,
    local_path: str | os.PathLike[str],
    *,
    mount_state: str,
    cwd: Path | None = None,
) -> tuple[list[dict[str, Any]], bool]:
    """Report file states under one selected path without scanning by default."""
    if mount_state not in {"mounted", "not mounted"}:
        raise StatusError("the kernel mount state is unknown; per-file status is unavailable")
    mountpoint = installer.configured_mountpoint(root)
    target, relative, target_info = _resolve_local_path(mountpoint, local_path, cwd=cwd)
    is_directory = target_info is not None and stat.S_ISDIR(target_info.st_mode)
    if target_info is not None and not is_directory and not stat.S_ISREG(target_info.st_mode):
        raise StatusError("the selected path must be a regular file or directory")

    if is_directory:
        context = _scan_context()
        local_records = _walk_directory(
            root,
            target,
            relative,
            mounted=mount_state == "mounted",
            context=context,
        )
        cache_records: list[dict[str, Any]] = []
        if mount_state == "not mounted":
            cache_records = _walk_cache_metadata(root, relative, context=context)
        return _merge_offline_records(local_records, cache_records), bool(context["truncated"])

    filename = _safe_text(relative.as_posix() or ".")
    if target_info is not None:
        if mount_state == "mounted":
            return [_xattr_record(target, relative, target_info.st_size)], False
        local_record = _local_record(target, relative)
        cached = _cache_record(root, relative, displayed=filename)
        return _merge_offline_records([local_record], [cached] if cached else []), False

    if mount_state == "mounted":
        raise StatusError("the selected file does not exist in the mounted vault")
    cached = _cache_record(root, relative, displayed=filename)
    if cached is not None:
        return [cached], False
    return [
        {
            "path": filename,
            "state": "unknown",
            "evidence": "no local file or cache metadata",
            "detail": "No persisted local upload state is available for this path.",
            "size_bytes": None,
            "live_cloud_verified": False,
        }
    ], False


def _service_error(service: str, *, configured: bool, mount_state: str) -> str | None:
    if not configured:
        return None
    if service.startswith("unknown ("):
        return f"SERVICE ERROR: the configured user service could not be verified ({service})."
    try:
        _, active = (part.strip() for part in service.split(";", 1))
    except ValueError:
        return "SERVICE ERROR: the configured user service state is invalid."
    if active == "failed" or (mount_state != "mounted" and active != "active"):
        return f"SERVICE ERROR: the configured user service is {active}; inspect degoo-rc logs and degoo-rc service."
    return None


def build_report(
    root: Path,
    manifest: dict[str, Any],
    *,
    local_path: str | os.PathLike[str] | None = None,
    cwd: Path | None = None,
) -> tuple[dict[str, Any], int]:
    """Build health-first local status, returning a nonzero code for health errors."""
    errors: list[str] = []
    auth_error = local_info.authentication_error(root / "config" / "auth.json", logs_dir=root / "logs")
    if auth_error:
        errors.append(f"AUTHENTICATION ERROR: {auth_error}")

    mountpoint = installer.configured_mountpoint(root)
    mount_state = local_info.mounted_state(mountpoint)
    if mount_state != "mounted":
        if mount_state == "not mounted":
            errors.append("MOUNT ERROR: the Degoo vault is not mounted; start it with degoo-rc mount.")
        else:
            errors.append("MOUNT ERROR: the kernel mount state could not be verified.")

    unmounted_entries = False
    if mount_state == "not mounted":
        try:
            with os.scandir(mountpoint) as entries:
                unmounted_entries = next(entries, None) is not None
        except OSError:
            errors.append("MOUNT ERROR: the unmounted mount directory could not be inspected.")
        if unmounted_entries:
            errors.append(
                "MOUNT DIRECTORY WARNING: local files remain below the unmounted mountpoint; recover or move "
                "them before mounting. The protected mount refuses a nonempty directory, and status did not "
                "modify these files; these files are local only."
            )

    service = local_info.service_status(root, manifest)
    service_links = manifest.get("service_links")
    service_configured = (isinstance(service_links, list) and bool(service_links)) or service.startswith("enabled;")
    service_error = _service_error(service, configured=service_configured, mount_state=mount_state)
    if service_error:
        errors.append(service_error)

    folder_id = manifest.get("root_folder_id")
    vault_configured = isinstance(folder_id, str) and bool(folder_id) and folder_id != "0"
    if not vault_configured:
        errors.append("VAULT ERROR: no dedicated vault folder is configured; run degoo-rc setup.")
        counts = None
    else:
        counts = pending_uploads(root)
    if counts is None:
        errors.append("STATUS ERROR: local pending-upload totals are unavailable.")

    live = live_uploads(root) if mount_state == "mounted" else None
    if mount_state == "mounted":
        if live is None:
            errors.append("STATUS ERROR: live upload state is unavailable; restart the updated mount and inspect degoo-rc logs.")
        elif live["retrying"]:
            errors.append(f"UPLOAD ERROR: {live['retrying']} file(s) have failed upload attempts and remain pending for retry; inspect degoo-rc logs.")

    files: list[dict[str, Any]] = []
    files_truncated = False
    if local_path is not None:
        try:
            files, files_truncated = inspect_path(root, local_path, mount_state=mount_state, cwd=cwd)
        except StatusError as exc:
            errors.append(f"FILE STATUS ERROR: {exc}")

    report: dict[str, Any] = {
        "health": "error" if errors else "ok",
        "health_errors": errors,
        "mount": mount_state,
        "service": service,
        "live_uploads": live,
        "pending": {
            "available": counts is not None,
            "files": counts[0] if counts is not None else None,
            "bytes": counts[1] if counts is not None else None,
            "source": "local dirty VFS metadata; not a cloud integrity check",
        },
        "cloud_note": "Zero pending local files does not verify cloud contents, completeness, or integrity.",
        "files": files,
        "files_truncated": files_truncated,
    }
    return report, 1 if errors else 0


def format_report(report: dict[str, Any]) -> str:
    """Format the health summary before pending and per-file details."""
    lines = list(report["health_errors"])
    if not lines:
        lines.append("No local health errors detected.")
    lines.append(f"Mount: {report['mount']}.")
    lines.append(f"User service: {_safe_text(str(report['service']))}.")
    pending = report["pending"]
    if not pending["available"]:
        lines.append("Pending local uploads: unavailable.")
    else:
        lines.append(f"Pending local uploads: {pending['files']} files, {pending['bytes']} bytes.")
    lines.append(report["cloud_note"])
    live = report["live_uploads"]
    if live is not None:
        lines.append(f"Live uploads: {live['queued']} queued, {live['uploading']} uploading, {live['retrying']} retrying after failure.")
    for record in report["files"]:
        size = record.get("size_bytes")
        size_text = f", {size} bytes" if type(size) is int else ""
        lines.append(
            f"File {record['path']}: {record['state']}{size_text} ({record['detail']})"
        )
        if "cached_state" in record:
            lines.append(f"  Separate cached vault state: {record['cached_state']} ({record['cached_detail']})")
    if report["files_truncated"]:
        lines.append(f"File scan stopped at the safety limit of {MAX_SCAN_FILES} files or {MAX_SCAN_SECONDS:g} seconds.")
    return "\n".join(lines) + "\n"


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("local_path", nargs="?", help="optional file or directory under the contained mountpoint")
    parser.add_argument("--root", default=str(installer.expected_root()), help="contained install root")
    parser.add_argument("--json", action="store_true", help="write machine-readable JSON")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        root, manifest = _validate_root(args.root)
        report, code = build_report(root, manifest, local_path=args.local_path)
        if args.json:
            sys.stdout.write(json.dumps(report, ensure_ascii=True, separators=(",", ":")) + "\n")
        else:
            sys.stdout.write(format_report(report))
        return code
    except StatusError as exc:
        if args.json:
            sys.stdout.write(json.dumps({"health": "error", "error": str(exc)}, ensure_ascii=True) + "\n")
        else:
            print(f"status refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        if args.json:
            sys.stdout.write(json.dumps({"health": "error", "error": "local status could not be read safely"}) + "\n")
        else:
            print("status refused: local status could not be read safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
