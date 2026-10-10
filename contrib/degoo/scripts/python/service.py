#!/usr/bin/env python3
"""Manage the contained Degoo mount as a persistent systemd user service."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import stat
import subprocess
import sys
from pathlib import Path
from typing import Any

import install as installer


UNIT_NAME = "rclone-degoo.service"


class ServiceError(Exception):
    """A safe, user-facing service management refusal."""


def _root(value: str) -> Path:
    try:
        root = Path(os.path.expanduser(value))
        if not root.is_absolute() or ".." in root.parts:
            raise ServiceError("--root must be an absolute path without parent traversal")
        root = Path(os.path.normpath(str(root)))
        installer.check_directory(root, private=True)
        installer.check_directory(root / "config", private=True)
        manifest = installer.load_manifest(root)
        if not manifest.get("root_folder_id"):
            raise ServiceError("configure a dedicated vault folder with setup.sh before enabling its service")
        if installer.read_private_file(root / "config/vault.json", missing_ok=True) is None:
            raise ServiceError("the vault configuration is missing")
        if installer.read_private_file(root / "config/rclone.conf", missing_ok=True) is None:
            raise ServiceError("the Degoo remote configuration is missing")
        return root
    except installer.InstallError as exc:
        raise ServiceError(str(exc)) from None


def _config_home(root: Path, manifest: dict[str, Any] | None = None) -> Path:
    value = manifest.get("service_config_home") if manifest else None
    if not value:
        value = os.environ.get("XDG_CONFIG_HOME")
    if value:
        if not isinstance(value, str):
            raise ServiceError("the recorded user-service config directory is invalid")
        path = Path(os.path.expanduser(value))
        if not path.is_absolute() or ".." in path.parts:
            raise ServiceError("XDG_CONFIG_HOME must be an absolute safe path")
        path = Path(os.path.normpath(str(path)))
    else:
        path = installer.home_directory() / ".config"
    try:
        if os.path.commonpath((str(root), str(path))) == str(root):
            raise ServiceError("the user-service config directory must be outside the contained install")
    except ValueError:
        raise ServiceError("XDG_CONFIG_HOME must be an absolute safe path") from None
    return path


def _unit_dir(root: Path, manifest: dict[str, Any] | None = None) -> Path:
    return _config_home(root, manifest) / "systemd" / "user"


def _unit_path(root: Path) -> Path:
    return root / UNIT_NAME


def _owned_paths(root: Path, manifest: dict[str, Any] | None = None) -> tuple[Path, Path]:
    directory = _unit_dir(root, manifest)
    return directory / UNIT_NAME, directory / "default.target.wants" / UNIT_NAME


def _ensure_external_directory(path: Path) -> None:
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            os.mkdir(current, 0o700)
        except FileExistsError:
            pass
        except OSError:
            raise ServiceError("the user systemd config directory could not be created") from None
        try:
            info = os.lstat(current)
        except OSError:
            raise ServiceError("the user systemd config directory could not be inspected") from None
        if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
            raise ServiceError("symlink or non-directory components in the user systemd path are not allowed")
        system_shared = info.st_uid == 0 and not (info.st_mode & 0o022)
        sticky_temp = info.st_uid == 0 and bool(info.st_mode & stat.S_ISVTX)
        if not (info.st_uid == os.getuid() or system_shared or sticky_temp):
            raise ServiceError(f"{current} has unsafe ownership for a user systemd directory")
        if info.st_mode & 0o022 and not sticky_temp:
            hint = f"; run chmod go-w {shlex.quote(str(current))} if appropriate" if info.st_uid == os.getuid() else ""
            raise ServiceError(f"{current} is writable by other users{hint}")
    try:
        final_info = os.lstat(path)
    except OSError:
        raise ServiceError("the user systemd config directory could not be inspected") from None
    if final_info.st_uid != os.getuid():
        raise ServiceError("the user systemd config directory is not owned by the current user")


def _normalized_target(path: Path) -> Path:
    try:
        target = Path(os.readlink(path))
    except OSError:
        raise ServiceError("an owned user-service link could not be read") from None
    if not target.is_absolute():
        target = path.parent / target
    return Path(os.path.normpath(str(target)))


def _validate_owned_link(path: Path, unit: Path) -> bool:
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return False
    except OSError:
        raise ServiceError("a user-service link could not be inspected safely") from None
    if not stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid():
        raise ServiceError("an expected user-service path is occupied by an unowned file")
    target = _normalized_target(path)
    if target != unit:
        raise ServiceError("an expected user-service link points to another unit")
    return True


def _load_manifest(root: Path) -> dict[str, Any]:
    try:
        raw = installer.read_private_file(root / "install.json", 64 * 1024)
    except installer.InstallError as exc:
        raise ServiceError(str(exc)) from None
    try:
        manifest = json.loads(raw or b"{}")
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise ServiceError("the install manifest is invalid") from None
    if not isinstance(manifest, dict) or not isinstance(manifest.get("service_links", []), list):
        raise ServiceError("the service ownership record is invalid")
    if "service_config_home" in manifest and not isinstance(manifest["service_config_home"], str):
        raise ServiceError("the service ownership record contains an invalid config directory")
    direct, wanted = _owned_paths(root, manifest)
    allowed = {str(direct), str(wanted)}
    links = manifest.get("service_links", [])
    if any(not isinstance(item, str) or item not in allowed for item in links) or len(set(links)) != len(links):
        raise ServiceError("the service ownership record contains an unexpected external path")
    return manifest


def _store_links(root: Path, manifest: dict[str, Any], *, present: bool) -> None:
    direct, wanted = _owned_paths(root, manifest)
    links: list[str] = []
    for path in (direct, wanted):
        if _validate_owned_link(path, _unit_path(root)):
            links.append(str(path))
    if present and str(direct) not in links:
        raise ServiceError("the contained user-service link was not created")
    manifest["service_links"] = links
    data = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
    try:
        installer.write_private_file(root / "install.json", data, replace=True)
    except installer.InstallError as exc:
        raise ServiceError(str(exc)) from None


def _systemctl(root: Path, *arguments: str, capture: bool = True) -> subprocess.CompletedProcess[bytes]:
    env = {
        key: value
        for key, value in os.environ.items()
        if key in {"HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS", "LANG", "LC_ALL"}
    }
    env.setdefault("HOME", str(installer.home_directory()))
    env["PATH"] = "/usr/bin:/bin"
    try:
        return subprocess.run(
            ["systemctl", "--user", *arguments],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE if capture else None,
            stderr=subprocess.PIPE if capture else None,
            env=env,
            timeout=45,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ServiceError("systemctl --user could not complete the request") from None


def _require_loaded_unit(root: Path, manifest: dict[str, Any]) -> None:
    result = _systemctl(root, "show", "--property=FragmentPath", "--value", UNIT_NAME)
    if result.returncode != 0:
        raise ServiceError("the Degoo user service is not loaded; run service.sh enable first")
    try:
        fragment = Path(result.stdout.decode("utf-8").strip())
    except UnicodeDecodeError:
        raise ServiceError("systemd returned an invalid service path") from None
    if not fragment.is_absolute():
        raise ServiceError("systemd resolved this service name to a different unit")
    fragment_text = os.path.normpath(str(fragment))
    allowed = {str(_unit_path(root))}
    direct, wanted = _owned_paths(root, manifest)
    for link in (direct, wanted):
        if str(link) in manifest.get("service_links", []) and _validate_owned_link(link, _unit_path(root)):
            allowed.add(str(link))
    if fragment_text not in allowed:
        raise ServiceError("systemd resolved this service name to a different unit")


def enable(root: Path) -> int:
    manifest = _load_manifest(root)
    if not manifest.get("service_config_home"):
        manifest["service_config_home"] = str(_config_home(root, manifest))
    unit = _unit_path(root)
    try:
        unit_data = installer.read_private_file(unit, 64 * 1024)
    except installer.InstallError as exc:
        raise ServiceError(str(exc)) from None
    if not unit_data:
        raise ServiceError("the contained user-service unit is empty")
    direct, wanted = _owned_paths(root, manifest)
    _ensure_external_directory(direct.parent)
    if wanted.parent.exists() or os.path.lexists(wanted.parent):
        _ensure_external_directory(wanted.parent)
    if _validate_owned_link(direct, unit):
        pass
    else:
        result = _systemctl(root, "link", str(unit))
        if result.returncode != 0:
            raise ServiceError("systemd could not link the contained user-service unit")
        if not _validate_owned_link(direct, unit):
            raise ServiceError("systemd did not create the expected user-service link")
    _store_links(root, manifest, present=True)
    result = _systemctl(root, "daemon-reload")
    if result.returncode != 0:
        raise ServiceError("systemd could not reload the user-service units")
    _require_loaded_unit(root, manifest)
    result = _systemctl(root, "enable", "--now", UNIT_NAME)
    _store_links(root, manifest, present=True)
    if result.returncode != 0:
        raise ServiceError(
            "the service is linked, but could not start; run login.sh, then service.sh status and inspect the user journal"
        )
    print("The Degoo user service is enabled and has been asked to start.")
    return 0


def disable(root: Path) -> int:
    manifest = _load_manifest(root)
    direct, _ = _owned_paths(root, manifest)
    if str(direct) not in manifest.get("service_links", []) or not _validate_owned_link(direct, _unit_path(root)):
        raise ServiceError("this install does not own a linked Degoo user service")
    _require_loaded_unit(root, manifest)
    result = _systemctl(root, "disable", "--now", UNIT_NAME)
    if result.returncode != 0:
        raise ServiceError("systemd could not stop and disable the Degoo user service")
    reload_result = _systemctl(root, "daemon-reload")
    if reload_result.returncode != 0:
        raise ServiceError("systemd could not reload after disabling the Degoo user service")
    _store_links(root, manifest, present=False)
    print("The Degoo user service is stopped and disabled. Run service.sh enable to link it again.")
    return 0


def status(root: Path) -> int:
    manifest = _load_manifest(root)
    direct, _ = _owned_paths(root, manifest)
    if str(direct) not in manifest.get("service_links", []) or not _validate_owned_link(direct, _unit_path(root)):
        raise ServiceError("the Degoo user service has not been linked; run service.sh enable")
    _require_loaded_unit(root, manifest)
    result = _systemctl(root, "status", UNIT_NAME, capture=False)
    return result.returncode


def service_action(root: Path, action: str) -> int:
    manifest = _load_manifest(root)
    direct, _ = _owned_paths(root, manifest)
    if str(direct) not in manifest.get("service_links", []) or not _validate_owned_link(direct, _unit_path(root)):
        raise ServiceError("the Degoo user service is not linked; run service.sh enable first")
    _require_loaded_unit(root, manifest)
    result = _systemctl(root, action, UNIT_NAME)
    if result.returncode != 0:
        raise ServiceError(f"systemd could not {action} the Degoo user service")
    if action == "stop":
        print("The Degoo user service stopped. It remains enabled for the next user-service start.")
    elif action == "start":
        print("The Degoo user service was asked to start.")
    else:
        print("The Degoo user service was asked to restart.")
    return 0


def run(root: Path, action: str) -> int:
    root = _root(str(root))
    if action == "enable":
        return enable(root)
    if action == "disable":
        return disable(root)
    if action == "status":
        return status(root)
    if action in {"start", "stop", "restart"}:
        return service_action(root, action)
    raise ServiceError("choose enable, disable, or status")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="contained install root")
    parser.add_argument("action", choices=("enable", "disable", "start", "stop", "restart", "status"))
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(Path(args.root), args.action)
    except (ServiceError, installer.InstallError) as exc:
        print(f"service refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("service refused: filesystem access failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
