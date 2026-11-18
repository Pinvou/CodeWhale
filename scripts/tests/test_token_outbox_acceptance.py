import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("token_acceptance", Path(__file__).resolve().parents[2] /
    "account-platform/test/integration/token-outbox-acceptance.py")
acceptance = importlib.util.module_from_spec(spec)
spec.loader.exec_module(acceptance)


class TokenAcceptanceEvidenceTests(unittest.TestCase):
    def events(self):
        def event(action, test=None):
            return {"Package": acceptance.PACKAGE, "Action": action, **({"Test": test} if test else {})}
        events = [event("start"), event("run", acceptance.TEST)]
        for name in sorted(acceptance.REQUIRED - {acceptance.TEST}):
            events += [event("run", name), event("pass", name)]
        return events + [event("pass", acceptance.TEST), event("pass")]

    def test_requires_executed_root_children_and_package(self):
        output = lambda events: "\n".join(map(json.dumps, events))
        acceptance.validate(output(self.events()), 0)
        for index in range(len(self.events())):
            events = self.events()
            del events[index]
            with self.assertRaises(ValueError):
                acceptance.validate(output(events), 0)
        for action in ("skip", "fail"):
            events = self.events()
            events[0]["Action"] = action
            with self.assertRaises(ValueError):
                acceptance.validate(output(events), 0)
        for code in (1, 2):
            with self.assertRaises(ValueError):
                acceptance.validate(output(self.events()), code)
        events = self.events()
        events[-1]["Package"] = "another/package"
        with self.assertRaises(ValueError):
            acceptance.validate(output(events), 0)

    def test_rejects_duplicate_runs_passes_and_package_terminals(self):
        for index in range(len(self.events())):
            events = self.events()
            events.insert(index, dict(events[index]))
            with self.subTest(index=index), self.assertRaises(ValueError):
                acceptance.validate("\n".join(map(json.dumps, events)), 0)

    def test_rejects_extra_names_pass_only_and_invalid_order(self):
        mutations = []
        for name in ("TestUnexpected", acceptance.TEST + "/unexpected"):
            events = self.events()
            events.insert(-2, {"Package": acceptance.PACKAGE, "Action": "run", "Test": name})
            events.insert(-2, {"Package": acceptance.PACKAGE, "Action": "pass", "Test": name})
            mutations.append(events)
        mutations.append([e for e in self.events() if e["Action"] == "pass"])
        events = self.events()
        root_pass = events.pop(-2)
        events.insert(2, root_pass)
        mutations.append(events)
        events = self.events()
        events[1], events[2] = events[2], events[1]
        mutations.append(events)
        for events in mutations:
            with self.subTest(events=events), self.assertRaises(ValueError):
                acceptance.validate("\n".join(map(json.dumps, events)), 0)

    def test_rejects_non_json_non_object_and_wrong_package_output(self):
        for suffix in ("not-json", "[]", "null", json.dumps({"Package": "another/package", "Action": "output"})):
            with self.subTest(suffix=suffix), self.assertRaises(ValueError):
                acceptance.validate("\n".join(map(json.dumps, self.events())) + "\n" + suffix, 0)


if __name__ == "__main__":
    unittest.main()
