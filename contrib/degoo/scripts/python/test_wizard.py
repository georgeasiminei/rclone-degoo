"""Offline checks for the interactive Degoo install wizard."""

from __future__ import annotations

import json
import contextlib
import io
import os
import shlex
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import wizard


class WizardTest(unittest.TestCase):
    def test_default_binary_uses_distribution_bin_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "bin/rclone"
            binary.parent.mkdir()
            binary.write_text("#!/bin/sh\nexit 0\n")
            binary.chmod(0o755)
            with (
                mock.patch.object(wizard, "_source_repo", return_value=None),
                mock.patch.object(wizard, "_source_roots", return_value=[root]),
                mock.patch.object(wizard.installer, "expected_root", return_value=root / "installed"),
            ):
                self.assertEqual(wizard._default_binary(), binary)

    def test_default_binary_prefers_source_build_directory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            binary = root / "build/rclone"
            binary.parent.mkdir()
            for candidate in (binary, root / "rclone"):
                candidate.write_text("#!/bin/sh\nexit 0\n")
                candidate.chmod(0o755)
            with (
                mock.patch.object(wizard, "_source_repo", return_value=root),
                mock.patch.object(wizard, "_source_roots", return_value=[root]),
            ):
                self.assertEqual(wizard._default_binary(), binary)

    def test_rclone_size_suffixes_and_encrypted_object_bounds(self) -> None:
        self.assertEqual(wizard._size_bytes("1Gi"), 1024**3)
        self.assertEqual(wizard._size_bytes("512MiB"), 512 * 1024**2)
        self.assertEqual(wizard._size_bytes("1B"), 1)
        self.assertEqual(wizard._size_bytes("1"), 1024)
        with self.assertRaises(wizard.WizardError):
            wizard._size_bytes("1GB")

        answers = iter(("1Gi", "19Gi"))
        sizes = wizard._new_vault_sizes(None, input_fn=lambda _: next(answers))
        self.assertEqual(sizes, {"chunk_size": "1Gi", "max_object_size": "19Gi"})

        answers = iter(("1Gi", "1Gi"))
        with self.assertRaises(wizard.WizardError):
            wizard._new_vault_sizes(None, input_fn=lambda _: next(answers))

    def test_imported_vault_size_parameters_are_not_prompted_over(self) -> None:
        self.assertIsNone(wizard._new_vault_sizes({"password": "private"}, input_fn=lambda _: self.fail("unexpected prompt")))

    def test_folder_listing_filters_to_selectable_non_root_folders(self) -> None:
        response = json.dumps(
            [
                {"id": "0", "name": "Account", "category": 1},
                {"id": "device", "name": "Device", "category": 1},
                {"id": "folder", "name": "Vault parent", "category": 2},
                {"id": "file", "name": "file.txt", "category": 0},
                {"id": "bad\nname", "name": "Invalid", "category": 1},
            ]
        ).encode()
        with mock.patch.object(wizard.subprocess, "run", return_value=mock.Mock(returncode=0, stdout=response)) as run:
            folders = wizard._load_folders(Path("/private/root"))
        self.assertEqual([folder["id"] for folder in folders], ["device", "folder"])
        self.assertEqual(run.call_args.args[0][-2:], ["--parent-id", "0"])

    def test_device_is_suggested_before_other_valid_folder(self) -> None:
        folders = [
            {"id": "folder", "name": "Other folder", "category": 2},
            {"id": "device", "name": "First device", "category": 1},
        ]
        self.assertEqual(wizard._select_parent(folders, input_fn=lambda _: ""), "device")

    def test_missing_fuse_device_allows_local_setup_without_systemd_start(self) -> None:
        with (
            mock.patch.object(wizard.sys, "platform", "linux"),
            mock.patch.object(wizard, "_dependency_state", return_value=(True, True)),
            mock.patch.object(wizard.os, "stat", side_effect=FileNotFoundError),
            mock.patch.object(wizard, "_systemd_user_available", return_value=True),
        ):
            self.assertEqual(wizard._check_runtime(input_fn=lambda _: self.fail("no package prompt expected")), (False, True))

    def test_missing_package_prompt_defaults_to_install(self) -> None:
        with (
            mock.patch.object(wizard, "_package_manager", return_value=("/usr/bin/apt-get", ("install", "-y"))),
            mock.patch.object(wizard.os, "geteuid", return_value=0),
            mock.patch.object(wizard.subprocess, "run", return_value=mock.Mock(returncode=0)) as run,
        ):
            installed = wizard._install_missing_packages(True, True, input_fn=lambda _: "")
        self.assertTrue(installed)
        self.assertEqual(run.call_args.args[0], ["/usr/bin/apt-get", "install", "-y", "fuse3", "util-linux"])

    def _run_default_flow(self, *, login_code: int = 0, setup_code: int = 0) -> tuple[int, mock.Mock, mock.Mock]:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install"
            answers = iter([""] * 8)
            args = type("Args", (), {"binary": None, "folder_id": "dedicated-id", "folder_name": "Vault", "vault_config": None, "upgrade": False})()
            with (
                mock.patch.object(wizard, "_check_runtime", return_value=(False, False)),
                mock.patch.object(wizard, "_choose_binary", return_value=("/tmp/rclone", None)),
                mock.patch.object(wizard.installer, "expected_root", return_value=root),
                mock.patch.object(wizard.installer, "install", return_value=0) as install_call,
                mock.patch.object(wizard.login, "run", return_value=login_code) as login_call,
                mock.patch.object(wizard.setup, "run", return_value=setup_code) as setup_call,
                mock.patch.object(wizard, "_save_settings") as save_call,
                contextlib.redirect_stdout(io.StringIO()),
            ):
                result = wizard._run(args, input_fn=lambda _: next(answers))
        return result, login_call, setup_call

    def test_default_answers_complete_staged_install(self) -> None:
        result, login_call, setup_call = self._run_default_flow()
        self.assertEqual(result, 0)
        login_call.assert_called_once()
        setup_call.assert_called_once()
        self.assertEqual(setup_call.call_args.args[0].folder_id, "dedicated-id")

    def test_failed_login_stops_before_vault_setup(self) -> None:
        result, _, setup_call = self._run_default_flow(login_code=2)
        self.assertEqual(result, 2)
        setup_call.assert_not_called()

    def test_failed_setup_stops_before_mount_service(self) -> None:
        result, _, setup_call = self._run_default_flow(setup_code=3)
        self.assertEqual(result, 3)
        setup_call.assert_called_once()

    def test_default_wizard_flow_installs_real_staged_vault_under_first_device(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            base = Path(temporary)
            home = base / "home"
            home.mkdir(mode=0o700)
            capture = base / "create-vault.args"
            binary = base / "rclone"
            binary.write_text(
                "#!/bin/sh\n"
                "for arg in \"$@\"; do\n"
                "  if [ \"$arg\" = folders ]; then\n"
                "    printf '%s\\n' '[{\"id\":\"device-123\",\"name\":\"Device\",\"category\":1}]'\n"
                "    exit 0\n"
                "  fi\n"
                "done\n"
                "case \"$*\" in\n"
                "  *'degoo --help'*) echo 'Commands: login mount folders create-vault'; exit 0;;\n"
                "esac\n"
                "create=0; parent=; name=\n"
                "while [ $# -gt 0 ]; do\n"
                "  case \"$1\" in\n"
                "    create-vault) create=1; shift;;\n"
                "    --parent-id) parent=$2; shift 2;;\n"
                "    --name) name=$2; shift 2;;\n"
                "    *) shift;;\n"
                "  esac\n"
                "done\n"
                "if [ \"$create\" = 1 ]; then\n"
                f"  printf '%s\\n%s\\n' \"$parent\" \"$name\" > {shlex.quote(str(capture))}\n"
                "  echo created-folder-123\n"
                "fi\n"
                "exit 0\n",
                encoding="utf-8",
            )
            binary.chmod(0o700)
            args = type("Args", (), {"binary": None, "folder_id": None, "folder_name": None, "vault_config": None, "upgrade": False})()
            answers = iter([""] * 12)
            with (
                mock.patch.dict(os.environ, {"HOME": str(home)}, clear=False),
                mock.patch.object(wizard, "_check_runtime", return_value=(False, False)),
                mock.patch.object(wizard, "_choose_binary", return_value=(str(binary), None)),
                mock.patch.object(wizard.login, "run", return_value=0) as login_call,
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                result = wizard._run(args, input_fn=lambda _: next(answers))

            root = home / ".local/share/rclone-degoo"
            self.assertEqual(result, 0)
            login_call.assert_called_once()
            self.assertEqual(capture.read_text(encoding="utf-8").splitlines(), ["device-123", "rclone-degoo-vault"])
            vault = json.loads((root / "config/vault.json").read_text(encoding="utf-8"))
            self.assertEqual(vault["chunk_size"], "1Gi")
            self.assertEqual(vault["max_object_size"], "19Gi")
            self.assertTrue(vault["password"])
            settings = json.loads((root / "config/mount.json").read_text(encoding="utf-8"))
            self.assertEqual(settings, wizard.installer.DEFAULT_MOUNT_SETTINGS)
            remote_config = (root / "config/rclone.conf").read_text(encoding="utf-8")
            self.assertIn("root_folder_id = created-folder-123", remote_config)
            self.assertTrue((root / "scripts/python/install.py").is_file())

    def test_requested_folder_id_is_validated_before_dependency_work(self) -> None:
        args = type("Args", (), {"binary": None, "folder_id": "0", "folder_name": None, "vault_config": None, "upgrade": False})()
        with mock.patch.object(wizard, "_check_runtime") as check_runtime, self.assertRaises(wizard.installer.InstallError):
            wizard._run(args, input_fn=lambda _: "")
        check_runtime.assert_not_called()

    def test_requested_folder_id_cannot_change_existing_binding(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "home"
            root = home / ".local/share/rclone-degoo"
            root.mkdir(parents=True, mode=0o700)
            args = type("Args", (), {"binary": None, "folder_id": "other-id", "folder_name": None, "vault_config": None, "upgrade": True})()
            with (
                mock.patch.dict(os.environ, {"HOME": str(home)}, clear=False),
                mock.patch.object(wizard.installer, "load_manifest", return_value={"root_folder_id": "bound-id"}),
                mock.patch.object(wizard, "_check_runtime") as check_runtime,
                self.assertRaises(wizard.WizardError),
            ):
                wizard._run(args, input_fn=lambda _: "")
            check_runtime.assert_not_called()


if __name__ == "__main__":
    unittest.main()
