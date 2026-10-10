"""Offline checks for contained installation, setup, login, and service controls."""

from __future__ import annotations

import contextlib
import io
import json
import os
import shutil
import shlex
import stat
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import info
import install
import login
import service
import setup


class InstallTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home with spaces ' $percent%"
        self.home.mkdir(mode=0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home)}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.root = self.home / ".local/share/rclone-degoo"
        self.binary = self.base / "custom rclone"
        self.write_binary(self.binary, "# version one\n")

    @staticmethod
    def write_binary(path: Path, extra: str = "") -> None:
        path.write_text(
            "#!/bin/sh\n"
            "case \"$*\" in\n"
            "  *'degoo --help'*) echo 'Commands: login mount status folders create-vault location'; exit 0;;\n"
            "esac\n"
            "for arg in \"$@\"; do\n"
            "  if [ \"$arg\" = create-vault ]; then echo created-folder-123; exit 0; fi\n"
            "done\n"
            + extra
            + "exit 0\n",
            encoding="utf-8",
        )
        path.chmod(0o700)

    def invoke(self, *args: str) -> tuple[int, str, str]:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            code = install.main(["--binary", str(self.binary), *args])
        return code, stdout.getvalue(), stderr.getvalue()

    def test_complete_install_creates_private_state_and_escaped_wrappers(self) -> None:
        code, stdout, stderr = self.invoke("--folder-id", "dedicated-folder-123", "--folder-name", "Vault")
        self.assertEqual(code, 0, stderr)
        self.assertIn("contained Degoo vault", stdout)
        vault = json.loads((self.root / "config/vault.json").read_text(encoding="utf-8"))
        self.assertEqual(vault["remote"], "degoo-live-test:")
        self.assertEqual(vault["cache_dir"], str(self.root / "cache"))
        self.assertGreaterEqual(len(vault["password"]), 40)
        settings = json.loads((self.root / "config/mount.json").read_text(encoding="utf-8"))
        self.assertEqual(settings, install.DEFAULT_MOUNT_SETTINGS)
        config = (self.root / "config/rclone.conf").read_text(encoding="utf-8")
        self.assertIn("root_folder_id = dedicated-folder-123", config)
        self.assertIn(f"auth_file = {self.root / 'config/auth.json'}", config)
        for path in (self.root, self.root / "config", self.root / "cache", self.root / "bin/rclone", self.root / "scripts/mount.sh", self.root / "degoo-rc"):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode) & 0o077, 0)
        mount_wrapper = (self.root / "scripts/mount.sh").read_text(encoding="utf-8")
        self.assertIn("--settings", mount_wrapper)
        self.assertIn(f"ROOT={shlex.quote(str(self.root))}", mount_wrapper)
        self.assertIn("$ROOT/tools:/usr/bin:/bin", mount_wrapper)
        self.assertTrue((self.root / "scripts/python/install.py").is_file())
        self.assertTrue((self.root / "scripts/python/cli.py").is_file())
        self.assertTrue((self.root / "scripts/install.sh").is_file())
        self.assertTrue((self.root / "scripts/trash.sh").is_file())
        self.assertEqual(os.readlink(self.home / ".local/bin/degoo-rc"), str(self.root / "degoo-rc"))
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["path_link"], str(self.home / ".local/bin/degoo-rc"))
        self.assertFalse((self.root / "install.py").exists())
        self.assertEqual(list(self.root.glob("*.sh")), [])
        self.assertIn("scripts/python/login.py", (self.root / "scripts/login.sh").read_text())
        for wrapper in (self.root / "scripts").glob("*.sh"):
            result = subprocess.run(["/bin/sh", "-n", str(wrapper)], check=False, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, f"{wrapper.name}: {result.stderr}")
        unit = (self.root / "rclone-degoo.service").read_text(encoding="utf-8")
        self.assertIn("StartLimitIntervalSec=0", unit.split("[Service]", 1)[0])
        self.assertIn(f"ExecStart={install.unit_quote(str(self.root / 'scripts/mount.sh'))}", unit)
        self.assertIn("RestartSec=30s", unit)
        self.assertIn("WantedBy=default.target", unit)

    def test_install_refuses_unrelated_command_at_path_destination(self) -> None:
        bin_dir = self.home / ".local/bin"
        bin_dir.mkdir(parents=True, mode=0o700)
        (self.home / ".local").chmod(0o700)
        bin_dir.chmod(0o700)
        destination = bin_dir / "degoo-rc"
        destination.write_text("keep this command\n", encoding="utf-8")
        destination.chmod(0o700)

        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")

        self.assertEqual(code, 2)
        self.assertIn("already exists and is not owned by this install", stderr)
        self.assertEqual(destination.read_text(encoding="utf-8"), "keep this command\n")
        self.assertFalse(self.root.exists())

    def test_install_prints_copyable_path_export_without_editing_profiles(self) -> None:
        with mock.patch.dict(os.environ, {"PATH": "/usr/bin:/bin"}):
            code, stdout, stderr = self.invoke()

        self.assertEqual(code, 0, stderr)
        self.assertIn('export PATH="$HOME/.local/bin:$PATH"', stdout)
        self.assertFalse((self.home / ".profile").exists())

    def test_install_omits_path_export_when_local_bin_is_already_in_path(self) -> None:
        local_bin = self.home / ".local/bin"
        with mock.patch.dict(os.environ, {"PATH": f"/usr/bin:{local_bin}:/bin"}):
            code, stdout, stderr = self.invoke()

        self.assertEqual(code, 0, stderr)
        self.assertNotIn('export PATH="$HOME/.local/bin:$PATH"', stdout)

    def test_mount_wrapper_defaults_to_notice_and_allows_later_info_override(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        capture = self.base / "mount argv"
        fake_binary = self.root / "bin/rclone"
        fake_binary.write_text(
            "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_ARGS\"\n",
            encoding="ascii",
        )
        fake_binary.chmod(0o700)
        env = {**os.environ, "CAPTURE_ARGS": str(capture)}

        result = subprocess.run([str(self.root / "scripts/mount.sh")], env=env, capture_output=True, check=False)
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        args = capture.read_text(encoding="utf-8").splitlines()
        levels = [args[index + 1] for index, value in enumerate(args[:-1]) if value == "--log-level"]
        self.assertEqual(levels, ["NOTICE"])

        result = subprocess.run(
            [str(self.root / "scripts/mount.sh"), "--log-level", "INFO"],
            env=env,
            capture_output=True,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
        args = capture.read_text(encoding="utf-8").splitlines()
        levels = [args[index + 1] for index, value in enumerate(args[:-1]) if value == "--log-level"]
        self.assertEqual(levels, ["NOTICE", "INFO"])

    def test_stop_wrapper_only_skips_unmount_for_mountpoint_exit_32(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        calls = self.base / "stop calls"
        tools = self.root / "tools"
        mountpoint = tools / "mountpoint"
        mountpoint.write_text(
            "#!/bin/sh\nprintf '%s\\n' mountpoint >> \"$CALLS_FILE\"\nexit \"$MOUNTPOINT_EXIT\"\n",
            encoding="ascii",
        )
        mountpoint.chmod(0o700)
        fusermount = tools / "fusermount3"
        if fusermount.is_symlink():
            fusermount.unlink()
        fusermount.write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' fusermount3 >> \"$CALLS_FILE\"\n"
            "printf '%s\\n' \"$@\" >> \"$CALLS_FILE\"\n"
            "exit \"$UNMOUNT_EXIT\"\n",
            encoding="ascii",
        )
        fusermount.chmod(0o700)

        cases = (
            (
                "disconnected mount, busy unmount",
                "1",
                "1",
                1,
                ["mountpoint", "fusermount3", "-u", str(self.root / "mount"), "mountpoint"],
            ),
            ("confirmed unmounted", "32", "1", 0, ["mountpoint"]),
            (
                "normal unmount succeeds",
                "0",
                "0",
                0,
                ["mountpoint", "fusermount3", "-u", str(self.root / "mount")],
            ),
            (
                "active mount unmount fails",
                "0",
                "1",
                1,
                ["mountpoint", "fusermount3", "-u", str(self.root / "mount"), "mountpoint"],
            ),
        )
        for label, mountpoint_exit, unmount_exit, expected_code, expected_calls in cases:
            with self.subTest(case=label):
                calls.unlink(missing_ok=True)
                env = {
                    **os.environ,
                    "CALLS_FILE": str(calls),
                    "MOUNTPOINT_EXIT": mountpoint_exit,
                    "UNMOUNT_EXIT": unmount_exit,
                }
                result = subprocess.run(
                    [str(self.root / "scripts/stop.sh")], env=env, capture_output=True, check=False
                )
                self.assertEqual(result.returncode, expected_code, result.stderr.decode(errors="replace"))
                self.assertEqual(calls.read_text(encoding="ascii").splitlines(), expected_calls)

    def test_systemd_accepts_unit_paths_with_spaces(self) -> None:
        analyzer = shutil.which("systemd-analyze")
        if not analyzer:
            self.skipTest("systemd-analyze is not installed")
        root = self.base / "systemd path with spaces %"
        root.mkdir(mode=0o700)
        for name in ("mount.sh", "stop.sh"):
            path = root / "scripts" / name
            path.parent.mkdir(exist_ok=True)
            path.write_text("#!/bin/sh\nexit 0\n", encoding="ascii")
            path.chmod(0o700)
        unit = root / "rclone-degoo.service"
        unit.write_bytes(install.service_unit(root))
        unit.chmod(0o600)
        result = subprocess.run(
            [analyzer, "verify", str(unit)],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stdout.decode("utf-8", errors="replace"))

    def test_setup_rejects_path_and_internal_folder_names(self) -> None:
        for name in ("../outside", "one/two", ".rclone-hidden", ".."):
            with self.subTest(name=name), self.assertRaises(setup.SetupError):
                setup._validate_name(name)

    def test_zero_or_missing_identity_rejected_before_creating_root(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "0")
        self.assertEqual(code, 2)
        self.assertIn("cannot be 0", stderr)
        self.assertFalse(self.root.exists())

    def test_shared_install_parent_error_names_the_path_and_fix(self) -> None:
        parent = self.home / ".local"
        parent.mkdir(mode=0o775)
        parent.chmod(0o775)
        code, _, stderr = self.invoke()
        self.assertEqual(code, 2)
        self.assertIn(str(parent), stderr)
        self.assertIn("chmod go-w", stderr)
        self.assertFalse(self.root.exists())

    def test_unsafe_local_bin_is_rejected_before_prompts_with_fix_command(self) -> None:
        local = self.home / ".local"
        local.mkdir(mode=0o755)
        local_bin = local / "bin"
        local_bin.mkdir(mode=0o775)
        local_bin.chmod(0o775)
        code, _, stderr = self.invoke()
        self.assertEqual(code, 2)
        self.assertIn(str(local_bin), stderr)
        self.assertIn("chmod go-w", stderr)
        self.assertIn("group or other", stderr)
        self.assertFalse(self.root.exists())

    def test_staged_install_defers_vault_secret_until_folder_setup(self) -> None:
        code, stdout, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        self.assertIn("no vault folder is configured yet", stdout)
        self.assertFalse((self.root / "config/vault.json").exists())
        self.assertFalse((self.root / "config/rclone.conf").exists())
        self.assertTrue((self.root / "scripts/login.sh").exists())
        self.assertTrue((self.root / "scripts/setup.sh").exists())

    def test_setup_creates_remote_identity_and_finishes_staged_install(self) -> None:
        code, _, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = setup.main(["--root", str(self.root), "--parent-id", "device-container", "--name", "My Vault"])
        self.assertEqual(result, 0, stderr.getvalue())
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["root_folder_id"], "created-folder-123")
        self.assertEqual(manifest["remote_folder_name"], "My Vault")
        self.assertTrue(manifest["remote_created_by_setup"])
        self.assertTrue((self.root / "config/vault.json").exists())
        self.assertTrue((self.root / "config/rclone.conf").exists())

    def test_setup_uses_private_umask_for_logged_command(self) -> None:
        code, _, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        result = subprocess.CompletedProcess([], 0, stdout=b"created-folder-123\n", stderr=b"")
        with mock.patch.object(setup.subprocess, "run", return_value=result) as run:
            self.assertEqual(setup._run_create(self.root, "device-container", "My Vault"), "created-folder-123")
        self.assertEqual(run.call_args.kwargs["umask"], 0o077)

    def test_upgrade_preserves_vault_login_remote_settings_and_cache(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        vault_path = self.root / "config/vault.json"
        vault_before = vault_path.read_bytes()
        config_before = (self.root / "config/rclone.conf").read_bytes()
        settings_path = self.root / "config/mount.json"
        settings = install.DEFAULT_MOUNT_SETTINGS | {"cache_max_age": "10s", "verify_upload": False}
        settings.pop("write_back")
        settings_path.write_text(json.dumps(settings, sort_keys=True) + "\n", encoding="utf-8")
        settings_path.chmod(0o600)
        settings_before = settings_path.read_bytes()
        auth_path = self.root / "config/auth.json"
        auth_path.write_text('{"refresh_token":"test-token"}\n', encoding="utf-8")
        auth_path.chmod(0o600)
        auth_before = auth_path.read_bytes()
        cache_file = self.root / "cache/pending-upload"
        cache_file.write_bytes(b"pending local data")
        cache_before = cache_file.read_bytes()
        (self.root / "config/login.json").write_text('{"email":"owner@example.test"}\n', encoding="utf-8")
        (self.root / "config/login.json").chmod(0o600)
        self.write_binary(self.binary, "# version two\n")
        legacy_helper = self.root / "install.py"
        legacy_helper.write_text("# old installer layout\n", encoding="utf-8")
        legacy_helper.chmod(0o700)
        legacy_mount = self.root / "mount.sh"
        legacy_mount.write_bytes(install.shell_wrapper("mount.sh", self.root))
        legacy_mount.chmod(0o700)

        code, _, stderr = self.invoke("--upgrade")
        self.assertEqual(code, 0, stderr)
        self.assertFalse(legacy_helper.exists())
        self.assertTrue((self.root / "scripts/python/install.py").is_file())
        self.assertEqual(vault_path.read_bytes(), vault_before)
        self.assertEqual((self.root / "config/rclone.conf").read_bytes(), config_before)
        self.assertEqual(settings_path.read_bytes(), settings_before)
        self.assertEqual(auth_path.read_bytes(), auth_before)
        self.assertEqual(cache_file.read_bytes(), cache_before)
        self.assertEqual((self.root / "config/login.json").read_text(encoding="utf-8"), '{"email":"owner@example.test"}\n')
        self.assertEqual(json.loads(settings_path.read_text(encoding="utf-8"))["verify_upload"], False)
        self.assertNotIn("write_back", json.loads(settings_path.read_text(encoding="utf-8")))
        self.assertIn('exec "$BASE/scripts/mount.sh" "$@"', legacy_mount.read_text(encoding="utf-8"))
        self.assertEqual(os.readlink(self.home / ".local/bin/degoo-rc"), str(self.root / "degoo-rc"))
        self.assertEqual(json.loads((self.root / "install.json").read_text(encoding="utf-8"))["path_link"], str(self.home / ".local/bin/degoo-rc"))

    def test_mount_settings_require_valid_positive_write_back_duration(self) -> None:
        legacy = install.DEFAULT_MOUNT_SETTINGS.copy()
        legacy.pop("write_back")
        install.validate_mount_settings(json.dumps(legacy).encode())

        for value in ("0s", "-1s", "off", "1S", "9223372036854775807ns", "9223372036854775808ns"):
            with self.subTest(value=value):
                settings = install.DEFAULT_MOUNT_SETTINGS | {"write_back": value}
                with self.assertRaises(install.InstallError):
                    install.validate_mount_settings(json.dumps(settings).encode())

        settings = install.DEFAULT_MOUNT_SETTINGS | {"write_back": "1h30m"}
        install.validate_mount_settings(json.dumps(settings).encode())
        settings = install.DEFAULT_MOUNT_SETTINGS | {"write_back": "9223372036854775806ns"}
        install.validate_mount_settings(json.dumps(settings).encode())

    def test_upgrade_privateizes_legacy_logs_without_changing_contents(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        log_contents = {
            "mount.log": b"mount bytes\n",
            "login.log": b"login bytes with legacy mode\n",
            "setup.log": b"setup bytes with legacy mode\n",
            "status.log": b"status bytes\n",
            "wizard-folders.log": b"wizard bytes\n",
        }
        for name, content in log_contents.items():
            path = self.root / "logs" / name
            path.write_bytes(content)
            path.chmod(0o640 if name in {"login.log", "setup.log", "wizard-folders.log"} else 0o600)

        code, _, stderr = self.invoke("--upgrade")

        self.assertEqual(code, 0, stderr)
        for name, content in log_contents.items():
            path = self.root / "logs" / name
            self.assertEqual(path.read_bytes(), content)
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)

    def test_upgrade_rejects_unsafe_log_before_binary_or_other_log_mutation(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        binary_before = (self.root / "bin/rclone").read_bytes()
        legacy_log = self.root / "logs/login.log"
        legacy_log.write_bytes(b"legacy private bytes\n")
        legacy_log.chmod(0o640)
        outside = self.base / "outside.log"
        outside.write_bytes(b"do not follow\n")
        outside.chmod(0o600)
        (self.root / "logs/setup.log").symlink_to(outside)
        self.write_binary(self.binary, "# version two\n")

        code, _, stderr = self.invoke("--upgrade")

        self.assertEqual(code, 2)
        self.assertIn("log", stderr)
        self.assertEqual((self.root / "bin/rclone").read_bytes(), binary_before)
        self.assertEqual(stat.S_IMODE(legacy_log.stat().st_mode), 0o640)
        self.assertEqual(os.readlink(self.root / "logs/setup.log"), str(outside))

    def test_log_normalization_rejects_special_files_and_wrong_owners(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        fifo = self.root / "logs/status.log"
        os.mkfifo(fifo, 0o600)
        with self.assertRaises(install.InstallError):
            install.normalize_legacy_logs(self.root / "logs")
        fifo.unlink()

        log_path = self.root / "logs/login.log"
        log_path.write_bytes(b"owned by another uid\n")
        log_path.chmod(0o640)
        real_lstat = os.lstat

        def wrong_owner_lstat(path: Path) -> os.stat_result:
            result = real_lstat(path)
            if path == log_path:
                fields = list(result)
                fields[4] += 1
                return os.stat_result(fields)
            return result

        with mock.patch.object(install.os, "lstat", side_effect=wrong_owner_lstat):
            with self.assertRaises(install.InstallError):
                install.normalize_legacy_logs(self.root / "logs")
        self.assertEqual(stat.S_IMODE(log_path.stat().st_mode), 0o640)

    def test_upgrade_refuses_a_different_folder_or_active_cache(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        code, _, stderr = self.invoke("--upgrade", "--folder-id", "another-folder")
        self.assertEqual(code, 2)
        self.assertIn("differs from this cache", stderr)

        fd = install._acquire_upgrade_lock(self.root)
        try:
            code, _, stderr = self.invoke("--upgrade")
            self.assertEqual(code, 2)
            self.assertIn("cache is active", stderr)
        finally:
            os.close(fd)

    def test_mounted_upgrade_refusal_gives_service_or_manual_stop_command(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        binary_before = (self.root / "bin/rclone").read_bytes()
        vault_before = (self.root / "config/vault.json").read_bytes()
        cache_file = self.root / "cache/pending-upload"
        cache_file.write_bytes(b"pending local data")

        for service_active, wrapper in ((False, "stop.sh"), (True, "service.sh")):
            with self.subTest(wrapper=wrapper), mock.patch.object(
                install.os.path, "ismount", side_effect=[False, True]
            ), mock.patch.object(install, "_user_service_active", return_value=service_active):
                code, _, stderr = self.invoke("--upgrade")

            self.assertEqual(code, 2)
            self.assertIn("stop the contained mount before upgrading", stderr)
            command = shlex.quote(str(self.root / "scripts" / wrapper))
            if wrapper == "service.sh":
                command += " stop"
            self.assertIn(command, stderr)
            self.assertEqual((self.root / "bin/rclone").read_bytes(), binary_before)
            self.assertEqual((self.root / "config/vault.json").read_bytes(), vault_before)
            self.assertEqual(cache_file.read_bytes(), b"pending local data")

    def test_upgrade_stop_hint_uses_active_service_states(self) -> None:
        for state, expected in (
            ("enabled; active", True),
            ("disabled; activating", True),
            ("enabled; deactivating", True),
            ("enabled; reloading", True),
            ("disabled; inactive", False),
            ("disabled; failed", False),
            ("unknown (user systemd manager is unavailable)", False),
        ):
            with self.subTest(state=state), mock.patch.object(info, "service_status", return_value=state) as status:
                self.assertEqual(install._user_service_active(self.root), expected)
                status.assert_called_once_with(self.root, {})

    def test_imported_vault_secret_is_adapted_to_local_cache(self) -> None:
        backup = self.home / "vault backup.json"
        backup.write_text(json.dumps({
            "version": 1,
            "remote": "old-machine:",
            "password": "kept-vault-secret",
            "password_salt": "independent-salt",
            "cache_dir": "/old/machine/cache",
            "chunk_size": "512Mi",
            "max_object_size": "18Gi",
        }), encoding="utf-8")
        backup.chmod(0o600)
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123", "--vault-config", str(backup))
        self.assertEqual(code, 0, stderr)
        imported = json.loads((self.root / "config/vault.json").read_text(encoding="utf-8"))
        self.assertEqual(imported["password"], "kept-vault-secret")
        self.assertEqual(imported["password_salt"], "independent-salt")
        self.assertEqual(imported["remote"], "degoo-live-test:")
        self.assertEqual(imported["cache_dir"], str(self.root / "cache"))
        self.assertEqual(imported["chunk_size"], "512Mi")
        self.assertEqual(json.loads(backup.read_text(encoding="utf-8"))["cache_dir"], "/old/machine/cache")


class LoginTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "contained root"
        for name in ("bin", "config", "cache", "tmp", "logs"):
            path = self.root / name
            path.mkdir(parents=True, mode=0o700)
            path.chmod(0o700)
        binary = self.root / "bin/rclone"
        binary.write_text("#!/bin/sh\nexit 0\n", encoding="ascii")
        binary.chmod(0o700)
        (self.root / "config/rclone.conf").write_text("", encoding="ascii")
        (self.root / "config/rclone.conf").chmod(0o600)
        self.root.chmod(0o700)

    def test_cached_email_skips_python_email_prompt_and_go_gets_cached_flag(self) -> None:
        email_path = self.root / "config/login.json"
        email_path.write_text('{"email":"owner@example.test"}\n', encoding="ascii")
        email_path.chmod(0o600)
        result = type("Result", (), {"returncode": 0})()
        with mock.patch("builtins.input", side_effect=AssertionError("cached email must skip the prompt")), mock.patch.object(login.subprocess, "run", return_value=result) as run:
            self.assertEqual(login.run(self.root, clear_email=False), 0)
        command = run.call_args.args[0]
        self.assertIn("--email", command)
        self.assertIn("owner@example.test", command)
        self.assertIn("--cached-email", command)
        self.assertNotIn("test-password", command)
        self.assertEqual(run.call_args.kwargs["umask"], 0o077)

    def test_first_email_is_cached_and_clear_only_removes_email(self) -> None:
        auth_path = self.root / "config/auth.json"
        auth_path.write_text('{"refresh_token":"token-to-keep"}\n', encoding="ascii")
        auth_path.chmod(0o600)
        with mock.patch("builtins.input", return_value="new@example.test"), mock.patch.object(
            login.subprocess, "run", return_value=type("Result", (), {"returncode": 0})()
        ):
            self.assertEqual(login.run(self.root, clear_email=False), 0)
        cached = self.root / "config/login.json"
        self.assertEqual(json.loads(cached.read_text(encoding="ascii")), {"email": "new@example.test"})
        self.assertEqual(stat.S_IMODE(cached.stat().st_mode), 0o600)
        self.assertEqual(login.run(self.root, clear_email=True), 0)
        self.assertFalse(cached.exists())
        self.assertEqual(auth_path.read_text(encoding="ascii"), '{"refresh_token":"token-to-keep"}\n')


class ServiceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.base = Path(self.temp.name)
        self.home = self.base / "home"
        self.home.mkdir(mode=0o700)
        self.env = mock.patch.dict(os.environ, {"HOME": str(self.home), "XDG_CONFIG_HOME": str(self.home / ".config")}, clear=False)
        self.env.start()
        self.addCleanup(self.env.stop)
        self.binary = self.base / "rclone"
        InstallTest.write_binary(self.binary)
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(install.main(["--binary", str(self.binary), "--folder-id", "service-test-folder"]), 0)
        self.root = self.home / ".local/share/rclone-degoo"
        self.unit_link = self.home / ".config/systemd/user/rclone-degoo.service"
        self.wanted_link = self.home / ".config/systemd/user/default.target.wants/rclone-degoo.service"
        self.calls: list[list[str]] = []

    def fake_systemctl(self, command: list[str], **kwargs: object):
        args = command[2:]
        self.calls.append(args)
        stdout = b""
        code = 0
        if args[0] == "link":
            self.unit_link.parent.mkdir(parents=True, exist_ok=True)
            if not self.unit_link.exists():
                self.unit_link.symlink_to(args[1])
        elif args[0] == "show":
            stdout = str(self.unit_link).encode()
        elif args[0] == "enable":
            self.wanted_link.parent.mkdir(parents=True, exist_ok=True)
            if not self.wanted_link.exists():
                self.wanted_link.symlink_to(self.root / "rclone-degoo.service")
        elif args[0] == "disable":
            for path in (self.wanted_link, self.unit_link):
                if path.is_symlink():
                    path.unlink()
        elif args[0] == "status":
            stdout = b"active\n"
        return service.subprocess.CompletedProcess(command, code, stdout=stdout, stderr=b"")

    def test_enable_stop_restart_status_and_disable_touch_only_owned_links(self) -> None:
        unrelated = self.home / ".config/systemd/user/other.service"
        unrelated.parent.mkdir(parents=True, exist_ok=True)
        for directory in (self.home / ".config", self.home / ".config/systemd", unrelated.parent):
            directory.chmod(0o700)
        unrelated.write_text("keep", encoding="ascii")
        with mock.patch.object(service.subprocess, "run", side_effect=self.fake_systemctl):
            self.assertEqual(service.run(self.root, "enable"), 0)
            self.assertTrue(self.unit_link.is_symlink())
            self.assertTrue(self.wanted_link.is_symlink())
            manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
            self.assertEqual(set(manifest["service_links"]), {str(self.unit_link), str(self.wanted_link)})
            self.assertEqual(service.run(self.root, "stop"), 0)
            self.assertEqual(service.run(self.root, "start"), 0)
            self.assertEqual(service.run(self.root, "restart"), 0)
            self.assertEqual(service.run(self.root, "status"), 0)
            self.assertEqual(service.run(self.root, "disable"), 0)
        self.assertFalse(self.unit_link.exists())
        self.assertFalse(self.wanted_link.exists())
        self.assertEqual(unrelated.read_text(encoding="ascii"), "keep")
        self.assertEqual(json.loads((self.root / "install.json").read_text(encoding="utf-8"))["service_links"], [])

    def test_enable_refuses_preexisting_unowned_service_link(self) -> None:
        self.unit_link.parent.mkdir(parents=True, exist_ok=True)
        for directory in (self.home / ".config", self.home / ".config/systemd", self.unit_link.parent):
            directory.chmod(0o700)
        other_unit = self.base / "other.service"
        other_unit.write_text("[Service]\n", encoding="ascii")
        self.unit_link.symlink_to(other_unit)
        with mock.patch.object(service.subprocess, "run", side_effect=self.fake_systemctl):
            with self.assertRaises(service.ServiceError):
                service.run(self.root, "enable")
        self.assertEqual(os.readlink(self.unit_link), str(other_unit))


if __name__ == "__main__":
    unittest.main()
