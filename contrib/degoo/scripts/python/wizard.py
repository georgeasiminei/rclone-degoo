#!/usr/bin/env python3
"""Guide a user through a contained Linux Degoo vault installation."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any, Callable

import install as installer
import login
import service
import setup


DEFAULT_FOLDER_NAME = "rclone-degoo-vault"
SIZE_PATTERN = re.compile(r"(?:[0-9]+(?:\.[0-9]+)?)(?:[KMGTPE](?:iB?)?|B)?\Z", re.IGNORECASE)
DURATION_PATTERN = re.compile(r"(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+\Z", re.IGNORECASE)
FUSE_HELPERS = ("fusermount3", "fusermount", "fusermount2", "fusermount2.9")
SELECTABLE_CATEGORIES = {1, 2, 6}
PACKAGE_MANAGERS: tuple[tuple[str, tuple[str, ...]], ...] = (
    ("apt-get", ("install", "-y")),
    ("dnf", ("install", "-y")),
    ("pacman", ("-S", "--needed", "--noconfirm")),
    ("zypper", ("--non-interactive", "install")),
)


class WizardError(Exception):
    """A safe, user-facing wizard error."""


class WizardCancelled(Exception):
    """The user cancelled the interactive wizard."""


def _prompt(label: str, default: str | None = None, *, input_fn: Callable[[str], str] | None = None) -> str:
    suffix = f" [{default}]" if default is not None and default else " [leave blank]" if default == "" else ""
    try:
        value = (input_fn or input)(f"{label}{suffix}: ").strip()
    except EOFError:
        raise WizardCancelled from None
    return default if not value and default is not None else value


def _yes_no(label: str, *, default: bool, input_fn: Callable[[str], str] | None = None) -> bool:
    marker = "Y/n" if default else "y/N"
    while True:
        try:
            value = (input_fn or input)(f"{label} [{marker}]: ").strip().casefold()
        except EOFError:
            raise WizardCancelled from None
        if not value:
            return default
        if value in {"y", "yes"}:
            return True
        if value in {"n", "no"}:
            return False
        print("Please answer yes or no.")


def _size_bytes(value: str) -> int:
    if not SIZE_PATTERN.fullmatch(value):
        raise WizardError("enter a size such as 1Gi or 512Mi")
    match = re.fullmatch(r"([0-9]+(?:\.[0-9]+)?)(.*)", value, re.IGNORECASE)
    if not match:
        raise WizardError("enter a size such as 1Gi or 512Mi")
    try:
        amount = Decimal(match.group(1))
    except InvalidOperation:
        raise WizardError("enter a valid size") from None
    suffix = match.group(2).upper()
    if suffix == "B":
        multiplier = Decimal(1)
    else:
        suffix = suffix.removesuffix("IB").removesuffix("I")
        if suffix:
            power = "KMGTPE".find(suffix) + 1
            if power <= 0:
                raise WizardError("enter a valid size")
            multiplier = Decimal(1024**power)
        else:
            # rclone interprets a bare number as kibibytes.
            multiplier = Decimal(1024)
    amount *= multiplier
    result = int(amount)
    if result <= 0:
        raise WizardError("size must be greater than zero")
    return result


def _read_text_setting(settings: dict[str, Any], key: str, default: str, *, duration: bool = False) -> str:
    value = settings.get(key)
    pattern = DURATION_PATTERN if duration else SIZE_PATTERN
    return value if isinstance(value, str) and len(value) <= 32 and pattern.fullmatch(value) else default


def _source_roots() -> list[Path]:
    source = Path(__file__).resolve()
    roots: list[Path] = []
    for index in (2, 4):
        if len(source.parents) > index:
            roots.append(source.parents[index])
    return list(dict.fromkeys(roots))


def _source_repo() -> Path | None:
    return next((root for root in _source_roots() if (root / "go.mod").is_file() and (root / "rclone.go").is_file()), None)


def _default_binary() -> Path:
    source_repo = _source_repo()
    candidates = [source_repo / "build/rclone", source_repo / "rclone"] if source_repo else []
    for root in _source_roots():
        if root != source_repo:
            candidates.extend((root / "bin/rclone", root / "rclone"))
    candidates.extend((installer.expected_root() / "bin/rclone", Path.cwd() / "rclone"))
    for candidate in candidates:
        try:
            info = os.stat(candidate)
        except OSError:
            continue
        if stat.S_ISREG(info.st_mode) and info.st_mode & 0o111:
            return candidate
    return candidates[0]


def _required_go_version(repo: Path) -> tuple[int, int] | None:
    try:
        text = (repo / "go.mod").read_text(encoding="utf-8")
    except OSError:
        return None
    match = re.search(r"(?m)^go\s+(\d+)\.(\d+)(?:\.\d+)?\s*$", text)
    return (int(match.group(1)), int(match.group(2))) if match else None


def _go_available(repo: Path) -> bool:
    go = shutil.which("go")
    required = _required_go_version(repo)
    if not go or required is None:
        return False
    try:
        result = subprocess.run(
            [go, "version"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    match = re.search(rb"\bgo(\d+)\.(\d+)(?:\.\d+)?\b", result.stdout)
    return bool(result.returncode == 0 and match and (int(match.group(1)), int(match.group(2))) >= required)


def _build_binary(repo: Path) -> tuple[str, tempfile.TemporaryDirectory]:
    go = shutil.which("go")
    if not go:
        raise WizardError("Go is unavailable; provide a prebuilt Degoo-enabled rclone binary")
    required = _required_go_version(repo)
    if required is None:
        raise WizardError("the repository Go version requirement could not be read")
    try:
        version = subprocess.run(
            [go, "version"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            timeout=10,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise WizardError("the Go toolchain could not be checked") from None
    match = re.search(rb"\bgo(\d+)\.(\d+)(?:\.\d+)?\b", version.stdout)
    if version.returncode != 0 or not match or (int(match.group(1)), int(match.group(2))) < required:
        raise WizardError(f"Go {required[0]}.{required[1]} or newer is required to build this checkout")
    temporary = tempfile.TemporaryDirectory(prefix="rclone-degoo-build-")
    target = Path(temporary.name) / "rclone"
    print(f"Building rclone with Go {required[0]}.{required[1]} or newer. Go may download module dependencies.")
    try:
        result = subprocess.run(
            [go, "build", "-o", str(target), "."],
            cwd=repo,
            stdin=None,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        temporary.cleanup()
        raise WizardError("rclone could not be built from this checkout") from None
    if result.returncode != 0:
        temporary.cleanup()
        raise WizardError("rclone build failed; check the Go build output")
    return str(target), temporary


def _dependency_state() -> tuple[bool, bool]:
    helper = any(shutil.which(name, path="/usr/bin:/bin") for name in FUSE_HELPERS)
    mountpoint = bool(shutil.which("mountpoint", path="/usr/bin:/bin"))
    return helper, mountpoint


def _package_manager() -> tuple[str, tuple[str, ...]] | None:
    for name, arguments in PACKAGE_MANAGERS:
        executable = shutil.which(name, path="/usr/bin:/bin")
        if executable:
            return executable, arguments
    return None


def _install_packages(packages: list[str], purpose: str, *, input_fn: Callable[[str], str]) -> bool:
    if not packages:
        return True
    manager = _package_manager()
    if not manager:
        print(f"{purpose} packages are missing; no supported package manager was found. Install them manually.")
        return False
    executable, prefix = manager
    package_name = Path(executable).name
    manager_names = {"apt-get": "apt-get", "dnf": "dnf", "pacman": "pacman", "zypper": "zypper"}
    manager_name = manager_names.get(package_name, package_name)
    print(f"Missing {purpose} packages: {', '.join(packages)} (package manager: {manager_name}).")
    if not _yes_no(f"Install these {purpose} packages now?", default=True, input_fn=input_fn):
        print(f"Install the {purpose} packages manually before using this feature.")
        return False
    command = [executable, *prefix, *packages]
    if os.geteuid() != 0:
        sudo = shutil.which("sudo", path="/usr/bin:/bin")
        if not sudo:
            print("sudo is unavailable; install the listed packages manually.")
            return False
        command.insert(0, sudo)
    print(f"Installing the required {purpose} packages…")
    try:
        result = subprocess.run(command, stdin=None, check=False, timeout=1800)
    except (OSError, subprocess.TimeoutExpired):
        raise WizardError("the package manager could not complete runtime dependency installation") from None
    if result.returncode != 0:
        raise WizardError(f"{purpose} package installation failed; check the package manager output")
    return True


def _install_missing_packages(helper_missing: bool, mountpoint_missing: bool, *, input_fn: Callable[[str], str]) -> bool:
    packages = []
    if helper_missing:
        packages.append("fuse3")
    if mountpoint_missing:
        packages.append("util-linux")
    return _install_packages(packages, "runtime dependency", input_fn=input_fn)


def _fuse_device_ready() -> bool:
    try:
        fuse_device = os.stat("/dev/fuse")
    except OSError:
        return False
    return stat.S_ISCHR(fuse_device.st_mode) and os.access("/dev/fuse", os.R_OK | os.W_OK)


def _print_dependency_report(*, systemd_available: bool, go_state: str | None = None) -> None:
    helper = None
    for name in FUSE_HELPERS:
        helper = shutil.which(name, path="/usr/bin:/bin")
        if helper:
            break
    mountpoint = shutil.which("mountpoint", path="/usr/bin:/bin")
    print("System requirements:")
    print(f"  Linux: {'available' if sys.platform.startswith('linux') else 'missing'}")
    print(f"  Python >= 3.10: {'available' if sys.version_info >= (3, 10) else 'missing'} ({sys.version_info.major}.{sys.version_info.minor})")
    print(f"  FUSE helper: {helper or 'missing'}")
    print(f"  /dev/fuse: {'available' if _fuse_device_ready() else 'missing or inaccessible'}")
    print(f"  mountpoint: {mountpoint or 'missing'}")
    print(f"  systemd user manager (optional): {'available' if systemd_available else 'unavailable'}")
    if go_state is not None:
        print(f"  Go build toolchain (optional): {go_state}")


def _check_runtime(*, input_fn: Callable[[str], str]) -> tuple[bool, bool]:
    if not sys.platform.startswith("linux"):
        raise WizardError("the Degoo mount installer currently supports Linux only")
    if sys.version_info < (3, 10):
        raise WizardError("Python 3.10 or newer is required")
    helper, mountpoint = _dependency_state()
    initial_systemd = _systemd_user_available()
    source_repo = _source_repo()
    go_state = None
    if source_repo:
        required = _required_go_version(source_repo)
        go_state = f"meets go {required[0]}.{required[1]}" if required and _go_available(source_repo) else "missing or older than this checkout requires"
    _print_dependency_report(systemd_available=initial_systemd, go_state=go_state)
    _install_missing_packages(not helper, not mountpoint, input_fn=input_fn)
    helper, mountpoint = _dependency_state()
    fuse_device_ready = _fuse_device_ready()
    if not fuse_device_ready:
        print("/dev/fuse is unavailable to this user; setup can continue, but mounting requires FUSE device access.")
    mount_ready = helper and mountpoint and fuse_device_ready
    if not mount_ready and (not helper or not mountpoint):
        print("Mount helper dependencies are still unavailable; complete setup and install them before mounting.")
    return mount_ready, initial_systemd


def _systemd_user_available() -> bool:
    systemctl = shutil.which("systemctl", path="/usr/bin:/bin")
    if not systemctl:
        return False
    try:
        result = subprocess.run(
            [systemctl, "--user", "show-environment"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=5,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    return result.returncode == 0


def _choose_binary(args: argparse.Namespace, *, input_fn: Callable[[str], str]) -> tuple[str, tempfile.TemporaryDirectory | None]:
    supplied = getattr(args, "binary", None)
    build_temp: tempfile.TemporaryDirectory | None = None
    source_repo = _source_repo()
    default_binary = _default_binary()
    binary_present = default_binary.is_file() and os.access(default_binary, os.X_OK)
    if not supplied and not binary_present and source_repo:
        go_available = _go_available(source_repo)
        if not go_available:
            required = _required_go_version(source_repo)
            go = shutil.which("go")
            requirement = f"Go {required[0]}.{required[1]} or newer" if required else "the Go version required by go.mod"
            print(f"This source checkout can build rclone, but {requirement} is unavailable.")
            package_name = None
            manager = _package_manager()
            if manager:
                manager_name = Path(manager[0]).name
                package_name = "golang" if manager_name in {"apt-get", "dnf"} else "go"
            if package_name:
                _install_packages([package_name], "Go toolchain", input_fn=input_fn)
                go_available = _go_available(source_repo)
            elif not package_name:
                print("No supported package manager is available for Go; provide a prebuilt Degoo-enabled binary.")
            if go and not go_available:
                print("The available Go version still does not meet this checkout's go.mod requirement.")
        if go_available and _yes_no("No prebuilt binary was found. Build rclone from this checkout with Go?", default=True, input_fn=input_fn):
            try:
                binary, build_temp = _build_binary(source_repo)
            except WizardError as exc:
                print(f"{exc}; provide a prebuilt binary to continue.")
            else:
                try:
                    installer.validate_binary(Path(binary))
                except installer.InstallError as exc:
                    build_temp.cleanup()
                    raise WizardError(str(exc)) from None
                return binary, build_temp
    default = str(supplied or default_binary)
    while True:
        value = _prompt("Path to the Linux rclone binary built with Degoo support", default, input_fn=input_fn)
        if not value:
            raise WizardError("a path to the Degoo-enabled rclone binary is required")
        try:
            path = installer._binary_source(value)
            installer.validate_binary(path)
        except installer.InstallError as exc:
            print(f"That binary cannot be used: {exc}")
            if supplied:
                raise WizardError(str(exc)) from None
            continue
        return str(path), build_temp


def _install_args(binary: str, *, upgrade: bool) -> argparse.Namespace:
    return argparse.Namespace(
        binary=binary,
        folder_id=None,
        folder_name=None,
        vault_config=None,
        upgrade=upgrade,
        folder_created_by_setup=False,
    )


def _load_folders(root: Path) -> list[dict[str, Any]]:
    binary = root / "bin/rclone"
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
                str(root / "logs/wizard-folders.log"),
                "--log-level",
                "ERROR",
                "degoo",
                "folders",
                str(root / "config/auth.json"),
                "--parent-id",
                "0",
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=env,
            timeout=120,
            check=False,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise WizardError("Degoo could not list device folders; run login.sh and retry") from None
    if result.returncode != 0:
        raise WizardError("Degoo could not list device folders; run login.sh and retry")
    try:
        data = json.loads(result.stdout)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        raise WizardError("Degoo returned an unrecognized device-folder list") from None
    if not isinstance(data, list):
        raise WizardError("Degoo returned an unrecognized device-folder list")
    folders: list[dict[str, Any]] = []
    for item in data:
        if not isinstance(item, dict):
            continue
        folder_id = item.get("id")
        name = item.get("name")
        category = item.get("category")
        if (
            not isinstance(folder_id, str)
            or folder_id == "0"
            or not folder_id
            or any(ord(character) < 0x21 or ord(character) == 0x7F for character in folder_id)
            or not isinstance(name, str)
            or type(category) is not int
            or category not in SELECTABLE_CATEGORIES
        ):
            continue
        folders.append({"id": folder_id, "name": name, "category": category})
    return folders


def _select_parent(folders: list[dict[str, Any]], *, input_fn: Callable[[str], str]) -> str:
    if not folders:
        raise WizardError("Degoo returned no device folders that can contain a dedicated vault")
    print("Degoo rejects vault creation under account root ID 0, so choose a device or folder below it.")
    for index, folder in enumerate(folders, start=1):
        kind = "device" if folder["category"] == 1 else "folder"
        print(f"  {index}. {json.dumps(folder['name'], ensure_ascii=True)} ({kind}, ID {folder['id']})")
    first_device = next((index for index, item in enumerate(folders, start=1) if item["category"] == 1), 1)
    while True:
        selection = _prompt("Parent number", str(first_device), input_fn=input_fn)
        try:
            index = int(selection)
        except ValueError:
            index = 0
        if 1 <= index <= len(folders):
            return folders[index - 1]["id"]
        print(f"Choose a number from 1 to {len(folders)}.")


def _imported_settings(path: str | None, root: Path) -> dict[str, Any] | None:
    if not path:
        return None
    try:
        rewritten = installer.import_vault_config(path, root)
        config = installer.parse_json_object(rewritten, "the vault configuration is invalid")
    except installer.InstallError as exc:
        raise WizardError(str(exc)) from None
    return config


def _current_vault_settings(root: Path) -> dict[str, Any] | None:
    try:
        raw = installer.read_private_file(root / "config/vault.json", missing_ok=True)
    except installer.InstallError as exc:
        raise WizardError(str(exc)) from None
    if raw is None:
        return None
    try:
        return installer.parse_json_object(raw, "the installed vault configuration is invalid")
    except installer.InstallError as exc:
        raise WizardError(str(exc)) from None


def _prompt_cache_settings(settings: dict[str, Any], *, input_fn: Callable[[str], str]) -> dict[str, Any]:
    defaults = installer.DEFAULT_MOUNT_SETTINGS
    result = {
        "cache_max_size": _read_text_setting(settings, "cache_max_size", defaults["cache_max_size"]),
        "cache_min_free_space": _read_text_setting(settings, "cache_min_free_space", defaults["cache_min_free_space"]),
        "cache_max_age": _read_text_setting(settings, "cache_max_age", defaults["cache_max_age"], duration=True),
        "cache_poll_interval": _read_text_setting(settings, "cache_poll_interval", defaults["cache_poll_interval"], duration=True),
        "verify_upload": settings.get("verify_upload") if type(settings.get("verify_upload")) is bool else defaults["verify_upload"],
    }
    result["cache_max_size"] = _prompt("Maximum local cache size", result["cache_max_size"], input_fn=input_fn)
    result["cache_min_free_space"] = _prompt("Minimum free-space target", result["cache_min_free_space"], input_fn=input_fn)
    result["cache_max_age"] = _prompt("Maximum cache age", result["cache_max_age"], input_fn=input_fn)
    result["cache_poll_interval"] = _prompt("Cache polling interval", result["cache_poll_interval"], input_fn=input_fn)
    result["verify_upload"] = _yes_no("Verify completed uploads", default=result["verify_upload"], input_fn=input_fn)
    for key in ("cache_max_size", "cache_min_free_space"):
        if not SIZE_PATTERN.fullmatch(result[key]):
            raise WizardError(f"{key} must be a size such as 20G")
    for key in ("cache_max_age", "cache_poll_interval"):
        if not DURATION_PATTERN.fullmatch(result[key]):
            raise WizardError(f"{key} must be a duration such as 10m or 5s")
    return result


def _cache_settings(root: Path, *, input_fn: Callable[[str], str]) -> dict[str, Any]:
    settings = installer.DEFAULT_MOUNT_SETTINGS
    if os.path.lexists(root / "config/mount.json"):
        try:
            raw = installer.read_private_file(root / "config/mount.json", missing_ok=True)
            loaded = installer.parse_json_object(raw, "config/mount.json is invalid") if raw is not None else None
        except installer.InstallError as exc:
            raise WizardError(str(exc)) from None
        if loaded is not None:
            settings = loaded
    return _prompt_cache_settings(settings, input_fn=input_fn)


def _new_vault_sizes(imported: dict[str, Any] | None, *, input_fn: Callable[[str], str]) -> dict[str, str] | None:
    if imported is not None:
        return None
    chunk = _prompt("Maximum plaintext chunk size", "1Gi", input_fn=input_fn)
    object_limit = _prompt("Maximum encrypted object size (provisional Degoo safety cap)", "19Gi", input_fn=input_fn)
    chunk_bytes = _size_bytes(chunk)
    object_bytes = _size_bytes(object_limit)
    if object_bytes > 19 * 1024**3:
        raise WizardError("the encrypted object limit cannot exceed the provisional 19Gi safety cap")
    encrypted_chunk_bytes = 32 + chunk_bytes + ((chunk_bytes + 65535) // 65536) * 16
    encrypted_name_bytes = 32 + 255 + 16
    if object_bytes < 1024:
        raise WizardError("the encrypted object limit must be at least 1024 bytes")
    if encrypted_chunk_bytes > object_bytes or encrypted_name_bytes > object_bytes:
        raise WizardError("the encrypted chunk or name size exceeds the encrypted object safety cap")
    return {"chunk_size": chunk, "max_object_size": object_limit}


def _save_settings(root: Path, mount_settings: dict[str, Any], vault_sizes: dict[str, str] | None) -> None:
    try:
        installer.write_private_file(
            root / "config/mount.json",
            (json.dumps(mount_settings, indent=2, sort_keys=True) + "\n").encode("utf-8"),
            replace=True,
        )
        if vault_sizes:
            path = root / "config/vault.json"
            raw = installer.read_private_file(path)
            vault = installer.parse_json_object(raw or b"", "the installed vault configuration is invalid")
            vault.update(vault_sizes)
            installer.write_private_file(
                path,
                (json.dumps(vault, indent=2, sort_keys=True) + "\n").encode("utf-8"),
                replace=True,
            )
    except installer.InstallError as exc:
        raise WizardError(str(exc)) from None


def _run(args: argparse.Namespace, *, input_fn: Callable[[str], str]) -> int:
    requested_folder_id = getattr(args, "folder_id", None)
    if requested_folder_id:
        requested_folder_id = installer.validate_folder_id(requested_folder_id)
    requested_folder_name = getattr(args, "folder_name", None)
    if requested_folder_name:
        requested_folder_name = setup._validate_name(requested_folder_name)
    root = installer.expected_root()
    root_exists = os.path.lexists(root)
    manifest = None
    if root_exists:
        try:
            installer.check_directory(root, private=True)
            manifest = installer.load_manifest(root)
        except installer.InstallError as exc:
            raise WizardError(str(exc)) from None
    configured_folder_id = manifest.get("root_folder_id", "") if manifest else ""
    if configured_folder_id and requested_folder_id and configured_folder_id != requested_folder_id:
        raise WizardError("the requested folder ID differs from this install's existing vault binding")
    upgrade = bool(getattr(args, "upgrade", False))
    if root_exists and not upgrade:
        if not _yes_no(f"Upgrade the existing contained install at {root}?", default=True, input_fn=input_fn):
            raise WizardCancelled
        upgrade = True
    if upgrade and not root_exists:
        raise WizardError("--upgrade requires an existing contained install")

    mount_ready, systemd_available = _check_runtime(input_fn=input_fn)
    print("Step 1 of 6: Choose the Degoo-enabled rclone binary.")
    binary, build_temp = _choose_binary(args, input_fn=input_fn)

    installed_vault = _current_vault_settings(root) if root_exists else None
    import_path = getattr(args, "vault_config", None)
    if installed_vault is None and not import_path:
        import_path = _prompt("Path to an existing private vault.json to import (optional)", "", input_fn=input_fn) or None
    imported = _imported_settings(import_path, root) if import_path else None
    if import_path and root_exists and installed_vault is not None:
        raise WizardError("an existing vault secret is already installed; it cannot be replaced during upgrade")
    if configured_folder_id and installed_vault is None and imported is None:
        raise WizardError("this install is bound to a remote vault but its secret is missing; import the matching private vault.json")

    print("Step 2 of 6: Set local cache and vault sizing.")
    mount_settings = _cache_settings(root, input_fn=input_fn)
    vault_sizes = _new_vault_sizes(imported, input_fn=input_fn) if installed_vault is None else None
    if imported is not None:
        print("The imported vault password and encryption settings will be preserved.")
    elif installed_vault is not None:
        print("The existing vault password and chunk parameters will be preserved.")

    print("Step 3 of 6: Install or upgrade the private client.")
    staged_args = _install_args(binary, upgrade=upgrade)
    if configured_folder_id and installed_vault is None and import_path:
        staged_args.vault_config = import_path
    try:
        result = installer.install(staged_args)
        if result != 0:
            return result
    finally:
        if build_temp is not None:
            build_temp.cleanup()
    root = installer.expected_root()

    print("Step 4 of 6: Log in to Degoo.")
    login_result = login.run(root, clear_email=False)
    if login_result != 0:
        return login_result
    if configured_folder_id:
        print(f"This installation is already bound to dedicated folder ID {configured_folder_id}; that binding is preserved.")
    else:
        print("Step 5 of 6: Choose an existing dedicated folder or create one under a device.")
        folder_id = requested_folder_id
        folder_name = requested_folder_name
        if not folder_id:
            folder_id = _prompt("Existing dedicated vault folder ID (leave blank to create a new folder)", "", input_fn=input_fn) or None
        if folder_id:
            folder_id = installer.validate_folder_id(folder_id)
            if not folder_name:
                folder_name = _prompt("Local name for this dedicated folder", "", input_fn=input_fn) or None
        else:
            folders = _load_folders(root)
            parent_id = _select_parent(folders, input_fn=input_fn)
            folder_name = folder_name or _prompt("Name for the new dedicated vault folder", DEFAULT_FOLDER_NAME, input_fn=input_fn)
            folder_id = None

        if folder_id:
            setup_args = argparse.Namespace(
                root=str(root),
                folder_id=folder_id,
                parent_id=None,
                name=folder_name,
                vault_config=import_path,
            )
        else:
            setup_args = argparse.Namespace(
                root=str(root),
                folder_id=None,
                parent_id=parent_id,
                name=folder_name,
                vault_config=import_path,
            )
        setup_result = setup.run(setup_args)
        if setup_result != 0:
            return setup_result

    _save_settings(root, mount_settings, vault_sizes)

    print("Step 6 of 6: Choose how to start the mount.")
    if systemd_available and mount_ready:
        if _yes_no("Enable and start the systemd user service?", default=True, input_fn=input_fn):
            return service.run(root, "enable")
    else:
        if not systemd_available:
            print("A systemd user manager is unavailable; start the mount manually when ready.")
        elif not mount_ready:
            print("Mount dependencies are incomplete; enable the systemd service after FUSE access is ready.")
    print(f"Installation complete. Start manually with {root / 'mount.sh'} or enable the service later with {root / 'service.sh'} enable.")
    return 0


def run(args: argparse.Namespace) -> int:
    if not sys.stdin.isatty():
        print("wizard refused: an interactive terminal is required", file=sys.stderr)
        return 2
    try:
        return _run(args, input_fn=input)
    except WizardCancelled:
        print("Installation wizard cancelled; any completed local setup is preserved.", file=sys.stderr)
        return 130
    except (installer.InstallError, login.LoginError, setup.SetupError, service.ServiceError, WizardError) as exc:
        print(f"wizard refused: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("wizard refused: local filesystem access failed safely", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("Installation wizard cancelled; any completed local setup is preserved.", file=sys.stderr)
        return 130


if __name__ == "__main__":
    raise SystemExit(run(argparse.Namespace()))
