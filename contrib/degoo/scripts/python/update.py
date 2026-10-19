#!/usr/bin/env python3
"""Update the contained Linux client without changing its vault configuration."""

from __future__ import annotations

import argparse
import ast
import contextlib
import fcntl
import hashlib
import http.client
import json
import math
import os
import platform
import re
import shutil
import stat
import subprocess
import sys
import threading
import time
import tarfile
import tempfile
from pathlib import Path, PurePosixPath
from typing import Any

import install as installer
import logs as log_reader
import service
import sync as mount_sync
import uninstall
import update_preflight
import wizard


REPOSITORY = "georgeasiminei/rclone-degoo"
WORKFLOW = "degoo-package.yml"
ARTIFACT_NAME = "rclone-degoo-linux-amd64"
UPDATE_ARTIFACT_NAME = f"{ARTIFACT_NAME}-update"
ARCHIVE_NAME = f"{ARTIFACT_NAME}.tar.gz"
UPDATE_MANIFEST_NAME = f"{ARCHIVE_NAME}.update.json"
MAX_PACKAGE_BYTES = 1024 * 1024 * 1024
MAX_PACKAGE_FILES = 4096
ACTIVE_STATES = {"active", "activating", "deactivating", "reloading"}
UPDATE_CONTROL_TIMEOUT = 5.0
UPDATE_RESPONSE_LIMIT = 4096
UPDATE_PREPARE_TOKEN = re.compile(r"[0-9a-f]{64}\Z")
UPDATE_PREPARATION_LEASE_SECONDS = 120
MAX_INSTALLER_SOURCE_BYTES = 1024 * 1024
MAX_PREFLIGHT_HELP_BYTES = 64 * 1024
MAX_PREFLIGHT_ERROR_BYTES = 16 * 1024
MAX_STARTUP_LOG_BYTES = 64 * 1024
MANAGED_DIRECTORIES = ("bin", "scripts", "tools", "immutable-mount")
MANAGED_FILES = ("degoo-rc", "install.json", "rclone-degoo.service",
                 *installer.COMPATIBILITY_SHELL_FILES, *installer.SOURCE_FILES)


class UpdateError(Exception):
    """A safe, actionable update failure."""


@contextlib.contextmanager
def progress(label: str, *, interval: float = 10.0):
    """Report elapsed time while a slow update phase is still running."""
    stop = threading.Event()
    started = time.monotonic()

    def report():
        while not stop.wait(interval):
            print(f"{label}: still working ({int(time.monotonic() - started)}s elapsed).", flush=True)

    thread = threading.Thread(target=report, daemon=True)
    thread.start()
    try:
        yield
    finally:
        stop.set()
        thread.join()


def format_bytes(num_bytes: int | float) -> str:
    """Format byte count into human-readable unit (B, KiB, MiB, GiB)."""
    if num_bytes < 0:
        num_bytes = 0
    val = float(num_bytes)
    for unit in ("B", "KiB", "MiB", "GiB"):
        if val < 1024 or unit == "GiB":
            if unit == "B":
                return f"{int(val)} B"
            return f"{val:.1f} {unit}"
        val /= 1024
    return f"{val:.1f} GiB"


def format_sizes(current: int, total: int) -> str:
    """Format current and total byte counts compactly (e.g. 28.6/63.6 MiB)."""
    def _unit_val(b: int | float) -> tuple[float, str]:
        val = float(max(0, b))
        for u in ("B", "KiB", "MiB", "GiB"):
            if val < 1024 or u == "GiB":
                return val, u
            val /= 1024
        return val, "GiB"

    v1, u1 = _unit_val(current)
    v2, u2 = _unit_val(total)
    s1 = f"{int(v1)}" if u1 == "B" else f"{v1:.1f}"
    s2 = f"{int(v2)}" if u2 == "B" else f"{v2:.1f}"
    if u1 == u2:
        return f"{s1}/{s2} {u2}"
    return f"{s1} {u1}/{s2} {u2}"


def format_rate(bytes_per_sec: float) -> str:
    """Format byte rate per second."""
    if bytes_per_sec <= 0:
        return "0 B/s"
    return f"{format_bytes(bytes_per_sec)}/s"


def format_eta(seconds: float | None) -> str:
    """Format remaining seconds into a concise human-readable string."""
    if seconds is None or seconds <= 0 or math.isinf(seconds) or math.isnan(seconds):
        return "--"
    sec = int(round(seconds))
    if sec < 60:
        return f"{sec}s"
    minutes, sec = divmod(sec, 60)
    if minutes < 60:
        return f"{minutes}m {sec:02d}s"
    hours, minutes = divmod(minutes, 60)
    return f"{hours}h {minutes:02d}m"


def render_progress(label: str, current: int, total: int | None, rate: float, eta: float | None, columns: int) -> str:
    """Render a single line download progress bar tailored to terminal width."""
    if columns < 70 and len(label) > 15:
        label = "Downloading"
    prefix = f"{label}: "
    if total and total > 0:
        fraction = min(1.0, max(0.0, current / total))
        pct_str = f"{int(fraction * 100):3d}%"
        sizes_str = format_sizes(current, total)
        rate_str = format_rate(rate)
        eta_str = f"ETA {format_eta(eta)}" if (eta is not None and fraction < 1.0) else ""

        candidates = []
        if eta_str:
            candidates.append(f"{pct_str}  {sizes_str}  {rate_str}  {eta_str}")
            candidates.append(f"{pct_str} {sizes_str} {rate_str} {eta_str}")
        candidates.append(f"{pct_str}  {sizes_str}  {rate_str}")
        candidates.append(f"{pct_str} {sizes_str} {rate_str}")
        candidates.append(f"{pct_str}  {sizes_str}")
        candidates.append(f"{pct_str} {sizes_str}")
        candidates.append(f"{pct_str}")

        chosen_suffix = candidates[-1]
        chosen_bar_width = 5
        for cand in candidates:
            overhead = len(prefix) + 4 + len(cand)
            avail = (columns - 1) - overhead
            if avail >= 8:
                chosen_suffix = cand
                chosen_bar_width = min(25, avail)
                break

        filled = int(round(chosen_bar_width * fraction))
        if 0 < filled < chosen_bar_width:
            bar_chars = "=" * (filled - 1) + ">" + " " * (chosen_bar_width - filled)
        elif filled >= chosen_bar_width:
            bar_chars = "=" * chosen_bar_width
        else:
            bar_chars = " " * chosen_bar_width

        line = f"{prefix}[{bar_chars}]  {chosen_suffix}"
    else:
        sizes_str = format_bytes(current)
        rate_str = format_rate(rate)
        line = f"{prefix}{sizes_str}  {rate_str}"

    if len(line) >= columns:
        line = line[:columns - 1]
    return line


class DownloadProgressTracker:
    """Track and render download progress in real time."""

    def __init__(self, label: str, total_bytes: int | None = None, destination: Path | None = None,
                 interval: float = 0.2, is_tty: bool | None = None):
        self.label = label
        self.total_bytes = total_bytes
        self.destination = destination
        self.interval = interval
        self.process: subprocess.Popen | None = None
        self.current_bytes = 0
        self.stop_event = threading.Event()
        self.thread: threading.Thread | None = None
        self.started_at = 0.0
        self.last_print_time = 0.0
        self.last_reported_pct = -1
        self.last_line_len = 0
        self.is_tty = sys.stdout.isatty() if is_tty is None else is_tty
        self._samples: list[tuple[float, int]] = []

    def attach(self, proc: subprocess.Popen) -> None:
        """Attach a running subprocess to monitor."""
        self.process = proc

    def update_bytes(self, num_bytes: int) -> None:
        """Manually update the downloaded bytes count."""
        if num_bytes > self.current_bytes:
            self.current_bytes = num_bytes

    def _find_bytes(self) -> int:
        if self.process is not None and self.process.pid:
            pid = self.process.pid
            fd_dir = f"/proc/{pid}/fd"
            if os.path.exists(fd_dir):
                try:
                    for fd in os.listdir(fd_dir):
                        link = os.readlink(os.path.join(fd_dir, fd))
                        if "gh-artifact" in link and link.endswith(".zip"):
                            return os.stat(link).st_size
                except (OSError, FileNotFoundError):
                    pass

        if self.destination is not None:
            archive = self.destination / ARCHIVE_NAME
            try:
                if archive.is_file():
                    return self.total_bytes if self.total_bytes is not None else archive.stat().st_size
            except OSError:
                pass
        return self.current_bytes

    def _rate(self, now: float) -> float:
        self._samples.append((now, self.current_bytes))
        cutoff = now - 3.0
        while len(self._samples) > 1 and self._samples[0][0] < cutoff:
            self._samples.pop(0)
        if len(self._samples) >= 2:
            dt = self._samples[-1][0] - self._samples[0][0]
            db = self._samples[-1][1] - self._samples[0][1]
            if dt > 0.1 and db >= 0:
                return db / dt
        elapsed = max(0.001, now - self.started_at)
        return self.current_bytes / elapsed

    def _tick(self, now: float | None = None) -> None:
        """Execute one progress check and output tick."""
        if now is None:
            now = time.monotonic()
        found = self._find_bytes()
        if found > self.current_bytes:
            self.current_bytes = found
        if self.process is not None and self.process.poll() is not None:
            if self.process.returncode == 0 and self.total_bytes is not None:
                self.current_bytes = self.total_bytes

        rate = self._rate(now)
        remaining = (self.total_bytes - self.current_bytes) if self.total_bytes is not None else None
        eta = (remaining / rate) if (rate > 0 and remaining is not None and remaining > 0) else None
        columns = shutil.get_terminal_size(fallback=(80, 24)).columns
        line = render_progress(self.label, self.current_bytes, self.total_bytes, rate, eta, columns)

        if self.is_tty:
            sys.stdout.write(f"\r{line.ljust(self.last_line_len)}")
            sys.stdout.flush()
            self.last_line_len = max(self.last_line_len, len(line))
        else:
            pct = int((self.current_bytes / self.total_bytes) * 100) if (self.total_bytes and self.total_bytes > 0) else None
            milestone = pct is not None and pct >= self.last_reported_pct + 10 and pct > 0
            heartbeat = (now - self.last_print_time) >= 10.0
            if milestone:
                self.last_reported_pct = pct
                self.last_print_time = now
                print(line, flush=True)
            elif heartbeat:
                self.last_print_time = now
                if self.current_bytes > 0:
                    print(line, flush=True)
                else:
                    elapsed = int(now - self.started_at)
                    print(f"{self.label}: still working ({elapsed}s elapsed).", flush=True)

    def _run_loop(self) -> None:
        while not self.stop_event.wait(self.interval):
            self._tick()

    def __enter__(self) -> DownloadProgressTracker:
        self.started_at = time.monotonic()
        self.last_print_time = self.started_at
        self.thread = threading.Thread(target=self._run_loop, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, exc_type, exc_val, exc_tb) -> None:
        self.stop_event.set()
        if self.thread is not None:
            self.thread.join()
        if exc_type is None:
            columns = shutil.get_terminal_size(fallback=(80, 24)).columns
            elapsed = max(0.001, time.monotonic() - self.started_at)
            if self.total_bytes and self.total_bytes > 0:
                rate = self.total_bytes / elapsed
                final_line = render_progress(self.label, self.total_bytes, self.total_bytes, rate, None, columns)
                if self.is_tty:
                    sys.stdout.write(f"\r{final_line.ljust(self.last_line_len)}\n")
                    sys.stdout.flush()
                elif self.last_reported_pct < 100:
                    print(final_line, flush=True)
        else:
            if self.is_tty and self.last_line_len > 0:
                sys.stdout.write("\n")
                sys.stdout.flush()


def download_progress(label: str, total_bytes: int | None = None, *,
                      destination: Path | None = None, interval: float = 0.2,
                      is_tty: bool | None = None) -> DownloadProgressTracker:
    """Context manager for reporting download progress with a progress bar."""
    return DownloadProgressTracker(label, total_bytes=total_bytes, destination=destination,
                                   interval=interval, is_tty=is_tty)


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


PAYLOAD_INVENTORIES = ("SOURCE_FILES", "SOURCE_SHELL_FILES", "IMMUTABLE_MOUNT_FILES")


def _parse_payload_inventory(source: bytes) -> tuple[Path, ...]:
    """Read the literal payload inventory declared by one installer source."""
    try:
        module = ast.parse(source.decode("utf-8"))
    except (SyntaxError, UnicodeDecodeError, ValueError, RecursionError):
        raise UpdateError("the package installer has an invalid payload inventory") from None

    declarations: dict[str, tuple[str, ...]] = {}
    for statement in module.body:
        if not isinstance(statement, ast.Assign):
            continue
        names = [target.id for target in statement.targets if isinstance(target, ast.Name)]
        for inventory_name in PAYLOAD_INVENTORIES:
            if inventory_name not in names:
                continue
            if len(names) != 1 or names[0] != inventory_name or inventory_name in declarations:
                raise UpdateError("the package installer has an invalid payload inventory")
            value = statement.value
            if not isinstance(value, (ast.Tuple, ast.List)):
                raise UpdateError("the package installer has an invalid payload inventory")
            filenames: list[str] = []
            for item in value.elts:
                if not isinstance(item, ast.Constant) or type(item.value) is not str:
                    raise UpdateError("the package installer has an invalid payload inventory")
                filename = item.value
                try:
                    encoded = filename.encode("utf-8")
                except UnicodeEncodeError:
                    raise UpdateError("the package installer has an invalid payload inventory") from None
                if (not filename or filename in {".", ".."} or len(encoded) > 255
                        or "/" in filename or "\\" in filename
                        or any(ord(character) < 32 or ord(character) == 127 for character in filename)
                        or PurePosixPath(filename).parts != (filename,)
                        or filename in filenames):
                    raise UpdateError("the package installer has an invalid payload inventory")
                filenames.append(filename)
            declarations[inventory_name] = tuple(filenames)

    if (set(declarations) != set(PAYLOAD_INVENTORIES)
            or "install.py" not in declarations["SOURCE_FILES"]):
        raise UpdateError("the package installer has an invalid payload inventory")

    paths = tuple(
        path
        for inventory_name, directory in (
            ("SOURCE_FILES", Path("scripts/python")),
            ("SOURCE_SHELL_FILES", Path("scripts")),
            ("IMMUTABLE_MOUNT_FILES", Path("immutable-mount")),
        )
        for path in (directory / filename for filename in declarations[inventory_name])
    )
    if len(paths) > MAX_PACKAGE_FILES - 2 or len(set(paths)) != len(paths):
        raise UpdateError("the package installer has an invalid payload inventory")
    return paths


def _open_private_regular_file(path: Path) -> tuple[int, os.stat_result]:
    """Open one private, regular file without following links or races."""
    path = path.absolute()
    fd: int | None = None
    try:
        installer.check_components(path)
        before = os.lstat(path)
        if (not stat.S_ISREG(before.st_mode) or stat.S_ISLNK(before.st_mode)
                or before.st_mode & 0o7077):
            raise UpdateError("a package payload file is not a private regular file")
        fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_CLOEXEC", 0))
        opened = os.fstat(fd)
    except UpdateError:
        if fd is not None:
            os.close(fd)
        raise
    except (OSError, installer.InstallError):
        if fd is not None:
            os.close(fd)
        raise UpdateError("a package payload file could not be read safely") from None
    if (not stat.S_ISREG(opened.st_mode) or opened.st_mode & 0o7077
            or (opened.st_dev, opened.st_ino) != (before.st_dev, before.st_ino)):
        os.close(fd)
        raise UpdateError("a package payload file changed while being opened")
    return fd, opened


def _read_private_source(path: Path, maximum: int) -> bytes:
    """Read a bounded source file that remains stable while it is opened."""
    fd, opened = _open_private_regular_file(path)
    try:
        if opened.st_size > maximum:
            raise UpdateError("the package installer source exceeds the supported size")
        data = bytearray()
        while len(data) <= maximum:
            chunk = os.read(fd, min(64 * 1024, maximum + 1 - len(data)))
            if not chunk:
                break
            data.extend(chunk)
        after = os.fstat(fd)
        if (len(data) > maximum or len(data) != opened.st_size
                or (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns)
                != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)):
            raise UpdateError("the package installer source changed while being read")
        return bytes(data)
    except OSError:
        raise UpdateError("the package installer source could not be read safely") from None
    finally:
        os.close(fd)


def _payload_inventory(source_root: Path) -> tuple[Path, ...]:
    """Read a tree's declared payload paths without importing its installer."""
    data = _read_private_source(source_root / "scripts/python/install.py", MAX_INSTALLER_SOURCE_BYTES)
    return _parse_payload_inventory(data)


def _hash_private_payload_file(path: Path) -> tuple[str, int]:
    """Hash a bounded private payload file and report the bytes read."""
    fd, opened = _open_private_regular_file(path)
    try:
        digest = hashlib.sha256()
        size = 0
        while True:
            chunk = os.read(fd, min(1024 * 1024, MAX_PACKAGE_BYTES - size + 1))
            if not chunk:
                break
            size += len(chunk)
            if size > MAX_PACKAGE_BYTES:
                raise UpdateError("the package payload exceeds the supported size")
            digest.update(chunk)
        after = os.fstat(fd)
        if (size != opened.st_size
                or (opened.st_dev, opened.st_ino, opened.st_size, opened.st_mtime_ns)
                != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)):
            raise UpdateError("a package payload file changed while being read")
        return digest.hexdigest(), size
    except OSError:
        raise UpdateError("a package payload file could not be read safely") from None
    finally:
        os.close(fd)


def package_identity(binary: Path, source_root: Path) -> dict[str, str]:
    """Hash one tree using the payload inventory declared by that tree."""
    inventory = _payload_inventory(source_root)
    files: dict[Path, str] = {}
    total_size = 0
    for relative, path in (
        (Path("bin/rclone"), binary),
        (Path("degoo-rc"), source_root / "degoo-rc"),
        *((relative, source_root / relative) for relative in inventory),
    ):
        file_hash, size = _hash_private_payload_file(path)
        total_size += size
        if total_size > MAX_PACKAGE_BYTES:
            raise UpdateError("the package payload exceeds the supported size")
        files[relative] = file_hash

    payload = hashlib.sha256()
    for relative, file_digest in sorted(files.items(), key=lambda item: item[0].as_posix()):
        payload.update(relative.as_posix().encode("utf-8"))
        payload.update(b"\0")
        payload.update(bytes.fromhex(file_digest))
    binary_sha256 = files[Path("bin/rclone")]
    identity = {"binary_sha256": binary_sha256, "payload_sha256": payload.hexdigest()}
    commit = installer._build_commit(source_root, binary_sha256)
    if commit is not None:
        identity["commit"] = commit
    return identity


class ActionsSource:
    """Resolve and download the latest successful staging package using gh."""

    def __init__(self):
        self.manifest: dict[str, Any] | None = None
        self.artifact_size: int | None = None

    @staticmethod
    def _run(arguments: list[str], *, timeout: int = 60, tracker: Any | None = None) -> bytes:
        if not shutil.which("gh"):
            raise UpdateError("GitHub CLI (gh) is required; install gh, run gh auth login, then retry degoo-rc update")
        proc: subprocess.Popen | None = None
        try:
            if tracker is not None:
                proc = subprocess.Popen(["gh", *arguments], stdin=subprocess.DEVNULL,
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
                tracker.attach(proc)
                stdout, _ = proc.communicate(timeout=timeout)
                if proc.returncode != 0:
                    raise UpdateError("GitHub package lookup/download failed; check gh auth status and connectivity, then retry degoo-rc update")
                return stdout
            result = subprocess.run(["gh", *arguments], stdin=subprocess.DEVNULL,
                                    stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                    timeout=timeout, check=False)
        except subprocess.TimeoutExpired:
            raise UpdateError("GitHub package lookup/download timed out; retry degoo-rc update") from None
        finally:
            if proc is not None and proc.poll() is None:
                proc.kill()
                proc.wait()
        if result.returncode != 0:
            raise UpdateError("GitHub package lookup/download failed; check gh auth status and connectivity, then retry degoo-rc update")
        return result.stdout

    def download(self, destination: Path, *, installed_root: Path | None = None) -> tuple[Path | None, dict[str, Any]]:
        print("Checking the latest successful staging build on GitHub.", flush=True)
        with progress("Checking the latest staging build"):
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
        source = {"source": "actions", "repository": REPOSITORY,
                  "run_id": run["id"], "commit": run["head_sha"]}
        print(f"Latest staging build: run {run['id']} ({run['head_sha'][:12]}).", flush=True)
        self.manifest = self._download_manifest(run, destination / "metadata")
        if self.manifest is not None and installed_root is not None:
            print("Comparing the installed program with the update manifest.", flush=True)
            with progress("Comparing installed files"):
                matches = installed_matches(installed_root, self.manifest)
            if matches:
                print("Installed program matches; the package archive was not downloaded.", flush=True)
                return None, source
            preflight_mount_directory(installed_root, installer.load_manifest(installed_root))
        total_bytes = self.artifact_size or (self.manifest["archive_size"] if self.manifest else None)
        size = f" ({total_bytes / (1024 * 1024):.1f} MiB)" if total_bytes else ""
        print(f"Downloading the staging package archive{size}.", flush=True)
        with download_progress("Downloading package archive", total_bytes=total_bytes, destination=destination) as tracker:
            self._run(["run", "download", str(run["id"]), "--name", ARTIFACT_NAME,
                       "--repo", REPOSITORY, "--dir", str(destination)], timeout=600,
                      tracker=tracker)
        archive = destination / ARCHIVE_NAME
        if not archive.is_file():
            raise UpdateError("the latest workflow artifact is missing or expired; no installation files were changed")
        print("Package archive download complete.", flush=True)
        return archive, source

    def _download_manifest(self, run: dict[str, Any], destination: Path) -> dict[str, Any] | None:
        with progress("Checking update artifacts"):
            response = self._run(["api", f"repos/{REPOSITORY}/actions/runs/{run['id']}/artifacts?per_page=100"])
        try:
            artifacts = json.loads(response)["artifacts"]
            if not isinstance(artifacts, list) or any(not isinstance(item, dict) for item in artifacts):
                raise ValueError
            matching = [item for item in artifacts if item.get("name") == UPDATE_ARTIFACT_NAME]
            if len(matching) > 1:
                raise ValueError
            if matching and type(matching[0].get("expired")) is not bool:
                raise ValueError
            package_match = [item for item in artifacts if item.get("name") == ARTIFACT_NAME]
            if package_match:
                size = package_match[0].get("size_in_bytes")
                if type(size) is int and 0 < size <= MAX_PACKAGE_BYTES:
                    self.artifact_size = size
        except (KeyError, TypeError, ValueError, RecursionError):
            raise UpdateError("GitHub returned an invalid update artifact listing") from None
        if not matching or matching[0]["expired"]:
            print("This build has no separate update manifest; downloading its archive for comparison.", flush=True)
            return None

        print("Downloading the small update manifest and checksum.", flush=True)
        destination.mkdir(mode=0o700)
        with progress("Downloading update manifest"):
            self._run(["run", "download", str(run["id"]), "--name", UPDATE_ARTIFACT_NAME,
                       "--repo", REPOSITORY, "--dir", str(destination)], timeout=60)
        manifest_path = destination / UPDATE_MANIFEST_NAME
        checksum_path = destination / (ARCHIVE_NAME + ".sha256")
        try:
            for path in (manifest_path, checksum_path):
                installer.check_components(path)
                if not path.is_file() or path.stat().st_size > 4096:
                    raise ValueError
            manifest = json.loads(manifest_path.read_bytes())
            checksum = re.fullmatch(r"([0-9a-f]{64})  ([^\r\n]+)\n?", checksum_path.read_text(encoding="ascii"))
            fields = {"version", "archive", "archive_sha256", "archive_size", "binary_sha256",
                      "payload_sha256", "commit", "source_dirty"}
            if (not isinstance(manifest, dict) or set(manifest) != fields
                    or type(manifest["version"]) is not int or manifest["version"] != 1
                    or manifest["archive"] != ARCHIVE_NAME
                    or type(manifest["archive_size"]) is not int
                    or not 0 < manifest["archive_size"] <= MAX_PACKAGE_BYTES
                    or manifest["commit"] != run["head_sha"] or manifest["source_dirty"] is not False
                    or checksum is None or checksum[2] != ARCHIVE_NAME
                    or checksum[1] != manifest["archive_sha256"]):
                raise ValueError
            for key in ("archive_sha256", "binary_sha256", "payload_sha256"):
                if not isinstance(manifest[key], str) or re.fullmatch(r"[0-9a-f]{64}", manifest[key]) is None:
                    raise ValueError
        except (OSError, ValueError, TypeError, UnicodeError, RecursionError, installer.InstallError):
            raise UpdateError("the update manifest or checksum is invalid; no installation files were changed") from None
        print("Update manifest and checksum verified.", flush=True)
        return manifest

    def verify_package(self, identity: dict[str, Any], archive_hash: str, archive: Path) -> None:
        """Bind downloaded archive bytes and payload to the lightweight manifest."""
        if self.manifest is not None and (
                archive_hash != self.manifest["archive_sha256"]
                or archive.stat().st_size != self.manifest["archive_size"]
                or any(identity.get(key) != self.manifest[key] for key in ("binary_sha256", "payload_sha256", "commit"))):
            raise UpdateError("the downloaded package does not match its update manifest; installation was preserved")


def preflight_mount_directory(root: Path, manifest: dict[str, Any]) -> None:
    """Require the updater and its invoking shell to be outside an active mount."""
    mountpoint = installer.configured_mountpoint(root, manifest)
    if wizard._mounted_state(mountpoint) == "mounted":
        try:
            update_preflight.require_outside_mount(mountpoint)
        except update_preflight.PreflightError as exc:
            raise UpdateError(str(exc)) from None


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
            installer_member = next((member for member in members
                                     if member.name == "rclone-degoo/scripts/python/install.py"), None)
            if installer_member is None or installer_member.size > MAX_INSTALLER_SOURCE_BYTES:
                raise UpdateError("the package archive is incomplete or predates automatic updates")
            installer_stream = archive.extractfile(installer_member)
            if installer_stream is None:
                raise UpdateError("the package archive is incomplete or predates automatic updates")
            with installer_stream:
                installer_source = installer_stream.read(MAX_INSTALLER_SOURCE_BYTES + 1)
            if len(installer_source) != installer_member.size:
                raise UpdateError("the package archive is incomplete or predates automatic updates")
            inventory = _parse_payload_inventory(installer_source)
            required = {"degoo-rc", "bin/rclone", "docs/BUILD.json", "scripts/python/install.py",
                        *(relative.as_posix() for relative in inventory)}
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
    return package_identity(package / "bin/rclone", package)


def installed_matches(root: Path, identity: dict[str, Any]) -> bool:
    """Compare the installed program payload, including installations without provenance."""
    try:
        current = package_identity(root / "bin/rclone", root)
    except (OSError, UpdateError, installer.InstallError):
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


def _update_socket_path(root: Path) -> Path:
    """Return the verified mounted VFS socket or refuse an unsafe update."""
    try:
        return mount_sync._socket_path(root)
    except mount_sync.SyncError as exc:
        raise UpdateError(
            f"the running mount cannot prepare safely for an update ({exc}); "
            "close applications, stop the mount manually, then retry"
        ) from None


def _update_request(socket_path: Path, route: str, payload: dict[str, Any], *, timeout: float) -> tuple[int, bytes]:
    """Send one bounded update-control request over the private mount socket."""
    try:
        body = json.dumps(payload, separators=(",", ":"), allow_nan=False).encode("ascii")
    except (TypeError, ValueError, UnicodeEncodeError):
        raise UpdateError("the mount update request could not be encoded safely") from None
    connection = None
    try:
        connection = mount_sync.UnixHTTPConnection(socket_path, timeout=timeout)
        connection.request("POST", route, body=body, headers={"Content-Type": "application/json"})
        response = connection.getresponse()
        data = response.read(UPDATE_RESPONSE_LIMIT + 1)
    except (OSError, http.client.HTTPException, TimeoutError):
        if route == "/update/prepare":
            raise UpdateError(_uncertain_preparation_message()) from None
        raise UpdateError("the mounted VFS update request failed; the mount was left running") from None
    finally:
        if connection is not None:
            connection.close()
    if len(data) > UPDATE_RESPONSE_LIMIT:
        if route == "/update/prepare":
            raise UpdateError(_uncertain_preparation_message())
        raise UpdateError("the mounted VFS returned an oversized update response")
    return response.status, data


def _uncertain_preparation_message() -> str:
    """Explain how to recover when the preparation response could be lost."""
    return (
        "the updater left the old mount running, but the preparation response could not be verified. "
        f"The server may have accepted preparation, which can block new operations for up to "
        f"{UPDATE_PREPARATION_LEASE_SECONDS // 60} minutes; wait for the preparation lease to expire, then retry"
    )


def _update_response(data: bytes) -> dict[str, Any] | None:
    """Decode a bounded JSON object without exposing server response text."""
    try:
        value = json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError, RecursionError):
        return None
    return value if isinstance(value, dict) else None


def prepare_mount_for_update(root: Path, *, writer_timeout: float, upload_timeout: float) -> tuple[Path, str]:
    """Quiesce mounted writers before updater shutdown and return its lease token."""
    socket_path = _update_socket_path(root)
    timeout = max(UPDATE_CONTROL_TIMEOUT, writer_timeout + upload_timeout + 10.0)
    status, data = _update_request(
        socket_path,
        "/update/prepare",
        {"writer_timeout": writer_timeout, "upload_timeout": upload_timeout},
        timeout=timeout,
    )
    if status == 409:
        response = _update_response(data)
        if (response is not None and set(response) == {"error"}
                and type(response.get("error")) is str
                and response["error"] == "another update preparation is active"):
            raise UpdateError(
                "another update preparation is already active; no update was made and the mount is still running. "
                f"Wait for it to finish, or for its {UPDATE_PREPARATION_LEASE_SECONDS // 60}-minute lease to expire, then retry"
            )
        detail = "open handles or requested uploads did not become idle before the deadline"
        if upload_timeout == 0:
            detail = "open file or directory handles did not close before the deadline"
        raise UpdateError(
            f"{detail}; no update was made and the mount is still running. "
            "Close files using the vault and retry, or increase the applicable timeout"
        )
    if status == 404:
        raise UpdateError(
            "the running mount does not support safe update preparation; no update was made. "
            "Close applications, stop the mount manually, then retry with it inactive"
        )
    if status == 500:
        raise UpdateError(
            "the mounted VFS could not flush or verify its local cache; "
            "no update was made and the mount is still running"
        )
    response = _update_response(data)
    if (status != 200 or response is None or set(response) != {"token", "prepared"}
            or response.get("prepared") is not True
            or not isinstance(response.get("token"), str)
            or UPDATE_PREPARE_TOKEN.fullmatch(response["token"]) is None):
        raise UpdateError(_uncertain_preparation_message())
    return socket_path, response["token"]


def renew_mount_preparation(socket_path: Path, token: str) -> None:
    """Extend an update barrier lease without exposing its token."""
    status, data = _update_request(socket_path, "/update/renew", {"token": token}, timeout=UPDATE_CONTROL_TIMEOUT)
    response = _update_response(data)
    if status != 200 or response is None or set(response) != {"renewed"} or response.get("renewed") is not True:
        raise UpdateError("the mounted VFS update barrier could not be renewed; the mount was left running")


def cancel_mount_preparation(socket_path: Path, token: str) -> None:
    """Release a preparation barrier when shutdown leaves the mount running."""
    status, data = _update_request(socket_path, "/update/cancel", {"token": token}, timeout=UPDATE_CONTROL_TIMEOUT)
    response = _update_response(data)
    if status != 200 or response is None or set(response) != {"cancelled"} or response.get("cancelled") is not True:
        raise UpdateError("the mounted VFS update barrier could not be cancelled; check the running mount")


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
    """Unmount normally before asking systemd to stop the owned service."""
    mountpoint = installer.configured_mountpoint(root, manifest)
    mounted = wizard._mounted_state(mountpoint)
    if state["service_active"] and not state["mounted"]:
        raise UpdateError("the active service has no confirmed mount; the update was refused without stopping it")
    if state["mounted"]:
        if mounted != "mounted":
            raise UpdateError("the prepared mount state changed before shutdown; the update was refused")
        wizard._stop_owned_mount(root)
        if wizard._mounted_state(mountpoint) != "not mounted":
            raise UpdateError("the normal FUSE unmount did not complete; close files using the mount and retry")
    elif mounted != "not mounted":
        raise UpdateError("a mount appeared after update inspection; no runtime process was stopped")
    if state["service_owned"]:
        print("Mount is absent; stopping the owned service before replacement.", flush=True)
        uninstall._stop_owned_service(root, manifest, disable=False)
    if wizard._mounted_state(mountpoint) != "not mounted":
        raise UpdateError("the mount returned during shutdown; inspect the service and retry")


def _wait_for_service_mount(root: Path, manifest: dict[str, Any], state: dict[str, Any]) -> dict[str, Any]:
    """Resolve a service startup race before deciding whether preparation is required."""
    if not state["service_active"] or state["mounted"]:
        return state
    if wizard._wait_for_mount(root) != "mounted":
        raise UpdateError("the owned service is active but its mount was not confirmed; the update left it running")
    refreshed = runtime_state(root, manifest)
    if not refreshed["service_active"] or not refreshed["mounted"]:
        raise UpdateError("the owned service state changed during mount startup; the update left it untouched")
    return refreshed


def _safe_failure_diagnostics(data: bytes) -> str:
    """Summarize known failure classes without exposing provider response text."""
    summaries: list[str] = []
    for raw in data[:MAX_STARTUP_LOG_BYTES].splitlines()[:32]:
        line = log_reader._clean_line(raw)
        if not re.search(r"\b(?:ERROR|CRITICAL)\s*:", line, re.IGNORECASE):
            continue
        if re.search(r"\bGetOverlay4\b", line, re.IGNORECASE):
            if re.search(r"\bGraphQLException\b", line, re.IGNORECASE):
                summary = "Degoo GetOverlay4 returned GraphQLException"
            else:
                summary = "Degoo GetOverlay4 root lookup failed"
        elif re.search(r"\bGraphQLException\b", line, re.IGNORECASE):
            summary = "Degoo returned GraphQLException"
        elif re.search(r"\bdegoo login required\b|\blogin required\b", line, re.IGNORECASE):
            summary = "Degoo login is required"
        else:
            status = re.search(r"\b(?:auth\s+)?HTTP(?:/\d(?:\.\d)?)?\s+([45]\d\d)\b", line, re.IGNORECASE)
            if status:
                summary = f"Degoo API returned HTTP {status.group(1)}"
            elif re.search(r"connection (?:refused|reset)|no such host|i/o timeout|context deadline exceeded", line, re.IGNORECASE):
                summary = "the Degoo API connection failed"
            else:
                summary = "rclone logged a startup error"
        if summary not in summaries:
            summaries.append(summary)
        if len(summaries) == 3:
            break
    return "; ".join(summaries)


def _candidate_command(root: Path, binary: Path, *arguments: str) -> list[str]:
    """Build a candidate query with the install's explicit account config."""
    return [str(binary), "--config", str(root / "config/rclone.conf"), "--log-level", "ERROR", *arguments]


def preflight_candidate_vault(root: Path, package: Path, *, timeout: float = 30.0) -> None:
    """Check the verified candidate can read the configured Degoo root before shutdown."""
    binary = package / "bin/rclone"
    vault = root / "config/vault.json"
    cache_dir = installer.configured_cache_dir(root)
    installer.check_directory(root / "tmp", private=True)
    env = dict(os.environ)
    env.setdefault("HOME", str(installer.home_directory()))
    env["XDG_CONFIG_HOME"] = str(root / "config")
    env["XDG_CACHE_HOME"] = str(cache_dir)
    env["TMPDIR"] = str(root / "tmp")
    env["PATH"] = "/usr/bin:/bin"
    env["PYTHONDONTWRITEBYTECODE"] = "1"

    try:
        with tempfile.TemporaryFile(dir=root / "tmp") as help_output, \
                tempfile.TemporaryFile(dir=root / "tmp") as help_error:
            help_result = subprocess.run(
                _candidate_command(root, binary, "degoo", "location", "--help"),
                stdin=subprocess.DEVNULL, stdout=help_output, stderr=help_error, env=env,
                timeout=timeout, check=False, umask=0o077,
            )
            help_output.seek(0)
            help_text = help_output.read(MAX_PREFLIGHT_HELP_BYTES + 1)
            help_error.seek(0)
            help_text += help_error.read(MAX_PREFLIGHT_HELP_BYTES + 1)
        if help_result.returncode != 0:
            raise UpdateError("the candidate's local Degoo location help failed; the current mount and installed program were left untouched")
        query = ["degoo", "location"]
        if b"--root-only" in help_text:
            query.append("--root-only")
        query.append(str(vault))
        with tempfile.TemporaryFile(dir=root / "tmp") as error:
            result = subprocess.run(
                _candidate_command(root, binary, *query),
                stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=error, env=env,
                timeout=timeout, check=False, umask=0o077,
            )
            error.seek(0)
            error_data = error.read(MAX_PREFLIGHT_ERROR_BYTES + 1)
    except subprocess.TimeoutExpired:
        raise UpdateError("the candidate Degoo root check timed out; the current mount and installed program were left untouched") from None
    except OSError:
        raise UpdateError("the candidate Degoo root check could not start; the current mount and installed program were left untouched") from None
    if result.returncode != 0:
        summary = _safe_failure_diagnostics(error_data[:MAX_PREFLIGHT_ERROR_BYTES])
        detail = f" ({summary})" if summary else ""
        raise UpdateError(
            "the candidate could not verify access to the configured Degoo root" + detail
            + "; the current mount and installed program were left untouched. Verify account access; the configured remote folder may have been moved or removed. "
            "Review degoo-rc location and degoo-rc setup --help before recovery; the updater will not create a replacement or change the configured folder"
        )


def _startup_log_snapshot(root: Path, name: str) -> tuple[Path, int | None, int | None, int] | None:
    """Record a private startup log's position so old failures are excluded."""
    path = root / "logs" / name
    try:
        fd = log_reader._open_log(path)
    except log_reader.LogsError:
        return None
    if fd is None:
        return (path, None, None, 0)
    try:
        info = os.fstat(fd)
        return (path, info.st_dev, info.st_ino, info.st_size)
    except OSError:
        return None
    finally:
        os.close(fd)


def _fresh_startup_diagnostics(snapshot: tuple[Path, int | None, int | None, int] | None) -> str:
    """Read only bounded log bytes appended since the current restart began."""
    if snapshot is None:
        return ""
    path, old_device, old_inode, old_size = snapshot
    try:
        fd = log_reader._open_log(path)
    except log_reader.LogsError:
        return ""
    if fd is None:
        return ""
    try:
        info = os.fstat(fd)
        size = info.st_size
        if (old_device, old_inode) == (info.st_dev, info.st_ino) and size >= old_size:
            offset = old_size
        else:
            offset = max(0, size - MAX_STARTUP_LOG_BYTES)
        if size - offset > MAX_STARTUP_LOG_BYTES:
            offset = size - MAX_STARTUP_LOG_BYTES
        partial_first_line = False
        if offset > 0:
            os.lseek(fd, offset - 1, os.SEEK_SET)
            partial_first_line = os.read(fd, 1) != b"\n"
        os.lseek(fd, offset, os.SEEK_SET)
        data = os.read(fd, MAX_STARTUP_LOG_BYTES)
        if partial_first_line:
            _, separator, data = data.partition(b"\n")
            if not separator:
                return ""
        return _safe_failure_diagnostics(data)
    except OSError:
        return ""
    finally:
        os.close(fd)


def _restart_failure(root: Path, state: dict[str, Any], snapshot: tuple[Path, int | None, int | None, int] | None,
                     reason: str) -> UpdateError:
    """Report a failed restart and stop only an owned service with no mount."""
    diagnostic = _fresh_startup_diagnostics(snapshot)
    service_action = ""
    if state.get("service_owned"):
        try:
            manifest = installer.load_manifest(root)
            mountpoint = installer.configured_mountpoint(root, manifest)
            if wizard._mounted_state(mountpoint) == "not mounted":
                stopped_units = uninstall._stop_owned_service(root, manifest, disable=False)
                if stopped_units:
                    service_action = " The owned service was stopped after the mount table confirmed no mount, to prevent repeated restart attempts."
                else:
                    service_action = " The service could not be reverified as owned, so it was left untouched; inspect degoo-rc service status."
        except (installer.InstallError, uninstall.CleanupError, OSError):
            service_action = " The owned service could not be safely stopped; inspect degoo-rc service status and degoo-rc logs."
    detail = f" Fresh startup error: {diagnostic}." if diagnostic else " No safe fresh error detail was available; inspect degoo-rc logs."
    recovery = "degoo-rc service start" if state.get("service_active") else "the manual mount"
    return UpdateError(
        f"{reason}.{detail}{service_action} The verified package remains installed; configuration and cached writes were preserved. "
        f"Resolve the reported cause, then retry {recovery}"
    )


def _bounded_duration(value: str, *, allow_zero: bool, maximum: int, label: str) -> float:
    """Parse an rclone duration in seconds within an update timeout bound."""
    if value == "off" and allow_zero:
        return 0.0
    nanoseconds = installer._mount_duration_ns(value)
    if nanoseconds is None:
        requirement = "nonnegative" if allow_zero else "positive"
        raise argparse.ArgumentTypeError(f"{label} must be a {requirement} duration no longer than {maximum}s")
    if (nanoseconds > maximum * 1_000_000_000 or (nanoseconds == 0 and not allow_zero)
            or 0 < nanoseconds < 1):
        requirement = "nonnegative" if allow_zero else "positive"
        raise argparse.ArgumentTypeError(f"{label} must be a {requirement} duration no longer than {maximum}s")
    seconds = float(nanoseconds / 1_000_000_000)
    if not math.isfinite(seconds):
        raise argparse.ArgumentTypeError(f"{label} is outside the supported range")
    return seconds


def parse_writer_timeout(value: str) -> float:
    """Parse a positive writer-quiescence timeout up to one hour."""
    return _bounded_duration(value, allow_zero=False, maximum=3600, label="--writer-timeout")


def parse_upload_timeout(value: str) -> float:
    """Parse an optional upload-drain timeout up to one day."""
    return _bounded_duration(value, allow_zero=True, maximum=86400, label="--wait-for-uploads")


def start_runtime(root: Path, state: dict[str, Any]) -> None:
    """Restore the previous active state and confirm a connected local mount."""
    startup_log = None
    if state["service_owned"]:
        result = service._systemctl(root, "daemon-reload")
        if result.returncode != 0:
            raise UpdateError("systemd could not reload the updated unit; run degoo-rc service restart")
    if state["service_active"]:
        print("Restarting the previously active user service.", flush=True)
        manifest = installer.load_manifest(root)
        service._require_loaded_unit(root, manifest)
        startup_log = _startup_log_snapshot(root, "mount.log")
        try:
            result = service._systemctl(root, "start", service.UNIT_NAME)
        except service.ServiceError:
            raise _restart_failure(root, state, startup_log, "the updated service could not start") from None
        if result.returncode != 0:
            raise _restart_failure(root, state, startup_log, "the updated service could not start")
    elif state["manual_arguments"]:
        print("Restarting the previous manual mount in the background.", flush=True)
        try:
            cache_dir = installer.configured_cache_dir(root)
        except installer.InstallError as exc:
            raise UpdateError(str(exc)) from None
        env = dict(os.environ, PATH=f"{root / 'tools'}:/usr/bin:/bin",
                   XDG_CONFIG_HOME=str(root / "config"), XDG_CACHE_HOME=str(cache_dir),
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
        startup_log = _startup_log_snapshot(root, "update-mount.log")
        with os.fdopen(descriptor, "ab") as output:
            subprocess.Popen(state["manual_arguments"], stdin=subprocess.DEVNULL,
                             stdout=output, stderr=output, env=env, cwd=state.get("manual_directory"),
                             start_new_session=True, umask=0o077)
    if (state["service_active"] or state["manual_arguments"]) and wizard._wait_for_mount(root) != "mounted":
        raise _restart_failure(root, state, startup_log, "the restarted mount was not confirmed")
    if state["service_active"] or state["manual_arguments"]:
        print("Restart complete; the vault mount is connected.", flush=True)


def _managed_path_error(path: Path, root: Path, problem: str) -> UpdateError:
    """Return a safe error naming a managed path and how to repair it."""
    try:
        relative = path.relative_to(root).as_posix()
    except ValueError:
        relative = "managed program path"
    parts = PurePosixPath(relative).parts
    if "__pycache__" in parts:
        cache_path = PurePosixPath(*parts[:parts.index("__pycache__") + 1]).as_posix()
        return UpdateError(
            f"runtime Python bytecode cache at {cache_path} has {problem}; "
            "remove that generated Python bytecode cache and retry degoo-rc update"
        )
    return UpdateError(
        f"managed program path {relative} has {problem}; "
        "restore that program path from a verified package and retry degoo-rc update"
    )


def check_managed_tree(path: Path, root: Path) -> None:
    """Reject unexpected links and special files before snapshotting program files."""
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode):
        if path != root / "tools/fusermount3":
            raise _managed_path_error(path, root, "an unexpected symbolic link")
        try:
            uninstall._validate_helper_link(path, require_executable=False)
        except uninstall.CleanupError:
            raise _managed_path_error(path, root, "an invalid FUSE helper link") from None
        return
    if info.st_uid != os.getuid():
        raise _managed_path_error(path, root, "ownership that does not match the current user")
    if info.st_mode & 0o022:
        raise _managed_path_error(path, root, "group- or world-writable permissions")
    if info.st_mode & 0o6000:
        raise _managed_path_error(path, root, "setuid or setgid permissions")
    if stat.S_ISDIR(info.st_mode):
        for child in path.iterdir():
            check_managed_tree(child, root)
    elif not stat.S_ISREG(info.st_mode):
        raise _managed_path_error(path, root, "an unexpected special file")


def _check_managed_program(root: Path) -> list[str]:
    """Check managed program paths before stopping the installed runtime."""
    names = list(dict.fromkeys((*MANAGED_DIRECTORIES, *MANAGED_FILES)))
    for name in names:
        path = root / name
        if os.path.lexists(path):
            check_managed_tree(path, root)
    return names


def snapshot_program(root: Path, backup: Path) -> list[str]:
    """Keep only program files and installation identity for installation rollback."""
    if os.path.lexists(backup):
        raise UpdateError(f"an interrupted update backup remains at {backup}; resolve that conflict before updating")
    names = _check_managed_program(root)
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
    try:
        cache = installer.configured_cache_dir(root)
    except installer.InstallError as exc:
        raise UpdateError(str(exc)) from None
    lock = uninstall._cache_lock_path(root)
    cache_name = lock.stem
    identity = cache / f"{cache_name}.identity"
    if (root / "config/vault.json").is_file():
        if uninstall._pending_uploads(root) != (0, 0):
            raise UpdateError("pending local uploads remain; --delete-cache was refused and the cache was preserved")
    elif uninstall._cache_has_data(root, lock):
        raise UpdateError("cached data has no matching vault configuration; cache deletion was refused")
    try:
        has_data = uninstall._audit_cache_namespace(root, cache)
    except uninstall.CleanupError as exc:
        raise UpdateError(f"cache deletion was refused: {exc}") from None
    if has_data and identity.is_file() and (root / "config/vault.json").is_file():
        try:
            uninstall._verify_cache_identity(root, cache)
        except uninstall.CleanupError as exc:
            raise UpdateError(f"cache deletion was refused: {exc}") from None
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
    cache = lock.parent
    for child in cache.iterdir():
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
                 archive_hash: str, *, delete_cache: bool, writer_timeout: float = 30.0,
                 upload_timeout: float = 0.0) -> bool:
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
    mount_unmounted = False
    changed = False
    backup = root / "tmp/update-backup"
    names = None
    state = None
    preparation = None
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise UpdateError("another update is running; retry after it finishes") from None
        if os.path.lexists(backup):
            raise UpdateError(f"an interrupted update backup remains at {backup}; resolve that conflict before updating")
        if installed_matches(root, identity):
            return False
        _check_managed_program(root)
        layout = uninstall._validate_layout(root)
        manifest = layout["manifest"]
        state = runtime_state(root, manifest)
        state = _wait_for_service_mount(root, manifest, state)
        mount_unmounted = not state["mounted"]
        if state["mounted"]:
            preflight_mount_directory(root, manifest)
        if state["service_active"] or state["manual_arguments"]:
            if wizard._mount_preflight(root, installer.configured_mountpoint(root, manifest),
                                       systemd_available=state["service_active"]):
                raise UpdateError("mount prerequisites need repair before updating; follow the setup commands above")
        if state["mounted"]:
            preflight_candidate_vault(root, package)
            wait_for = "open handles and requested uploads" if upload_timeout > 0 else "open handles"
            print(f"Preparing the mounted vault; waiting for {wait_for} (handle/flush deadline: {writer_timeout:g}s).", flush=True)
            if upload_timeout > 0:
                print(f"Upload completion is required within a separate {upload_timeout:g}s deadline.", flush=True)
            with progress("Preparing the mounted vault"):
                preparation = prepare_mount_for_update(
                    root, writer_timeout=writer_timeout, upload_timeout=upload_timeout
                )
            print("Mount prepared; local cached data and recovery metadata have been flushed.", flush=True)
            if upload_timeout == 0:
                print("Pending uploads will remain in the cache and resume after restart.", flush=True)
        if preparation is not None:
            renew_mount_preparation(*preparation)
        if state["mounted"]:
            print("Unmounting the vault normally before stopping its runtime.", flush=True)
        stop_runtime(root, manifest, state)
        mount_unmounted = True
        print("Acquiring exclusive access to the retained local cache.", flush=True)
        lock = uninstall._acquire_cache_lock(root, 10)
        stopped = True
        if delete_cache:
            require_clean_cache(root)
        names = snapshot_program(root, backup)
        try:
            print("Installing the verified package; configuration and local writes are preserved.", flush=True)
            with progress("Installing the package"):
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
        active_error = sys.exc_info()[1]
        barrier_error = None
        if preparation is not None and not mount_unmounted and state is not None:
            mountpoint = installer.configured_mountpoint(root, manifest)
            try:
                if wizard._mounted_state(mountpoint) != "not mounted":
                    cancel_mount_preparation(*preparation)
            except UpdateError as exc:
                barrier_error = exc
        if lock is not None:
            os.close(lock)
        try:
            if stopped and state is not None:
                start_runtime(root, state)
        finally:
            os.close(fd)
        if barrier_error is not None:
            if isinstance(active_error, UpdateError):
                raise UpdateError(f"{active_error}; {barrier_error}") from None
            raise barrier_error
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
    parser.add_argument("--writer-timeout", type=parse_writer_timeout, default=30.0,
                        metavar="DURATION", help="wait for mounted file and directory handles to close (default: 30s; maximum: 1h)")
    parser.add_argument("--wait-for-uploads", type=parse_upload_timeout, default=0.0,
                        metavar="DURATION", help="also wait for uploads (default: off; maximum: 1d)")
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
            download = None
            print("Using the local package archive.", flush=True)
        else:
            download = ActionsSource()
            archive, source = download.download(workspace, installed_root=root)
            if archive is None:
                print("Already up to date; no installation, service, or cache changes.")
                return 0
        print("Verifying the archive checksum and extracting the package.", flush=True)
        with progress("Verifying and extracting the package"):
            package, archive_hash = extract_package(archive, workspace / "extracted")
        print("Checking binary provenance, command readiness, and program hashes.", flush=True)
        with progress("Validating the package"):
            identity = validate_package(package, source)
        if download is not None:
            download.verify_package(identity, archive_hash, archive)
        print("Package verified; comparing with the installed program.", flush=True)
        if installed_matches(root, identity):
            print("Already up to date; no installation, service, or cache changes.")
            return 0
        changed = apply_update(root, package, identity, source, archive_hash,
                               delete_cache=args.delete_cache, writer_timeout=args.writer_timeout,
                               upload_timeout=args.wait_for_uploads)
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
