"""Offline checks for login cache-path selection."""

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
import login


class LoginCachePathTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.base = Path(self.temporary.name)
        self.root = self.base / "install"
        self.root.mkdir(mode=0o700)
        for name in ("config", "logs", "tmp", "cache", "bin"):
            (self.root / name).mkdir(mode=0o700)
        self.external_cache = self.base / "private external cache"
        self.external_cache.mkdir(mode=0o700)
        self.write_private(
            self.root / "config/vault.json",
            {
                "version": 1,
                "remote": f"{install.REMOTE_NAME}:",
                "password": "vault-secret",
                "cache_dir": str(self.external_cache),
            },
        )
        self.write_private(self.root / "config/login.json", {"email": "owner@example.test"})
        binary = self.root / "bin/rclone"
        binary.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        binary.chmod(0o700)

    @staticmethod
    def write_private(path: Path, value: object) -> None:
        path.write_text(json.dumps(value) + "\n", encoding="utf-8")
        path.chmod(0o600)

    def test_login_uses_external_configured_cache_in_child_environment(self) -> None:
        result = SimpleNamespace(returncode=0)
        with mock.patch.object(login.subprocess, "run", return_value=result) as run:
            self.assertEqual(login.run(self.root, clear_email=False), 0)

        self.assertEqual(run.call_args.kwargs["env"]["XDG_CACHE_HOME"], str(self.external_cache))
        self.assertNotEqual(run.call_args.kwargs["env"]["XDG_CACHE_HOME"], str(self.root / "cache"))

    def test_clear_email_does_not_require_a_cache_directory(self) -> None:
        (self.root / "config/vault.json").unlink()
        (self.root / "cache").rmdir()
        with mock.patch.object(login.installer, "configured_cache_dir", side_effect=AssertionError("cache should not be resolved")):
            self.assertEqual(login.run(self.root, clear_email=True), 0)


if __name__ == "__main__":
    unittest.main()
