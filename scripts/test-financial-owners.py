"""Run existing owners phases in one Job, with separately admitted PostgreSQLs.

Each lane retains its original absent-role, migration, race, coverage and
retirement gates. Insufficient CPU/memory selects explicit serial execution;
unverified or shared database identities never select a fallback.
"""
import concurrent.futures
import json
import math
import os
from pathlib import Path
import re
import runpy
import subprocess
import sys
import threading
import time

ROOT = Path(__file__).resolve().parents[1]
FIN = runpy.run_path(str(ROOT / "scripts/test-token-financial-postgres.py"))
LANES = FIN["OWNER_LANES"]
# Map by phase identity: main's schema partition adds a phase without changing
# the original root sets or shifting another lane's coverage requirement.
REQUIRED = {
    "owner-ledger": FIN["MIGRATIONS_REQUIRED"],
    "owner-account": FIN["ACCOUNT_REQUIRED"],
    "owner-wallet": FIN["OWNER_REQUIRED"],
    "owner-token-schema": FIN["TOKEN_SCHEMA_REQUIRED"],
    "owner-provisioning": FIN["PROVISIONING_REQUIRED"],
    "lifecycle-reconciliation": FIN["LIFECYCLE_RECONCILIATION_REQUIRED"],
    **dict(FIN["schema_batches"]()),
}
OUTPUT_LOCK = threading.Lock()


def service_bindings(raw):
    services = json.loads(raw)
    if not isinstance(services, dict) or set(services) != set(LANES):
        raise ValueError("all owners lanes require an admitted service")
    ports, systems, ids = set(), set(), set()
    bindings = {}
    for lane, service in services.items():
        if not isinstance(service, dict) or set(service) != {"url", "port", "system", "directory", "id"}:
            raise ValueError("invalid owners service metadata")
        port, system, identifier = service["port"], service["system"], service["id"]
        if (not isinstance(port, str) or not re.fullmatch(r"[1-9][0-9]{3,4}", port)
                or not 1024 <= int(port) <= 65535
                or not isinstance(system, str) or not re.fullmatch(r"[1-9][0-9]*", system)
                or not isinstance(identifier, str) or not re.fullmatch(r"[0-9a-f]{64}", identifier)
                or service["directory"] != "/var/lib/postgresql/data"
                or service["url"] != f"postgres://postgres@127.0.0.1:{port}/postgres?sslmode=disable"
                or port in ports or system in systems or identifier in ids):
            raise ValueError("owners services must have distinct verified identities")
        ports.add(port); systems.add(system); ids.add(identifier)
        bindings[lane] = {
            "PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL": service["url"],
            "PINVOU_ACCOUNT_CI_PG_PORT": port,
            "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID": system,
            "PINVOU_ACCOUNT_CI_PG_DIRECTORY": service["directory"],
        }
    return bindings


def resource_plan(env, affinity, *, cpu_max=None, memory_max=None):
    cpus = sorted(affinity)
    if not cpus or any(type(cpu) is not int or cpu < 0 for cpu in cpus):
        raise ValueError("owners requires observable CPU affinity")
    cpu = float(len(cpus))
    memory = math.inf
    for key, current in (("PINVOU_CI_CPU_CORES", cpu), ("PINVOU_CI_MEMORY_GIB", memory)):
        if key in env:
            value = float(env[key])
            if not math.isfinite(value) or value <= 0:
                raise ValueError("invalid owners runner resource declaration")
            if key.endswith("CORES"):
                cpu = min(current, value)
            else:
                memory = value * 1024**3
    if cpu_max:
        quota, period = cpu_max.split()
        if quota != "max":
            if int(quota) <= 0 or int(period) <= 0:
                raise ValueError("invalid cgroup CPU quota")
            cpu = min(cpu, int(quota) / int(period))
    if memory_max and memory_max.strip() != "max":
        if int(memory_max) <= 0:
            raise ValueError("invalid cgroup memory limit")
        memory = min(memory, int(memory_max))
    # Unknown memory must not authorize additional parallelism. The three PG
    # containers have independently admitted hard bounds of 2/1/1 CPU, 1 GiB
    # each. Go worker affinity + GOMAXPROCS bounds each worker to two CPUs;
    # GOMEMLIMIT is a soft runtime limit, inside the unchanged Runner hard cap.
    parallel = cpu >= 10 and len(cpus) >= 6 and math.isfinite(memory) and memory >= 12 * 1024**3
    sets = {lane: cpus[index * 2:index * 2 + 2] if parallel else cpus[:max(1, min(2, int(cpu)))]
            for index, lane in enumerate(LANES)}
    return {"mode": "parallel" if parallel else "serial-resource-bound",
            "workers": 3 if parallel else 1, "cpu_limit": cpu,
            "memory_limit_bytes": int(memory) if math.isfinite(memory) else None,
            "cpu_sets": sets, "go_memory_limit": bounded_go_memory(env.get("GOMEMLIMIT"))}


def bounded_go_memory(value):
    if value is None or value == "off":
        return "2GiB"
    match = re.fullmatch(r"([0-9]+)(B|KiB|MiB|GiB|TiB)", value)
    if not match or int(match[1]) <= 0:
        raise ValueError("invalid inherited Go memory limit")
    size = int(match[1]) * 1024 ** ("B", "KiB", "MiB", "GiB", "TiB").index(match[2])
    return value if size < 2 * 1024**3 else "2GiB"


def cgroup_limit(name):
    path = Path("/sys/fs/cgroup") / name
    return path.read_text().strip() if path.is_file() else None


def verify_lane(lane, ledger):
    receipt = json.loads((ledger / f"owner-{lane}-process.json").read_text())
    if (receipt.get("schema") != 1 or receipt.get("kind") != "process"
            or receipt.get("state") != "released" or receipt.get("cleanup_failed") is not False
            or receipt.get("exit_code") != 0):
        raise RuntimeError("owners worker retirement was not confirmed")
    directory = ledger / f"owner-{lane}-process-resources"
    profiles = {}
    for path in directory.glob("financial-phase-*.txt"):
        value = json.loads(path.read_text())
        if value["phase"] in profiles:
            raise RuntimeError("duplicate owners phase evidence")
        profiles[value["phase"]] = value
    expected = set(LANES[lane]) | {"fixture-template-isolation"}
    if lane == "wallet":
        expected.add("owner-wallet-scope-isolation")
    if set(profiles) != expected:
        raise RuntimeError("owners phase coverage is incomplete")
    for label, value in profiles.items():
        roots = REQUIRED.get(label, {"TestDatabaseTemplateIsolationPostgres"} if label == "fixture-template-isolation"
                             else {"TestOwnerWalletTemplateScopePostgres"})
        if (value.get("exit_code") != 0 or set(value.get("root_passes", [])) != roots
                or value.get("templates", {}).get("remaining") != 0):
            raise RuntimeError("owners coverage or snapshot retirement failed")
    return profiles


def run_lane(lane, binding, ledger, plan):
    started = time.monotonic()
    receipt = ledger / f"owner-{lane}-process.json"
    env = {**os.environ, **binding, "PINVOU_CI_OWNER_CPU_SET": json.dumps(plan["cpu_sets"][lane]),
           "GOMAXPROCS": str(len(plan["cpu_sets"][lane])), "GOMEMLIMIT": plan["go_memory_limit"],
           "OMP_NUM_THREADS": "2", "RAYON_NUM_THREADS": "2", "CMAKE_BUILD_PARALLEL_LEVEL": "2"}
    command = [sys.executable, str(ROOT / "scripts/ci-fixture-process.py"), "--receipt", str(receipt), "--",
               sys.executable, str(ROOT / "scripts/test-token-financial-postgres.py"), "--group", "owners",
               "--owner-lane", lane, "--verify-fixture-cache"]
    with (ledger / f"owner-{lane}.log").open("w", encoding="utf-8") as log:
        with subprocess.Popen(command, cwd=ROOT, env=env, stdout=subprocess.PIPE,
                              stderr=subprocess.STDOUT, text=True, encoding="utf-8") as process:
            for line in process.stdout:
                log.write(line)
                with OUTPUT_LOCK:
                    sys.stdout.write(f"[{lane}] " + line)
                    sys.stdout.flush()
            code = process.wait()
    profiles = verify_lane(lane, ledger) if code == 0 else {}
    return {"lane": lane, "exit_code": code, "seconds": time.monotonic() - started,
            "phases": {label: value["elapsed_seconds"] for label, value in profiles.items()}}


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("owners coordinator requires verified CI service identities")
    bindings = service_bindings(os.environ["ACCOUNT_FINANCIAL_OWNER_SERVICES"])
    # The schema lane must be the very same service admitted by the parent.
    if any(os.environ.get(key) != value for key, value in bindings["schema"].items()):
        raise ValueError("owners parent PostgreSQL identity mismatch")
    ledger = Path(os.environ["PINVOU_CI_RESOURCE_LEDGER"])
    plan = resource_plan(os.environ, os.sched_getaffinity(0), cpu_max=cgroup_limit("cpu.max"),
                         memory_max=cgroup_limit("memory.max"))
    print("Owners execution plan: " + json.dumps(plan), flush=True)
    started, results = time.monotonic(), []
    with concurrent.futures.ThreadPoolExecutor(max_workers=plan["workers"]) as pool:
        futures = {pool.submit(run_lane, lane, binding, ledger, plan): lane for lane, binding in bindings.items()}
        # A failing lane never cancels siblings: all owned fixtures still run
        # their existing cleanup; the always-run service stop remains mandatory.
        for future in concurrent.futures.as_completed(futures):
            lane = futures[future]
            try:
                results.append(future.result())
            except Exception as error:
                results.append({"lane": lane, "exit_code": -1, "error_class": type(error).__name__})
    summary = {"schema": 1, **plan, "seconds": time.monotonic() - started, "results": results,
               "coverage": "all declared owners roots and phases; race and required child-case gates unchanged"}
    # .json is reserved for resource ownership leases in this directory.
    (ledger / "owners-summary.txt").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print("Owners result: " + json.dumps(summary), flush=True)
    if len(results) != len(LANES) or any(result["exit_code"] for result in results):
        raise RuntimeError("owners lane failed; diagnostics and retirement receipts retained")


if __name__ == "__main__":
    main()
