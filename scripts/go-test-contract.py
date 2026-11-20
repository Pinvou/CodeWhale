"""Validate complete Go JSON test lifecycles against declared original cases."""
import json


def read_events(text):
    events = []
    for line in text.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if not isinstance(event, dict) or not isinstance(event.get("Action"), str):
            raise RuntimeError("invalid Go JSON event")
        events.append(event)
    return events


def verify_lifecycles(events, roots, package, *, allow_other_roots=False, allow_extra_children=False):
    if not roots or not package:
        raise RuntimeError("a nonempty named test contract and package are required")
    expected = set(roots)
    for root, children in roots.items():
        expected.update(root + "/" + child for child in children)
    states = {}
    package_state = None
    for event in events:
        action, name = event.get("Action"), event.get("Test", "")
        if action == "build-fail":
            raise RuntimeError("Go test input did not build")
        if isinstance(action, str) and action.startswith("build-"):
            if name:
                raise RuntimeError("build diagnostics cannot be test execution evidence")
            continue
        if event.get("Package") != package:
            raise RuntimeError("Go test evidence came from an unexpected package")
        if action == "fail":
            raise RuntimeError("Go test contract failed")
        if action == "start" and not name:
            if package_state is not None or states:
                raise RuntimeError("duplicate or late Go package start")
            package_state = "running"
            continue
        if package_state != "running":
            raise RuntimeError("Go evidence is outside the running package lifecycle")
        if not name:
            if action == "pass":
                if expected - states.keys() or any(state != "passed" for state in states.values()):
                    raise RuntimeError("Go package completed without all required cases")
                package_state = "passed"
            elif action != "output":
                raise RuntimeError("unexpected Go package event")
            continue
        root = name.split("/", 1)[0]
        if root not in roots:
            if allow_other_roots:
                continue
            raise RuntimeError("unexpected test root in the selected Go package")
        if name not in expected and not allow_extra_children:
            raise RuntimeError("unexpected child case; update the reviewed case contract")
        if action == "skip":
            raise RuntimeError("required Go root or child case skipped")
        if action == "run":
            if name in states:
                raise RuntimeError("duplicate Go test execution")
            if name != root and states.get(root) not in {"running", "paused"}:
                raise RuntimeError("Go child started outside its root lifecycle")
            states[name] = "running"
        elif action == "pause":
            if states.get(name) != "running":
                raise RuntimeError("Go test paused without running")
            states[name] = "paused"
        elif action == "cont":
            if states.get(name) != "paused":
                raise RuntimeError("Go test continued without pausing")
            states[name] = "running"
        elif action == "pass":
            if states.get(name) != "running":
                raise RuntimeError("Go test passed without one matching run")
            if any(child.startswith(name + "/") and state != "passed" for child, state in states.items()):
                raise RuntimeError("Go parent passed before its children completed")
            states[name] = "passed"
        elif action != "output":
            raise RuntimeError("unexpected Go test event")
    if package_state != "passed" or expected - states.keys() or any(state != "passed" for state in states.values()):
        raise RuntimeError("incomplete or truncated Go test lifecycle")
    return len(states)
