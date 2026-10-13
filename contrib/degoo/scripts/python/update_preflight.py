"""Preflight checks that keep the updater outside the mounted vault."""

from __future__ import annotations

import os
import shlex
from pathlib import Path


PROC_ROOT = Path("/proc")
MAX_PROCESS_ANCESTORS = 64
_DELETED_SUFFIX = " (deleted)"


class PreflightError(Exception):
    """A safe, actionable update preflight failure."""


def _parent_pid(stat: str) -> int:
    """Read the parent PID from proc stat, whose command field may contain spaces."""
    end_command = stat.rfind(")")
    fields = stat[end_command + 1:].split() if end_command >= 0 else []
    if len(fields) < 2:
        raise PreflightError("cannot safely verify the caller shell's working directory")
    try:
        return int(fields[1])
    except ValueError:
        raise PreflightError("cannot safely verify the caller shell's working directory") from None


def _cwd_path(target: str) -> Path:
    """Normalize a proc cwd link, including the kernel's deleted-directory suffix."""
    if target.endswith(_DELETED_SUFFIX):
        target = target[:-len(_DELETED_SUFFIX)]
    if not target.startswith("/") or target.startswith(" (unreachable)"):
        raise PreflightError("cannot safely verify the caller shell's working directory")
    return Path(os.path.realpath(target))


def _process_cwds(proc_root: Path, start_pid: int) -> list[Path]:
    """Read cwd links for this process and its bounded same-owner ancestor chain."""
    paths: list[Path] = []
    pid = start_pid
    owner_uid: int | None = None
    seen: set[int] = set()

    for depth in range(MAX_PROCESS_ANCESTORS):
        if pid in seen:
            raise PreflightError("cannot safely verify the caller shell's working directory")
        seen.add(pid)
        process_dir = proc_root / str(pid)
        try:
            owner = process_dir.stat().st_uid
        except FileNotFoundError:
            if depth == 0:
                raise PreflightError("cannot read this process's working directory from /proc") from None
            break
        except OSError:
            if depth > 0:
                break
            raise PreflightError("cannot safely verify the caller shell's working directory via /proc") from None

        if owner_uid is None:
            owner_uid = owner
        elif owner != owner_uid:
            break

        try:
            target = os.readlink(process_dir / "cwd")
        except FileNotFoundError:
            # An ancestor can exit while the process tree is being inspected.
            if depth == 0:
                raise PreflightError("cannot read this process's working directory from /proc") from None
            break
        except OSError:
            if depth > 0:
                # Linux may restrict a child from reading an ancestor's cwd via ptrace policy.
                break
            raise PreflightError("cannot safely verify the caller shell's working directory via /proc") from None
        paths.append(_cwd_path(target))

        try:
            stat = (process_dir / "stat").read_text(encoding="ascii")
        except FileNotFoundError:
            if depth == 0:
                raise PreflightError("cannot read this process's parent from /proc") from None
            break
        except (OSError, UnicodeError):
            if depth > 0:
                break
            raise PreflightError("cannot safely verify the caller shell's working directory via /proc") from None
        parent_pid = _parent_pid(stat)
        if parent_pid <= 1 or parent_pid == pid:
            return paths
        pid = parent_pid

    if pid > 1 and len(paths) == MAX_PROCESS_ANCESTORS:
        raise PreflightError("the caller process tree is too deep to safely check; run the update from a fresh shell")
    return paths


def _inherited_pwd() -> Path | None:
    """Read the shell's logical cwd, which remains inherited after a child chdir."""
    target = os.environ.get("PWD", "")
    if not target.startswith("/"):
        return None
    try:
        return _cwd_path(target)
    except PreflightError:
        return None


def _is_inside(path: Path, mountpoint: Path) -> bool:
    """Return whether a normalized path equals or is below the mountpoint."""
    try:
        path.relative_to(mountpoint)
    except ValueError:
        return False
    return True


def require_outside_mount(mountpoint: Path) -> None:
    """Reject an update if this process or a same-owner ancestor is inside the mount."""
    try:
        resolved_mount = Path(os.path.realpath(mountpoint))
    except (OSError, TypeError, ValueError):
        raise PreflightError("cannot safely resolve the configured Degoo mountpoint") from None
    inherited_pwd = _inherited_pwd()
    if inherited_pwd is not None and _is_inside(inherited_pwd, resolved_mount):
        working_directories = [inherited_pwd]
    else:
        try:
            working_directories = _process_cwds(PROC_ROOT, os.getpid())
        except PreflightError:
            raise
        if inherited_pwd is not None:
            working_directories.append(inherited_pwd)

    if any(_is_inside(path, resolved_mount) for path in working_directories):
        parent = shlex.quote(str(resolved_mount.parent))
        raise PreflightError(
            f"the caller shell or updater is inside the Degoo mount at {resolved_mount}; "
            f"close files opened there, run `cd {parent}` in that same shell, then run "
            "`degoo-rc update` again. The shell's working directory can keep the mount busy."
        )
