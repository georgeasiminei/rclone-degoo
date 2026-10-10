#!/usr/bin/env python3
"""Create a secret-free Linux Degoo install archive from a built rclone binary."""

from __future__ import annotations

import argparse
import gzip
import io
import os
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath


ROOT_NAME = "rclone-degoo"
PYTHON_HELPERS = (
    "install.py",
    "login.py",
    "setup.py",
    "service.py",
    "info.py",
    "uninstall.py",
    "wizard.py",
)
PACKAGE_ENTRIES = (
    ("DEGOO.md", "DEGOO.md"),
    ("contrib/degoo/install.sh", "install.sh"),
    ("contrib/degoo/uninstall.sh", "uninstall.sh"),
    *((f"contrib/degoo/scripts/python/{name}", f"scripts/python/{name}") for name in PYTHON_HELPERS),
    ("backend/degoo/NOTICE", "backend/degoo/NOTICE"),
    ("contrib/degoo/README.md", "contrib/degoo/README.md"),
    ("docs/degoo/INSTALLATION.md", "docs/degoo/INSTALLATION.md"),
    ("docs/degoo/USAGE.md", "docs/degoo/USAGE.md"),
    ("docs/degoo/VALIDATION.md", "docs/degoo/VALIDATION.md"),
)
LICENSE_FILES = ("LICENSE", "COPYING")
HELP_TIMEOUT_SECONDS = 30
INSTALL_TEXT = """\
Encrypted Degoo vault for Linux

This archive contains a prebuilt rclone executable for the Linux architecture
named in the archive filename. Go is not required to install or use it. Use a
binary that matches your machine's Linux architecture. The installer requires
Python 3.10 or newer. Mounting also requires FUSE 3 or a compatible fusermount
helper, readable and writable /dev/fuse, and the mountpoint utility from
util-linux. A systemd user manager is optional and is used only for the
persistent service.

From the extracted archive directory, start the guided installer:

    ./install.sh

With no arguments in a terminal, the installer opens the wizard. You can also
request it explicitly with --wizard. It checks dependencies and offers to
install missing packages through a detected apt-get, dnf, pacman, or zypper
package manager. Vault setup can continue when mount dependencies are
unavailable, but the service will not be enabled until they are ready. It
suggests a discovered Degoo device folder and the name rclone-degoo-vault for a
new dedicated vault. Account root ID 0 is only used to list device folders; it
cannot be selected as the vault parent.

For unattended or staged installs, pass --binary and optionally --no-wizard:

    ./install.sh --binary ./rclone --no-wizard

You can then log in and finish setup manually:

    ~/.local/share/rclone-degoo/login.sh
    ~/.local/share/rclone-degoo/folders.sh --parent-id 0
    ~/.local/share/rclone-degoo/setup.sh --parent-id DEVICE_ID --name rclone-degoo-vault
    ~/.local/share/rclone-degoo/service.sh enable
    ~/.local/share/rclone-degoo/info.sh

To inspect an installed copy without changing it, run
~/.local/share/rclone-degoo/uninstall.sh --inventory. Run
~/.local/share/rclone-degoo/uninstall.sh in a terminal to start the guided
local cleanup flow. Retained settings, cache, and logs are copied to a private
backup directory. Cleanup never removes remote Degoo data and refuses to
discard pending local uploads.

The installer keeps account credentials, vault settings, cache, logs, and
runtime files inside ~/.local/share/rclone-degoo. This archive contains only
the executable, launchers, Python helper sources, documentation, and license
files. It does not contain account tokens, configuration, logs, caches, tests,
or other runtime state.

Read DEGOO.md and docs/degoo/INSTALLATION.md before using the vault. Each user
must create or import their own dedicated vault and protect a backup of its
encryption settings.
"""


class PackageError(Exception):
    """A safe, user-facing packaging error."""


def repository_root() -> Path:
    return Path(__file__).resolve().parents[1]


def package_sources(root: Path) -> list[tuple[Path, str]]:
    """Return the explicit documentation and installer allowlist."""
    selected = list(PACKAGE_ENTRIES)
    licenses = [(name, name) for name in LICENSE_FILES if (root / name).exists()]
    if not licenses:
        raise PackageError("no LICENSE or COPYING file is available for the archive")
    selected.extend(licenses)

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

    output = Path(output_arg).expanduser()
    try:
        output.parent.mkdir(parents=True, exist_ok=True)
        if output.is_symlink() or output.is_dir():
            raise PackageError("--output must name a regular file path, not a symlink or directory")
        if output.exists() and os.path.samefile(output, binary):
            raise PackageError("--output cannot replace the supplied binary")
    except PackageError:
        raise
    except OSError:
        raise PackageError("the output path is unavailable") from None

    output = output.absolute()
    binary_size = binary.stat().st_size
    temp_name: str | None = None
    try:
        with tempfile.NamedTemporaryFile(
            prefix=f".{output.name}.", suffix=".tmp", dir=output.parent, delete=False
        ) as raw:
            temp_name = raw.name
            with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
                with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.USTAR_FORMAT) as archive:
                    with binary.open("rb") as stream:
                        archive.addfile(tar_info(f"{ROOT_NAME}/rclone", binary_size, 0o755), stream)
                    add_bytes(archive, f"{ROOT_NAME}/INSTALL.txt", INSTALL_TEXT.encode("utf-8"))
                    for source, arcname in sources:
                        with source.open("rb") as stream:
                            mode = 0o755 if PurePosixPath(arcname).name in {"install.sh", "uninstall.sh"} else 0o644
                            archive.addfile(tar_info(arcname, source.stat().st_size, mode), stream)
            raw.flush()
            os.fsync(raw.fileno())
        os.chmod(temp_name, 0o644)
        os.replace(temp_name, output)
        temp_name = None
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
        if temp_name is not None:
            try:
                os.unlink(temp_name)
            except OSError:
                pass
    return output


def self_check(root: Path) -> None:
    """Smoke-test allowlisting and help validation without a real binary."""
    with tempfile.TemporaryDirectory(prefix="rclone-degoo-self-check-") as temporary:
        workspace = Path(temporary)
        fixture_root = workspace / "repo"
        for source, _ in package_sources(root):
            relative = source.relative_to(root)
            target = fixture_root / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source, target)

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
            "if [ \"$1\" = degoo ] && [ \"$2\" = --help ]; then\n"
            "  printf '%s\\n' 'Usage: rclone degoo command'\n"
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

        archive_path = workspace / "test-package.tar.gz"
        create_archive(fixture_root, str(good_binary), str(archive_path))
        expected = {
            f"{ROOT_NAME}/rclone",
            f"{ROOT_NAME}/INSTALL.txt",
            *(arcname for _, arcname in package_sources(fixture_root)),
        }
        with tarfile.open(archive_path, "r:gz") as archive:
            names = archive.getnames()
            if set(names) != expected or len(names) != len(expected):
                raise PackageError("self-check found an unexpected or missing archive path")
            binary_info = archive.getmember(f"{ROOT_NAME}/rclone")
            if not binary_info.mode & 0o111:
                raise PackageError("self-check found a non-executable packaged binary")
            for launcher in (f"{ROOT_NAME}/install.sh", f"{ROOT_NAME}/uninstall.sh"):
                launcher_info = archive.getmember(launcher)
                if not launcher_info.mode & 0o111:
                    raise PackageError(f"self-check found a non-executable launcher: {launcher}")
            install_data = archive.extractfile(f"{ROOT_NAME}/INSTALL.txt")
            if install_data is None:
                raise PackageError("self-check could not read INSTALL.txt")
            install_text = install_data.read().decode("utf-8")
            for required in (
                "Go is not required",
                "./install.sh",
                "--wizard",
                "--no-wizard",
                "Python 3.10 or newer",
                "install missing packages",
                "uninstall.sh --inventory",
                "login.sh",
                "folders.sh",
                "setup.sh",
                "service.sh enable",
            ):
                if required not in install_text:
                    raise PackageError(f"self-check found missing install instructions: {required}")
            for member in archive.getmembers():
                stream = archive.extractfile(member)
                if stream is not None and b"private-token-self-check-sentinel" in stream.read():
                    raise PackageError("self-check found runtime secret data in the archive")


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
        return 0
    except PackageError as exc:
        print(f"package-degoo: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
