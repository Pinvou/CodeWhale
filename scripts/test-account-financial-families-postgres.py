"""Run the remaining original account financial families on an admitted empty PG.

Existing financial workers remain mandatory. The owners worker invokes this
additional entry after restoring its original fixtures. Original coordinator
commands, runtime/migration actors, race and internal budgets are preserved.
"""
import base64
import json
import os
from pathlib import Path
import re
import runpy
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[1]
FIN = runpy.run_path(str(ROOT / "scripts/test-token-financial-postgres.py"))
GO = runpy.run_path(str(ROOT / "scripts/go-test-contract.py"))
MANIFEST = ROOT / "scripts/account-financial-families.json"
INPUTS = {"account": "PINVOU_TEST_ACCOUNT_DATABASE_URL", "billing": "PINVOU_TEST_BILLING_DATABASE_URL", "schema": "PINVOU_TEST_SCHEMA_DATABASE_URL"}
STREAMS = {"F11_CHILD_LOG": "F11_GO_BINARY", "F12_CHILD_LOG": "F12_GO_BINARY", "F13_CHILD_LOG": "F13_GO_BINARY", "PINVOU_F09_CHILD_STREAM": "PINVOU_PRIVATE_GO"}
TARGETS = {"./internal/" + name: "github.com/Pinvou/pinvou3/account-platform/internal/" + name for name in ("account", "billing", "billingschema", "f09fixture")}
LAYOUTS = {("./internal/account", "owner"): {"account"}, ("./internal/account", "retained"): {"account", "billing"}, ("./internal/billing", "retained"): {"billing"}, ("./internal/billingschema", "owner"): {"schema"}, ("./internal/f09fixture", "owner"): {"schema"}}
BILLING_SETUP = """BEGIN;
CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
GRANT billing_service,billing_schema_owner TO billing_migrator;
GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE;
COMMIT;"""
ROLE_FACTS = "SELECT COALESCE(jsonb_agg(jsonb_build_array(oid,rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit) ORDER BY rolname),'[]'::jsonb)::text FROM pg_roles WHERE rolname !~ '^pg_'"
DATABASE_FACTS = "SELECT json_agg(row(oid,datname,datdba,datistemplate,datallowconn,datacl)::text ORDER BY datname)::text FROM pg_database"
MEMBER_FACTS = "SELECT COALESCE(jsonb_agg(jsonb_build_array(roleid,member,grantor,admin_option,inherit_option,set_option) ORDER BY roleid,member,grantor),'[]'::jsonb)::text FROM pg_auth_members"
EXTERNAL_BILLING_DEPENDENCIES = """SELECT
 EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) AND dbid<>0)
 OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) OR member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service')) AND member IN(SELECT oid FROM pg_roles WHERE rolname IN('billing_migrator','billing_schema_owner','billing_service'))))"""


def validate_roots(roots):
    if not isinstance(roots, dict) or not roots:
        raise ValueError("each family needs explicit roots and complete child cases")
    for root, children in roots.items():
        if (not re.fullmatch(r"Test[A-Za-z0-9_]+", root) or not isinstance(children, list)
                or any(not isinstance(child, str) or not child or child.startswith("/") for child in children)
                or len(children) != len(set(children))):
            raise ValueError("invalid named family case contract")


def load_manifest(path=MANIFEST):
    data = json.loads(path.read_text(encoding="utf-8"))
    if set(data) != {"schema", "issue_roots", "existing_roots", "phases"} or data["schema"] != 1:
        raise ValueError("unsupported financial family manifest")
    validate_roots(data["existing_roots"])
    existing_required = FIN["ACCOUNT_REQUIRED"] | FIN["RETAINED_REQUIRED"] | FIN["SCHEMA_REQUIRED"] | FIN["OWNER_REQUIRED"]
    if set(data["existing_roots"]) - existing_required:
        raise ValueError("previously covered roots must remain mandatory in the original runner")
    ids, covered, retained = set(), set(data["existing_roots"]), False
    for phase in data["phases"]:
        if (set(phase) != {"id", "target", "package", "roots", "inputs", "mode", "children"}
                or not re.fullmatch(r"[a-z0-9-]+", phase["id"]) or phase["id"] in ids
                or phase["target"] not in TARGETS or phase["package"] != TARGETS[phase["target"]]
                or phase["mode"] not in {"owner", "retained"}
                or not phase["inputs"] or len(set(phase["inputs"])) != len(phase["inputs"])
                or set(phase["inputs"]) - INPUTS.keys()):
            raise ValueError("invalid financial family phase")
        if LAYOUTS.get((phase["target"], phase["mode"])) != set(phase["inputs"]):
            raise ValueError("family inputs do not match their original actor and database layout")
        if retained and phase["mode"] == "owner":
            raise ValueError("absent-actor coordinators must run before retained Billing actors")
        retained |= phase["mode"] == "retained"
        ids.add(phase["id"])
        validate_roots(phase["roots"])
        streams = set()
        for contract in [phase, *phase["children"]]:
            validate_roots(contract["roots"])
            if covered.intersection(contract["roots"]):
                raise ValueError("duplicate original family root")
            covered.update(contract["roots"])
        for child in phase["children"]:
            if (set(child) != {"id", "stream_env", "go_env", "package", "roots"}
                    or not re.fullmatch(r"[a-z0-9-]+", child["id"])
                    or child["stream_env"] not in STREAMS or STREAMS[child["stream_env"]] != child["go_env"]
                    or child["stream_env"] in streams
                    or child["package"] not in {TARGETS["./internal/billing"], "command-line-arguments"}):
                raise ValueError("invalid original coordinator child contract")
            streams.add(child["stream_env"])
    if not data["phases"] or len(data["issue_roots"]) != len(set(data["issue_roots"])) or covered != set(data["issue_roots"]):
        raise ValueError("the issue's complete original root inventory must be covered")
    return data


def query_function(raw):
    psql = os.environ.get("PINVOU_ACCOUNT_TEST_PSQL", "psql")
    identity = "SELECT jsonb_build_array(session_user,current_user,pg_catalog.current_database(),pg_catalog.current_setting('port'),pg_catalog.current_setting('data_directory'),(SELECT system_identifier::text FROM pg_catalog.pg_control_system()))::text"

    def execute(sql, guard=()):
        result = subprocess.run([psql, raw, "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", *FIN["ci_identity_guard"](raw), *guard, "-c", sql], capture_output=True, text=True, encoding="utf-8", check=False, timeout=30)
        if result.returncode:
            raise RuntimeError("owned family PostgreSQL operation failed")
        return result.stdout.strip()

    admitted = execute(identity)
    values = json.loads(admitted)
    if not isinstance(values, list) or len(values) != 6 or not all(isinstance(value, str) and value for value in values):
        raise RuntimeError("coordinator identity could not be recorded")
    # Encode database-sourced identity values instead of interpolating paths or
    # role names as SQL literals. Recheck on the SAME backend before every query.
    encoded = base64.b64encode(admitted.encode("utf-8")).decode("ascii")
    bound_guard = "DO $owned$ BEGIN IF (" + identity + ")::jsonb <> convert_from(decode('" + encoded + "','base64'),'UTF8')::jsonb THEN RAISE EXCEPTION 'owned coordinator identity changed; no operation permitted'; END IF; END $owned$;"

    def query(sql):
        return execute(sql, ("-c", bound_guard))
    query.identity = values
    return query


class OwnedInstance:
    def __init__(self, query):
        self.query = query
        self.user = query("SELECT current_user")
        self.databases = query(DATABASE_FACTS)
        self.roles = json.loads(query(ROLE_FACTS))
        self.members = json.loads(query(MEMBER_FACTS))
        if (query("SELECT string_agg(datname,',' ORDER BY datname) FROM pg_database") != "postgres,template0,template1"
                or query("SELECT string_agg(rolname,',' ORDER BY rolname) FROM pg_roles WHERE rolname !~ '^pg_'") != self.user
                or query("SELECT (session_user=current_user AND (SELECT rolsuper FROM pg_roles WHERE rolname=session_user))::text") != "true"
                or int(query("SHOW server_version_num")) < 160000):
            raise RuntimeError("an exclusive empty disposable PG16+ coordinator is required")
        self.retained_roles = None
        self.retained_members = None
        self.assert_restored()

    def assert_restored(self):
        expected = self.retained_roles if self.retained_roles is not None else self.roles
        expected_members = self.retained_members if self.retained_roles is not None else self.members
        observed = {}
        self.last_observation = observed
        observed["databases"] = self.query(DATABASE_FACTS)
        observed["roles"] = json.loads(self.query(ROLE_FACTS))
        observed["members"] = json.loads(self.query(MEMBER_FACTS))
        observed["objects"] = self.query("""SELECT
 (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema') +
 (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema')""")
        observed["clients"] = self.query("SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()")
        if observed["databases"] != self.databases or observed["roles"] != expected:
            raise RuntimeError("database or role identity changed; no fixture continuation or deletion permitted")
        if observed["members"] != expected_members:
            raise RuntimeError("role membership facts changed; no fixture continuation or deletion permitted")
        if observed["objects"] != "0":
            raise RuntimeError("coordinator database contains user objects")
        if observed["clients"] != "0":
            raise RuntimeError("another client is active; exclusive fixture required")
        if self.retained_roles is not None and self.query(EXTERNAL_BILLING_DEPENDENCIES) != "f":
            raise RuntimeError("external dependencies prohibit Billing fixture continuation or retirement")

    def create_billing(self):
        self.assert_restored()
        # Capture OIDs in the creation transaction, before a same-name role can
        # be substituted after commit. Do not adopt unrelated actors in a scan.
        capture = "SELECT jsonb_build_object('roles',(" + ROLE_FACTS + ")::jsonb,'members',(" + MEMBER_FACTS + ")::jsonb)::text; COMMIT;"
        captured = json.loads(self.query(BILLING_SETUP.replace("COMMIT;", capture)))
        rows, members = captured["roles"], captured["members"]
        baseline = self.roles
        if (not isinstance(rows, list) or len(rows) != 4 or len(baseline) != 1
                or any(not isinstance(row, list) or len(row) != 10 for row in rows)
                or {row[1] for row in rows} != {self.user, *FIN["ACTORS"]}
                or [row for row in rows if row[1] == self.user] != baseline
                or len({row[0] for row in rows}) != 4):
            raise RuntimeError("created Billing actor identities were not confirmed; no deletion permitted")
        own_ids = {row[0] for row in rows if row[1] in FIN["ACTORS"]}
        new_members = [row for row in members if row not in self.members]
        if (any(row not in members for row in self.members) or len(new_members) != 3
                or any(len(row) != 6 or row[0] not in own_ids or row[1] not in own_ids or row[2] != baseline[0][0] for row in new_members)):
            raise RuntimeError("created Billing membership identities were not confirmed; no deletion permitted")
        self.retained_roles = rows
        self.retained_members = members
        self.assert_restored()

    def retire_billing(self):
        if self.retained_roles is None:
            return
        self.assert_restored()
        self.query("BEGIN; DROP ROLE billing_migrator; DROP ROLE billing_schema_owner; DROP ROLE billing_service; COMMIT;")
        self.retained_roles, self.retained_members = None, None
        self.assert_restored()


def phase_environment(raw, go, phase, directory):
    env = {key: value for key, value in os.environ.items() if not key.startswith(("PINVOU_TEST_", "F11_", "F12_", "F13_", "F18_", "PINVOU_F09_")) and key != "PINVOU_PRIVATE_GO"}
    env["GOFLAGS"] = " ".join(filter(None, (env.get("GOFLAGS", ""), "-race -tags=nomsgpack -mod=readonly")))
    for domain in phase["inputs"]:
        env[INPUTS[domain]] = raw
    for child in phase["children"]:
        env[child["stream_env"]] = str(directory / (child["id"] + ".jsonl"))
        env[child["go_env"]] = go
    return env


def run_phase(raw, go, phase, artifacts):
    directory = artifacts / phase["id"]
    directory.mkdir()
    command = [go, "test", "-p=1", "-race", "-tags=nomsgpack", "-mod=readonly", "-count=1", "-timeout=20m", "-json", "-run", "^(" + "|".join(sorted(phase["roots"])) + ")$", phase["target"]]
    env = phase_environment(raw, go, phase, directory)
    invocation = {"command": command, "go_flags": env["GOFLAGS"], "input_fields": [INPUTS[domain] for domain in phase["inputs"]]}
    (directory / "invocation.json").write_text(json.dumps(invocation, indent=2) + "\n", encoding="utf-8")
    result = subprocess.run(command, cwd=ROOT / "account-platform", env=env, capture_output=True, text=True, encoding="utf-8", check=False)
    invocation["exit_code"] = result.returncode
    (directory / "invocation.json").write_text(json.dumps(invocation, indent=2) + "\n", encoding="utf-8")
    (directory / "parent.jsonl").write_text(result.stdout, encoding="utf-8")
    (directory / "stderr.log").write_text(result.stderr, encoding="utf-8")
    print("Financial family parent: " + phase["id"], flush=True)
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    for child in phase["children"]:
        path = Path(env[child["stream_env"]])
        if path.is_file():
            print("Financial family child: " + child["id"], flush=True)
            sys.stdout.write(path.read_text(encoding="utf-8"))
    sys.stdout.flush()
    if result.returncode:
        raise RuntimeError("original financial family failed: " + phase["id"])
    count = GO["verify_lifecycles"](GO["read_events"](result.stdout), phase["roots"], phase["package"])
    children = {}
    for child in phase["children"]:
        path = Path(env[child["stream_env"]])
        children[child["id"]] = GO["verify_lifecycles"](GO["read_events"](path.read_text(encoding="utf-8")), child["roots"], child["package"])
    return {"phase": phase["id"], "parent_named_passes": count, "child_named_passes": children}


def source_identity(go):
    def output(command, cwd=ROOT):
        result = subprocess.run(command, cwd=cwd, capture_output=True, text=True, encoding="utf-8", check=False)
        if result.returncode:
            raise RuntimeError("financial fixture source/toolchain identity could not be recorded")
        return result.stdout.strip()
    return {"head": output(["git", "rev-parse", "HEAD"]),
            "tree": output(["git", "rev-parse", "HEAD^{tree}"]),
            "dirty": bool(output(["git", "status", "--porcelain=v1"])),
            "go_version": output([go, "version"], ROOT / "account-platform")}


def main():
    manifest = load_manifest()
    raw = FIN["local_database_url"](os.environ.get("PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL", ""))
    go = shutil.which(os.environ.get("PINVOU_ACCOUNT_TEST_GO", "go"))
    if not go:
        raise RuntimeError("the declared Go toolchain is required")
    requested = os.environ.get("PINVOU_FINANCIAL_CHILD_ARTIFACTS")
    if requested:
        artifacts = Path(requested)
        if not artifacts.is_absolute():
            raise ValueError("family evidence directory must be absolute and fresh")
        artifacts.mkdir()
    else:
        artifacts = Path(tempfile.mkdtemp(prefix="financial-families-", dir=os.environ.get("RUNNER_TEMP")))
    print("Financial family evidence: " + str(artifacts), flush=True)
    source = source_identity(go)
    (artifacts / "source.json").write_text(json.dumps(source, indent=2) + "\n", encoding="utf-8")
    owner = OwnedInstance(query_function(raw))
    results = []
    resources = {"identity": owner.query.identity, "before": owner.last_observation, "phases": [], "state": "registered"}

    def save_resources():
        (artifacts / "resources.json").write_text(json.dumps(resources, indent=2) + "\n", encoding="utf-8")

    save_resources()
    try:
        for phase in manifest["phases"]:
            if phase["mode"] == "retained" and owner.retained_roles is None:
                owner.create_billing()
            observation = {"phase": phase["id"], "resources_confirmed": False}
            resources["phases"].append(observation)
            save_resources()
            try:
                results.append(run_phase(raw, go, phase, artifacts))
            finally:
                try:
                    owner.assert_restored()
                    observation["resources_confirmed"] = True
                finally:
                    observation["after"] = owner.last_observation
                    save_resources()
            (artifacts / "summary.json").write_text(json.dumps(results, indent=2) + "\n", encoding="utf-8")
    finally:
        try:
            owner.retire_billing()
            owner.assert_restored()
            resources["state"] = "released"
        finally:
            resources["after"] = owner.last_observation
            if resources["state"] != "released":
                resources["state"] = "unconfirmed"
            save_resources()
    if len(results) != len(manifest["phases"]):
        raise RuntimeError("financial family phases did not all complete")
    print("All original financial family phases and child streams completed; actors/databases restored", flush=True)


if __name__ == "__main__":
    main()
