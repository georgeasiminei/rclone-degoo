"""Offline checks for the interactive Degoo install wizard."""

from __future__ import annotations

import json
import contextlib
import errno
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
        self.assertEqual(run.call_args.kwargs.get("umask"), 0o077)

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

    def test_mount_state_decodes_paths_and_matches_only_the_mountpoint(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            mountinfo = Path(temporary) / "mountinfo"
            mountinfo.write_bytes(b"36 25 0:32 / /tmp/my\\040mount rw,relatime - fuse.degoo degoo rw\n")
            self.assertEqual(wizard._mounted_state(Path("/tmp/my mount"), mountinfo), "mounted")
            self.assertEqual(wizard._mounted_state(Path("/tmp/my mount/child"), mountinfo), "not mounted")

    def test_mount_state_is_unknown_when_proc_snapshot_exceeds_bound(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            mountinfo = Path(temporary) / "mountinfo"
            mountinfo.write_bytes(b"x" * (wizard.MAX_MOUNTINFO_BYTES + 1))
            self.assertIn("unknown", wizard._mounted_state(Path("/tmp/degoo"), mountinfo))

    def test_missing_package_prompt_defaults_to_install(self) -> None:
        with (
            mock.patch.object(wizard, "_package_manager", return_value=("/usr/bin/apt-get", ("install", "-y"))),
            mock.patch.object(wizard.os, "geteuid", return_value=0),
            mock.patch.object(wizard.subprocess, "run", return_value=mock.Mock(returncode=0)) as run,
        ):
            installed = wizard._install_missing_packages(True, True, input_fn=lambda _: "")
        self.assertTrue(installed)
        self.assertEqual(run.call_args.args[0], ["/usr/bin/apt-get", "install", "-y", "fuse3", "util-linux"])

    def _run_default_flow(self, *, login_code: int = 0, setup_code: int = 0) -> tuple[int, mock.Mock, mock.Mock, str]:
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
                contextlib.redirect_stdout(io.StringIO()) as output,
            ):
                result = wizard._run(args, input_fn=lambda _: next(answers))
        return result, login_call, setup_call, output.getvalue()

    def test_default_answers_complete_staged_install(self) -> None:
        result, login_call, setup_call, _ = self._run_default_flow()
        self.assertEqual(result, 0)
        login_call.assert_called_once()
        setup_call.assert_called_once()
        self.assertEqual(setup_call.call_args.args[0].folder_id, "dedicated-id")

    def test_failed_login_stops_before_vault_setup(self) -> None:
        result, _, setup_call, output = self._run_default_flow(login_code=2)
        self.assertEqual(result, 2)
        setup_call.assert_not_called()
        self.assertIn("Installation summary:", output)
        self.assertIn("retry with", output)
        self.assertIn("degoo-rc login", output)

    def test_failed_setup_stops_before_mount_service(self) -> None:
        result, _, setup_call, output = self._run_default_flow(setup_code=3)
        self.assertEqual(result, 3)
        setup_call.assert_called_once()
        self.assertIn("Installation summary:", output)
        self.assertIn("retry with", output)
        self.assertIn("degoo-rc setup help", output)

    def test_manual_flow_prints_completion_summary_when_dependencies_are_incomplete(self) -> None:
        result, _, _, output = self._run_default_flow()
        self.assertEqual(result, 0)
        self.assertIn("Install root:", output)
        self.assertIn("Mount point:", output)
        self.assertIn("not mounted", output)
        for command in (
            "info",
            "login",
            "setup help",
            "status",
            "service status",
            "service enable",
            "service stop",
            "mount",
            "stop",
            "uninstall",
            "logs",
            "logs --follow",
            "logs help",
        ):
            self.assertIn(f"degoo-rc {command}", output)
        for legacy in ("info.sh", "login.sh", "status.sh", "service.sh", "mount.sh", "uninstall.sh", "logs.sh"):
            self.assertNotIn(legacy, output)
        self.assertIn("/logs", output)

    def test_service_enable_times_out_without_claiming_mount_or_success(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install root"
            answers = iter([""] * 9)
            args = type("Args", (), {"binary": None, "folder_id": "dedicated-id", "folder_name": "Vault", "vault_config": None, "upgrade": False})()
            output = io.StringIO()
            with (
                mock.patch.object(wizard, "_check_runtime", return_value=(True, True)),
                mock.patch.object(wizard, "_choose_binary", return_value=("/tmp/rclone", None)),
                mock.patch.object(wizard.installer, "expected_root", return_value=root),
                mock.patch.object(wizard.installer, "install", return_value=0),
                mock.patch.object(wizard.login, "run", return_value=0),
                mock.patch.object(wizard.setup, "run", return_value=0),
                mock.patch.object(wizard, "_save_settings"),
                mock.patch.object(wizard.service, "run", return_value=0) as service_call,
                mock.patch.object(wizard, "_mounted_state", return_value="not mounted"),
                mock.patch.object(wizard, "_wait_for_mount", return_value="not mounted") as wait_mount,
                mock.patch.object(wizard.local_info, "service_status", return_value="enabled; active"),
                contextlib.redirect_stdout(output),
            ):
                result = wizard._run(args, input_fn=lambda _: next(answers))

        self.assertEqual(result, 2)
        service_call.assert_called_once_with(root, "enable")
        wait_mount.assert_called_once_with(root, timeout=wizard.MOUNT_STATE_TIMEOUT)
        mountpoint = shlex.quote(str(root / "mount"))
        self.assertIn(f"Mount point: {mountpoint} — not mounted", output.getvalue())
        self.assertNotIn(f"Mount point: {mountpoint} — mounted", output.getvalue())
        self.assertIn("mount was not confirmed after up to 15 seconds", output.getvalue())
        self.assertIn("Recent mount, login, setup, and status logs", output.getvalue())
        cli = shlex.quote(str(root / "degoo-rc"))
        self.assertIn(f"# Local status\n  {cli} info", output.getvalue())
        self.assertIn(f"{cli} logs --follow", output.getvalue())

    def test_failed_service_start_keeps_nonzero_result_and_explains_recovery(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install"
            answers = iter([""] * 9)
            args = type("Args", (), {"binary": None, "folder_id": "dedicated-id", "folder_name": "Vault", "vault_config": None, "upgrade": False})()
            output = io.StringIO()
            failure = wizard.service.ServiceError("the service is linked, but could not start")
            with (
                mock.patch.object(wizard, "_check_runtime", return_value=(True, True)),
                mock.patch.object(wizard, "_choose_binary", return_value=("/tmp/rclone", None)),
                mock.patch.object(wizard.installer, "expected_root", return_value=root),
                mock.patch.object(wizard.installer, "install", return_value=0),
                mock.patch.object(wizard.login, "run", return_value=0),
                mock.patch.object(wizard.setup, "run", return_value=0),
                mock.patch.object(wizard, "_save_settings"),
                mock.patch.object(wizard.service, "run", side_effect=failure),
                mock.patch.object(wizard, "_mounted_state", return_value="not mounted"),
                mock.patch.object(wizard.local_info, "service_status", return_value="enabled; failed"),
                contextlib.redirect_stdout(output),
            ):
                result = wizard._run(args, input_fn=lambda _: next(answers))

        self.assertEqual(result, 2)
        self.assertIn("service is configured but the mount did not start", output.getvalue().lower())
        self.assertIn("Mount point:", output.getvalue())
        self.assertIn("degoo-rc service status", output.getvalue())
        self.assertIn("degoo-rc logs", output.getvalue())

    def test_upgrade_stops_owned_service_even_before_mount_attaches(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install"
            root.mkdir(mode=0o700)
            args = type("Args", (), {"binary": None, "folder_id": None, "folder_name": None, "vault_config": None, "upgrade": True})()
            events: list[str] = []
            with (
                mock.patch.object(wizard.installer, "expected_root", return_value=root),
                mock.patch.object(wizard.installer, "load_manifest", return_value={"root_folder_id": "vault-id", "service_links": ["owned-link"]}),
                mock.patch.object(wizard, "_mounted_state", return_value="not mounted"),
                mock.patch.object(wizard.local_info, "service_status", return_value="enabled; activating"),
                mock.patch.object(wizard, "_wait_for_mount_state", return_value="not mounted"),
                mock.patch.object(wizard, "_check_runtime", side_effect=lambda **_: events.append("runtime") or (False, False)),
                mock.patch.object(wizard, "_choose_binary", return_value=("/tmp/rclone", None)),
                mock.patch.object(wizard, "_current_vault_settings", return_value={"password": "private"}),
                mock.patch.object(wizard, "_cache_settings", return_value=wizard.installer.DEFAULT_MOUNT_SETTINGS),
                mock.patch.object(wizard.installer, "install", side_effect=lambda _: events.append("install") or 0),
                mock.patch.object(wizard.login, "run", return_value=0),
                mock.patch.object(wizard, "_save_settings"),
                mock.patch.object(wizard.service, "run", side_effect=lambda _root, action: events.append(f"service:{action}") or 0),
                contextlib.redirect_stdout(io.StringIO()),
            ):
                result = wizard._run(args, input_fn=lambda _: "")

        self.assertEqual(result, 0)
        self.assertLess(events.index("service:stop"), events.index("runtime"))
        self.assertLess(events.index("runtime"), events.index("install"))

    def test_upgrade_without_owned_service_uses_contained_stop_wrapper(self) -> None:
        root = Path("/tmp/degoo-test-install")
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="not configured for this install"),
            mock.patch.object(wizard, "_stop_owned_mount") as stop_mount,
            mock.patch.object(wizard, "_wait_for_mount_state", return_value="not mounted") as wait_mount,
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": []}, input_fn=lambda _: "")

        stop_mount.assert_called_once_with(root)
        wait_mount.assert_called_once_with(root / "mount", "not mounted", timeout=15.0)

    def test_stop_owned_mount_uses_generated_wrapper_not_installed_helper(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            stop = root / "stop.sh"
            installed_contents = b"#!/bin/sh\necho compromised-helper\nexit 99\n"
            stop.write_bytes(installed_contents)
            stop.chmod(0o700)
            with mock.patch.object(wizard.subprocess, "run", return_value=mock.Mock(returncode=1)) as run:
                wizard._stop_owned_mount(root)

        generated_contents = wizard.installer.shell_wrapper("stop.sh", root)
        run.assert_called_once_with(
            ["/bin/sh"],
            input=generated_contents,
            stdout=wizard.subprocess.DEVNULL,
            stderr=wizard.subprocess.DEVNULL,
            timeout=30,
            check=False,
        )
        self.assertNotEqual(run.call_args.kwargs["input"], installed_contents)

    def test_inactive_owned_service_skips_wait_before_manual_unmount(self) -> None:
        root = Path("/tmp/degoo-test-install")
        with (
            mock.patch.object(wizard, "_mounted_state", side_effect=("mounted", "mounted")),
            mock.patch.object(wizard.local_info, "service_status", return_value="enabled; inactive"),
            mock.patch.object(wizard.service, "run", return_value=0) as stop_service,
            mock.patch.object(wizard, "_stop_owned_mount") as stop_mount,
            mock.patch.object(wizard, "_wait_for_mount_state", return_value="not mounted") as wait_mount,
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": ["owned-link"]}, input_fn=lambda _: "")

        stop_service.assert_called_once_with(root, "stop")
        stop_mount.assert_called_once_with(root)
        wait_mount.assert_called_once_with(root / "mount", "not mounted", timeout=15.0)

    def test_upgrade_refuses_unknown_mount_after_stopping_service(self) -> None:
        root = Path("/tmp/degoo-test-install")
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="enabled; active"),
            mock.patch.object(wizard.service, "run", return_value=0) as stop_service,
            mock.patch.object(wizard, "_wait_for_mount_state", return_value="unknown (mount table unavailable)"),
            mock.patch.object(wizard, "_stop_owned_mount") as stop_mount,
            self.assertRaises(wizard.WizardError),
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": ["owned-link"]}, input_fn=lambda _: "")

        stop_service.assert_called_once_with(root, "stop")
        stop_mount.assert_not_called()

    def test_upgrade_decline_or_owned_service_stop_error_refuses_before_install(self) -> None:
        root = Path("/tmp/degoo-test-install")
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="enabled; active"),
            mock.patch.object(wizard.service, "run") as stop_service,
            self.assertRaises(wizard.WizardCancelled),
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": ["owned-link"]}, input_fn=lambda _: "n")
        stop_service.assert_not_called()

        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="enabled; active"),
            mock.patch.object(wizard.service, "run", side_effect=wizard.service.ServiceError("stop refused")) as stop_service,
            mock.patch.object(wizard, "_stop_owned_mount") as stop_mount,
            self.assertRaises(wizard.WizardError),
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": ["owned-link"]}, input_fn=lambda _: "")
        stop_service.assert_called_once_with(root, "stop")
        stop_mount.assert_not_called()

    def test_upgrade_refuses_connected_busy_mount_with_safe_recovery_hints(self) -> None:
        root = Path("/tmp/degoo-test-install")
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="not configured for this install"),
            mock.patch.object(wizard, "_stop_owned_mount"),
            mock.patch.object(wizard, "_wait_for_mount_state", return_value="mounted"),
            mock.patch.object(wizard.os, "stat", return_value=mock.sentinel.mount_info),
            mock.patch.object(wizard.subprocess, "run") as run,
            self.assertRaisesRegex(wizard.WizardError, "close applications using it") as failure,
        ):
            wizard._prepare_upgrade_mount(root, {"service_links": []}, input_fn=lambda _: "")

        cli = shlex.quote(str(root / "degoo-rc"))
        self.assertIn(f"{cli} logs", str(failure.exception))
        self.assertIn(f"{cli} stop", str(failure.exception))
        run.assert_not_called()

    def test_startup_wait_returns_mount_evidence_with_fake_clock(self) -> None:
        root = Path("/tmp/degoo-test-install")
        now = [0.0]
        states = iter(("not mounted", "not mounted", "mounted"))
        sleep = mock.Mock(side_effect=lambda delay: now.__setitem__(0, now[0] + delay))
        with (
            mock.patch.object(wizard, "_mounted_state", side_effect=lambda _: next(states)),
            mock.patch.object(wizard.os, "stat", return_value=mock.sentinel.mount_info),
        ):
            state = wizard._wait_for_mount(root, timeout=15.0, monotonic=lambda: now[0], sleep=sleep)

        self.assertEqual(state, "mounted")
        self.assertGreaterEqual(sleep.call_count, 1)

    def test_startup_wait_does_not_accept_disconnected_kernel_mount(self) -> None:
        root = Path("/tmp/degoo-test-install")
        now = [0.0]
        sleep = mock.Mock(side_effect=lambda delay: now.__setitem__(0, now[0] + delay))
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.os, "stat", side_effect=OSError(errno.ENOTCONN, "transport endpoint is not connected")),
        ):
            state = wizard._wait_for_mount(
                root,
                timeout=1.0,
                interval=0.5,
                monotonic=lambda: now[0],
                sleep=sleep,
            )

        self.assertEqual(state, "disconnected (ENOTCONN)")

    def test_disconnected_mount_lazy_recovery_requires_idle_cache_lock(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install"
            root.mkdir(mode=0o700)
            (root / "cache").mkdir(mode=0o700)
            with (
                mock.patch.object(wizard.cleanup, "_acquire_cache_lock", return_value=73) as acquire_lock,
                mock.patch.object(wizard.os, "close") as close_lock,
                mock.patch.object(
                    wizard,
                    "_observed_mount_state",
                    side_effect=("disconnected (ENOTCONN)", "disconnected (ENOTCONN)"),
                ),
                mock.patch.object(wizard, "_trusted_fusermount", return_value=Path("/usr/bin/fusermount")),
                mock.patch.object(wizard.subprocess, "run", return_value=mock.Mock(returncode=0)) as run,
                mock.patch.object(wizard, "_wait_for_mount_state", return_value="not mounted") as wait_mount,
            ):
                wizard._recover_disconnected_mount(root, input_fn=lambda _: "", service_configured=True)

        acquire_lock.assert_called_once_with(root, wait_seconds=0)
        close_lock.assert_called_once_with(73)
        run.assert_called_once_with(
            ["/usr/bin/fusermount", "-u", "-z", str(root / "mount")],
            stdin=wizard.subprocess.DEVNULL,
            stdout=wizard.subprocess.DEVNULL,
            stderr=wizard.subprocess.DEVNULL,
            timeout=30,
            check=False,
        )
        wait_mount.assert_called_once_with(root / "mount", "not mounted", timeout=15.0)

    def test_disconnected_mount_recovery_never_lazily_unmounts_without_safe_checks(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "install"
            root.mkdir(mode=0o700)
            (root / "cache").mkdir(mode=0o700)

            with (
                mock.patch.object(wizard.cleanup, "_acquire_cache_lock", side_effect=wizard.cleanup.CleanupError("active")),
                mock.patch.object(wizard, "_observed_mount_state") as observed,
                mock.patch.object(wizard.subprocess, "run") as run,
                self.assertRaises(wizard.WizardError),
            ):
                wizard._recover_disconnected_mount(root, input_fn=lambda _: "", service_configured=False)
            observed.assert_not_called()
            run.assert_not_called()

            with (
                mock.patch.object(wizard.cleanup, "_acquire_cache_lock", return_value=73),
                mock.patch.object(wizard.os, "close"),
                mock.patch.object(wizard, "_observed_mount_state", side_effect=("disconnected (ENOTCONN)", "mounted")),
                mock.patch.object(wizard, "_trusted_fusermount", return_value=Path("/usr/bin/fusermount")),
                mock.patch.object(wizard.subprocess, "run") as run,
                self.assertRaisesRegex(wizard.WizardError, "close applications using it"),
            ):
                wizard._recover_disconnected_mount(root, input_fn=lambda _: "", service_configured=False)
            run.assert_not_called()

            with (
                mock.patch.object(wizard.cleanup, "_acquire_cache_lock", return_value=73),
                mock.patch.object(wizard.os, "close"),
                mock.patch.object(wizard, "_observed_mount_state", return_value="disconnected (ENOTCONN)"),
                mock.patch.object(wizard, "_trusted_fusermount", return_value=Path("/usr/bin/fusermount")) as trusted,
                mock.patch.object(wizard.subprocess, "run") as run,
                self.assertRaises(wizard.WizardCancelled),
            ):
                wizard._recover_disconnected_mount(root, input_fn=lambda _: "n", service_configured=False)
            trusted.assert_not_called()
            run.assert_not_called()

    def test_startup_timeout_is_non_success_and_summary_commands_are_copyable(self) -> None:
        root = Path("/tmp/degoo test install")
        now = [0.0]
        sleep = mock.Mock(side_effect=lambda delay: now.__setitem__(0, now[0] + delay))
        with mock.patch.object(wizard, "_mounted_state", return_value="not mounted"):
            state = wizard._wait_for_mount(root, timeout=1.0, interval=0.5, monotonic=lambda: now[0], sleep=sleep)
        self.assertEqual(state, "not mounted")

        output = io.StringIO()
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="not mounted"),
            mock.patch.object(wizard.local_info, "service_status", return_value="not configured for this install"),
            contextlib.redirect_stdout(output),
        ):
            wizard._completion_summary(root, mount_ready=True, systemd_available=False)
        summary = output.getvalue()
        cli = shlex.quote(str(root / "degoo-rc"))
        self.assertIn("# Local status", summary)
        self.assertIn(f"  {cli} info\n", summary)
        self.assertIn("# Stop the user service before an upgrade, if configured", summary)
        self.assertIn(f"  {cli} service stop\n", summary)
        self.assertIn("# Unmount this install's foreground/manual mount", summary)
        self.assertIn(f"  {cli} stop\n", summary)
        self.assertIn(f"  {cli} logs\n", summary)
        self.assertIn("# Log directory:", summary)
        self.assertIn("Recent mount, login, setup, and status logs", summary)

    def test_completion_summary_reports_disconnected_mount_truthfully(self) -> None:
        root = Path("/tmp/degoo-test-install")
        output = io.StringIO()
        with (
            mock.patch.object(wizard, "_mounted_state", return_value="mounted"),
            mock.patch.object(wizard.os, "stat", side_effect=OSError(errno.ENOTCONN, "transport endpoint is not connected")),
            mock.patch.object(wizard.local_info, "service_status", return_value="enabled; inactive"),
            contextlib.redirect_stdout(output),
        ):
            state = wizard._completion_summary(root, mount_ready=True, systemd_available=True)

        self.assertEqual(state, "disconnected (ENOTCONN)")
        self.assertIn("Mount point: /tmp/degoo-test-install/mount — disconnected (ENOTCONN)", output.getvalue())

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
            home.mkdir(mode=0o700)
            root = home / ".local/share/rclone-degoo"
            root.mkdir(parents=True, mode=0o700)
            (home / ".local").chmod(0o700)
            (home / ".local/share").chmod(0o700)
            args = type("Args", (), {"binary": None, "folder_id": "other-id", "folder_name": None, "vault_config": None, "upgrade": True})()
            with (
                mock.patch.dict(os.environ, {"HOME": str(home)}, clear=False),
                mock.patch.object(wizard.installer, "load_manifest", return_value={"root_folder_id": "bound-id"}),
                mock.patch.object(wizard, "_check_runtime") as check_runtime,
                self.assertRaises(wizard.WizardError),
            ):
                wizard._run(args, input_fn=lambda _: "")
            check_runtime.assert_not_called()


    def test_preflight_refuses_unsafe_local_bin_before_dependency_prompts(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "home"
            home.mkdir(mode=0o700)
            local = home / ".local"
            local.mkdir(mode=0o755)
            local_bin = local / "bin"
            local_bin.mkdir(mode=0o775)
            local_bin.chmod(0o775)
            args = type("Args", (), {"binary": None, "folder_id": None, "folder_name": None, "vault_config": None, "upgrade": False})()
            with (
                mock.patch.object(wizard.installer, "home_directory", return_value=home),
                self.assertRaises(wizard.WizardError) as context,
            ):
                wizard._run(args, input_fn=lambda _: self.fail("no prompt expected"))
        self.assertIn(str(local_bin), str(context.exception))
        self.assertIn("chmod go-w", str(context.exception))
        self.assertIn("group or other", str(context.exception))

if __name__ == "__main__":
    unittest.main()
