"""Offline checks for the contained Degoo connection helper."""

from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import connection
import install


class ConnectionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.base = Path(self.temporary.name)
        self.root = self.base / "install"
        self.root.mkdir(mode=0o700)
        (self.root / "tmp").mkdir()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _run_result(self, *, returncode: int = 0, stdout: bytes = b"[]", stderr: bytes = b""):
        def run(_arguments, **kwargs):
            run.private_modes = [os.fstat(kwargs[name].fileno()).st_mode & 0o777 for name in ("stdout", "stderr")]
            kwargs["stdout"].write(stdout)
            kwargs["stderr"].write(stderr)
            return SimpleNamespace(returncode=returncode)

        run.private_modes = []
        return run

    def test_check_connection_queries_account_root_with_private_contained_settings(self) -> None:
        response = json.dumps(
            [
                {"id": "device", "name": "Linux device", "category": 1},
                {"id": "folder", "name": "Other folder", "category": 2},
                {"id": "0", "name": "Account root", "category": 1},
                {"id": "file", "name": "ordinary file", "category": 0},
                {"id": "vault", "name": "Vault", "category": 6},
            ]
        ).encode()
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(stdout=response)) as run:
            folders = connection.check_connection(self.root)

        self.assertEqual([item["id"] for item in folders], ["device", "folder", "vault"])
        args = run.call_args.args[0]
        options = run.call_args.kwargs
        self.assertEqual(args[0], str(self.root / "bin/rclone"))
        self.assertEqual(
            args[1:5],
            ["--config", str(self.root / "config/rclone.conf"), "--log-level", "ERROR"],
        )
        self.assertNotIn("--log-file", args)
        self.assertEqual(args[-4:], ["folders", str(self.root / "config/auth.json"), "--parent-id", "0"])
        self.assertEqual(options["env"]["XDG_CONFIG_HOME"], str(self.root / "config"))
        self.assertEqual(options["env"]["XDG_CACHE_HOME"], str(self.root / "cache"))
        self.assertEqual(options["env"]["TMPDIR"], str(self.root / "tmp"))
        self.assertEqual(options["env"]["PATH"], "/usr/bin:/bin")
        self.assertEqual(options["umask"], 0o077)
        self.assertEqual(options["stdin"], connection.subprocess.DEVNULL)
        self.assertEqual(options["timeout"], connection.QUERY_TIMEOUT)
        self.assertEqual(run.side_effect.private_modes, [0o600, 0o600])

    def test_existing_vault_enables_saved_password_for_connection_checks(self) -> None:
        vault = self.root / "config/vault.json"
        vault.parent.mkdir(mode=0o700)
        vault.write_text(
            json.dumps(
                {
                    "version": 1,
                    "remote": f"{install.REMOTE_NAME}:",
                    "password": "vault-secret",
                    "cache_dir": str(self.root / "cache"),
                }
            )
            + "\n",
            encoding="utf-8",
        )
        vault.chmod(0o600)
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result()) as run:
            connection.check_connection(self.root)
        args = run.call_args.args[0]
        self.assertEqual(args[args.index("--vault-config") + 1], str(vault))

    def test_folder_query_uses_configured_external_cache_not_default_decoy(self) -> None:
        config = self.root / "config"
        config.mkdir(mode=0o700)
        default_cache = self.root / "cache"
        default_cache.mkdir(mode=0o700)
        external_cache = self.base / "external cache"
        external_cache.mkdir(mode=0o700)
        vault = config / "vault.json"
        vault.write_text(
            json.dumps(
                {
                    "version": 1,
                    "remote": f"{install.REMOTE_NAME}:",
                    "password": "vault-secret",
                    "cache_dir": str(external_cache),
                }
            )
            + "\n",
            encoding="utf-8",
        )
        vault.chmod(0o600)

        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result()) as run:
            connection.check_connection(self.root)

        self.assertEqual(run.call_args.kwargs["env"]["XDG_CACHE_HOME"], str(external_cache))
        self.assertNotEqual(run.call_args.kwargs["env"]["XDG_CACHE_HOME"], str(default_cache))

    def test_list_folders_queries_children_and_preserves_same_name_entries(self) -> None:
        response = json.dumps(
            [
                {"id": "first", "name": "rclone-degoo-vault", "category": 2},
                {"id": "second", "name": "rclone-degoo-vault", "category": 2},
            ]
        ).encode()
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(stdout=response)) as run:
            folders = connection.list_folders(self.root, parent_id="device-123")

        self.assertEqual([item["id"] for item in folders], ["first", "second"])
        self.assertEqual(run.call_args.args[0][-2:], ["--parent-id", "device-123"])

    def test_rejected_saved_auth_is_classified_as_login_required_without_raw_output(self) -> None:
        secret = "private-refresh-token"
        stderr = f"Error: degoo login required (auth HTTP 401): {secret}\n".encode()
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(returncode=1, stderr=stderr)):
            with self.assertRaises(connection.LoginRequiredError) as caught:
                connection.check_connection(self.root)

        self.assertNotIn(secret, str(caught.exception))

    def test_network_and_provider_errors_are_not_classified_as_login_required(self) -> None:
        private_detail = "provider-body-account-token"
        stderr = f"connection reset by peer {private_detail}\n".encode()
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(returncode=1, stderr=stderr)):
            with self.assertRaises(connection.ProviderQueryError) as caught:
                connection.check_connection(self.root)

        self.assertNotIsInstance(caught.exception, connection.LoginRequiredError)
        self.assertNotIn(private_detail, str(caught.exception))

    def test_timeout_is_a_provider_failure_and_does_not_prompt_for_login(self) -> None:
        with mock.patch.object(connection.subprocess, "run", side_effect=connection.subprocess.TimeoutExpired("rclone", 1)):
            with self.assertRaises(connection.ProviderQueryError) as caught:
                connection.check_connection(self.root)

        self.assertIn("timed out", str(caught.exception))

    def test_invalid_parent_id_is_rejected_before_spawning_rclone(self) -> None:
        with mock.patch.object(connection.subprocess, "run") as run:
            with self.assertRaises(connection.ConnectionCheckError):
                connection.list_folders(self.root, parent_id="bad\nparent")

        run.assert_not_called()

    def test_response_is_bounded_and_invalid_metadata_is_filtered(self) -> None:
        malformed_items = json.dumps(
            [
                {"id": "safe", "name": "Visible", "category": 1},
                {"id": "bad\nfolder", "name": "Unsafe id", "category": 1},
                {"id": "unsafe-name", "name": "Bad\u001b[31mname", "category": 1},
                {"id": "too-long", "name": "x" * (connection.MAX_NAME_LENGTH + 1), "category": 1},
            ]
        ).encode()
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(stdout=malformed_items)):
            self.assertEqual([entry["id"] for entry in connection.check_connection(self.root)], ["safe"])

        with mock.patch.object(
            connection.subprocess,
            "run",
            side_effect=self._run_result(stdout=b" " * (connection.MAX_RESPONSE_BYTES + 1)),
        ):
            with self.assertRaises(connection.ProviderQueryError):
                connection.check_connection(self.root)

    def test_unrecognized_response_does_not_include_provider_data(self) -> None:
        private_detail = "opaque-provider-response"
        with mock.patch.object(connection.subprocess, "run", side_effect=self._run_result(stdout=private_detail.encode())):
            with self.assertRaises(connection.ProviderQueryError) as caught:
                connection.check_connection(self.root)

        self.assertNotIn(private_detail, str(caught.exception))


if __name__ == "__main__":
    unittest.main()
