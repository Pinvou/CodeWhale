import contextlib
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("financial_parallel", ROOT / "scripts/test-financial-parallel.py")
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)


class FinancialParallelTests(unittest.TestCase):
    def test_weighted_shards_cover_new_roots_once_and_balance_long_roots(self):
        roots = {"slow", "medium", "third", "fourth", "new-root"}
        durations = {"default_seconds": 20, "seconds": {"slow": 100, "medium": 80, "third": 70, "fourth": 60, "removed": 1000}}
        partition = RUNNER.FIN["billing_partitions"](roots, durations)
        self.assertEqual(len(partition), 4)
        self.assertEqual(set().union(*partition.values()), roots)
        self.assertEqual(sum(map(len, partition.values())), len(roots))
        self.assertEqual(partition, RUNNER.FIN["billing_partitions"](reversed(sorted(roots)), durations))
        loads = [sum(durations["seconds"].get(name, 20) for name in bucket) for bucket in partition.values()]
        self.assertLessEqual(max(loads), 100)
        for value in (0, -1, float("nan"), float("inf"), True, "slow"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                RUNNER.FIN["billing_partitions"](roots, {**durations, "default_seconds": value})

    def services(self):
        return {group: {"url": f"postgres://postgres@127.0.0.1:{49173+i}/postgres?sslmode=disable",
                        "port": str(49173+i), "system": str(100+i),
                        "directory": "/var/lib/postgresql/data"}
                for i, group in enumerate(RUNNER.GROUPS)}

    def test_all_four_distinct_bindings_required_before_any_worker(self):
        good = self.services()
        self.assertEqual(set(RUNNER.service_bindings(json.dumps(good))), set(RUNNER.GROUPS))
        for field in ("port", "system", "directory", "url"):
            bad = copy.deepcopy(good)
            bad["token"][field] = good["owners"][field] if field != "directory" else "/data"
            with self.subTest(field=field), self.assertRaises(ValueError):
                RUNNER.service_bindings(json.dumps(bad))
        for bad in ({}, {**good, "extra": good["owners"]}, [], {**good, "token": None}):
            with self.assertRaises(ValueError):
                RUNNER.service_bindings(json.dumps(bad))
        for field, value in (("port", "049174"), ("system", "0"), ("url", "postgres://postgres:secret@127.0.0.1:49174/postgres?sslmode=disable")):
            bad = copy.deepcopy(good)
            bad["token"][field] = value
            with self.assertRaises(ValueError):
                RUNNER.service_bindings(json.dumps(bad))
        with patch.dict(os.environ, {"GITHUB_ACTIONS": "true", "ACCOUNT_FINANCIAL_SERVICES": "{}"}), patch.object(RUNNER, "run_group") as worker:
            with self.assertRaises(ValueError):
                RUNNER.main()
            worker.assert_not_called()

    def test_workers_overlap_and_failure_still_waits_for_every_cleanup(self):
        for failure in (None, "exit", "exception"):
            barrier = threading.Barrier(len(RUNNER.GROUPS), timeout=5)
            finished = []
            def worker(group, binding, artifacts):
                barrier.wait()  # would time out if the coordinator serialized workers
                self.assertEqual(binding["PINVOU_ACCOUNT_CI_PG_SYSTEM_ID"], self.services()[group]["system"])
                finished.append(group)
                if failure == "exception" and group == "token":
                    raise OSError("controlled launch failure")
                path = artifacts / (group + ".log")
                path.write_text(group + "\n", encoding="utf-8")
                return dict(group=group, exit_code=7 if failure == "exit" and group == "token" else 0,
                            seconds=1, log=str(path))
            with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {
                "GITHUB_ACTIONS": "true", "RUNNER_TEMP": directory,
                "ACCOUNT_FINANCIAL_SERVICES": json.dumps(self.services()),
            }), patch.object(RUNNER, "run_group", side_effect=worker), contextlib.redirect_stdout(io.StringIO()):
                if failure:
                    with self.assertRaises(RuntimeError):
                        RUNNER.main()
                else:
                    RUNNER.main()
                self.assertEqual(set(finished), set(RUNNER.GROUPS))
                summary = json.loads(next(Path(directory).glob("*/summary.json")).read_text())
                self.assertEqual({result["group"] for result in summary}, set(RUNNER.GROUPS))
                self.assertEqual(any(result["exit_code"] for result in summary), bool(failure))

    def test_worker_keeps_exit_status_and_per_service_identity(self):
        binding = RUNNER.service_bindings(json.dumps(self.services()))["billing-0"]
        with tempfile.TemporaryDirectory() as directory, patch.object(RUNNER.subprocess, "run") as run:
            run.return_value.returncode = 23
            result = RUNNER.run_group("billing-0", binding, Path(directory))
            self.assertEqual(result["exit_code"], 23)
            args, kwargs = run.call_args
            self.assertEqual(args[0][-2:], ["--group", "billing-0"])
            for key, value in binding.items():
                self.assertEqual(kwargs["env"][key], value)
            self.assertFalse(kwargs["check"])


if __name__ == "__main__":
    unittest.main()
