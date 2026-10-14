"""Read Degoo folder metadata through the contained rclone installation."""

from __future__ import annotations

import json
import os
import re
import subprocess
import tempfile
from pathlib import Path
from typing import Any

import install as installer


MAX_RESPONSE_BYTES = 1024 * 1024
MAX_ERROR_BYTES = 16 * 1024
MAX_FOLDER_COUNT = 4096
MAX_FOLDER_ID_LENGTH = 256
MAX_NAME_LENGTH = 255
QUERY_TIMEOUT = 120
SELECTABLE_CATEGORIES = frozenset({1, 2, 6})
LOGIN_REQUIRED_PATTERN = re.compile(rb"\bdegoo login required\b", re.IGNORECASE)


class ConnectionCheckError(Exception):
    """A safe error returned when an account metadata check cannot complete."""


class LoginRequiredError(ConnectionCheckError):
    """The stored Degoo account authentication needs an interactive login."""


class ProviderQueryError(ConnectionCheckError):
    """The Degoo folder query failed for a reason other than rejected auth."""


def _safe_parent_id(parent_id: str) -> bool:
    return (
        isinstance(parent_id, str)
        and 0 < len(parent_id) <= MAX_FOLDER_ID_LENGTH
        and all(0x20 < ord(character) < 0x7F for character in parent_id)
    )


def _safe_folder_id(value: Any) -> bool:
    return (
        isinstance(value, str)
        and value != "0"
        and 0 < len(value) <= MAX_FOLDER_ID_LENGTH
        and all(0x20 < ord(character) < 0x7F for character in value)
    )


def _safe_folder_name(value: Any) -> bool:
    return (
        isinstance(value, str)
        and 0 < len(value) <= MAX_NAME_LENGTH
        and all(character.isprintable() for character in value)
    )


def _folder_entries(data: bytes) -> list[dict[str, Any]]:
    if len(data) > MAX_RESPONSE_BYTES:
        raise ProviderQueryError("Degoo returned an unrecognized folder response")
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise ProviderQueryError("Degoo returned an unrecognized folder response") from None
    if not isinstance(value, list) or len(value) > MAX_FOLDER_COUNT:
        raise ProviderQueryError("Degoo returned an unrecognized folder response")
    entries: list[dict[str, Any]] = []
    for item in value:
        if not isinstance(item, dict):
            continue
        folder_id = item.get("id")
        name = item.get("name")
        category = item.get("category")
        if (
            not _safe_folder_id(folder_id)
            or not _safe_folder_name(name)
            or type(category) is not int
            or category not in SELECTABLE_CATEGORIES
        ):
            continue
        entries.append({"id": folder_id, "name": name, "category": category})
    return entries


def _run_folder_query(root: Path, parent_id: str) -> bytes:
    root = Path(root)
    try:
        cache_dir = installer.configured_cache_dir(root)
    except installer.InstallError:
        raise ProviderQueryError("the configured local cache directory is unavailable or unsafe") from None
    binary = root / "bin" / "rclone"
    config = root / "config" / "rclone.conf"
    auth = root / "config" / "auth.json"
    output_path = root / "tmp"
    env = {
        "HOME": str(installer.home_directory()),
        "XDG_CONFIG_HOME": str(root / "config"),
        "XDG_CACHE_HOME": str(cache_dir),
        "TMPDIR": str(output_path),
        "PATH": "/usr/bin:/bin",
        "LANG": "C.UTF-8",
    }
    arguments = [
        str(binary),
        "--config",
        str(config),
        "--log-level",
        "ERROR",
        "degoo",
        "folders",
        str(auth),
        "--parent-id",
        parent_id,
    ]
    vault = root / "config/vault.json"
    if os.path.lexists(vault):
        arguments.extend(("--vault-config", str(vault)))
    try:
        with tempfile.TemporaryFile(dir=output_path) as output, tempfile.TemporaryFile(dir=output_path) as error:
            result = subprocess.run(
                arguments,
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=error,
                env=env,
                timeout=QUERY_TIMEOUT,
                check=False,
                umask=0o077,
            )
            output.seek(0)
            response = output.read(MAX_RESPONSE_BYTES + 1)
            error.seek(0)
            error_output = error.read(MAX_ERROR_BYTES + 1)
    except subprocess.TimeoutExpired:
        raise ProviderQueryError("Degoo folder query timed out; check connectivity and retry") from None
    except OSError:
        raise ProviderQueryError("Degoo folder query could not start; check the installation and retry") from None
    if result.returncode != 0:
        if len(error_output) <= MAX_ERROR_BYTES and LOGIN_REQUIRED_PATTERN.search(error_output):
            raise LoginRequiredError("Degoo account authentication is missing or expired; log in and retry")
        raise ProviderQueryError("Degoo folder query failed; check connectivity and provider availability")
    if len(response) > MAX_RESPONSE_BYTES:
        raise ProviderQueryError("Degoo returned an unrecognized folder response")
    return response


def list_folders(root: Path, parent_id: str = "0") -> list[dict[str, Any]]:
    """Return safe selectable folder metadata under a Degoo parent ID."""
    if not _safe_parent_id(parent_id):
        raise ConnectionCheckError("Degoo folder query requires a valid parent folder ID")
    response = _run_folder_query(root, parent_id)
    return _folder_entries(response)


def check_connection(root: Path) -> list[dict[str, Any]]:
    """Check saved Degoo authentication and return account device folders."""
    return list_folders(root, parent_id="0")
