import contextlib
import importlib.util
import io
import json
from pathlib import Path
import re
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

SPEC = importlib.util.spec_from_file_location(
    "token_batch_runner", Path(__file__).resolve().parents[1] / "test-token-financial-postgres.py")
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class TokenFinancialBatchTests(unittest.TestCase):
    def command(self):
        return ["go", "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1",
                "-timeout=20m", "-json", "-skip",
                "^(" + "|".join(sorted(RUNNER.PROVISIONING_REQUIRED)) + ")$", "./internal/token"]

    def events(self, names):
        events = []
        for name in sorted(names):
            events.append({"Action": "run", "Test": name, "Package": RUNNER.TOKEN_PACKAGE})
            children = RUNNER.EXACT_REQUIRED_CHILDREN.get(name, set())
            if name in RUNNER.TOKEN_CHILD_REQUIRED:
                children = children or {"controlled"}
            for child in sorted(children):
                events.append({"Action": "pass", "Test": name + "/" + child, "Package": RUNNER.TOKEN_PACKAGE})
            events.append({"Action": "pass", "Test": name, "Package": RUNNER.TOKEN_PACKAGE})
        events.append({"Action": "pass", "Package": RUNNER.TOKEN_PACKAGE})
        return events

    def execute(self, *, mutate=None, fail_at=None, dirty_at=None):
        ordinary = {"TestNewOrdinary", "ExampleNew", "FuzzNew"}
        catalog = RUNNER.REQUIRED | RUNNER.PROVISIONING_REQUIRED | ordinary
        calls, restored = [], []

        def run(command, **options):
            index = len(calls)
            selected = set(catalog)
            for flag, include in [("-skip", False), ("-run", True)]:
                if flag in command:
                    matching = {name for name in catalog if re.fullmatch(command[command.index(flag) + 1], name)}
                    selected = selected & matching if include else selected - matching
            events = self.events(selected)
            if mutate:
                events = mutate(index, events)
            calls.append((command, options, selected))
            return SimpleNamespace(returncode=1 if fail_at == index else 0,
                                   stdout="".join(json.dumps(e) + "\n" for e in events), stderr="diagnostic\n")

        def clean():
            restored.append(len(calls) - 1)
            if dirty_at == restored[-1]:
                raise RuntimeError("owned service not restored")

        self.calls, self.restored = calls, restored
        self.output = io.StringIO()
        self.stderr = io.StringIO()
        with patch.object(RUNNER.subprocess, "run", side_effect=run), contextlib.redirect_stdout(self.output), contextlib.redirect_stderr(self.stderr):
            result = RUNNER.run_token_batches(self.command(), cwd=Path("owned"), env={"owned": "service"}, assert_restored=clean)
        return result, ordinary

    def test_complete_disjoint_selection_keeps_future_tests_examples_and_fuzz(self):
        result, ordinary = self.execute()
        selections = [call[2] for call in self.calls]
        self.assertEqual(selections[0], ordinary)
        self.assertEqual(selections[1] | selections[2], RUNNER.REQUIRED)
        self.assertFalse(selections[1] & selections[2])
        self.assertEqual(self.restored, [0, 1, 2])
        self.assertEqual(len(self.calls), 3)
        self.assertEqual(self.stderr.getvalue(), "diagnostic\n" * 3)
        self.assertEqual(result.stderr, self.stderr.getvalue())
        for command, options, _ in self.calls:
            for flag in ["-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=20m", "-json"]:
                self.assertIn(flag, command)
            self.assertEqual(options["env"], {"owned": "service"})
        events = [json.loads(line) for line in result.stdout.splitlines()]
        RUNNER.verify_results(events, package=RUNNER.TOKEN_PACKAGE, require_children=True,
                              children_required=RUNNER.TOKEN_CHILD_REQUIRED)

    def test_failure_stops_before_next_batch_and_keeps_output(self):
        for index in range(3):
            with self.subTest(batch=index), self.assertRaisesRegex(RuntimeError, "batch failed"):
                self.execute(fail_at=index)
            self.assertEqual(len(self.calls), index + 1)
            self.assertEqual(self.restored, list(range(index)))
            self.assertIn('"Action": "pass"', self.output.getvalue())

    def test_dirty_owned_service_prevents_following_batch(self):
        for index in range(3):
            with self.subTest(batch=index), self.assertRaisesRegex(RuntimeError, "not restored"):
                self.execute(dirty_at=index)
            self.assertEqual(len(self.calls), index + 1)

    def test_zero_exit_does_not_hide_missing_package_completion(self):
        with self.assertRaisesRegex(RuntimeError, "package did not complete"):
            self.execute(mutate=lambda index, events: [e for e in events if e.get("Test")] if index == 0 else events)
        self.assertEqual(len(self.calls), 1)

    def test_required_skip_missing_child_or_unexpected_root_fails_closed(self):
        def skipped(index, events):
            return events + [{"Action": "skip", "Test": sorted(RUNNER.REQUIRED)[0], "Package": RUNNER.TOKEN_PACKAGE}] if index == 1 else events

        def missing_root(index, events):
            return [e for e in events if e.get("Test", "").split("/", 1)[0] != sorted(RUNNER.REQUIRED)[0]] if index == 1 else events

        def no_children(index, events):
            return [e for e in events if "/" not in e.get("Test", "")] if index == 1 else events

        def foreign(index, events):
            return events + self.events({"TestUnexpectedForeign"}) if index == 1 else events

        for mutate in [skipped, missing_root, no_children, foreign]:
            with self.subTest(mutation=mutate.__name__), self.assertRaises(RuntimeError):
                self.execute(mutate=mutate)
            self.assertEqual(len(self.calls), 2)
            self.assertEqual(self.restored, [0])

    def test_ordinary_failure_is_not_hidden_by_required_subset(self):
        with self.assertRaisesRegex(RuntimeError, "contract failed"):
            self.execute(mutate=lambda index, events: events + [{"Action": "fail", "Test": "TestNewOrdinary", "Package": RUNNER.TOKEN_PACKAGE}] if index == 0 else events)
        self.assertEqual(len(self.calls), 1)

    def test_malformed_json_stops_before_restoration_or_next_batch(self):
        clean = Mock()
        output = io.StringIO()
        result = SimpleNamespace(returncode=0, stdout="{malformed\n", stderr="original diagnostic\n")
        errors = io.StringIO()
        with patch.object(RUNNER.subprocess, "run", return_value=result) as run, contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            with self.assertRaises(json.JSONDecodeError):
                RUNNER.run_token_batches(self.command(), cwd=Path("owned"), env={}, assert_restored=clean)
        self.assertEqual(run.call_count, 1)
        clean.assert_not_called()
        self.assertIn(result.stdout, output.getvalue())
        self.assertEqual(errors.getvalue(), result.stderr)

    def test_spawn_exception_does_not_start_another_batch_or_claim_restoration(self):
        clean = Mock()
        with patch.object(RUNNER.subprocess, "run", side_effect=OSError("controlled spawn failure")) as run, contextlib.redirect_stdout(io.StringIO()):
            with self.assertRaisesRegex(OSError, "controlled spawn failure"):
                RUNNER.run_token_batches(self.command(), cwd=Path("owned"), env={}, assert_restored=clean)
        self.assertEqual(run.call_count, 1)
        clean.assert_not_called()


if __name__ == "__main__":
    unittest.main()
