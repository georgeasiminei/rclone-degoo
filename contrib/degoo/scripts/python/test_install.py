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
            code = install.main(["--binary", str(self.binary), "--mountpoint", "mount", *args])
        return code, stdout.getvalue(), stderr.getvalue()

    def test_new_install_defaults_to_external_mountpoint(self) -> None:
        stdout = io.StringIO()
        stderr = io.StringIO()
        with mock.patch.object(install, "prepare_mountpoint") as prepare_mountpoint:
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = install.main(["--binary", str(self.binary), "--folder-id", "dedicated-folder-123"])

        self.assertEqual(code, 0, stderr.getvalue())
        prepare_mountpoint.assert_called_once_with(Path(install.DEFAULT_MOUNTPOINT))
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["mountpoint"], install.DEFAULT_MOUNTPOINT)
        self.assertEqual(manifest["build"]["version"], 1)
        self.assertEqual(manifest["build"]["binary_sha256"], install._file_sha256(self.root / "bin/rclone"))
        wrapper = (self.root / "scripts/mount.sh").read_text(encoding="utf-8")
        self.assertIn(f"MOUNTPOINT={shlex.quote(install.DEFAULT_MOUNTPOINT)}", wrapper)

    def test_prepare_mountpoint_creates_private_directory(self) -> None:
        mountpoint = self.base / "external" / "degoo"

        install.prepare_mountpoint(mountpoint)

        self.assertTrue(mountpoint.is_dir())
        self.assertEqual(stat.S_IMODE(mountpoint.stat().st_mode), 0o700)

    def test_binary_help_cannot_create_configuration_in_the_real_home(self) -> None:
        self.binary.write_text(
            "#!/bin/sh\nmkdir -p \"$HOME/.config/rclone\"\necho 'degoo login mount'\n",
            encoding="utf-8",
        )
        self.binary.chmod(0o700)
        install.validate_binary(self.binary)
        self.assertFalse((self.home / ".config").exists())

    def test_package_identity_is_stable_and_accepts_matching_clean_build_provenance(self) -> None:
        package = self.base / "package"
        binary = self.base / "rclone"
        self.write_binary(binary, "# package identity fixture\n")
        source_names = {
            Path("degoo-rc"),
            Path("scripts/install.sh"),
            Path("scripts/python/cli.py"),
            Path("scripts/python/update.py"),
            Path("immutable-mount/helper.c"),
        }
        with (
            mock.patch.object(install, "SOURCE_FILES", ("cli.py", "update.py")),
            mock.patch.object(install, "SOURCE_SHELL_FILES", ("install.sh",)),
            mock.patch.object(install, "IMMUTABLE_MOUNT_FILES", ("helper.c",)),
        ):
            for relative in source_names:
                path = package / relative
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(f"fixture:{relative.as_posix()}\n", encoding="utf-8")
            (package / "docs").mkdir()
            commit = "a" * 40
            metadata = {
                "version": 1,
                "binary_sha256": install._file_sha256(binary),
                "commit": commit,
                "source_dirty": False,
            }
            (package / "docs/BUILD.json").write_text(json.dumps(metadata), encoding="utf-8")

            first = install.package_identity(binary, package)
            self.assertEqual(first, install.package_identity(binary, package))
            self.assertEqual(first["binary_sha256"], metadata["binary_sha256"])
            self.assertEqual(first["commit"], commit)
            self.assertRegex(first["payload_sha256"], r"^[0-9a-f]{64}$")

            (package / "scripts/python/update.py").write_text("changed helper\n", encoding="utf-8")
            changed = install.package_identity(binary, package)
            self.assertNotEqual(changed["payload_sha256"], first["payload_sha256"])

            metadata["binary_sha256"] = "0" * 64
            (package / "docs/BUILD.json").write_text(json.dumps(metadata), encoding="utf-8")
            self.assertNotIn("commit", install.package_identity(binary, package))

    def test_mountpoint_permission_instructions_precede_install_mutation(self) -> None:
        mountpoint = self.base / "restricted" / "degoo"
        with mock.patch.object(install, "DEFAULT_MOUNTPOINT", str(mountpoint)), mock.patch.object(
            Path, "mkdir", side_effect=PermissionError("permission denied")
        ):
            stdout = io.StringIO()
            stderr = io.StringIO()
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = install.main(["--binary", str(self.binary)])

        self.assertEqual(code, 2)
        self.assertIn("could not be created or is not owner-private", stderr.getvalue())
        self.assertIn(f"sudo mkdir -p -- {mountpoint}", stderr.getvalue())
        self.assertIn("sudo chown --", stderr.getvalue())
        self.assertIn("sudo chmod 700 --", stderr.getvalue())
        self.assertIn("sudo chattr +i --", stderr.getvalue())
        self.assertFalse(self.root.exists())

    def test_upgrade_without_mountpoint_preserves_legacy_location(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        stdout = io.StringIO()
        stderr = io.StringIO()
        with mock.patch.object(install, "prepare_mountpoint") as prepare_mountpoint:
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = install.main(["--binary", str(self.binary), "--upgrade"])

        self.assertEqual(code, 0, stderr.getvalue())
        prepare_mountpoint.assert_not_called()
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["mountpoint"], "mount")
        wrapper = (self.root / "scripts/mount.sh").read_text(encoding="utf-8")
        self.assertIn(f"MOUNTPOINT={shlex.quote(str(self.root / 'mount'))}", wrapper)

    def test_external_mountpoint_is_not_prepared_when_upgrade_lock_is_rejected(self) -> None:
        external_mountpoint = self.base / "external" / "degoo"
        stdout = io.StringIO()
        stderr = io.StringIO()
        with mock.patch.object(install, "DEFAULT_MOUNTPOINT", str(external_mountpoint)):
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = install.main(["--binary", str(self.binary), "--folder-id", "dedicated-folder-123"])
            self.assertEqual(code, 0, stderr.getvalue())
            self.assertTrue(external_mountpoint.is_dir())

            lock_fd = install._acquire_upgrade_lock(self.root)
            try:
                stdout = io.StringIO()
                stderr = io.StringIO()
                with mock.patch.object(install, "prepare_mountpoint") as prepare_mountpoint:
                    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                        code = install.main(["--binary", str(self.binary), "--upgrade"])

                self.assertEqual(code, 2)
                self.assertIn("cache is active", stderr.getvalue())
                prepare_mountpoint.assert_not_called()
            finally:
                os.close(lock_fd)

    def test_mountpoint_migration_refuses_nonempty_legacy_directory(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        local_file = self.root / "mount" / "b.txt"
        local_file.write_text("pending local data\n", encoding="utf-8")
        external_mountpoint = self.base / "external" / "degoo"
        with mock.patch.object(install, "DEFAULT_MOUNTPOINT", str(external_mountpoint)):
            stdout = io.StringIO()
            stderr = io.StringIO()
            with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                code = install.main(
                    ["--binary", str(self.binary), "--upgrade", "--mountpoint", str(external_mountpoint)]
                )

        self.assertEqual(code, 2)
        self.assertIn("recover local files from the old mountpoint", stderr.getvalue())
        self.assertEqual(local_file.read_text(encoding="utf-8"), "pending local data\n")
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["mountpoint"], "mount")

    def test_complete_install_creates_private_state_and_escaped_wrappers(self) -> None:
        code, stdout, stderr = self.invoke("--folder-id", "dedicated-folder-123", "--folder-name", "Vault")
        self.assertEqual(code, 0, stderr)
        self.assertIn("contained Degoo vault", stdout)
        vault = json.loads((self.root / "config/vault.json").read_text(encoding="utf-8"))
        self.assertEqual(vault["remote"], "degoo-live-test:")
        self.assertEqual(vault["cache_dir"], str(self.root / "cache"))
        self.assertEqual(vault["chunk_size"], "128Mi")
        self.assertGreaterEqual(len(vault["password"]), 40)
        settings = json.loads((self.root / "config/mount.json").read_text(encoding="utf-8"))
        self.assertEqual(
            settings,
            {
                "cache_max_size": "60G",
                "cache_min_free_space": "60G",
                "cache_max_age": "750h",
                "cache_poll_interval": "5s",
                "write_back": "5s",
                "verify_upload": False,
                "buffer_size": "16Mi",
                "read_ahead": "1Mi",
                "read_chunk_size": "128Mi",
                "read_chunk_size_limit": "off",
                "read_chunk_streams": 8,
                "dir_cache_time": "30m",
                "attr_timeout": "1s",
                "transfers": 4,
                "bwlimit": "off",
                "timeout": "5m",
            },
        )
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

    def test_location_and_folders_wrappers_log_to_private_diagnostics(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        capture = self.base / "helper argv"
        fake_binary = self.root / "bin/rclone"
        fake_binary.write_text(
            "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$CAPTURE_ARGS\"\n",
            encoding="ascii",
        )
        fake_binary.chmod(0o700)
        env = {**os.environ, "CAPTURE_ARGS": str(capture)}

        for helper in ("location.sh", "folders.sh"):
            with self.subTest(helper=helper):
                result = subprocess.run(
                    [str(self.root / "scripts" / helper)], env=env, capture_output=True, check=False
                )
                self.assertEqual(result.returncode, 0, result.stderr.decode(errors="replace"))
                args = capture.read_text(encoding="utf-8").splitlines()
                self.assertIn("--log-file", args)
                self.assertEqual(args[args.index("--log-file") + 1], str(self.root / "logs/diagnostics.log"))
                self.assertIn("--log-level", args)
                self.assertEqual(args[args.index("--log-level") + 1], "NOTICE")
                self.assertIn("degoo", args)
                self.assertIn("location" if helper == "location.sh" else "folders", args)

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
        self.assertEqual(manifest["parent_folder_id"], "device-container")
        self.assertTrue(manifest["remote_created_by_setup"])
        self.assertTrue((self.root / "config/vault.json").exists())
        self.assertTrue((self.root / "config/rclone.conf").exists())

    def test_upgrade_preserves_setup_folder_ancestry(self) -> None:
        code, _, stderr = self.invoke()
        self.assertEqual(code, 0, stderr)
        stdout = io.StringIO()
        stderr = io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            result = setup.main(
                ["--root", str(self.root), "--parent-id", "device-container-alpha", "--name", "My Vault"]
            )
        self.assertEqual(result, 0, stderr.getvalue())

        code, _, stderr = self.invoke("--upgrade")

        self.assertEqual(code, 0, stderr)
        manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(manifest["root_folder_id"], "created-folder-123")
        self.assertEqual(manifest["remote_folder_name"], "My Vault")
        self.assertEqual(manifest["parent_folder_id"], "device-container-alpha")

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
        vault = json.loads(vault_path.read_bytes())
        vault["account_credentials"] = "generated-encrypted-credential-envelope"
        vault_path.write_text(json.dumps(vault) + "\n", encoding="utf-8")
        vault_before = vault_path.read_bytes()
        config_before = (self.root / "config/rclone.conf").read_bytes()
        settings_path = self.root / "config/mount.json"
        settings = {
            key: install.DEFAULT_MOUNT_SETTINGS[key]
            for key in ("cache_max_size", "cache_min_free_space", "cache_max_age", "cache_poll_interval", "verify_upload")
        }
        settings["cache_max_age"] = "10s"
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
        original_payload_hash = json.loads((self.root / "install.json").read_text(encoding="utf-8"))["build"]["payload_sha256"]
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
        upgraded_settings = json.loads(settings_path.read_text(encoding="utf-8"))
        self.assertEqual(upgraded_settings["verify_upload"], False)
        self.assertNotIn("write_back", upgraded_settings)
        self.assertNotIn("buffer_size", upgraded_settings)
        self.assertIn('exec "$BASE/scripts/mount.sh" "$@"', legacy_mount.read_text(encoding="utf-8"))
        self.assertEqual(os.readlink(self.home / ".local/bin/degoo-rc"), str(self.root / "degoo-rc"))
        upgraded_manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
        self.assertEqual(upgraded_manifest["path_link"], str(self.home / ".local/bin/degoo-rc"))
        self.assertEqual(upgraded_manifest["build"]["version"], 1)
        self.assertEqual(upgraded_manifest["build"]["binary_sha256"], install._file_sha256(self.root / "bin/rclone"))
        self.assertNotEqual(upgraded_manifest["build"]["payload_sha256"], original_payload_hash)

    def test_mount_settings_require_valid_positive_write_back_duration(self) -> None:
        legacy = {
            key: install.DEFAULT_MOUNT_SETTINGS[key]
            for key in ("cache_max_size", "cache_min_free_space", "cache_max_age", "cache_poll_interval", "verify_upload")
        }
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
        for value in (".5s", "1d", "1w", "1M", "1y"):
            with self.subTest(accepted_write_back=value):
                settings = install.DEFAULT_MOUNT_SETTINGS | {"write_back": value}
                install.validate_mount_settings(json.dumps(settings).encode())

    def test_upgrade_preserves_manually_tuned_mount_settings(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        settings_path = self.root / "config/mount.json"
        settings = install.DEFAULT_MOUNT_SETTINGS | {
            "buffer_size": "32Mi",
            "read_ahead": "2Mi",
            "read_chunk_size": "64Mi",
            "read_chunk_streams": 0,
            "transfers": 6,
            "bwlimit": "10M:20M",
        }
        settings_path.write_text(json.dumps(settings, sort_keys=True) + "\n", encoding="utf-8")
        settings_path.chmod(0o600)
        before = settings_path.read_bytes()
        self.write_binary(self.binary, "# upgraded binary\n")

        code, _, stderr = self.invoke("--upgrade")

        self.assertEqual(code, 0, stderr)
        self.assertEqual(settings_path.read_bytes(), before)

    def test_mount_settings_validate_optional_tuning_values_and_reject_unknown_keys(self) -> None:
        settings = install.DEFAULT_MOUNT_SETTINGS | {
            "buffer_size": "0",
            "read_ahead": "1.5MiB",
            "read_chunk_size": "off",
            "read_chunk_size_limit": "0",
            "read_chunk_streams": 0,
            "dir_cache_time": "0",
            "attr_timeout": "250ms",
            "transfers": 4,
            "bwlimit": "Mon-08:00,10M:2M Tue-18:00,off",
            "timeout": ".5s",
        }
        install.validate_mount_settings(json.dumps(settings).encode())

        invalid_values = (
            ("buffer_size", "many"),
            ("buffer_size", "off"),
            ("read_ahead", "-1Mi"),
            ("read_ahead", "off"),
            ("read_chunk_size", "1MB"),
            ("read_chunk_size_limit", "nope"),
            ("read_chunk_streams", -1),
            ("read_chunk_streams", True),
            ("dir_cache_time", "soon"),
            ("attr_timeout", "-1s"),
            ("transfers", 0),
            ("transfers", "4"),
            ("bwlimit", "Mon-25:00,10M"),
            ("timeout", "a while"),
        )
        for key, value in invalid_values:
            with self.subTest(key=key, value=value):
                invalid = settings | {key: value}
                with self.assertRaises(install.InstallError):
                    install.validate_mount_settings(json.dumps(invalid).encode())

        with self.assertRaises(install.InstallError):
            install.validate_mount_settings(json.dumps(settings | {"experimental": True}).encode())

    def test_upgrade_privateizes_legacy_logs_without_changing_contents(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        log_contents = {
            "mount.log": b"mount bytes\n",
            "login.log": b"login bytes with legacy mode\n",
            "setup.log": b"setup bytes with legacy mode\n",
            "status.log": b"status bytes\n",
            "wizard-folders.log": b"wizard bytes\n",
            "diagnostics.log": b"diagnostic metadata\n",
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

    def test_upgrade_accepts_inherited_lock_without_releasing_parent_lock(self) -> None:
        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        old_binary = (self.root / "bin/rclone").read_bytes()
        self.write_binary(self.binary, "# version inherited lock\n")
        fd = install._acquire_upgrade_lock(self.root)
        try:
            package_sha256 = "b" * 64
            code, _, stderr = self.invoke(
                "--upgrade", "--upgrade-lock-fd", str(fd), "--package-sha256", package_sha256
            )
            self.assertEqual(code, 0, stderr)
            self.assertNotEqual((self.root / "bin/rclone").read_bytes(), old_binary)
            manifest = json.loads((self.root / "install.json").read_text(encoding="utf-8"))
            self.assertEqual(manifest["build"]["package_sha256"], package_sha256)
            with self.assertRaisesRegex(install.InstallError, "cache is active"):
                install._acquire_upgrade_lock(self.root)
        finally:
            os.close(fd)

    def test_upgrade_rejects_invalid_or_unrequested_inherited_lock_fd(self) -> None:
        code, _, stderr = self.invoke("--upgrade-lock-fd", "999")
        self.assertEqual(code, 2)
        self.assertIn("only be used with --upgrade", stderr)

        code, _, stderr = self.invoke("--package-sha256", "not-a-digest")
        self.assertEqual(code, 2)
        self.assertIn("lowercase SHA-256 digest", stderr)

        code, _, stderr = self.invoke("--folder-id", "dedicated-folder-123")
        self.assertEqual(code, 0, stderr)
        cache_lock_fd = install._acquire_upgrade_lock(self.root)
        os.close(cache_lock_fd)
        wrong_lock = self.base / "wrong-lock"
        wrong_lock.write_text("not this cache lock\n", encoding="ascii")
        wrong_lock.chmod(0o600)
        fd = os.open(wrong_lock, os.O_RDWR)
        try:
            code, _, stderr = self.invoke("--upgrade", "--upgrade-lock-fd", str(fd))
            self.assertEqual(code, 2)
            self.assertIn("does not refer to this install's cache lock", stderr)
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
            "account_credentials": "legacy-encrypted-account-envelope",
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
        self.assertNotIn("account_credentials", imported, "importing a vault always requires a fresh account login")
        saved_source = json.loads(backup.read_text(encoding="utf-8"))
        self.assertEqual(saved_source["cache_dir"], "/old/machine/cache")
        self.assertEqual(saved_source["account_credentials"], "legacy-encrypted-account-envelope")


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
        vault_path = self.root / "config/vault.json"
        vault_path.write_text("{}\n", encoding="ascii")
        vault_path.chmod(0o600)
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
        self.assertEqual(command[command.index("--vault-config") + 1], str(vault_path))
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
            self.assertEqual(install.main(["--binary", str(self.binary), "--folder-id", "service-test-folder", "--mountpoint", "mount"]), 0)
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
