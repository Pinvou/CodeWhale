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
        return [{"Package": acceptance.PACKAGE, "Action": "pass", "Test": test}
                for test in sorted(acceptance.REQUIRED)] + [{"Package": acceptance.PACKAGE, "Action": "pass"}]

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


if __name__ == "__main__":
    unittest.main()
