"""Exercise local Degoo sync path checks and the private Unix HTTP endpoint."""

from __future__ import annotations

import contextlib
import http.server
import json
import os
import socketserver
import tempfile
import threading
import unittest
from io import StringIO
from pathlib import Path
from unittest import mock

import install as installer
import sync


class UnixHTTPServer(socketserver.UnixStreamServer):
    """Serve one fake mounted-VFS HTTP endpoint for client tests."""

    allow_reuse_address = True
    daemon_threads = True


class SyncHandler(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        self.server.requests.append((self.path, json.loads(body)))
        payload = self.server.payload
        self.send_response(self.server.status)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(payload)

    def log_message(self, format, *args):
        pass


class SyncTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="degoo-sync-")
        self.addCleanup(self.temporary.cleanup)
        self.workspace = Path(self.temporary.name)
        self.root = self.workspace / "home/.local/share/rclone-degoo"
        self.mount = self.root / "mount"
        self.tmp = self.root / "tmp"
        self.root.mkdir(parents=True, mode=0o700)
        self.mount.mkdir(mode=0o700)
        self.tmp.mkdir(mode=0o700)
        (self.root / "install.json").write_text(
            json.dumps(
                {
                    "version": 1,
                    "remote_name": installer.REMOTE_NAME,
                    "mountpoint": "mount",
                    "cache_dir": "cache",
                    "root_folder_id": "test-vault-id",
                    "path_link": "",
                }
            ),
            encoding="utf-8",
        )
        (self.root / "install.json").chmod(0o600)
        self.server = None
        self.server_thread = None
        self.mount_state_patch = mock.patch.object(sync.local_info, "mounted_state", return_value="mounted")
        self.mount_state_patch.start()
        self.addCleanup(self.mount_state_patch.stop)

    def tearDown(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        if self.server_thread is not None:
            self.server_thread.join(timeout=2)

    def start_server(self, status=200, payload=None):
        socket_path = self.tmp / sync.SOCKET_NAME
        if payload is None:
            payload = b'{"path":"","refreshed":true}'
        server = UnixHTTPServer(str(socket_path), SyncHandler)
        server.requests = []
        server.status = status
        server.payload = payload
        os.chmod(socket_path, 0o600)
        self.server = server
        self.server_thread = threading.Thread(target=server.serve_forever, daemon=True)
        self.server_thread.start()
        return socket_path

    def run_sync(self, *args):
        stdout = StringIO()
        stderr = StringIO()
        with mock.patch.object(sync.local_info, "mounted_state", return_value="mounted"):
            with mock.patch.object(Path, "cwd", return_value=self.mount):
                with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
                    result = sync.main(["--root", str(self.root), *args])
        return result, stdout.getvalue(), stderr.getvalue()

    def test_default_relative_directory_refreshes_recursively(self):
        self.start_server(payload=b'{"path":"","refreshed":true}')
        stdout = StringIO()
        with mock.patch.object(Path, "cwd", return_value=self.mount):
            with contextlib.redirect_stdout(stdout):
                result = sync.main(["--root", str(self.root)])

        self.assertEqual(result, 0)
        self.assertIn(f"Refreshed remote directory metadata for {self.mount}.", stdout.getvalue())
        self.assertIn(
            "Pending local writes preserved; reopen files to see changed content.",
            stdout.getvalue(),
        )
        self.assertEqual(self.server.requests, [("/sync", {"path": "", "recursive": True})])

    def test_absolute_file_refreshes_its_containing_directory(self):
        directory = self.mount / "sub"
        directory.mkdir()
        file_path = directory / "file.txt"
        file_path.write_text("local data", encoding="utf-8")
        self.start_server(payload=b'{"path":"sub","refreshed":true}')

        refreshed = sync.refresh_directory(self.root, file_path)

        self.assertEqual(refreshed, directory)
        self.assertEqual(self.server.requests, [("/sync", {"path": "sub", "recursive": True})])

    def test_missing_target_refreshes_nearest_existing_parent(self):
        existing = self.mount / "present"
        existing.mkdir()
        missing = existing / "deleted-directory" / "child"
        self.start_server(payload=b'{"path":"present","refreshed":true}')

        refreshed = sync.refresh_directory(self.root, missing)

        self.assertEqual(refreshed, existing)
        self.assertEqual(self.server.requests[0][1], {"path": "present", "recursive": True})

    def test_root_path_maps_to_empty_vfs_path(self):
        self.start_server(payload=b'{"path":"","refreshed":true}')

        refreshed = sync.refresh_directory(self.root, self.mount)

        self.assertEqual(refreshed, self.mount)
        self.assertEqual(self.server.requests[0][1]["path"], "")

    def test_mount_directory_permissions_do_not_restrict_fuse_flags(self):
        os.chmod(self.mount, 0o777)
        self.start_server(payload=b'{"path":"","refreshed":true}')

        refreshed = sync.refresh_directory(self.root, self.mount)

        self.assertEqual(refreshed, self.mount)
        self.assertEqual(self.server.requests[0][1]["path"], "")

    def test_outside_paths_and_symlinks_are_refused_before_connecting(self):
        outside = self.workspace / "outside"
        outside.mkdir()
        link = self.mount / "outside-link"
        link.symlink_to(outside, target_is_directory=True)
        self.start_server()

        for target in (outside, link / "secret"):
            with self.subTest(target=target):
                with self.assertRaises(sync.SyncError):
                    sync.refresh_directory(self.root, target)
        self.assertEqual(self.server.requests, [])

    def test_missing_socket_explains_old_mount_upgrade_and_restart(self):
        result, stdout, stderr = self.run_sync(".")

        self.assertEqual(result, 2)
        self.assertEqual(stdout, "")
        self.assertIn("upgrade the client and restart the mount", stderr)

    def test_manifest_must_be_owner_private(self):
        self.start_server()
        os.chmod(self.root / "install.json", 0o644)

        result, stdout, stderr = self.run_sync(".")

        self.assertEqual(result, 2)
        self.assertEqual(stdout, "")
        self.assertIn("install is missing or unsafe", stderr)
        self.assertEqual(self.server.requests, [])

    def test_relative_install_root_is_rejected(self):
        with self.assertRaisesRegex(sync.SyncError, "absolute path"):
            sync.refresh_directory(Path("relative-install"), ".")

    def test_inactive_mount_is_refused_before_socket_connection(self):
        self.start_server()
        with mock.patch.object(sync.local_info, "mounted_state", return_value="not mounted"):
            stderr = StringIO()
            with contextlib.redirect_stderr(stderr):
                result = sync.main(["--root", str(self.root), "."])

        self.assertEqual(result, 2)
        self.assertIn("vault is not mounted", stderr.getvalue())
        self.assertEqual(self.server.requests, [])

    def test_unsafe_socket_mode_is_refused(self):
        socket_path = self.start_server()
        os.chmod(socket_path, 0o666)

        with self.assertRaises(sync.SyncError):
            sync.refresh_directory(self.root, self.mount)
        self.assertEqual(self.server.requests, [])

    def test_socket_path_longer_than_linux_limit_is_refused(self):
        long_root = Path("/" + "x" * 100)

        with self.assertRaisesRegex(sync.SyncError, "too long"):
            sync._socket_path(long_root)

    def test_http_failure_does_not_print_provider_error(self):
        self.start_server(status=500, payload=b'{"error":"private provider detail"}')

        result, stdout, stderr = self.run_sync(".")

        self.assertEqual(result, 2)
        self.assertEqual(stdout, "")
        self.assertIn("check the mount logs", stderr)
        self.assertNotIn("private provider detail", stdout + stderr)

    def test_malformed_or_mismatched_response_is_rejected(self):
        self.start_server(payload=b"not json")
        with self.assertRaises(sync.SyncError):
            sync.refresh_directory(self.root, self.mount)

        self.server.payload = b'{"path":"wrong","refreshed":true}'
        with self.assertRaises(sync.SyncError):
            sync.refresh_directory(self.root, self.mount)


if __name__ == "__main__":
    unittest.main()
