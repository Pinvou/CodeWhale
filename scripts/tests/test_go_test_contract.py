import importlib.util
import json
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("go_test_contract", Path(__file__).resolve().parents[1] / "go-test-contract.py")
CONTRACT = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CONTRACT)


class GoTestContractTests(unittest.TestCase):
    package = "example.test/domain"
    roots = {"TestOriginal": ["first", "slash/name"]}

    def event(self, action, name=None):
        value = {"Action": action, "Package": self.package}
        if name:
            value["Test"] = name
        return value

    def events(self):
        return [self.event("start"), self.event("run", "TestOriginal"),
                self.event("run", "TestOriginal/first"), self.event("pass", "TestOriginal/first"),
                self.event("run", "TestOriginal/slash/name"), self.event("pass", "TestOriginal/slash/name"),
                self.event("pass", "TestOriginal"), self.event("pass")]

    def verify(self, events):
        return CONTRACT.verify_lifecycles(events, self.roots, self.package)

    def test_complete_named_slash_cases_and_json_roundtrip(self):
        events = CONTRACT.read_events("\n".join(json.dumps(event) for event in self.events()))
        self.assertEqual(self.verify(events), 3)

    def test_missing_start_run_case_or_terminal_is_rejected(self):
        events = self.events()
        for index in range(len(events)):
            with self.subTest(index=index), self.assertRaises(RuntimeError):
                self.verify(events[:index] + events[index + 1:])
        with self.assertRaises(RuntimeError):
            self.verify([event for event in events if event.get("Test") != "TestOriginal/first"])

    def test_skip_failure_and_wrong_package_cannot_hide_behind_parent_pass(self):
        for action in ("skip", "fail"):
            events = self.events()
            events[3] = self.event(action, "TestOriginal/first")
            with self.subTest(action=action), self.assertRaises(RuntimeError):
                self.verify(events)
        with self.assertRaises(RuntimeError):
            self.verify([{**event, "Package": "another/package"} for event in self.events()])

    def test_duplicate_or_out_of_order_execution_is_rejected(self):
        events = self.events()
        for invalid in (events[:3] + [events[2]] + events[3:],
                        [events[0], events[2], events[1], *events[3:]],
                        [*events[:3], events[6], *events[3:6], events[7]],
                        events + [events[-1]]):
            with self.subTest(invalid=invalid), self.assertRaises(RuntimeError):
                self.verify(invalid)

    def test_unexpected_root_or_case_is_rejected(self):
        for name in ("TestReplacement", "TestOriginal/new-case"):
            events = self.events()
            with self.subTest(name=name), self.assertRaises(RuntimeError):
                self.verify(events[:2] + [self.event("run", name), self.event("pass", name)] + events[2:])

    def test_parallel_pause_continuation_is_real_lifecycle(self):
        events = self.events()
        self.assertEqual(self.verify(events[:3] + [self.event("pause", "TestOriginal/first"), self.event("cont", "TestOriginal/first")] + events[3:]), 3)
        with self.assertRaises(RuntimeError):
            self.verify(events[:3] + [self.event("cont", "TestOriginal/first")] + events[3:])

    def test_build_output_zero_hits_and_invalid_json_are_not_execution(self):
        with self.assertRaises(RuntimeError):
            self.verify([{"Action": "build-output", "Output": "compiled"}])
        with self.assertRaises(RuntimeError):
            self.verify([{"Action": "build-fail"}, *self.events()])
        with self.assertRaises((ValueError, RuntimeError)):
            CONTRACT.read_events('{"Action":"start"}\n{"Action":')
        with self.assertRaises(RuntimeError):
            CONTRACT.read_events("[]")

    def test_partial_existing_package_keeps_required_cases_mandatory(self):
        events = self.events()
        extra = [self.event("run", "TestOther"), self.event("pass", "TestOther"),
                 self.event("run", "TestOriginal/extra"), self.event("pass", "TestOriginal/extra")]
        self.assertEqual(CONTRACT.verify_lifecycles(events[:6] + extra + events[6:], self.roots, self.package, allow_other_roots=True, allow_extra_children=True), 4)
        with self.assertRaises(RuntimeError):
            CONTRACT.verify_lifecycles([event for event in events if event.get("Test") != "TestOriginal/first"], self.roots, self.package, allow_other_roots=True, allow_extra_children=True)


if __name__ == "__main__":
    unittest.main()
