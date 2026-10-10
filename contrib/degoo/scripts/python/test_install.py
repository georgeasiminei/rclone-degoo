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
        for path in (self.root, self.root / "config", self.root / "cache", self.root / "bin/rclone", self.root / "mount.sh"):
            self.assertEqual(stat.S_IMODE(path.stat().st_mode) & 0o077, 0)
        mount_wrapper = (self.root / "mount.sh").read_text(encoding="utf-8")
        self.assertIn("--settings", mount_wrapper)
        self.assertIn(f"ROOT={shlex.quote(str(self.root))}", mount_wrapper)
        self.assertIn("$ROOT/tools:/usr/bin:/bin", mount_wrapper)
        self.assertTrue((self.root / "scripts/python/install.py").is_file())
        self.assertFalse((self.root / "install.py").exists())
        self.assertIn("scripts/python/login.py", (self.root / "login.sh").read_text())
        for wrapper in self.root.glob("*.sh"):
            result = subprocess.run(["/bin/sh", "-n", str(wrapper)], check=False, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, f"{wrapper.name}: {result.stderr}")
        unit = (self.root / "rclone-degoo.service").read_text(encoding="utf-8")
        self.assertIn("StartLimitIntervalSec=0", unit.split("[Service]", 1)[0])
        self.assertIn("RestartSec=30s", unit)
        self.assertIn("WantedBy=default.target", unit)

    def test_systemd_accepts_unit_paths_with_spaces(self) -> None:
        analyzer = shutil.which("systemd-analyze")
        if not analyzer:
            self.skipTest("systemd-analyze is not installed")
        root = self.base / "systemd path with spaces %"
        root.mkdir(mode=0o700)
        for name in ("mount.sh", "stop.sh"):
            path = root / name
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

    def test_staged_install_defers_vault_secret_until_folder_setup(self) -> None:
        code, stdout, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        self.assertIn("no vault folder is configured yet", stdout)
        self.assertFalse((self.root / "config/vault.json").exists())
        self.assertFalse((self.root / "config/rclone.conf").exists())
        self.assertTrue((self.root / "login.sh").exists())
        self.assertTrue((self.root / "setup.sh").exists())

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

    def test_upgrade_preserves_vault_login_remote_settings_and_cache(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        vault_path = self.root / "config/vault.json"
        vault_before = vault_path.read_bytes()
        config_before = (self.root / "config/rclone.conf").read_bytes()
        settings_path = self.root / "config/mount.json"
        settings = install.DEFAULT_MOUNT_SETTINGS | {"cache_max_age": "10s", "verify_upload": False}
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
