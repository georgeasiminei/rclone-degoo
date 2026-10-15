#!/usr/bin/env python3
"""Build and install the Degoo immutable-mount helper from a sudo command."""

from __future__ import annotations

import argparse
import os
import pwd
import secrets
import shlex
import shutil
import stat
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping

import mount_prerequisites


HELPER_PATH = Path("/usr/local/libexec/rclone-degoo/fusermount3")
HELPER_DIRECTORY = HELPER_PATH.parent
SAFE_PATH = "/usr/bin:/bin"
MAX_SOURCE_SIZE = 1024 * 1024
MAX_BINARY_SIZE = 32 * 1024 * 1024
COMPILE_TIMEOUT = 120
_DIRECTORY_FLAGS = os.O_RDONLY | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)


class MountSetupError(Exception):
    """A safe refusal while installing the immutable-mount helper."""


@dataclass(frozen=True)
class SudoIdentity:
    """The unprivileged account that invoked this root command through sudo."""

    uid: int
    gid: int
    name: str
    groups: tuple[int, ...]


@dataclass
class Destination:
    """Pinned descriptors for the helper directory and directories created by this run."""

    fds: list[int]
    created: list[tuple[int, str]]
    complete: bool

    @property
    def fd(self) -> int:
        return self.fds[-1]

    def close(self, *, rollback: bool = False) -> None:
        if rollback:
            for parent_fd, name in reversed(self.created):
                try:
                    os.rmdir(name, dir_fd=parent_fd)
                except OSError:
                    pass
        for fd in reversed(self.fds):
            try:
                os.close(fd)
            except OSError:
                pass


def _root_path(value: str) -> Path:
    if "\x00" in value:
        raise MountSetupError("--root cannot contain a NUL byte")
    root = Path(value)
    if not root.is_absolute() or ".." in root.parts:
        raise MountSetupError("--root must be an absolute path without parent traversal")
    return Path(os.path.normpath(str(root)))


def _positive_id(value: str | None, name: str) -> int:
    if value is None or not value.isascii() or not value.isdecimal():
        raise MountSetupError(f"sudo did not provide a valid {name}")
    parsed = int(value)
    if parsed <= 0 or parsed > 0xFFFFFFFE:
        raise MountSetupError(f"sudo did not provide a valid positive {name} in the Linux ID range")
    return parsed


def _sudo_identity(environment: Mapping[str, str] | None = None) -> SudoIdentity:
    if os.geteuid() != 0:
        raise MountSetupError("run this command through sudo so it can install the root-owned helper")
    env = os.environ if environment is None else environment
    uid = _positive_id(env.get("SUDO_UID"), "SUDO_UID")
    gid = _positive_id(env.get("SUDO_GID"), "SUDO_GID")
    name = env.get("SUDO_USER", "")
    if not name or "/" in name or "\x00" in name:
        raise MountSetupError("sudo did not provide a valid SUDO_USER")
    try:
        by_uid = pwd.getpwuid(uid)
        by_name = pwd.getpwnam(name)
    except KeyError:
        raise MountSetupError("SUDO_USER, SUDO_UID, and SUDO_GID do not identify a local account") from None
    if by_uid.pw_name != name or by_name.pw_uid != uid or by_name.pw_gid != gid:
        raise MountSetupError("SUDO_USER, SUDO_UID, and SUDO_GID do not match the local account database")
    try:
        groups = tuple(sorted(set(os.getgrouplist(name, gid)) | {gid}))
    except OSError:
        raise MountSetupError("the sudo user's supplementary groups could not be determined") from None
    return SudoIdentity(uid, gid, name, groups)


def _user_can_search(info: os.stat_result, identity: SudoIdentity) -> bool:
    mode = info.st_mode
    if info.st_uid == identity.uid:
        return bool(mode & stat.S_IXUSR)
    if info.st_gid in identity.groups:
        return bool(mode & stat.S_IXGRP)
    return bool(mode & stat.S_IXOTH)


def _user_can_execute(info: os.stat_result, identity: SudoIdentity) -> bool:
    mode = info.st_mode
    if info.st_uid == identity.uid:
        return bool(mode & stat.S_IXUSR)
    if info.st_gid in identity.groups:
        return bool(mode & stat.S_IXGRP)
    return bool(mode & stat.S_IXOTH)


def _check_directory(info: os.stat_result, path: str, identity: SudoIdentity, *, final: bool) -> None:
    if not stat.S_ISDIR(info.st_mode):
        raise MountSetupError(f"helper path component {path} is not a directory")
    if info.st_uid != 0:
        raise MountSetupError(f"helper path component {path} must be root-owned")
    if info.st_mode & 0o022:
        raise MountSetupError(f"helper path component {path} is writable by group or other users")
    if not _user_can_search(info, identity):
        raise MountSetupError(f"helper path component {path} is not searchable by the sudo user")
    if final and stat.S_IMODE(info.st_mode) != 0o755:
        raise MountSetupError(f"{path} must be root-owned with mode 0755")


def _check_filesystem(fd: int, path: str) -> None:
    try:
        filesystem = os.fstatvfs(fd)
    except OSError:
        raise MountSetupError(f"the filesystem containing {path} cannot be inspected") from None
    if filesystem.f_flag & mount_prerequisites.ST_NOSUID:
        raise MountSetupError(f"the filesystem containing {path} is mounted nosuid")
    if filesystem.f_flag & mount_prerequisites.ST_NOEXEC:
        raise MountSetupError(f"the filesystem containing {path} is mounted noexec")


def _existing_helper(fd: int) -> os.stat_result | None:
    try:
        info = os.stat(HELPER_PATH.name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return None
    except OSError:
        raise MountSetupError("the installed helper path could not be inspected safely") from None
    if stat.S_ISLNK(info.st_mode):
        return info
    if not stat.S_ISREG(info.st_mode):
        raise MountSetupError(f"refusing to replace non-regular file at {HELPER_PATH}")
    return info


def _open_destination(identity: SudoIdentity, *, create_missing: bool) -> Destination:
    fds: list[int] = []
    created: list[tuple[int, str]] = []
    try:
        root_fd = os.open("/", _DIRECTORY_FLAGS)
        fds.append(root_fd)
        root_info = os.fstat(root_fd)
        _check_directory(root_info, "/", identity, final=False)

        components = HELPER_DIRECTORY.parts[1:]
        complete = True
        for index, component in enumerate(components):
            parent_fd = fds[-1]
            component_path = str(Path("/").joinpath(*components[: index + 1]))
            made_directory = False
            try:
                child_fd = os.open(component, _DIRECTORY_FLAGS, dir_fd=parent_fd)
            except FileNotFoundError:
                if not create_missing:
                    complete = False
                    break
                try:
                    os.mkdir(component, 0o755, dir_fd=parent_fd)
                    made_directory = True
                except FileExistsError:
                    pass
                if made_directory:
                    created.append((parent_fd, component))
                child_fd = os.open(component, _DIRECTORY_FLAGS, dir_fd=parent_fd)
                if made_directory:
                    os.fchown(child_fd, 0, 0)
                    os.fchmod(child_fd, 0o755)
            except OSError:
                raise MountSetupError(f"helper path component {component_path} is unsafe or cannot be opened") from None

            fds.append(child_fd)
            info = os.fstat(child_fd)
            _check_directory(info, component_path, identity, final=index == len(components) - 1)

        directory_path = str(HELPER_DIRECTORY) if complete else str(Path("/").joinpath(*components[: len(fds) - 1]))
        _check_filesystem(fds[-1], directory_path)
        if complete:
            _existing_helper(fds[-1])
        return Destination(fds, created, complete)
    except BaseException:
        for parent_fd, name in reversed(created):
            try:
                os.rmdir(name, dir_fd=parent_fd)
            except OSError:
                pass
        for fd in reversed(fds):
            try:
                os.close(fd)
            except OSError:
                pass
        raise


def _read_source(root: Path) -> bytes:
    source = root / "immutable-mount" / "helper.c"
    flags = (
        os.O_RDONLY
        | getattr(os, "O_NOFOLLOW", 0)
        | getattr(os, "O_NONBLOCK", 0)
        | getattr(os, "O_CLOEXEC", 0)
    )
    try:
        fd = os.open(source, flags)
    except OSError:
        raise MountSetupError(f"helper source is missing or unsafe at {source}; re-extract the complete Degoo archive") from None
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_size <= 0 or info.st_size > MAX_SOURCE_SIZE:
            raise MountSetupError(f"helper source at {source} is not a regular file of an expected size")
        content = bytearray()
        while len(content) <= MAX_SOURCE_SIZE:
            chunk = os.read(fd, min(65536, MAX_SOURCE_SIZE + 1 - len(content)))
            if not chunk:
                break
            content.extend(chunk)
        if len(content) != info.st_size or len(content) > MAX_SOURCE_SIZE:
            raise MountSetupError(f"helper source at {source} changed or exceeded its size limit")
        return bytes(content)
    finally:
        os.close(fd)


def _compiler(identity: SudoIdentity) -> Path | None:
    found = shutil.which("gcc", path=SAFE_PATH)
    if found is None:
        return None
    try:
        compiler = Path(found).resolve(strict=True)
        info = compiler.stat()
        filesystem = os.statvfs(compiler)
    except (OSError, RuntimeError):
        raise MountSetupError("the GCC compiler could not be inspected safely") from None
    if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022:
        raise MountSetupError("the GCC compiler must be a root-owned executable not writable by group or other users")
    if not _user_can_execute(info, identity):
        raise MountSetupError("GCC is not executable by the sudo user")
    if filesystem.f_flag & mount_prerequisites.ST_NOEXEC:
        raise MountSetupError("the filesystem containing GCC is mounted noexec")
    for parent in compiler.parents:
        try:
            parent_info = parent.stat()
        except OSError:
            raise MountSetupError("the GCC compiler path could not be inspected safely") from None
        if (
            not stat.S_ISDIR(parent_info.st_mode)
            or parent_info.st_uid != 0
            or parent_info.st_mode & 0o022
            or not _user_can_search(parent_info, identity)
        ):
            raise MountSetupError("the GCC compiler path contains an unsafe or inaccessible directory")
    return compiler


def _workspace(source: bytes, identity: SudoIdentity) -> tuple[str, int]:
    """Create a root-pinned workspace with a caller-owned writable build directory."""
    build_fd: int | None = None
    try:
        temporary_info = os.lstat("/tmp")
    except OSError:
        raise MountSetupError("/tmp could not be inspected for a safe unprivileged build") from None
    if (
        stat.S_ISLNK(temporary_info.st_mode)
        or not stat.S_ISDIR(temporary_info.st_mode)
        or temporary_info.st_uid != 0
        or not temporary_info.st_mode & stat.S_ISVTX
    ):
        raise MountSetupError("/tmp must be a root-owned sticky directory for the unprivileged build")

    workspace = tempfile.mkdtemp(prefix="rclone-degoo-mount-setup-", dir="/tmp")
    try:
        os.chmod(workspace, 0o711)
        build_directory = Path(workspace) / "build"
        os.mkdir(build_directory, 0o700)
        os.chown(build_directory, identity.uid, identity.gid)
        os.chmod(build_directory, 0o700)
        parent_fd = os.open(workspace, _DIRECTORY_FLAGS)
        try:
            build_fd = os.open("build", _DIRECTORY_FLAGS, dir_fd=parent_fd)
        finally:
            os.close(parent_fd)
        source_fd = os.open(
            "helper.c",
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0),
            0o600,
            dir_fd=build_fd,
        )
        try:
            os.fchown(source_fd, identity.uid, identity.gid)
            os.fchmod(source_fd, 0o400)
            view = memoryview(source)
            while view:
                count = os.write(source_fd, view)
                if count <= 0:
                    raise OSError("short write")
                view = view[count:]
            os.fsync(source_fd)
        finally:
            os.close(source_fd)
        assert build_fd is not None
        return workspace, build_fd
    except BaseException:
        if build_fd is not None:
            try:
                os.close(build_fd)
            except OSError:
                pass
        shutil.rmtree(workspace, ignore_errors=True)
        raise


def _drop_privileges(identity: SudoIdentity):
    """Return a child hook that drops supplementary and primary privileges."""
    def drop() -> None:
        os.setgroups(list(identity.groups))
        os.setgid(identity.gid)
        os.setuid(identity.uid)
        os.umask(0o022)

    return drop


def _compile(source_directory: str, compiler: Path, identity: SudoIdentity) -> None:
    build_directory = Path(source_directory) / "build"
    command = [
        str(compiler),
        "-O2",
        "-D_FORTIFY_SOURCE=2",
        "-fstack-protector-strong",
        "-fPIE",
        "-pie",
        "-Wl,-z,relro,-z,now",
        "-Wall",
        "-Wextra",
        "-Werror",
        "-std=c11",
        "-o",
        str(build_directory / "fusermount3"),
        str(build_directory / "helper.c"),
    ]
    env = {"PATH": SAFE_PATH, "LANG": "C", "LC_ALL": "C", "TMPDIR": str(build_directory)}
    try:
        result = subprocess.run(
            command,
            cwd=build_directory,
            env=env,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            close_fds=True,
            timeout=COMPILE_TIMEOUT,
            check=False,
            preexec_fn=_drop_privileges(identity),
        )
    except subprocess.TimeoutExpired:
        raise MountSetupError("GCC did not finish within the build time limit") from None
    except subprocess.SubprocessError:
        raise MountSetupError("GCC could not be started with the sudo user's privileges") from None
    except OSError:
        raise MountSetupError("GCC could not be started as the sudo user") from None
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", errors="replace").strip()
        if len(detail) > 1200:
            detail = detail[-1200:]
        suffix = f":\n{detail}" if detail else ""
        raise MountSetupError(f"GCC could not build the immutable-mount helper{suffix}")
    return None


def _read_binary(workspace: str, build_fd: int, identity: SudoIdentity) -> bytes:
    del workspace
    try:
        fd = os.open(
            "fusermount3",
            os.O_RDONLY
            | getattr(os, "O_NOFOLLOW", 0)
            | getattr(os, "O_NONBLOCK", 0)
            | getattr(os, "O_CLOEXEC", 0),
            dir_fd=build_fd,
        )
    except OSError:
        raise MountSetupError("GCC did not produce a regular helper executable") from None
    try:
        before = os.fstat(fd)
        if (
            not stat.S_ISREG(before.st_mode)
            or before.st_uid != identity.uid
            or before.st_mode & 0o6022
            or before.st_mode & 0o111 != 0o111
            or before.st_size < 4
            or before.st_size > MAX_BINARY_SIZE
            or before.st_nlink != 1
        ):
            raise MountSetupError("GCC output has unsafe ownership, permissions, or size")
        chunks: list[bytes] = []
        remaining = before.st_size
        while remaining:
            chunk = os.read(fd, min(1024 * 1024, remaining))
            if not chunk:
                raise MountSetupError("GCC output changed while it was being read")
            chunks.append(chunk)
            remaining -= len(chunk)
        after = os.fstat(fd)
        if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise MountSetupError("GCC output changed while it was being read")
        binary = b"".join(chunks)
        if not binary.startswith(b"\x7fELF"):
            raise MountSetupError("GCC output is not an ELF executable")
        return binary
    finally:
        os.close(fd)


def _write_atomic_helper(binary: bytes, destination: Destination) -> None:
    _existing_helper(destination.fd)
    _check_filesystem(destination.fd, str(HELPER_DIRECTORY))
    temporary = f".fusermount3-{secrets.token_hex(12)}.tmp"
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0)
    try:
        fd = os.open(temporary, flags, 0o600, dir_fd=destination.fd)
    except OSError:
        raise MountSetupError("a temporary root-owned helper could not be created safely") from None
    published = False
    try:
        os.fchown(fd, 0, 0)
        view = memoryview(binary)
        while view:
            count = os.write(fd, view)
            if count <= 0:
                raise MountSetupError("the helper could not be written completely")
            view = view[count:]
        os.fchmod(fd, 0o4755)
        os.fsync(fd)
        os.replace(temporary, HELPER_PATH.name, src_dir_fd=destination.fd, dst_dir_fd=destination.fd)
        published = True
        os.fsync(destination.fd)
    except OSError:
        raise MountSetupError("the helper could not be installed atomically") from None
    finally:
        os.close(fd)
        if not published:
            try:
                os.unlink(temporary, dir_fd=destination.fd)
            except OSError:
                pass


def _continuation(root: Path, *, foreground: bool) -> None:
    launcher = shlex.quote(str(root / "degoo-rc"))
    print("Immutable mount helper is ready at /usr/local/libexec/rclone-degoo/fusermount3.")
    print("Continue as your ordinary user (without sudo):")
    if foreground:
        print(f"  {launcher} mount")
        print(f"  {launcher} info  # inspect mount state from another terminal")
    else:
        print(f"  {launcher} service enable")
        print(f"  {launcher} info")


def _missing_compiler_message(root: Path, foreground: bool) -> str:
    package_commands = (
        ("apt-get", "install build-essential"),
        ("dnf", "install gcc glibc-devel"),
        ("pacman", "-S --needed base-devel"),
        ("zypper", "install gcc glibc-devel"),
    )
    package_hint = None
    for manager, package in package_commands:
        if shutil.which(manager, path=SAFE_PATH):
            package_hint = f"Install it with `sudo {manager} {package}`."
            break
    if package_hint is None:
        package_hint = "Install GCC and the C development headers with your distribution package manager."
    retry = shlex.quote(str(root / "degoo-rc")) + " mount-setup"
    if foreground:
        retry += " --foreground"
    return (
        "GCC is required to build the helper. "
        f"{package_hint} Then rerun `sudo {retry}`."
    )


def run(root: Path, *, foreground: bool = False) -> int:
    if os.geteuid() != 0:
        raise MountSetupError("run this command through sudo so it can install the root-owned helper")
    identity = _sudo_identity()

    destination = _open_destination(identity, create_missing=False)
    try:
        if destination.complete and mount_prerequisites._helper_problem(HELPER_PATH) is None:
            _continuation(root, foreground=foreground)
            return 0
    finally:
        destination.close()

    source = _read_source(root)
    compiler = _compiler(identity)
    if compiler is None:
        raise MountSetupError(_missing_compiler_message(root, foreground))

    workspace, build_fd = _workspace(source, identity)
    try:
        _compile(workspace, compiler, identity)
        binary = _read_binary(workspace, build_fd, identity)
    finally:
        os.close(build_fd)
        shutil.rmtree(workspace, ignore_errors=True)

    destination = _open_destination(identity, create_missing=True)
    installed = False
    try:
        _write_atomic_helper(binary, destination)
        installed = True
        problem = mount_prerequisites._helper_problem(HELPER_PATH)
        if problem is not None:
            raise MountSetupError(f"helper installation did not pass validation: {problem}")
    finally:
        destination.close(rollback=not installed)

    _continuation(root, foreground=foreground)
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, help="source or contained install root")
    parser.add_argument("--foreground", action="store_true", help="print a foreground mount continuation")
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return run(_root_path(args.root), foreground=args.foreground)
    except MountSetupError as exc:
        print(f"mount-setup refused: {exc}", file=sys.stderr)
        return 2
    except (OSError, ValueError, subprocess.SubprocessError):
        print("mount-setup refused: local helper setup failed safely", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
