"""Offline contracts for the unified command's Bash completion."""

from __future__ import annotations

import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest


SCRIPT_DIRECTORY = Path(__file__).resolve().parent
REPOSITORY_LAUNCHER = SCRIPT_DIRECTORY.parents[1] / "degoo-rc"
sys.path.insert(0, str(SCRIPT_DIRECTORY))
import cli  # noqa: E402


class BashCompletionTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.home = Path(self.temporary.name) / "home"
        self.home.mkdir(mode=0o700)
        self.environment = dict(
            os.environ,
            HOME=str(self.home),
            PYTHONDONTWRITEBYTECODE="1",
            XDG_CONFIG_HOME=str(self.home / ".config"),
            XDG_CACHE_HOME=str(self.home / ".cache"),
        )

    def completion_script(self, *arguments: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(REPOSITORY_LAUNCHER), "completion", *arguments],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=self.environment,
            text=True,
            timeout=30,
            check=False,
        )

    def complete(self, script: str, words: list[str], word_index: int) -> list[str]:
        array = " ".join(shlex.quote(word) for word in words)
        invocation = (
            f"COMP_WORDS=({array})\n"
            f"COMP_CWORD={word_index}\n"
            "_degoo_rc\n"
            "if ((${#COMPREPLY[@]})); then printf '%s\\n' \"${COMPREPLY[@]}\"; fi\n"
        )
        result = subprocess.run(
            ["bash", "--noprofile", "--norc", "-c", f"{script}\n{invocation}"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=self.environment,
            text=True,
            timeout=30,
            check=False,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout.splitlines()

    def test_top_level_completion_includes_all_commands_and_abbreviated_update_matches(self) -> None:
        result = self.completion_script("bash")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(list(self.home.iterdir()), [])

        all_commands = self.complete(result.stdout, ["degoo-rc", ""], 1)
        expected_commands = {*cli.COMMANDS, "help"}
        self.assertEqual(set(all_commands), expected_commands)
        self.assertEqual(len(all_commands), len(expected_commands))
        self.assertEqual(set(self.complete(result.stdout, ["degoo-rc", "u"], 1)), {"update", "uninstall"})
        self.assertEqual(self.complete(result.stdout, ["degoo-rc", "up"], 1), ["update"])

    def test_help_completion_lists_known_commands(self) -> None:
        result = self.completion_script("bash")
        self.assertEqual(result.returncode, 0, result.stderr)
        matches = self.complete(result.stdout, ["degoo-rc", "help", ""], 2)
        self.assertEqual(set(matches), set(cli.COMMANDS))
        self.assertEqual(len(matches), len(cli.COMMANDS))

    def test_completion_help_forms_match_and_missing_shell_fails_concisely(self) -> None:
        command_help = self.completion_script("help")
        topic_help = subprocess.run(
            [str(REPOSITORY_LAUNCHER), "help", "completion"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=self.environment,
            text=True,
            timeout=30,
            check=False,
        )
        self.assertEqual(command_help.returncode, 0, command_help.stderr)
        self.assertEqual(topic_help.returncode, 0, topic_help.stderr)
        self.assertEqual(command_help.stdout, topic_help.stdout)
        self.assertIn("source <(degoo-rc completion bash)", command_help.stdout)

        missing_shell = self.completion_script()
        self.assertNotEqual(missing_shell.returncode, 0)
        self.assertEqual(missing_shell.stdout, "")
        self.assertEqual(len(missing_shell.stderr.splitlines()), 1)
        self.assertIn("bash", missing_shell.stderr.lower())
        self.assertEqual(list(self.home.iterdir()), [])

    def test_nested_service_benchmark_and_completion_choices_are_available(self) -> None:
        result = self.completion_script("bash")
        self.assertEqual(result.returncode, 0, result.stderr)

        service = self.complete(result.stdout, ["degoo-rc", "service", ""], 2)
        self.assertEqual(set(service), {"enable", "disable", "start", "stop", "restart", "status"})
        self.assertEqual(set(self.complete(result.stdout, ["degoo-rc", "benchmark", ""], 2)), {"read", "write"})
        self.assertEqual(self.complete(result.stdout, ["degoo-rc", "completion", ""], 2), ["bash"])

    def test_registration_leaves_filename_fallback_for_command_arguments(self) -> None:
        result = self.completion_script("bash")
        self.assertEqual(result.returncode, 0, result.stderr)
        registration = subprocess.run(
            ["bash", "--noprofile", "--norc", "-c", f"{result.stdout}\ncomplete -p degoo-rc"],
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=self.environment,
            text=True,
            timeout=30,
            check=False,
        )
        self.assertEqual(registration.returncode, 0, registration.stderr)
        self.assertIn("-o bashdefault", registration.stdout)
        self.assertIn("-o default", registration.stdout)

        self.assertEqual(self.complete(result.stdout, ["degoo-rc", "update", "--archive", ""], 3), [])

    def test_unknown_shell_fails_without_installing_or_creating_local_state(self) -> None:
        result = self.completion_script("zsh")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("bash", result.stderr.lower())
        self.assertEqual(len(result.stderr.splitlines()), 1)
        self.assertFalse((self.home / ".local/share/rclone-degoo").exists())
        self.assertFalse((self.home / ".local/bin/degoo-rc").exists())

    def test_installed_path_launcher_generates_the_same_completion(self) -> None:
        from test_cli import CLITest

        fixture = CLITest("test_all_help_paths_work_before_install_without_state_changes")
        fixture.setUp()
        try:
            fixture.install()
            launcher = fixture.home / ".local/bin/degoo-rc"
            result = subprocess.run(
                [str(launcher), "completion", "bash"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=fixture.env,
                text=True,
                timeout=30,
                check=False,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("complete -o bashdefault -o default -F _degoo_rc degoo-rc", result.stdout)
            self.assertFalse((fixture.root / "config/auth.json").exists())
        finally:
            fixture.doCleanups()


if __name__ == "__main__":
    unittest.main()
