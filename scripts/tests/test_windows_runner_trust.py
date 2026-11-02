"""Replay checked-in Windows runner expressions with trusted/fork PR payloads.

These tests cover the routing policy, not an ACL: a fork can edit workflow YAML.
Persistent runner access also requires independently enforced administrator policy.
"""
import ast
import copy
import json
import math
from pathlib import Path
import re
import unittest
from ci_routing_test_support import default_ci_routing


ROOT = Path(__file__).resolve().parents[2]
EXPECTED = {
    "pr-check": {
        "backflow-portability": "LIGHT",
        "windows-relay-dacl": "LIGHT",
        "windows-relay-tools-test": "LIGHT",
        "release-contract-test": "MEDIUM",
        "windows-static-updater-contract": "LIGHT",
        "windows-rust-test": "HEAVY",
        "windows-rustup-repair-test": "HEAVY",
        "windows-codex-runtime-test": "LIGHT",
        "windows-runtime-test": "LIGHT",
    },
    "release-packages": {
        "remote-control-release-gate": "REMOTE",
        "validate-windows-runtime-pr": "HEAVY",
        "build-windows-x64": "HEAVY",
    },
    "connector-verify": {"verify-connectors": "LIGHT"},
    "rustc-wrapper-smoke": {"smoke": "LIGHT"},
}
EVENTS = {
    "pr-check": {"pull_request", "push", "merge_group"},
    "release-packages": {"pull_request", "push", "workflow_dispatch"},
    "connector-verify": {"pull_request", "push", "workflow_dispatch"},
    "rustc-wrapper-smoke": {"pull_request", "push", "merge_group"},
}
TOKEN = re.compile(r"'(?:[^']|'')*'|[A-Za-z_][A-Za-z0-9_.-]*|&&|\|\||==|!=|[(),]")


def _truth(value):
    # GitHub arrays/objects remain truthy, including empty arrays.
    return isinstance(value, (list, dict)) or bool(value)


def _number(value):
    if value is None or value == "":
        return 0
    if isinstance(value, (bool, int, float)):
        return float(value)
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
            if type(parsed) in (int, float):
                return parsed
        except ValueError:
            pass
    return math.nan


def _equal(left, right):
    # Match Actions' case-insensitive strings and loose numeric comparison of
    # webhook numeric repo IDs with the string github.repository_id context.
    if isinstance(left, str) and isinstance(right, str):
        return left.casefold() == right.casefold()
    if type(left) is not type(right):
        return _number(left) == _number(right)
    if isinstance(left, (list, dict)):
        return left is right
    return left == right


def evaluate_route(expression, context):
    """Evaluate the small Actions expression subset actually used by runs-on.

    Unknown syntax is rejected, and BoolOp is short-circuited so an untrusted
    route must not even parse a malformed JSON self-hosted override.
    """
    if not expression.startswith("${{") or not expression.endswith("}}"):
        raise ValueError("runs-on must be a complete Actions expression")
    source = expression[3:-2].strip()
    variables = {}
    translated = []
    position = 0
    for match in TOKEN.finditer(source):
        if source[position:match.start()].strip():
            raise ValueError("unrecognized Actions expression token")
        token = match.group()
        position = match.end()
        if token.startswith("'"):
            translated.append(repr(token[1:-1].replace("''", "'")))
        elif token in {"&&", "||"}:
            translated.append({"&&": "and", "||": "or"}[token])
        elif token in {"fromJSON", "==", "!=", "(", ")", ","}:
            translated.append(token)
        elif token in {"true", "false", "null"}:
            translated.append({"true": "True", "false": "False", "null": "None"}[token])
        else:
            value = context
            for part in token.split("."):
                value = value.get(part, "") if isinstance(value, dict) else ""
            name = "value_" + str(len(variables))
            variables[name] = value
            translated.append(name)
    if source[position:].strip():
        raise ValueError("unrecognized Actions expression suffix")

    def visit(node):
        if isinstance(node, ast.Constant):
            return node.value
        if isinstance(node, ast.Name):
            return variables[node.id]
        if isinstance(node, ast.BoolOp):
            result = visit(node.values[0])
            for value in node.values[1:]:
                if isinstance(node.op, ast.And) and not _truth(result):
                    return result
                if isinstance(node.op, ast.Or) and _truth(result):
                    return result
                result = visit(value)
            return result
        if isinstance(node, ast.Compare) and len(node.ops) == 1:
            same = _equal(visit(node.left), visit(node.comparators[0]))
            if isinstance(node.ops[0], ast.Eq):
                return same
            if isinstance(node.ops[0], ast.NotEq):
                return not same
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Name)
                and node.func.id == "fromJSON" and len(node.args) == 1
                and not node.keywords):
            return json.loads(visit(node.args[0]))
        raise ValueError("unrecognized Actions expression syntax")

    return visit(ast.parse(" ".join(translated), mode="eval").body)


def context(event="pull_request", variables=None):
    repo = {"full_name": "Pinvou/pinvou3", "id": 123456}
    return {
        "github": {
            "event_name": event,
            "repository": repo["full_name"],
            "repository_id": str(repo["id"]),
            "event": {"pull_request": {
                "head": {"repo": dict(repo)},
                "base": {"repo": dict(repo)},
                "author_association": "CONTRIBUTOR",
            }},
        },
        "vars": variables or {},
        "matrix": {"os": "windows-latest", "runs-on": "windows-latest"},
    }


def configured_pools():
    return {
        **{f"PINVOU_WINDOWS_{pool}_RUNNER": json.dumps(["self-hosted", "Windows", pool])
           for pool in ("HEAVY", "MEDIUM", "LIGHT")},
        "PINVOU_WINDOWS_RUNNER": '["self-hosted", "Windows", "legacy"]',
        "PINVOU_REMOTE_RELEASE_RUNNER": "windows-release-signer",
        "PINVOU_LINUX_RUNNER": '["self-hosted", "Linux", "legacy"]',
        "PINVOU_LINUX_LIGHT_RUNNER": '["self-hosted", "Linux", "LIGHT"]',
        "PINVOU_LINUX_MEDIUM_RUNNER": '["self-hosted", "Linux", "MEDIUM"]',
    }


def windows_routes():
    result = {}
    for workflow in EXPECTED:
        source = (ROOT / f".github/workflows/{workflow}.yml").read_text(encoding="utf-8")
        jobs = re.findall(r"(?ms)^  ([\w-]+):\n(.*?)(?=^  [\w-]+:\n|\Z)", source)
        result[workflow] = {}
        for job, body in jobs:
            routes = re.findall(r"(?m)^    runs-on: (.+)$", body)
            if any("PINVOU_WINDOWS_" in route or "PINVOU_REMOTE_RELEASE_RUNNER" in route
                   for route in routes):
                if len(routes) != 1:
                    raise ValueError("expected exactly one job-level runs-on")
                result[workflow][job] = routes[0]
    return result


def labels(value):
    return value if isinstance(value, list) else [value]


class WindowsRunnerTrustTests(unittest.TestCase):
    def setUp(self):
        self.routes = windows_routes()

    def each_route(self):
        for workflow, jobs in self.routes.items():
            for job, expression in jobs.items():
                yield workflow, job, expression

    def test_all_windows_override_surfaces_are_covered(self):
        self.assertEqual({key: set(value) for key, value in self.routes.items()},
                         {key: set(value) for key, value in EXPECTED.items()})
        self.assertEqual(sum(map(len, self.routes.values())), 14)
        for workflow, events in EVENTS.items():
            source = (ROOT / f".github/workflows/{workflow}.yml").read_text(encoding="utf-8")
            trigger = re.search(r"(?ms)^on:\n(.*?)(?=^[A-Za-z_][\w-]*:|\Z)", source).group(1)
            declared = set(re.findall(r"(?m)^  ([a-z_]+):", trigger))
            with self.subTest(workflow=workflow):
                self.assertEqual(declared, events)

    def test_fork_and_incomplete_or_inconsistent_metadata_use_hosted(self):
        cases = []
        fork = context(variables=configured_pools())
        fork["github"]["event"]["pull_request"]["head"]["repo"] = {
            "full_name": "outside/pinvou3", "id": 654321,
        }
        for association in ("FIRST_TIMER", "CONTRIBUTOR", "MEMBER", "COLLABORATOR", "OWNER"):
            value = copy.deepcopy(fork)
            value["github"]["event"]["pull_request"]["author_association"] = association
            cases.append(("fork-" + association, value))
        for side in ("head", "base"):
            for field in ("full_name", "id"):
                for bad in (None, "", 0, "other/repo" if field == "full_name" else 654321):
                    value = context(variables=configured_pools())
                    value["github"]["event"]["pull_request"][side]["repo"][field] = bad
                    cases.append((f"{side}-{field}-{bad}", value))
                value = context(variables=configured_pools())
                del value["github"]["event"]["pull_request"][side]["repo"][field]
                cases.append((f"missing-{side}-{field}", value))
            value = context(variables=configured_pools())
            value["github"]["event"]["pull_request"][side]["repo"] = None
            cases.append((f"deleted-{side}-repo", value))
        for field in ("repository", "repository_id"):
            value = context(variables=configured_pools())
            del value["github"][field]
            cases.append(("missing-context-" + field, value))
        for missing in ({}, {"pull_request": {}}, {"pull_request": None}):
            value = context(variables=configured_pools())
            value["github"]["event"] = missing
            cases.append(("missing-pr-" + repr(missing), value))
        for workflow, job, expression in self.each_route():
            for name, value in cases:
                with self.subTest(workflow=workflow, job=job, case=name):
                    self.assertEqual(labels(evaluate_route(expression, value)), ["windows-latest"])

    def test_untrusted_route_does_not_decode_any_runner_override(self):
        value = context(variables={key: "not JSON" for key in configured_pools()})
        del value["github"]["event"]["pull_request"]["head"]
        for workflow, job, expression in self.each_route():
            with self.subTest(workflow=workflow, job=job):
                self.assertEqual(labels(evaluate_route(expression, value)), ["windows-latest"])

    def test_default_hosted_renderer_keeps_unknown_trust_edits_visible(self):
        expression = self.routes["pr-check"]["windows-runtime-test"]
        source = "    runs-on: " + expression
        self.assertEqual(default_ci_routing(source), "    runs-on: windows-latest")
        for clause in (
            "github.event.pull_request.head.repo.full_name == github.repository",
            "github.event.pull_request.base.repo.full_name == github.repository",
            "github.event.pull_request.head.repo.id == github.repository_id",
            "github.event.pull_request.base.repo.id == github.repository_id",
        ):
            with self.subTest(removed=clause):
                changed = source.replace(clause, "true")
                self.assertNotEqual(changed, source)
                rendered = default_ci_routing(changed)
                self.assertIn("github.event_name", rendered)
                self.assertIn("true", rendered)
                self.assertNotEqual(rendered, "    runs-on: windows-latest")

    def test_same_repository_pr_keeps_pool_precedence_and_fallbacks(self):
        for workflow, job, expression in self.each_route():
            pool = EXPECTED[workflow][job]
            for variables, expected in (
                (configured_pools(), ["windows-release-signer"] if pool == "REMOTE"
                 else ["self-hosted", "Windows", pool]),
                ({"PINVOU_WINDOWS_RUNNER": '["self-hosted", "Windows", "legacy"]'},
                 ["windows-latest"] if pool == "REMOTE" else ["self-hosted", "Windows", "legacy"]),
                ({}, ["windows-latest"]),
            ):
                for string_ids in (False, True):
                    value = context(variables=variables)
                    if string_ids:
                        for side in ("head", "base"):
                            value["github"]["event"]["pull_request"][side]["repo"]["id"] = "123456"
                    with self.subTest(workflow=workflow, job=job, variables=variables, string_ids=string_ids):
                        self.assertEqual(labels(evaluate_route(expression, value)), expected)

    def test_existing_non_pr_event_routing_is_preserved_without_pr_metadata(self):
        for workflow, job, expression in self.each_route():
            pool = EXPECTED[workflow][job]
            for event in EVENTS[workflow] - {"pull_request"}:
                value = context(event, configured_pools())
                value["github"]["event"] = {}
                expected = (["windows-release-signer"] if pool == "REMOTE"
                            else ["self-hosted", "Windows", pool])
                with self.subTest(workflow=workflow, job=job, event=event):
                    self.assertEqual(labels(evaluate_route(expression, value)), expected)

    def test_missing_unknown_and_undeclared_events_use_hosted(self):
        for workflow, job, expression in self.each_route():
            unknown = {"", "schedule", "workflow_run", "pull_request_target", "repository_dispatch"}
            unknown |= {"push", "workflow_dispatch", "merge_group"} - EVENTS[workflow]
            for event in [None] + sorted(unknown):
                # Keep otherwise valid same-repository PR metadata: an unknown
                # event cannot gain pool access by carrying a copied PR payload.
                value = context(variables=configured_pools())
                if event is None:
                    del value["github"]["event_name"]
                else:
                    value["github"]["event_name"] = event
                with self.subTest(workflow=workflow, job=job, event=event):
                    self.assertEqual(labels(evaluate_route(expression, value)), ["windows-latest"])
            for missing in ({}, None):
                value = context(variables=configured_pools())
                value["github"] = missing
                with self.subTest(workflow=workflow, job=job, github=missing):
                    self.assertEqual(labels(evaluate_route(expression, value)), ["windows-latest"])

    def test_matrix_non_windows_legs_keep_the_existing_route(self):
        for workflow, job, expression in self.each_route():
            if "matrix." not in expression:
                continue
            for os in ("ubuntu-22.04", "ubuntu-22.04-arm", "macos-15", "macos-15-intel"):
                for trusted in (False, True):
                    value = context(variables=configured_pools())
                    value["matrix"] = {"os": os, "runs-on": os}
                    if not trusted:
                        value["github"]["event"] = {}
                    expected = (["self-hosted", "Linux", EXPECTED[workflow][job]]
                                if os == "ubuntu-22.04" else [os])
                    with self.subTest(workflow=workflow, job=job, os=os, trusted=trusted):
                        self.assertEqual(labels(evaluate_route(expression, value)), expected)


if __name__ == "__main__":
    unittest.main()
