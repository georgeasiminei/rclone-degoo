"""Offline checks for vault setup cache-path selection."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))
import install
import setup


class SetupCachePathTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)

    @staticmethod
    def write_private(path: Path, value: object) -> None:
        path.write_text(json.dumps(value) + "\n", encoding="utf-8")
        path.chmod(0o600)

    def create_root(self, *, cache_dir: Path | None = None, include_default_cache: bool = True) -> Path:
        root = self.base / "install"
        root.mkdir(mode=0o700)
        for name in ("config", "logs", "tmp", "bin"):
            (root / name).mkdir(mode=0o700)
        if include_default_cache:
            (root / "cache").mkdir(mode=0o700)
        self.write_private(
            root / "install.json",
            {
                "version": 1,
                "remote_name": install.REMOTE_NAME,
                "mountpoint": "mount",
                "cache_dir": "cache",
                "root_folder_id": "vault-folder",
            },
        )
        if cache_dir is not None:
            self.write_private(
                root / "config/vault.json",
                {
                    "version": 1,
                    "remote": f"{install.REMOTE_NAME}:",
                    "password": "vault-secret",
                    "cache_dir": str(cache_dir),
                },
            )
        binary = root / "bin/rclone"
        binary.write_text("#!/bin/sh\nprintf 'created-folder-123\\n'\n", encoding="utf-8")
        binary.chmod(0o700)
        return root

    def test_setup_preflight_and_child_environment_use_external_cache(self) -> None:
        external_cache = self.base / "external cache"
        external_cache.mkdir(mode=0o700)
        root = self.create_root(cache_dir=external_cache, include_default_cache=False)

        self.assertEqual(setup._root(str(root)), root)
        result = SimpleNamespace(returncode=0, stdout=b"created-folder-123\n", stderr=b"")
        with mock.patch.object(setup.subprocess, "run", return_value=result) as run:
            self.assertEqual(setup._run_create(root, "device-container", "My Vault"), "created-folder-123")

        self.assertEqual(run.call_args.kwargs["env"]["XDG_CACHE_HOME"], str(external_cache))
        self.assertFalse((root / "cache").exists())

    def test_bootstrap_without_vault_config_keeps_root_cache_fallback(self) -> None:
        root = self.create_root()
        self.assertEqual(setup._root(str(root)), root)
        self.assertEqual(install.configured_cache_dir(root), root / "cache")

    def test_setup_preflight_rejects_missing_configured_cache(self) -> None:
        missing_cache = self.base / "missing external cache"
        root = self.create_root(cache_dir=missing_cache, include_default_cache=True)
        with self.assertRaises(setup.SetupError):
            setup._root(str(root))


if __name__ == "__main__":
    unittest.main()
