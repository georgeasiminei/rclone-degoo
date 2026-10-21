#!/usr/bin/env python3
"""Create a secret-free Linux Degoo install archive from a built rclone binary."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import importlib.util
import io
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath


ROOT_NAME = "rclone-degoo"
PYTHON_HELPERS = (
    "cli.py",
    "install.py",
    "login.py",
    "setup.py",
    "service.py",
    "info.py",
    "logs.py",
    "uninstall.py",
    "wizard.py",
    "mount_prerequisites.py",
    "mount_setup.py",
    "connection.py",
    "restore.py",
    "backup_config.py",
    "sync.py",
    "status.py",
    "stats.py",
    "wait.py",
    "benchmark.py",
    "update.py",
    "update_preflight.py",
)
DOC_FILES = (
    "API_EVIDENCE.md",
    "BORG_BACKUP_OPERATIONS.md",
    "IMPLEMENTATION_PLAN.md",
    "INSTALLATION.md",
    "REQUIREMENTS.md",
    "USAGE.md",
    "VALIDATION.md",
)
SHELL_LAUNCHERS = (
    "degoo-rc",
    "scripts/install.sh", "scripts/uninstall.sh", "scripts/trash.sh",
)
CLI_COMMANDS = (
    "install", "uninstall", "backup-config", "benchmark", "sync", "login", "setup", "mount", "stop", "status", "stats", "wait", "update",
    "location", "folders", "service", "info", "logs", "trash", "empty-trash", "completion",
)
PACKAGE_ENTRIES = (
    ("contrib/degoo/degoo-rc", "degoo-rc"),
    *((f"contrib/degoo/{name}", name) for name in SHELL_LAUNCHERS if name.startswith("scripts/")),
    ("contrib/degoo/DISTRIBUTION.md", "README.md"),
    ("contrib/degoo/immutable-mount/helper.c", "immutable-mount/helper.c"),
    ("contrib/degoo/immutable-mount/README.md", "immutable-mount/README.md"),
    *((f"contrib/degoo/scripts/python/{name}", f"scripts/python/{name}") for name in PYTHON_HELPERS),
    *((f"docs/degoo/{name}", f"docs/{name}") for name in DOC_FILES),
    ("COPYING", "licenses/COPYING"),
    ("backend/degoo/NOTICE", "licenses/degoo-NOTICE"),
)
HELP_TIMEOUT_SECONDS = 30
SHA256_PATTERN = re.compile(r"[0-9a-f]{64}\Z")
COMMIT_PATTERN = re.compile(r"[0-9a-f]{40}\Z")


class PackageError(Exception):
    """A safe, user-facing packaging error."""


def repository_root() -> Path:
    return Path(__file__).resolve().parents[1]


def package_sources(root: Path) -> list[tuple[Path, str]]:
    """Return the explicit installer, documentation, and license allowlist."""
    selected = list(PACKAGE_ENTRIES)

    missing: list[str] = []
    entries: list[tuple[Path, str]] = []
    for relative, package_path in selected:
        source = root / relative
        info = safe_source_info(root, relative)
        if info is None:
            missing.append(relative)
            continue
        arcname = PurePosixPath(ROOT_NAME, *PurePosixPath(package_path).parts).as_posix()
        entries.append((source, arcname))
    if missing:
        missing_text = ", ".join(missing)
        raise PackageError(f"required package files are not ready: {missing_text}")
    return entries


def safe_source_info(root: Path, relative: str) -> os.stat_result | None:
    current = root
    parts = PurePosixPath(relative).parts
    for index, part in enumerate(parts):
        current /= part
        try:
            info = os.lstat(current)
        except FileNotFoundError:
            return None
        except OSError:
            raise PackageError(f"could not inspect required package file: {relative}") from None
        if stat.S_ISLNK(info.st_mode):
            raise PackageError(f"required package path contains a symlink: {relative}")
        if index + 1 < len(parts) and not stat.S_ISDIR(info.st_mode):
            raise PackageError(f"required package path has a non-directory parent: {relative}")
    if not stat.S_ISREG(info.st_mode):
        raise PackageError(f"required package path is not a regular file: {relative}")
    return info


def executable_binary(path_arg: str) -> Path:
    try:
        binary = Path(path_arg).expanduser().resolve(strict=True)
        info = os.stat(binary)
    except OSError:
        raise PackageError("the supplied binary path is unavailable") from None
    if not stat.S_ISREG(info.st_mode) or not os.access(binary, os.X_OK):
        raise PackageError("--binary must name a regular executable file")
    return binary


def validate_degoo_help(binary: Path) -> None:
    """Run only local command help in an isolated temporary configuration."""
    try:
        with tempfile.TemporaryDirectory(prefix="rclone-degoo-package-") as temporary:
            isolated = Path(temporary)
            env = {
                "PATH": os.defpath,
                "HOME": str(isolated),
                "XDG_CONFIG_HOME": str(isolated / "config"),
                "XDG_CACHE_HOME": str(isolated / "cache"),
                "RCLONE_CONFIG": os.devnull,
                "LC_ALL": "C",
            }
            result = subprocess.run(
                [str(binary), "degoo", "--help"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
                timeout=HELP_TIMEOUT_SECONDS,
                check=False,
            )
    except subprocess.TimeoutExpired:
        raise PackageError("the binary did not finish `degoo --help` within 30 seconds") from None
    except OSError:
        raise PackageError("could not run `degoo --help`; check that the binary matches this Linux host") from None

    output = (result.stdout + result.stderr).decode("utf-8", errors="replace").lower()
    if result.returncode != 0:
        raise PackageError(f"the binary rejected `degoo --help` (exit status {result.returncode})")
    if "degoo" not in output or "usage:" not in output:
        raise PackageError("the binary did not display Degoo command usage for `degoo --help`")


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    try:
        with path.open("rb") as stream:
            while chunk := stream.read(1024 * 1024):
                digest.update(chunk)
    except OSError:
        raise PackageError("could not read a package file") from None
    return digest.hexdigest()


def checksum_sidecar_path(archive: Path) -> Path:
    return archive.with_name(f"{archive.name}.sha256")


def update_manifest_path(archive: Path) -> Path:
    return archive.with_name(f"{archive.name}.update.json")


def checksum_sidecar_data(archive: Path, archive_name: str | None = None) -> bytes:
    name = archive.name if archive_name is None else archive_name
    if any(ord(character) < 32 or ord(character) == 127 for character in name):
        raise PackageError("the archive filename cannot contain control characters")
    return f"{sha256_file(archive)}  {name}\n".encode("utf-8")


def verify_checksum_sidecar(archive: Path, sidecar: Path, archive_name: str | None = None) -> bool:
    """Check that a GNU sha256sum-style sidecar matches the archive bytes."""
    try:
        return sidecar.read_bytes() == checksum_sidecar_data(archive, archive_name)
    except OSError:
        return False


def validate_output_target(path: Path, label: str) -> None:
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        return
    except OSError:
        raise PackageError(f"{label} path is unavailable") from None
    if not stat.S_ISREG(info.st_mode):
        raise PackageError(f"{label} must name a regular file path")


def build_metadata(root: Path, binary: Path) -> bytes:
    """Describe the source revision and executable included in the archive."""
    def git_output(*arguments: str) -> str | None:
        try:
            result = subprocess.run(
                ["git", "-C", str(root), *arguments],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                timeout=HELP_TIMEOUT_SECONDS,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            return None
        if result.returncode != 0:
            return None
        return result.stdout.decode("utf-8", errors="replace").strip()

    commit = git_output("rev-parse", "--verify", "HEAD")
    if commit is not None and not COMMIT_PATTERN.fullmatch(commit):
        commit = None
    branch = git_output("rev-parse", "--abbrev-ref", "HEAD")
    status = git_output("status", "--porcelain", "--untracked-files=normal")
    metadata = {
        "binary_sha256": sha256_file(binary),
        "branch": branch if branch and branch != "HEAD" else None,
        "commit": commit,
        "source_dirty": status is None or bool(status),
        "version": 1,
    }
    return (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode("utf-8")


def expected_archive_paths(sources: list[tuple[Path, str]]) -> set[str]:
    return {
        f"{ROOT_NAME}/bin/rclone",
        f"{ROOT_NAME}/docs/BUILD.json",
        *(arcname for _, arcname in sources),
    }


def extracted_archive(archive_path: Path, expected_paths: set[str], destination: Path) -> Path:
    """Check the archive allowlist and unpack only regular files into a temporary tree."""
    try:
        with tarfile.open(archive_path, "r:gz") as archive:
            members = archive.getmembers()
            names = [member.name for member in members]
            if set(names) != expected_paths or len(names) != len(expected_paths):
                raise PackageError("the package archive has an unexpected or missing path")
            for member in members:
                path = PurePosixPath(member.name)
                if path.is_absolute() or ".." in path.parts or not member.isfile():
                    raise PackageError(f"the package archive contains an unsafe entry: {member.name}")
                if member.name in {f"{ROOT_NAME}/{name}" for name in ("bin/rclone", *SHELL_LAUNCHERS)} and not member.mode & 0o111:
                    raise PackageError(f"the package entrypoint is not executable: {member.name}")
            root = destination / ROOT_NAME
            for member in members:
                target = destination.joinpath(*PurePosixPath(member.name).parts)
                target.parent.mkdir(parents=True, exist_ok=True)
                stream = archive.extractfile(member)
                if stream is None:
                    raise PackageError(f"the package entry could not be read: {member.name}")
                with stream, target.open("xb") as output:
                    shutil.copyfileobj(stream, output)
                target.chmod(member.mode & 0o777)
    except (OSError, tarfile.TarError):
        raise PackageError("the package archive could not be read safely") from None
    return root


def package_identity_from_archive(
    archive_path: Path, expected_paths: set[str]
) -> tuple[dict[str, str], dict[str, object]]:
    """Compute the install identity and build provenance from finalized archive bytes."""
    with tempfile.TemporaryDirectory(prefix="rclone-degoo-identity-") as temporary:
        package = extracted_archive(archive_path, expected_paths, Path(temporary))
        try:
            spec = importlib.util.spec_from_file_location(
                "_rclone_degoo_package_installer", package / "scripts/python/install.py"
            )
            if spec is None or spec.loader is None:
                raise PackageError("the package installer identity helper could not be loaded")
            installer = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(installer)
            identity = installer.package_identity(package / "bin/rclone", package)
            build_info = json.loads((package / "docs/BUILD.json").read_bytes())
        except PackageError:
            raise
        except Exception:
            raise PackageError("the finalized package identity could not be verified") from None
        if not isinstance(identity, dict) or not isinstance(build_info, dict):
            raise PackageError("the finalized package identity has an invalid shape")
        return identity, build_info


def update_manifest_data(archive_path: Path, archive_name: str, expected_paths: set[str]) -> bytes:
    """Describe a finalized archive for fast update preflight checks."""
    identity, build_info = package_identity_from_archive(archive_path, expected_paths)
    archive_sha256 = sha256_file(archive_path)
    archive_size = archive_path.stat().st_size
    binary_sha256 = identity.get("binary_sha256")
    payload_sha256 = identity.get("payload_sha256")
    source_dirty = build_info.get("source_dirty")
    commit = build_info.get("commit")
    if (
        not isinstance(binary_sha256, str)
        or not SHA256_PATTERN.fullmatch(binary_sha256)
        or build_info.get("binary_sha256") != binary_sha256
        or not isinstance(payload_sha256, str)
        or not SHA256_PATTERN.fullmatch(payload_sha256)
        or not SHA256_PATTERN.fullmatch(archive_sha256)
        or archive_size <= 0
        or type(source_dirty) is not bool
        or (commit is not None and (not isinstance(commit, str) or not COMMIT_PATTERN.fullmatch(commit)))
    ):
        raise PackageError("the finalized package metadata failed validation")
    metadata = {
        "version": 1,
        "archive": archive_name,
        "archive_sha256": archive_sha256,
        "archive_size": archive_size,
        "binary_sha256": binary_sha256,
        "payload_sha256": payload_sha256,
        "commit": commit,
        "source_dirty": source_dirty,
    }
    return (json.dumps(metadata, indent=2, sort_keys=True) + "\n").encode("utf-8")


def self_check_payload_sha256(package: Path) -> str:
    """Independently reproduce the installer's payload identity in the fixture."""
    relative_paths = (
        Path("bin/rclone"),
        Path("degoo-rc"),
        *(Path("scripts") / name for name in ("install.sh", "trash.sh", "uninstall.sh")),
        *(Path("scripts/python") / name for name in PYTHON_HELPERS),
        *(Path("immutable-mount") / name for name in ("helper.c", "README.md")),
    )
    payload = hashlib.sha256()
    for relative in sorted(relative_paths, key=lambda path: path.as_posix()):
        payload.update(relative.as_posix().encode("utf-8"))
        payload.update(b"\0")
        payload.update(hashlib.sha256((package / relative).read_bytes()).digest())
    return payload.hexdigest()


def run_launcher_help(launcher: Path, env: dict[str, str], *arguments: str) -> None:
    try:
        result = subprocess.run(
            [str(launcher), *(arguments or ("--help",))],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=HELP_TIMEOUT_SECONDS,
            check=False,
        )
    except subprocess.TimeoutExpired:
        raise PackageError(f"the packaged launcher did not finish --help: {launcher.name}") from None
    except OSError:
        raise PackageError(f"the packaged launcher could not be run: {launcher.name}") from None
    if result.returncode != 0:
        raise PackageError(f"the packaged launcher rejected --help: {launcher.name}")


def validate_archive_launchers(archive_path: Path, expected_paths: set[str]) -> None:
    """Extract the checked archive and run each user-facing launcher help path."""
    with tempfile.TemporaryDirectory(prefix="rclone-degoo-archive-check-") as temporary:
        extracted = extracted_archive(archive_path, expected_paths, Path(temporary))
        try:
            metadata = json.loads((extracted / "docs/BUILD.json").read_text(encoding="utf-8"))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            raise PackageError("the package build provenance is invalid") from None
        if metadata.get("binary_sha256") != sha256_file(extracted / "bin/rclone"):
            raise PackageError("the package binary does not match its recorded SHA-256")
        validate_degoo_help(extracted / "bin/rclone")
        env = {
            "PATH": os.defpath,
            "HOME": str(Path(temporary) / "home"),
            "XDG_CONFIG_HOME": str(Path(temporary) / "config"),
            "XDG_CACHE_HOME": str(Path(temporary) / "cache"),
            "PYTHONDONTWRITEBYTECODE": "1",
            "LC_ALL": "C",
        }
        Path(env["HOME"]).mkdir()
        for name in SHELL_LAUNCHERS:
            run_launcher_help(extracted / name, env)
        for command in CLI_COMMANDS:
            run_launcher_help(extracted / "degoo-rc", env, command, "help")


def tar_info(arcname: str, size: int, mode: int) -> tarfile.TarInfo:
    info = tarfile.TarInfo(arcname)
    info.size = size
    info.mode = mode
    info.mtime = 0
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    return info


def add_bytes(archive: tarfile.TarFile, arcname: str, data: bytes, mode: int = 0o644) -> None:
    archive.addfile(tar_info(arcname, len(data), mode), io.BytesIO(data))


def create_archive(root: Path, binary_arg: str, output_arg: str) -> Path:
    sources = package_sources(root)
    binary = executable_binary(binary_arg)
    validate_degoo_help(binary)
    build_info = build_metadata(root, binary)

    output = Path(output_arg).expanduser()
    checksum = checksum_sidecar_path(output)
    update_manifest = update_manifest_path(output)
    try:
        protected_paths = {source.resolve(strict=True) for source, _ in sources}
        protected_paths.add(Path(__file__).resolve(strict=True))
        for candidate, label in (
            (output, "--output"),
            (checksum, "the checksum sidecar"),
            (update_manifest, "the update manifest"),
        ):
            if candidate.resolve(strict=False) in protected_paths:
                raise PackageError(f"{label} cannot replace a package source file")
            validate_output_target(candidate, label)
        output.parent.mkdir(parents=True, exist_ok=True)
        for candidate, label in (
            (output, "--output"),
            (checksum, "the checksum sidecar"),
            (update_manifest, "the update manifest"),
        ):
            if candidate.exists() and os.path.samefile(candidate, binary):
                raise PackageError(f"{label} cannot replace the supplied binary")
        previous_checksum: tuple[bytes, int] | None = None
        if checksum.exists():
            info = os.lstat(checksum)
            if info.st_size > 4096:
                raise PackageError("the existing checksum sidecar is not a regular checksum file")
            previous_checksum = (checksum.read_bytes(), stat.S_IMODE(info.st_mode))
        if update_manifest.exists() and os.lstat(update_manifest).st_size > 16384:
            raise PackageError("the existing update manifest is too large")
    except PackageError:
        raise
    except OSError:
        raise PackageError("the output path is unavailable") from None

    output = output.absolute()
    checksum = checksum.absolute()
    update_manifest = update_manifest.absolute()
    binary_size = binary.stat().st_size
    temp_name: str | None = None
    checksum_temp_name: str | None = None
    checksum_backup_name: str | None = None
    manifest_temp_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            prefix=f".{output.name}.", suffix=".tmp", dir=output.parent, delete=False
        ) as raw:
            temp_name = raw.name
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.USTAR_FORMAT) as archive:
                    with binary.open("rb") as stream:
                        archive.addfile(tar_info(f"{ROOT_NAME}/bin/rclone", binary_size, 0o755), stream)
                    add_bytes(archive, f"{ROOT_NAME}/docs/BUILD.json", build_info)
                    for source, arcname in sources:
                        if PurePosixPath(arcname).parent == PurePosixPath(f"{ROOT_NAME}/docs"):
                            data = source.read_bytes()
                            data = data.replace(b"../../DEGOO.md", b"../README.md")
                            data = data.replace(b"../../contrib/degoo/README.md", b"../README.md")
                            add_bytes(archive, arcname, data)
                            continue
                        with source.open("rb") as stream:
                            mode = 0o755 if str(PurePosixPath(arcname).relative_to(ROOT_NAME)) in SHELL_LAUNCHERS else 0o644
                            archive.addfile(tar_info(arcname, source.stat().st_size, mode), stream)
            raw.flush()
            os.fsync(raw.fileno())
        expected_paths = expected_archive_paths(sources)
        validate_archive_launchers(Path(temp_name), expected_paths)
        os.chmod(temp_name, 0o644)
        checksum_data = checksum_sidecar_data(Path(temp_name), output.name)
        with tempfile.NamedTemporaryFile(
            prefix=f".{checksum.name}.", suffix=".tmp", dir=checksum.parent, delete=False
        ) as sidecar_file:
            checksum_temp_name = sidecar_file.name
            sidecar_file.write(checksum_data)
            sidecar_file.flush()
            os.fsync(sidecar_file.fileno())
        os.chmod(checksum_temp_name, 0o644)
        if not verify_checksum_sidecar(Path(temp_name), Path(checksum_temp_name), output.name):
            raise PackageError("the package checksum sidecar failed verification")
        manifest_data = update_manifest_data(Path(temp_name), output.name, expected_paths)
        with tempfile.NamedTemporaryFile(
            prefix=f".{update_manifest.name}.", suffix=".tmp", dir=update_manifest.parent, delete=False
        ) as manifest_file:
            manifest_temp_name = manifest_file.name
            manifest_file.write(manifest_data)
            manifest_file.flush()
            os.fsync(manifest_file.fileno())
        os.chmod(manifest_temp_name, 0o644)
        if previous_checksum is not None:
            with tempfile.NamedTemporaryFile(
                prefix=f".{checksum.name}.backup.", suffix=".tmp", dir=checksum.parent, delete=False
            ) as sidecar_backup:
                checksum_backup_name = sidecar_backup.name
                sidecar_backup.write(previous_checksum[0])
                sidecar_backup.flush()
                os.fsync(sidecar_backup.fileno())
            os.chmod(checksum_backup_name, previous_checksum[1])
        os.replace(checksum_temp_name, checksum)
        checksum_temp_name = None
        try:
            os.replace(temp_name, output)
            temp_name = None
        except OSError:
            if checksum_backup_name is not None:
                try:
                    os.replace(checksum_backup_name, checksum)
                    checksum_backup_name = None
                except OSError:
                    try:
                        checksum.unlink()
                    except OSError:
                        pass
            else:
                try:
                    checksum.unlink()
                except OSError:
                    pass
            raise
        if checksum_backup_name is not None:
            os.unlink(checksum_backup_name)
            checksum_backup_name = None
        if not verify_checksum_sidecar(output, checksum):
            try:
                checksum.unlink()
            except OSError:
                pass
            raise PackageError("the published package checksum sidecar failed verification")
        os.replace(manifest_temp_name, update_manifest)
        manifest_temp_name = None
        if update_manifest.read_bytes() != manifest_data:
            raise PackageError("the published update manifest failed verification")
        try:
            directory_fd = os.open(output.parent, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
        except OSError:
            directory_fd = None
        if directory_fd is not None:
            try:
                os.fsync(directory_fd)
            finally:
                os.close(directory_fd)
    except PackageError:
        raise
    except OSError:
        raise PackageError("could not write the package archive") from None
    finally:
        for temporary_path in (temp_name, checksum_temp_name, checksum_backup_name, manifest_temp_name):
            if temporary_path is not None:
                try:
                    os.unlink(temporary_path)
                except OSError:
                    pass
    return output


def self_check(root: Path) -> None:
    """Exercise the archive entrypoints and install lifecycle without a real binary."""
    with tempfile.TemporaryDirectory(prefix="rclone-degoo-self-check-") as temporary:
        workspace = Path(temporary)
        fixture_root = workspace / "repo"
        for source, _ in package_sources(root):
            relative = source.relative_to(root)
            target = fixture_root / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)

        launcher_source = fixture_root / "contrib/degoo/degoo-rc"
        hidden_launcher = launcher_source.with_suffix(".missing")
        launcher_source.rename(hidden_launcher)
        try:
            package_sources(fixture_root)
        except PackageError as exc:
            if "contrib/degoo/degoo-rc" not in str(exc):
                raise PackageError("self-check did not identify a missing install launcher") from None
        else:
            raise PackageError("self-check did not reject a missing install launcher")
        finally:
            hidden_launcher.rename(launcher_source)

        missing_source = fixture_root / "contrib/degoo/scripts/python/install.py"
        hidden_source = missing_source.with_suffix(".missing")
        missing_source.rename(hidden_source)
        try:
            package_sources(fixture_root)
        except PackageError as exc:
            if "contrib/degoo/scripts/python/install.py" not in str(exc):
                raise PackageError("self-check did not identify a missing installer source") from None
        else:
            raise PackageError("self-check did not reject a missing installer source")
        finally:
            hidden_source.rename(missing_source)

        decoy = fixture_root / ".local/share/rclone-degoo/config/auth.json"
        decoy.parent.mkdir(parents=True, exist_ok=True)
        decoy.write_text("private-token-self-check-sentinel", encoding="utf-8")
        (fixture_root / ".local/share/rclone-degoo/cache").mkdir(parents=True)
        (fixture_root / ".local/share/rclone-degoo/logs").mkdir(parents=True)

        good_binary = workspace / "fake-rclone"
        good_binary.write_text(
            "#!/bin/sh\n"
            "if [ \"$1\" = degoo ]; then\n"
            "  printf '%s\\n' 'Usage: rclone degoo command mount login'\n"
            "  exit 0\n"
            "fi\n"
            "exit 19\n",
            encoding="utf-8",
        )
        good_binary.chmod(0o700)
        bad_binary = workspace / "broken-rclone"
        bad_binary.write_text("#!/bin/sh\nexit 23\n", encoding="utf-8")
        bad_binary.chmod(0o700)
        try:
            validate_degoo_help(bad_binary)
        except PackageError:
            pass
        else:
            raise PackageError("self-check failed to reject a binary without Degoo help")

        original_launcher = launcher_source.read_bytes()
        try:
            create_archive(fixture_root, str(good_binary), str(launcher_source))
        except PackageError as exc:
            if "cannot replace a package source file" not in str(exc):
                raise PackageError("self-check rejected a package source for the wrong reason") from None
        else:
            raise PackageError("self-check allowed the output archive to overwrite its installer source")
        if launcher_source.read_bytes() != original_launcher:
            raise PackageError("self-check changed the installer source while rejecting an unsafe output")

        special_output = workspace / "special-output"
        os.mkfifo(special_output)
        try:
            create_archive(fixture_root, str(good_binary), str(special_output))
        except PackageError as exc:
            if "regular file" not in str(exc):
                raise PackageError("self-check rejected a special output for the wrong reason") from None
        else:
            raise PackageError("self-check allowed replacing a special output path")

        archive_path = workspace / "test-package.tar.gz"
        checksum_path = checksum_sidecar_path(archive_path)
        manifest_path = update_manifest_path(archive_path)
        existing_archive = b"existing archive sentinel\n"
        existing_checksum = b"existing checksum sentinel\n"
        existing_manifest = b"existing update manifest sentinel\n"
        archive_path.write_bytes(existing_archive)
        checksum_path.write_bytes(existing_checksum)
        manifest_path.write_bytes(existing_manifest)
        launcher_source.rename(hidden_launcher)
        try:
            create_archive(fixture_root, str(good_binary), str(archive_path))
        except PackageError:
            pass
        else:
            raise PackageError("self-check allowed packaging when the install launcher was missing")
        finally:
            hidden_launcher.rename(launcher_source)
        if (
            archive_path.read_bytes() != existing_archive
            or checksum_path.read_bytes() != existing_checksum
            or manifest_path.read_bytes() != existing_manifest
        ):
            raise PackageError("self-check changed existing outputs after rejecting a missing launcher")

        try:
            create_archive(fixture_root, str(bad_binary), str(archive_path))
        except PackageError:
            pass
        else:
            raise PackageError("self-check allowed packaging with a binary without Degoo support")
        if (
            archive_path.read_bytes() != existing_archive
            or checksum_path.read_bytes() != existing_checksum
            or manifest_path.read_bytes() != existing_manifest
        ):
            raise PackageError("self-check changed existing outputs after rejecting an invalid binary")

        create_archive(fixture_root, str(good_binary), str(archive_path))
        sources = package_sources(fixture_root)
        expected = expected_archive_paths(sources)
        if not verify_checksum_sidecar(archive_path, checksum_path):
            raise PackageError("self-check found an invalid package checksum sidecar")
        try:
            manifest = json.loads(manifest_path.read_bytes())
        except (OSError, UnicodeDecodeError, json.JSONDecodeError):
            raise PackageError("self-check could not read the update manifest") from None
        if set(manifest) != {
            "version", "archive", "archive_sha256", "archive_size", "binary_sha256",
            "payload_sha256", "commit", "source_dirty",
        }:
            raise PackageError("self-check found an unexpected update manifest schema")
        if (
            type(manifest["version"]) is not int
            or manifest["version"] != 1
            or manifest["archive"] != archive_path.name
            or manifest["archive_sha256"] != sha256_file(archive_path)
            or type(manifest["archive_size"]) is not int
            or manifest["archive_size"] != archive_path.stat().st_size
            or manifest["binary_sha256"] != sha256_file(good_binary)
            or manifest["commit"] is not None
            or manifest["source_dirty"] is not True
        ):
            raise PackageError("self-check found inaccurate archive or build data in the update manifest")
        with tempfile.TemporaryDirectory(prefix="rclone-degoo-manifest-check-") as extracted_dir:
            package = extracted_archive(archive_path, expected, Path(extracted_dir))
            if manifest["payload_sha256"] != self_check_payload_sha256(package):
                raise PackageError("self-check found an inaccurate payload hash in the update manifest")
        first_archive = archive_path.read_bytes()
        first_checksum = checksum_path.read_bytes()
        first_manifest = manifest_path.read_bytes()
        create_archive(fixture_root, str(good_binary), str(archive_path))
        if (
            archive_path.read_bytes() != first_archive
            or checksum_path.read_bytes() != first_checksum
            or manifest_path.read_bytes() != first_manifest
        ):
            raise PackageError("self-check found that repeated packaging is not deterministic")
        required_paths = {
            *(f"{ROOT_NAME}/{name}" for name in SHELL_LAUNCHERS),
            f"{ROOT_NAME}/README.md",
            f"{ROOT_NAME}/bin/rclone",
            f"{ROOT_NAME}/licenses/COPYING",
            f"{ROOT_NAME}/licenses/degoo-NOTICE",
            f"{ROOT_NAME}/docs/BUILD.json",
            f"{ROOT_NAME}/immutable-mount/helper.c",
            f"{ROOT_NAME}/immutable-mount/README.md",
        }
        required_paths.update(f"{ROOT_NAME}/scripts/python/{name}" for name in PYTHON_HELPERS)
        required_paths.update(f"{ROOT_NAME}/docs/{name}" for name in DOC_FILES)
        if required_paths != expected:
            raise PackageError("self-check package allowlist differs from the expected distribution files")
        with tarfile.open(archive_path, "r:gz") as archive:
            names = archive.getnames()
            if set(names) != expected or len(names) != len(expected):
                raise PackageError("self-check found an unexpected or missing archive path")
            root_files = {
                PurePosixPath(name).name
                for name in names
                if PurePosixPath(name).parent == PurePosixPath(ROOT_NAME)
            }
            if root_files != {"degoo-rc", "README.md"}:
                raise PackageError("self-check requires degoo-rc as the only archive root command")
            binary_info = archive.getmember(f"{ROOT_NAME}/bin/rclone")
            if not binary_info.mode & 0o111:
                raise PackageError("self-check found a non-executable packaged binary")
            for launcher in (f"{ROOT_NAME}/{name}" for name in SHELL_LAUNCHERS):
                launcher_info = archive.getmember(launcher)
                if not launcher_info.mode & 0o111:
                    raise PackageError(f"self-check found a non-executable launcher: {launcher}")
            readme = archive.extractfile(f"{ROOT_NAME}/README.md")
            if readme is None or not readme.read().strip():
                raise PackageError("self-check could not read the distribution README")
            metadata_stream = archive.extractfile(f"{ROOT_NAME}/docs/BUILD.json")
            if metadata_stream is None:
                raise PackageError("self-check could not read build provenance")
            metadata = json.loads(metadata_stream.read())
            if metadata.get("binary_sha256") != sha256_file(good_binary):
                raise PackageError("self-check found incorrect binary provenance")
            for member in archive.getmembers():
                stream = archive.extractfile(member)
                if stream is None:
                    continue
                data = stream.read()
                if b"private-token-self-check-sentinel" in data:
                    raise PackageError("self-check found runtime secret data in the archive")
                if member.name.startswith(f"{ROOT_NAME}/docs/"):
                    if b"../../DEGOO.md" in data or b"../../contrib/degoo/README.md" in data:
                        raise PackageError("self-check found a source-tree link in flattened documentation")

        package_dir = workspace / "extracted"
        extracted = extracted_archive(archive_path, expected, package_dir)
        home = workspace / "home"
        home.mkdir()
        home.chmod(0o700)
        env = {
            "PATH": os.defpath,
            "HOME": str(home),
            "XDG_CONFIG_HOME": str(workspace / "config"),
            "XDG_CACHE_HOME": str(workspace / "cache"),
            "PYTHONDONTWRITEBYTECODE": "1",
            "LC_ALL": "C",
        }
        install_result = subprocess.run(
            [
                str(extracted / "degoo-rc"),
                "install",
                "--binary",
                str(extracted / "bin/rclone"),
                "--no-wizard",
                "--mountpoint",
                "mount",
            ],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            timeout=HELP_TIMEOUT_SECONDS,
            check=False,
        )
        if install_result.returncode != 0:
            detail = (install_result.stderr or install_result.stdout).decode("utf-8", errors="replace").strip()
            raise PackageError(
                f"self-check could not stage an install with the packaged launcher: {detail[:500]}"
            )

        staged_root = home / ".local/share/rclone-degoo"
        for arguments, failure in (
            (["--inventory", "--root", str(staged_root)], "inventory the staged install"),
            (["--delete-local", "--root", str(staged_root)], "remove the staged local install"),
        ):
            result = subprocess.run(
                [str(extracted / "degoo-rc"), "uninstall", *arguments],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
                timeout=HELP_TIMEOUT_SECONDS,
                check=False,
            )
            if result.returncode != 0:
                detail = (result.stderr or result.stdout).decode("utf-8", errors="replace").strip()
                raise PackageError(
                    f"self-check could not {failure} with the packaged launcher: {detail[:500]}"
                )
        if staged_root.exists():
            raise PackageError("self-check found the staged local install was not removed")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", help="path to a built executable with the Degoo command")
    parser.add_argument("--output", help="output .tar.gz path; choose the Linux architecture filename")
    parser.add_argument("--self-check", action="store_true", help="run a fixture-based packaging smoke check")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.self_check:
            if args.binary or args.output:
                parser.error("--self-check cannot be combined with --binary or --output")
            self_check(repository_root())
            print("Degoo package self-check passed")
            return 0
        if not args.binary or not args.output:
            parser.error("--binary and --output are required")
        output = create_archive(repository_root(), args.binary, args.output)
        print(f"created {output}")
        print(f"sha256 {checksum_sidecar_path(output)}")
        print(f"update manifest {update_manifest_path(output)}")
        return 0
    except PackageError as exc:
        print(f"package-degoo: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
