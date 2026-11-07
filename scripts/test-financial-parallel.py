"""Run all financial groups concurrently on four separately admitted CI services.

Each worker retains the original exclusive-role checks, UUID databases, real
migrations, race detector, required roots/subcases and strict cleanup. No two
workers may share an endpoint or PostgreSQL system identifier.
"""
import concurrent.futures
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
FIN = runpy.run_path(str(ROOT / "scripts/test-token-financial-postgres.py"))
GROUPS = FIN["GROUPS"]


def service_bindings(raw):
    services = json.loads(raw)
    if not isinstance(services, dict) or set(services) != set(GROUPS):
        raise ValueError("all four financial groups require an admitted service")
    ports, systems = set(), set()
    bindings = {}
    for group in GROUPS:
        service = services[group]
        if not isinstance(service, dict) or set(service) != {"url", "port", "system", "directory"}:
            raise ValueError("invalid financial service metadata")
        port, system = service["port"], service["system"]
        if (not isinstance(port, str) or not re.fullmatch(r"[1-9][0-9]{3,4}", port)
                or not 1024 <= int(port) <= 65535
                or not isinstance(system, str) or not re.fullmatch(r"[1-9][0-9]*", system)
                or service["directory"] != "/var/lib/postgresql/data"
                or service["url"] != f"postgres://postgres@127.0.0.1:{port}/postgres?sslmode=disable"
                or port in ports or system in systems):
            raise ValueError("financial services must have distinct verified identities")
        ports.add(port)
        systems.add(system)
        bindings[group] = {
            "PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL": service["url"],
            "PINVOU_ACCOUNT_CI_PG_PORT": port,
            "PINVOU_ACCOUNT_CI_PG_SYSTEM_ID": system,
            "PINVOU_ACCOUNT_CI_PG_DIRECTORY": service["directory"],
        }
    return bindings


def run_group(group, binding, artifacts):
    started = time.monotonic()
    path = artifacts / (group + ".log")
    with path.open("w", encoding="utf-8") as log:
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/test-token-financial-postgres.py"), "--group", group],
            cwd=ROOT, env={**os.environ, **binding}, stdout=log, stderr=subprocess.STDOUT,
            check=False,
        )
        if group == "owners" and result.returncode == 0:
            # Original owner fixtures have restored their absent actors. The
            # remaining original families must also pass on this same admitted
            # service before the owners worker can report success.
            result = subprocess.run(
                [sys.executable, str(ROOT / "scripts/test-account-financial-families-postgres.py")],
                cwd=ROOT, env={**os.environ, **binding,
                              "PINVOU_FINANCIAL_CHILD_ARTIFACTS": str(artifacts / "owners-families")},
                stdout=log, stderr=subprocess.STDOUT, check=False,
            )
    return {"group": group, "exit_code": result.returncode,
            "seconds": round(time.monotonic() - started, 3), "log": str(path)}


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true":
        raise RuntimeError("parallel runner requires verified CI service identities")
    bindings = service_bindings(os.environ["ACCOUNT_FINANCIAL_SERVICES"])
    artifacts = Path(tempfile.mkdtemp(prefix="financial-parallel-", dir=os.environ["RUNNER_TEMP"]))
    print(f"Financial parallel logs: {artifacts}", flush=True)
    results = []
    # Bound concurrency to four. Finish every worker so its strict fixture
    # cleanup and diagnostic logs are retained even if another worker fails.
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(GROUPS)) as pool:
        futures = {pool.submit(run_group, group, bindings[group], artifacts): group for group in GROUPS}
        for future in concurrent.futures.as_completed(futures):
            group = futures[future]
            try:
                result = future.result()
            except Exception as error:
                result = {"group": group, "exit_code": -1, "error": str(error)}
            results.append(result)
            print(json.dumps(result), flush=True)
            if "log" in result:
                with Path(result["log"]).open(encoding="utf-8") as log:
                    shutil.copyfileobj(log, sys.stdout)
                sys.stdout.flush()
    (artifacts / "summary.json").write_text(json.dumps(results, indent=2) + "\n", encoding="utf-8")
    if len(results) != len(GROUPS) or any(result["exit_code"] for result in results):
        raise RuntimeError("financial group failed; logs and summary retained at " + str(artifacts))
    print("All four financial groups completed; no root or child-case checks were relaxed", flush=True)


if __name__ == "__main__":
    main()
