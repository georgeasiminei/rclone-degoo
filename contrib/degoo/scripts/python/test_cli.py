#!/usr/bin/env python3
"""Exercise command discovery and dispatch with isolated homes and fake rclone."""

from __future__ import annotations

import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

import cli


class CLITest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="degoo-cli-")
        self.addCleanup(self.temporary.cleanup)
        self.workspace = Path(self.temporary.name)
        self.home = self.workspace / 'home with spaces'
        self.home.mkdir(mode=0o700)
        self.env = dict(os.environ, HOME=str(self.home), PYTHONDONTWRITEBYTECODE="1")
        self.env.pop("RCLONE_DEGOO_BIN", None)
        self.env.pop("RCLONE_DEGOO_AUTH", None)
        self.env["XDG_CONFIG_HOME"] = str(self.home / ".config")
        self.launcher = cli.BASE / "degoo-rc"
        self.root = self.home / ".local/share/rclone-degoo"
        self.binary = self.workspace / "fake-rclone"
        self.binary.write_text(
            '#!/usr/bin/python3\nimport json, sys\n'
            'if sys.argv[1:] == ["degoo", "--help"]:\n'
            '    print("Usage: rclone degoo command mount login")\n'
            'else:\n'
            '    print(json.dumps(sys.argv[1:]))\n', encoding="utf-8"
        )
        self.binary.chmod(0o700)

    def run_cli(self, *args, launcher=None):
        return subprocess.run(
            [str(launcher or self.launcher), *args, *(["--mountpoint", "mount"] if args and args[0] == "install" and "--binary" in args else [])], env=self.env,
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, timeout=30, check=False,
        )

    def install(self):
        result = self.run_cli("install", "--binary", str(self.binary), "--no-wizard")
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_all_help_paths_work_before_install_without_state_changes(self):
        general = self.run_cli("help")
        self.assertEqual(general.returncode, 0, general.stderr)
        for command in cli.COMMANDS:
            self.assertIn(command, general.stdout)
            with self.subTest(command=command):
                first = self.run_cli(command, "help")
                second = self.run_cli("help", command)
                self.assertEqual(first.returncode, 0, first.stderr)
                self.assertEqual(first.stdout, second.stdout)
                self.assertIn(f"degoo-rc {command}", first.stdout)
        self.assertFalse(self.root.exists())
        self.assertEqual(list(self.home.iterdir()), [])

    def test_python_parameters_and_unknown_commands(self):
        help_result = self.run_cli("uninstall", "help")
        self.assertIn("--keep-cache", help_result.stdout)
        self.assertIn("--inventory", help_result.stdout)
        self.assertIn("--parent-id", self.run_cli("setup", "help").stdout)
        self.assertIn("--follow", self.run_cli("logs", "help").stdout)
        self.assertIn("account-authentication summary", self.run_cli("logs", "help").stdout)
        result = self.run_cli("misspelled")
        self.assertEqual(result.returncode, 2)
        self.assertIn("degoo-rc help", result.stderr)
        self.assertEqual(self.run_cli("stop", "unexpected").returncode, 2)

    def test_backup_config_dispatch_creates_portable_configuration_only(self):
        result = self.run_cli("install", "--binary", str(self.binary), "--folder-id", "test-vault-id", "--no-wizard")
        self.assertEqual(result.returncode, 0, result.stderr)
        destination = self.home / "portable configuration"
        result = self.run_cli("backup-config", "--directory", str(destination))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((destination / "config/vault.json").is_file())
        self.assertEqual(json.loads((destination / "install.json").read_text())["root_folder_id"], "test-vault-id")
        self.assertEqual(set(path.name for path in destination.iterdir()), {"config", "install.json"})
        secret = json.loads((destination / "config/vault.json").read_text())["password"]
        self.assertNotIn(secret, result.stdout + result.stderr)
        self.assertIn("excluded", result.stdout.lower())

    def test_installed_path_link_dispatches_mount_flags_without_splitting(self):
        self.install()
        link = self.home / ".local/bin/degoo-rc"
        self.assertTrue(link.is_symlink())
        result = self.run_cli("mount", "--log-file", "a path with spaces", launcher=link)
        self.assertEqual(result.returncode, 0, result.stderr)
        args = json.loads(result.stdout)
        self.assertIn(str(self.root / "config/vault.json"), args)
        self.assertIn(str(self.root / "mount"), args)
        self.assertNotIn("--sync-socket", args)
        self.assertEqual(args[-2:], ["--log-file", "a path with spaces"])
        self.assertTrue((self.root / "scripts/mount.sh").is_file())
        self.assertIn("degoo-rc", self.run_cli("help", launcher=link).stdout)
        result = subprocess.run(
            ["degoo-rc", "help"],
            env=dict(self.env, PATH=f"{link.parent}:/usr/bin:/bin"),
            capture_output=True, text=True, check=False, timeout=30,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("empty-trash", result.stdout)
        self.assertEqual(list(self.root.glob("*.sh")), [])

    def test_mount_enables_sync_socket_when_binary_supports_it(self):
        self.install()
        binary = self.root / "bin/rclone"
        binary.write_text(
            '#!/usr/bin/python3\nimport json, sys\n'
            'if sys.argv[1:] == ["degoo", "mount", "--help"]:\n'
            '    print("--sync-socket PATH")\n'
            'else:\n'
            '    print(json.dumps(sys.argv[1:]))\n', encoding="utf-8"
        )
        result = self.run_cli("mount")
        self.assertEqual(result.returncode, 0, result.stderr)
        args = json.loads(result.stdout)
        self.assertEqual(args[args.index("--sync-socket") + 1], str(self.root / "tmp/vfs-sync.sock"))

    def test_trash_and_empty_trash_preserve_explicit_native_operations(self):
        self.install()
        auth = self.workspace / "auth with spaces.json"
        auth.write_text("{}", encoding="utf-8")
        for command, native, flags in (
            ("trash", "trash", ["--json", "--limit", "7"]),
            ("empty-trash", "empty-trash", ["--dry-run"]),
        ):
            with self.subTest(command=command):
                result = self.run_cli(command, "--auth", str(auth), "--rclone", str(self.binary), *flags)
                self.assertEqual(result.returncode, 0, result.stderr)
                args = json.loads(result.stdout)
                self.assertEqual(args[:3], ["degoo", native, str(auth)])
                for flag in flags:
                    self.assertIn(flag, args)
        self.assertIn("irreversible", self.run_cli("empty-trash", "help").stdout)

    def test_inventory_and_uninstall_via_path_link(self):
        self.install()
        link = self.home / ".local/bin/degoo-rc"
        result = self.run_cli("uninstall", "--inventory", launcher=link)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.root.exists())
        result = self.run_cli("uninstall", "--delete-local", launcher=link)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.root.exists())
        self.assertFalse(os.path.lexists(link))


if __name__ == "__main__":
    unittest.main()
