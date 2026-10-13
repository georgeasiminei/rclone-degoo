#!/usr/bin/env python3
"""Dispatch the contained Degoo helpers through one discoverable command."""

from __future__ import annotations

import importlib
import os
import subprocess
import sys
import tempfile
from pathlib import Path

import install as installer


BASE = Path(__file__).resolve().parents[2]
COMMANDS = {
    "install": "Install or upgrade the client and configure a vault with a guided wizard.",
    "update": "Install the latest verified package, preserving configuration and pending writes.",
    "uninstall": "Inventory or remove the local installation, with optional state retention.",
    "backup-config": "Back up vault configuration without account passwords or authentication tokens.",
    "login": "Sign in and save an encrypted account password for automatic recovery.",
    "setup": "Select or create a dedicated remote folder and configure vault encryption.",
    "mount": "Mount the vault in the foreground with transparent encryption and decryption.",
    "mount-setup": "Install the system helper for an immutable /mnt/degoo (run explicitly with sudo).",
    "stop": "Unmount the foreground vault safely, preserving pending cached writes.",
    "status": "Show authentication, mount health, pending uploads, and optional per-file upload state.",
    "benchmark": "Measure mounted read/write throughput with cleanup by default.",
    "sync": "Refresh the mounted remote directory view for a local path, preserving pending writes.",
    "location": "Query Degoo for the configured vault folder and its ancestry.",
    "folders": "List account device folders or children of a selected folder.",
    "service": "Enable, disable, start, stop, restart, or inspect the systemd user service.",
    "info": "Report local mount, service, authentication, cache, and disk state.",
    "logs": "View recent command logs and account-authentication status or follow new messages with credentials redacted.",
    "trash": "List account-wide trash without deleting anything.",
    "empty-trash": "Permanently delete all account trash after confirmation; supports dry-run.",
}
PYTHON_COMMANDS = {"install", "update", "uninstall", "backup-config", "benchmark", "sync", "login", "setup", "service", "info", "logs", "status", "mount-setup"}
DETAILS = {
    "update": "Downloads the latest successful staging package using authenticated gh. Verifies checksums and build provenance before stopping anything. Restores an active service or recognised manual mount; inactive installs stay inactive. Configuration and cache are preserved. --delete-cache refuses pending or unverifiable writes, and has no effect when already current. --archive PATH uses a local tar.gz and its checksum sidecar. Install failures restore the previous program files; startup failures retain the installed update for recovery.",
    "install": "With no parameters, starts the wizard. Installs into ~/.local/share/rclone-degoo and links ~/.local/bin/degoo-rc. An unattended install needs --binary and --no-wizard. Upgrade preserves configuration and cached writes.",
    "uninstall": "With no parameters in a terminal, starts guided cleanup. --inventory is read-only. Removal affects local installation files and owned service/PATH links; remote files remain. Pending writes require retaining both cache and matching configuration.",
    "backup-config": "Creates a private vault configuration backup without stopping the mount. --directory selects a new destination outside the install. Account passwords, authentication tokens, file data, cache, and pending uploads are excluded. Restore with install --wizard --restore-directory PATH and log in again.",
    "login": "Prompts visibly for account email and a hidden password. Saves tokens and, when vault.json exists, encrypts the account credentials with the vault key for automatic re-login. Before vault setup it saves tokens only; run login after setup to enable password fallback. --clear-email forgets the prompt's email only. Vault encryption settings and pending writes are preserved.",
    "setup": "Use --folder-id for an existing dedicated vault or --parent-id DEVICE_ID --name VAULT_NAME to create one. Account root ID 0 is not a vault or creation parent. Reusing encrypted data requires its original --vault-config.",
    "mount": "Uses the contained vault configuration, cache settings, and mount directory. Keep only one writer using the cache; stop the user service before a foreground mount. Writes persist locally first and upload asynchronously. Ctrl+C ends the foreground session.",
    "stop": "Takes no parameters. Unmounts the contained mount using FUSE. To stop a service-managed mount, use degoo-rc service stop so systemd does not restart it. Cached pending writes remain available for recovery.",
    "status": "Reports authentication failures first, then mount/service health and local pending uploads. An optional local file path shows upload state; --json returns machine-readable status. Unhealthy states return a nonzero exit code. A zero pending count does not confirm cloud contents. Uploaded describes the current version known to VFS, without independent remote integrity verification.",
    "sync": "Use sync . from inside the mount, or pass an absolute mounted path. Refreshes remote directory metadata recursively; a file path refreshes its containing directory, and a missing path refreshes its nearest existing directory inside the mount. Preserves pending local writes. Reopen files to see changed content. This command does not upload, mirror, or download the whole directory.",
    "location": "Takes no positional parameters. Contacts Degoo and may renew account tokens. Returns the configured folder ancestry as JSON; does not download file contents.",
    "folders": "Uses the contained account authentication. Defaults to account root ID 0 to discover device folders. --parent-id ID lists children of that folder; output is JSON. Does not modify remote files.",
    "service": "enable links and starts the owned user service; disable stops and unlinks it. start, stop, restart, and status operate on that service. Account lingering is configured separately with loginctl.",
    "info": "The default report is local-only. --check-remote queries folder ancestry and may renew account tokens. Reports pending uploads separately from remote completion; never prints saved credentials.",
    "logs": "Shows recent mount, login, setup, status, and Degoo diagnostic messages plus an account-authentication summary. Cobra usage blocks are compacted to keep command errors visible. --follow watches for new lines; Ctrl+C exits. Recognized credential fields and signed URLs are redacted from displayed lines.",
}


def root_directory() -> Path:
    """Return the contained installation used by operational commands."""
    return installer.expected_root()


def binary_path() -> str | None:
    """Find a local Degoo-enabled binary for native flag help."""
    override = os.environ.get("RCLONE_DEGOO_BIN")
    if override:
        return override
    for path in (BASE / "bin/rclone", BASE / "../../build/rclone", root_directory() / "bin/rclone"):
        if path.is_file() and os.access(path, os.X_OK):
            return str(path)
    return None


def show_help(command: str | None = None) -> int:
    """Display command help without loading authentication or changing state."""
    if command is None:
        print("Usage: degoo-rc COMMAND [PARAMETERS]\n       degoo-rc COMMAND help\n\nCommands:")
        for name, description in COMMANDS.items():
            print(f"  {name:13} {description}")
        print("  help          List commands or describe one command.")
        print("\nUse degoo-rc help COMMAND or degoo-rc COMMAND help for parameters and details.")
        return 0
    print(f"{COMMANDS[command]}\n", flush=True)
    if command in PYTHON_COMMANDS:
        module = importlib.import_module(command.replace("-", "_"))
        parser = module.build_parser()
        parser.prog = f"degoo-rc {command}"
        for action in parser._actions:
            if action.dest == "root":
                action.required = False
                action.default = str(BASE if command == "mount-setup" else root_directory())
        parser.print_help()
    elif command in {"trash", "empty-trash"}:
        print(f"Usage: degoo-rc {command} [AUTH_FILE] [OPTIONS]\n")
        print("  --auth FILE     Account auth file (defaults to the contained config/auth.json).\n"
              "  --rclone PATH   Override the executable (also RCLONE_DEGOO_BIN).")
        if command == "trash":
            print("  --limit N       Maximum items to list; 0 means all (default).\n"
                  "  --json          Return machine-readable JSON.")
        else:
            print("  --dry-run       List candidates without changing anything.\n"
                  "  --yes           Skip both confirmation prompts.")
        print("  -h, --help      Show help.\n\n"
              "Trash covers the whole account, including files outside the vault. Emptying is\n"
              "irreversible and prompts twice unless --yes is supplied. No restore is provided.\n"
              "RCLONE_DEGOO_AUTH can override the default auth file.\n")
    else:
        print(f"Usage: degoo-rc {command} [OPTIONS]\n")
        if command == "folders":
            print("  --parent-id ID  Folder to list (default: 0, the account root).\n")
        if command != "stop":
            binary = binary_path()
            if binary:
                print("Additional rclone parameters (config/auth/mount paths are supplied automatically):", flush=True)
                with tempfile.TemporaryDirectory(prefix="degoo-help-") as temporary:
                    env = dict(os.environ, HOME=temporary, XDG_CONFIG_HOME=temporary,
                               XDG_CACHE_HOME=temporary, RCLONE_CONFIG=os.devnull)
                    result = subprocess.run([binary, "degoo", command, "--help"],
                                            env=env, timeout=30, check=False)
                if result.returncode != 0:
                    return result.returncode
            else:
                print("Build or install the client for the full rclone flag reference.\n"
                      "Common flags: --log-level LEVEL, --log-file FILE.")
                if command == "mount":
                    print("Mount overrides include --read-only, --vfs-cache-max-size SIZE,\n"
                          "--vfs-cache-min-free-space SIZE, --vfs-cache-max-age DURATION,\n"
                          "--dir-cache-time DURATION, and --verify-upload=true|false.")
    if command in DETAILS:
        print(f"\n{DETAILS[command]}")
    return 0


def main(argv: list[str] | None = None) -> int:
    args = list(sys.argv[1:] if argv is None else argv)
    if not args or args == ["help"] or args in (["--help"], ["-h"]):
        return show_help()
    if args[0] == "help":
        if len(args) != 2 or args[1] not in COMMANDS:
            print("Usage: degoo-rc help [COMMAND]", file=sys.stderr)
            return 2
        return show_help(args[1])
    command, *parameters = args
    if command not in COMMANDS:
        print(f"Unknown command: {command}. Use degoo-rc help.", file=sys.stderr)
        return 2
    if command == "benchmark" and parameters and parameters[0] in {"read", "write"} and any(
        item in {"--help", "-h", "help"} for item in parameters[1:]
    ):
        module = importlib.import_module("benchmark")
        return module.main([parameters[0], "--help"])
    if parameters == ["help"] or "--help" in parameters or "-h" in parameters:
        return show_help(command)
    if command == "mount-setup":
        module = importlib.import_module("mount_setup")
        return module.main(["--root", str(BASE), *parameters])
    root = root_directory()
    if command in PYTHON_COMMANDS:
        module = importlib.import_module(command.replace("-", "_"))
        if command != "install":
            parameters = ["--root", str(root), *parameters]
        return module.main(parameters)
    if command in {"trash", "empty-trash"}:
        mode = "list" if command == "trash" else "empty"
        os.execv(str(BASE / "scripts/trash.sh"), ["trash.sh", mode, *parameters])
    if command == "stop" and parameters:
        print("degoo-rc stop takes no parameters. Use degoo-rc stop help.", file=sys.stderr)
        return 2
    wrapper = root / "scripts" / f"{command}.sh"
    if not wrapper.is_file():
        wrapper = root / f"{command}.sh"
    if not wrapper.is_file():
        print("Install the client first with degoo-rc install.", file=sys.stderr)
        return 2
    os.execv(str(wrapper), [str(wrapper), *parameters])
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, subprocess.TimeoutExpired):
        print("degoo-rc: could not run the local helper or executable.", file=sys.stderr)
        raise SystemExit(2) from None
