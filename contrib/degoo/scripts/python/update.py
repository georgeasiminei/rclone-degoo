#!/usr/bin/env python3
"""Update the contained Linux client without changing its vault configuration."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import platform
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath
from typing import Any

import install as installer
import service
import uninstall
import wizard


REPOSITORY = "georgeasiminei/rclone-degoo"
WORKFLOW = "degoo-package.yml"
ARTIFACT_NAME = "rclone-degoo-linux-amd64"
ARCHIVE_NAME = f"{ARTIFACT_NAME}.tar.gz"
MAX_PACKAGE_BYTES = 1024 * 1024 * 1024
MAX_PACKAGE_FILES = 4096
ACTIVE_STATES = {"active", "activating", "deactivating", "reloading"}
MANAGED_DIRECTORIES = ("bin", "scripts", "tools", "immutable-mount")
MANAGED_FILES = ("degoo-rc", "install.json", "rclone-degoo.service",
                 *installer.COMPATIBILITY_SHELL_FILES, *installer.SOURCE_FILES)


class UpdateError(Exception):
    """A safe, actionable update failure."""


def sha256_file(path: Path) -> str:
    """Return the SHA-256 of a regular file without loading it into memory."""
    installer.check_components(path)
    info = path.stat()
    if not stat.S_ISREG(info.st_mode):
        raise UpdateError("a package file is not a regular file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        while chunk := stream.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


class ActionsSource:
    """Resolve and download the latest successful staging package using gh."""

    @staticmethod
    def _run(arguments: list[str], *, timeout: int = 60) -> bytes:
        if not shutil.which("gh"):
            raise UpdateError("GitHub CLI (gh) is required; install gh, run gh auth login, then retry degoo-rc update")
        try:
            result = subprocess.run(["gh", *arguments], stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                    timeout=timeout, check=False)
        except subprocess.TimeoutExpired:
            raise UpdateError("GitHub package lookup/download timed out; retry degoo-rc update") from None
        if result.returncode != 0:
            raise UpdateError("GitHub package lookup/download failed; check gh auth status and connectivity, then retry degoo-rc update")
        return result.stdout

    def download(self, destination: Path) -> tuple[Path, dict[str, Any]]:
        response = self._run(["api", f"repos/{REPOSITORY}/actions/workflows/{WORKFLOW}/runs?branch=staging&status=success&per_page=1"])
        try:
            runs = json.loads(response)["workflow_runs"]
            if not runs:
                raise UpdateError("No successful degoo-package run found on staging.")
            run = runs[0]
            if (run["head_branch"] != "staging" or run["status"] != "completed"
                    or run["conclusion"] != "success" or type(run["id"]) is not int
                    or not re.fullmatch(r"[0-9a-f]{40}", run["head_sha"])):
                raise ValueError
        except (KeyError, TypeError, ValueError, IndexError, RecursionError):
            raise UpdateError("GitHub returned an invalid package run response") from None
        print(f"Downloading staging package from workflow run {run['id']} ({run['head_sha'][:12]}).", flush=True)
        self._run(["run", "download", str(run["id"]), "--name", ARTIFACT_NAME,
                   "--repo", REPOSITORY, "--dir", str(destination)], timeout=600)
        archive = destination / ARCHIVE_NAME
        if not archive.is_file():
            raise UpdateError("the latest workflow artifact is missing or expired; no installation files were changed")
        return archive, {"source": "actions", "repository": REPOSITORY,
                         "run_id": run["id"], "commit": run["head_sha"]}


def extract_package(archive_path: Path, destination: Path) -> tuple[Path, str]:
    """Verify the sidecar and extract only bounded, regular distribution files."""
    archive_path = archive_path.absolute()
    checksum_path = archive_path.with_name(archive_path.name + ".sha256")
    try:
        installer.check_components(checksum_path)
        if not checksum_path.is_file() or checksum_path.stat().st_size > 4096:
            raise UpdateError("the package checksum sidecar is missing or invalid")
        checksum = checksum_path.read_text(encoding="ascii")
    except (OSError, UnicodeError):
        raise UpdateError("the package checksum sidecar is missing or invalid") from None
    expected = re.fullmatch(r"([0-9a-f]{64})  ([^\r\n]+)\n?", checksum)
    if expected is None or expected[2] != archive_path.name or sha256_file(archive_path) != expected[1]:
        raise UpdateError("the package checksum does not match; no installation files were changed")
    if archive_path.stat().st_size > MAX_PACKAGE_BYTES:
        raise UpdateError("the package archive exceeds the supported size")
    try:
        with tarfile.open(archive_path, "r:gz") as archive:
            members: list[tarfile.TarInfo] = []
            seen: set[str] = set()
            total_size = 0
            for member in archive:
                path = PurePosixPath(member.name)
                relative = path.parts[1:]
                allowed = (relative in {("degoo-rc",), ("README.md",), ("bin", "rclone")}
                           or (len(relative) >= 2 and relative[0] in {"scripts", "docs", "licenses", "immutable-mount"}))
                if (not path.parts or path.parts[0] != "rclone-degoo" or path.is_absolute()
                        or ".." in path.parts or path.as_posix() != member.name
                        or not member.isfile() or not allowed or member.name in seen
                        or member.mode & 0o6000 or member.size < 0):
                    raise UpdateError("the package archive contains an unsafe or duplicate path")
                seen.add(member.name)
                total_size += member.size
                members.append(member)
                if len(members) > MAX_PACKAGE_FILES or total_size > MAX_PACKAGE_BYTES:
                    raise UpdateError("the extracted package exceeds the supported size")
            required = {"degoo-rc", "bin/rclone", "docs/BUILD.json", "scripts/python/install.py",
                        *(f"scripts/python/{name}" for name in installer.SOURCE_FILES),
                        *(f"scripts/{name}" for name in installer.SOURCE_SHELL_FILES),
                        *(f"immutable-mount/{name}" for name in installer.IMMUTABLE_MOUNT_FILES)}
            if not {f"rclone-degoo/{name}" for name in required} <= seen:
                raise UpdateError("the package archive is incomplete or predates automatic updates")
            for member in members:
                target = destination.joinpath(*PurePosixPath(member.name).parts)
                target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
                source = archive.extractfile(member)
                if source is None:
                    raise UpdateError("a package file could not be extracted")
                with source, target.open("xb") as output:
                    shutil.copyfileobj(source, output)
                target.chmod(0o700 if member.mode & 0o111 else 0o600)
    except (OSError, tarfile.TarError):
        raise UpdateError("the package archive could not be extracted safely") from None
    return destination / "rclone-degoo", expected[1]


def validate_package(package: Path, source: dict[str, Any]) -> dict[str, Any]:
    """Validate binary provenance and local help before stopping the old mount."""
    try:
        build_path = package / "docs/BUILD.json"
        if build_path.stat().st_size > installer.MAX_BUILD_METADATA_SIZE:
            raise ValueError
        metadata = json.loads(build_path.read_bytes())
        if (metadata.get("version") != 1
                or metadata.get("binary_sha256") != sha256_file(package / "bin/rclone")
                or (source.get("commit") and (metadata.get("commit") != source["commit"]
                                               or metadata.get("source_dirty") is not False))):
            raise ValueError
    except (ValueError, AttributeError, RecursionError):
        raise UpdateError("the package binary or source revision does not match BUILD.json") from None
    installer.validate_binary(package / "bin/rclone")
    for command in ("update", "install", "service", "mount"):
        result = subprocess.run([str(package / "degoo-rc"), command, "help"],
                                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=30, check=False,
                                env=dict(os.environ, PYTHONDONTWRITEBYTECODE="1", RCLONE_CONFIG=os.devnull))
        if result.returncode != 0:
            raise UpdateError(f"the package failed its local {command} help check")
    return installer.package_identity(package / "bin/rclone", package)


def installed_matches(root: Path, identity: dict[str, Any]) -> bool:
    """Compare the installed program payload, including installations without provenance."""
    try:
        current = installer.package_identity(root / "bin/rclone", root)
    except (OSError, installer.InstallError):
        return False
    return current["payload_sha256"] == identity["payload_sha256"]


def runtime_state(root: Path, manifest: dict[str, Any]) -> dict[str, Any]:
    """Identify the owned service or a manual process before changing runtime state."""
    mountpoint = installer.configured_mountpoint(root, manifest)
    mounted = wizard._mounted_state(mountpoint)
    if mounted.startswith("unknown"):
        raise UpdateError("the kernel mount state cannot be verified; the installation was preserved")
    links, recorded = uninstall._owned_service_links(root, manifest)
    active = False
    service_owned = bool(links or recorded)
    if service_owned:
        state = uninstall._systemctl_service_state()
        fragment = state["FragmentPath"]
        allowed = {str(root / "rclone-degoo.service"), *recorded, *(str(link) for link in links)}
        if fragment and (not Path(fragment).is_absolute() or os.path.normpath(fragment) not in allowed):
            raise UpdateError("systemd resolved rclone-degoo.service to another unit")
        if fragment:
            if state["LoadState"] != "loaded" or state["ActiveState"] not in ACTIVE_STATES | {"inactive", "failed"}:
                raise UpdateError("the owned service state cannot be verified")
        elif state["LoadState"] != "not-found" or state["ActiveState"] not in {"inactive", "failed"}:
            raise UpdateError("the owned service state cannot be verified")
        active = state["ActiveState"] in ACTIVE_STATES
    arguments = None
    directory = None
    if mounted == "mounted" and not active:
        arguments, directory = manual_mount_arguments(root, mountpoint)
    return {"service_active": active, "service_owned": service_owned,
            "manual_arguments": arguments, "manual_directory": directory, "mounted": mounted == "mounted"}


def manual_mount_arguments(root: Path, mountpoint: Path) -> tuple[list[str], str]:
    """Find a single owned mount process whose original arguments can be restored."""
    matches = []
    for process in Path("/proc").iterdir():
        if not process.name.isdecimal():
            continue
        try:
            if process.stat().st_uid != os.getuid():
                continue
            executable = os.readlink(process / "exe").removesuffix(" (deleted)")
            if executable != str(root / "bin/rclone"):
                continue
            arguments = [os.fsdecode(item) for item in (process / "cmdline").read_bytes().split(b"\0") if item]
            if ("degoo" in arguments and "mount" in arguments
                    and str(root / "config/vault.json") in arguments and str(mountpoint) in arguments):
                directory = os.readlink(process / "cwd")
                if not Path(directory).is_dir():
                    raise UpdateError("the manual mount's original working directory is unavailable; stop it and retry")
                arguments[0] = str(root / "bin/rclone")
                matches.append((arguments, directory))
        except (OSError, ProcessLookupError):
            continue
    if len(matches) != 1:
        raise UpdateError("the mounted path has no uniquely identified Degoo process; stop the conflicting mount and retry")
    return matches[0]


def stop_runtime(root: Path, manifest: dict[str, Any], state: dict[str, Any]) -> None:
    """Stop owned runtime processes without disabling the service or discarding writes."""
    if state["service_owned"]:
        uninstall._stop_owned_service(root, manifest, disable=False)
    mountpoint = installer.configured_mountpoint(root, manifest)
    if wizard._mounted_state(mountpoint) == "mounted":
        wizard._stop_owned_mount(root)
    if wizard._wait_for_mount_state(mountpoint, "not mounted") != "not mounted":
        raise UpdateError("the mount is busy or cannot be unmounted; close files using it and retry")


def start_runtime(root: Path, state: dict[str, Any]) -> None:
    """Restore the previous active state and confirm a connected local mount."""
    if state["service_owned"]:
        result = service._systemctl(root, "daemon-reload")
        if result.returncode != 0:
            raise UpdateError("systemd could not reload the updated unit; run degoo-rc service restart")
    if state["service_active"]:
        manifest = installer.load_manifest(root)
        service._require_loaded_unit(root, manifest)
        result = service._systemctl(root, "start", service.UNIT_NAME)
        if result.returncode != 0:
            raise UpdateError("the updated service could not start; inspect degoo-rc logs and retry degoo-rc service start")
    elif state["manual_arguments"]:
        env = dict(os.environ, PATH=f"{root / 'tools'}:/usr/bin:/bin",
                   XDG_CONFIG_HOME=str(root / "config"), XDG_CACHE_HOME=str(root / "cache"),
                   TMPDIR=str(root / "tmp"), PYTHONDONTWRITEBYTECODE="1")
        log = root / "logs/update-mount.log"
        installer.check_directory(root / "logs", private=True)
        if os.path.lexists(log):
            installer.check_components(log)
        descriptor = os.open(log, os.O_CREAT | os.O_WRONLY | os.O_APPEND | os.O_NOFOLLOW, 0o600)
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
            os.close(descriptor)
            raise UpdateError("the manual mount startup log is unsafe")
        with os.fdopen(descriptor, "ab") as output:
            subprocess.Popen(state["manual_arguments"], stdin=subprocess.DEVNULL,
                             stdout=output, stderr=output, env=env, cwd=state.get("manual_directory"),
                             start_new_session=True, umask=0o077)
    if (state["service_active"] or state["manual_arguments"]) and wizard._wait_for_mount(root) != "mounted":
        raise UpdateError("the restarted mount was not confirmed; inspect degoo-rc logs and degoo-rc info, then retry the mount")


def check_managed_tree(path: Path, root: Path) -> None:
    """Reject unexpected links and special files before snapshotting program files."""
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode):
        if path != root / "tools/fusermount3":
            raise UpdateError("unexpected symlink in program files")
        uninstall._validate_helper_link(path, require_executable=False)
        return
    if info.st_uid != os.getuid() or info.st_mode & 0o022 or info.st_mode & 0o6000:
        raise UpdateError("program files have unsafe ownership or permissions")
    if stat.S_ISDIR(info.st_mode):
        for child in path.iterdir():
            check_managed_tree(child, root)
    elif not stat.S_ISREG(info.st_mode):
        raise UpdateError("unexpected special file in program files")


def snapshot_program(root: Path, backup: Path) -> list[str]:
    """Keep only program files and installation identity for installation rollback."""
    if os.path.lexists(backup):
        raise UpdateError(f"an interrupted update backup remains at {backup}; resolve that conflict before updating")
    names = list(dict.fromkeys((*MANAGED_DIRECTORIES, *MANAGED_FILES)))
    for name in names:
        if os.path.lexists(root / name):
            check_managed_tree(root / name, root)
    backup.mkdir(mode=0o700)
    try:
        for name in names:
            source = root / name
            if not os.path.lexists(source):
                continue
            if source.is_dir():
                shutil.copytree(source, backup / name, symlinks=True)
            else:
                shutil.copy2(source, backup / name, follow_symlinks=False)
    except BaseException:
        shutil.rmtree(backup)
        raise
    return names


def restore_program(root: Path, backup: Path, names: list[str]) -> None:
    """Restore program bytes after an installation failure while the cache is locked."""
    for name in names:
        target = root / name
        if os.path.lexists(target):
            check_managed_tree(target, root)
            if target.is_dir() and not target.is_symlink():
                shutil.rmtree(target)
            else:
                target.unlink()
        source = backup / name
        if os.path.lexists(source):
            if source.is_dir() and not source.is_symlink():
                shutil.copytree(source, target, symlinks=True)
            else:
                shutil.copy2(source, target, follow_symlinks=False)


def require_clean_cache(root: Path) -> None:
    """Refuse dirty, unpaired, unrecognised, or unverifiable cache data."""
    cache = root / "cache"
    lock = uninstall._cache_lock_path(root)
    cache_name = lock.stem
    identity = cache / f"{cache_name}.identity"
    if (root / "config/vault.json").is_file():
        if uninstall._pending_uploads(root) != (0, 0):
            raise UpdateError("pending local uploads remain; --delete-cache was refused and the cache was preserved")
    elif uninstall._cache_has_data(root, lock):
        raise UpdateError("cached data has no matching vault configuration; cache deletion was refused")
    allowed = {lock.name, identity.name, "vfs", "vfsMeta"}
    for child in cache.iterdir():
        if child.name not in allowed:
            raise UpdateError("unrecognised cache data remains; cache deletion was refused")
        check_managed_tree(child, root)
    for parent in (cache / "vfs", cache / "vfsMeta"):
        if parent.exists() and (not parent.is_dir() or any(child.name != cache_name for child in parent.iterdir())):
            raise UpdateError("another cache identity remains; cache deletion was refused")
    data_root = cache / "vfs" / cache_name
    meta_root = cache / "vfsMeta" / cache_name
    data_files = {path.relative_to(data_root) for path in data_root.rglob("*") if path.is_file()}
    meta_files = {path.relative_to(meta_root) for path in meta_root.rglob("*") if path.is_file()}
    if data_files != meta_files or (data_files and not identity.is_file()):
        raise UpdateError("cache data or its recovery metadata is incomplete; cache deletion was refused")
    for relative in meta_files:
        try:
            metadata = json.loads(installer.read_private_file(meta_root / relative))
            if (not isinstance(metadata, dict) or metadata.get("Dirty") is not False
                    or type(metadata.get("Size")) is not int or metadata["Size"] < 0
                    or not isinstance(metadata.get("Fingerprint"), str) or not metadata["Fingerprint"]):
                raise ValueError
        except (ValueError, TypeError, RecursionError):
            raise UpdateError("cache metadata does not prove the data is clean; cache deletion was refused") from None


def delete_clean_cache(root: Path) -> None:
    """Remove verified clean cached data, retaining the inode of the held writer lock."""
    require_clean_cache(root)
    lock = uninstall._cache_lock_path(root)
    for child in (root / "cache").iterdir():
        if child == lock:
            continue
        if child.is_dir():
            shutil.rmtree(child)
        else:
            child.unlink()


def install_package(root: Path, package: Path, lock: int) -> None:
    """Run the downloaded installer with the inherited exclusive cache lock."""
    result = subprocess.run([sys.executable, str(package / "scripts/python/install.py"),
                             "--binary", str(package / "bin/rclone"), "--upgrade", "--no-wizard",
                             "--upgrade-lock-fd", str(lock)], stdin=subprocess.DEVNULL,
                            pass_fds=(lock,), timeout=180, check=False,
                            env=dict(os.environ, PYTHONDONTWRITEBYTECODE="1"))
    if result.returncode != 0:
        raise UpdateError("the downloaded package installer failed")


def apply_update(root: Path, package: Path, identity: dict[str, Any], source: dict[str, Any],
                 archive_hash: str, *, delete_cache: bool) -> bool:
    """Serialize installation, preserve state, and restore the previous runtime mode."""
    installer.check_directory(root / "tmp", private=True)
    path = root / "tmp/update.lock"
    installer.read_private_file(path, missing_ok=True)
    fd = os.open(path, os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    opened = os.fstat(fd)
    current = path.lstat()
    if (not stat.S_ISREG(opened.st_mode) or opened.st_uid != os.getuid()
            or opened.st_mode & 0o077 or (opened.st_dev, opened.st_ino) != (current.st_dev, current.st_ino)):
        os.close(fd)
        raise UpdateError("the update lock could not be opened safely")
    lock = None
    stopped = False
    changed = False
    backup = root / "tmp/update-backup"
    names = None
    state = None
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise UpdateError("another update is running; retry after it finishes") from None
        if os.path.lexists(backup):
            raise UpdateError(f"an interrupted update backup remains at {backup}; resolve that conflict before updating")
        if installed_matches(root, identity):
            return False
        layout = uninstall._validate_layout(root)
        manifest = layout["manifest"]
        state = runtime_state(root, manifest)
        if state["service_active"] or state["manual_arguments"]:
            if wizard._mount_preflight(root, installer.configured_mountpoint(root, manifest),
                                       systemd_available=state["service_active"]):
                raise UpdateError("mount prerequisites need repair before updating; follow the setup commands above")
        print("Installing the verified package, preserving configuration and local writes.", flush=True)
        stop_runtime(root, manifest, state)
        lock = uninstall._acquire_cache_lock(root, 10)
        stopped = True
        if delete_cache:
            require_clean_cache(root)
        names = snapshot_program(root, backup)
        try:
            install_package(root, package, lock)
            if not installed_matches(root, identity):
                raise UpdateError("the installed package does not match the downloaded program payload")
            manifest = installer.load_manifest(root)
            manifest["build"] = {"version": 1, **identity, "package_sha256": archive_hash, **source}
            installer.write_private_file(root / "install.json", (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode(), replace=True)
        except BaseException:
            try:
                restore_program(root, backup, names)
            except BaseException:
                stopped = False
                raise UpdateError(f"installation rollback could not finish; keep the mount stopped and recover program files from {backup}") from None
            shutil.rmtree(backup)
            names = None
            print("Installation failed; the previous program files were restored.", file=sys.stderr)
            raise
        changed = True
        shutil.rmtree(backup)
        names = None
        if delete_cache:
            delete_clean_cache(root)
            print("Removed the clean local cache.", flush=True)
    finally:
        if lock is not None:
            os.close(lock)
        try:
            if stopped and state is not None:
                start_runtime(root, state)
        finally:
            os.close(fd)
    if changed:
        print(f"Updated Degoo client ({identity['binary_sha256'][:12]}). Configuration preserved.")
        if state and state["manual_arguments"]:
            print("The previous manual mount is running in the background; degoo-rc stop unmounts it.")
    return changed


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="contained installation directory")
    parser.add_argument("--archive", help="use a local package tar.gz with its adjacent .sha256 sidecar instead of GitHub")
    parser.add_argument("--delete-cache", action="store_true", help="delete clean cache only when an update is installed; refuse pending or unverifiable writes")
    return parser


def run(args: argparse.Namespace) -> int:
    if not sys.platform.startswith("linux") or platform.machine() not in {"x86_64", "amd64"}:
        raise UpdateError("the current package source supports Linux amd64 only")
    root = Path(os.path.abspath(os.path.expanduser(args.root)))
    if root != installer.expected_root():
        raise UpdateError("--root must identify this user's contained installation")
    installer.check_directory(root, private=True)
    installer.load_manifest(root)
    backup = root / "tmp/update-backup"
    if os.path.lexists(backup):
        raise UpdateError(f"an interrupted update backup remains at {backup}; resolve that conflict before updating")
    with tempfile.TemporaryDirectory(prefix="rclone-degoo-update-") as temporary:
        workspace = Path(temporary)
        if args.archive:
            archive, source = Path(args.archive).expanduser().absolute(), {"source": "archive"}
        else:
            archive, source = ActionsSource().download(workspace)
        package, archive_hash = extract_package(archive, workspace / "extracted")
        identity = validate_package(package, source)
        if installed_matches(root, identity):
            print("Already up to date; no installation, service, or cache changes.")
            return 0
        changed = apply_update(root, package, identity, source, archive_hash, delete_cache=args.delete_cache)
        if not changed:
            print("Already up to date; no installation, service, or cache changes.")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(args)
    except KeyboardInterrupt:
        print("Update interrupted. Configuration and pending writes were preserved; inspect degoo-rc info before retrying.", file=sys.stderr)
        return 130
    except (UpdateError, installer.InstallError, uninstall.CleanupError, service.ServiceError,
            wizard.WizardError, OSError, subprocess.TimeoutExpired) as exc:
        reason = str(exc) if not isinstance(exc, (OSError, subprocess.TimeoutExpired)) else "a local update operation failed or timed out"
        print(f"Update incomplete: {reason}. Configuration and pending writes were preserved. Inspect degoo-rc info and degoo-rc logs before retrying.", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
