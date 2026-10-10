// Routing contract tests for .github/workflows/pr-check.yml and
// .github/workflows/pr-title-check.yml.
//
// Guards against gate-routing regressions that paths-filter YAML makes easy:
// a lint step advertised as a hard gate can be silently bypassed when its
// path filter is narrower than the changes it claims to cover.
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { existsSync, mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";

const workflow = await readFile(
  new URL("../../.github/workflows/pr-check.yml", import.meta.url),
  "utf8",
);
const titleWorkflow = await readFile(
  new URL("../../.github/workflows/pr-title-check.yml", import.meta.url),
  "utf8",
);

// Extract the path list of a named dorny/paths-filter output.
function filterPaths(text, name) {
  const section = text.match(
    new RegExp(`^ {12}${name}:\\r?\\n((?: {14}- .*\\r?\\n?)+)`, "m"),
  );
  if (!section) throw new Error(`paths-filter output '${name}' not found`);
  return section[1]
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line.startsWith("- "))
    .map((line) => line.slice(2).trim().replace(/^['"]|['"]$/g, ""));
}

// Extract the `if:` condition of a named workflow step.
function stepCondition(text, stepName) {
  const escaped = stepName.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = text.match(
    new RegExp(`- name: ${escaped}\\r?\\n[\\s\\S]*?if: \\$\\{\\{ (.*?) \\}\\}`, "m"),
  );
  if (!match) throw new Error(`step '${stepName}' or its if-condition not found`);
  return match[1];
}

// Extract the full YAML block of a top-level trigger key: from `  key:` up to
// the next sibling trigger (2-space indent) or top-level key. Anchoring to
// the subtree keeps the lookup fail-closed: a reformat of the types list can
// never silently match another trigger's list (a lazy cross-key match let a
// block-sequence reformat plus an `edited` regression pass this suite 8/8 —
// review finding on #501).
function triggerBlock(text, key) {
  const start = text.match(new RegExp(`^  ${key}:\\r?\\n`, "m"));
  assert.ok(start, `\`${key}:\` trigger not found`);
  const rest = text.slice(start.index + start[0].length);
  const end = rest.search(/^(?: {2}[^ #\s]|\S)/m);
  return end === -1 ? rest : rest.slice(0, end);
}

// The pull_request.types list in either flow (`types: [a, b]`) or block
// (`types:\n      - a`) form; any other shape must throw, never fall through
// to another trigger's list.
function pullRequestTypes(text) {
  const block = triggerBlock(text, "pull_request");
  const flow = block.match(/^ {4}types: \[(.+)\]\r?$/m);
  if (flow) {
    return flow[1]
      .split(",")
      .map((entry) => entry.trim().replace(/^['"]|['"]$/g, ""));
  }
  const items = [...block.matchAll(/^ {6}- (.+)$/gm)].map((match) =>
    match[1].trim().replace(/^['"]|['"]$/g, ""),
  );
  assert.ok(
    items.length > 0,
    "pull_request.types list not found (flow or block form)",
  );
  return items;
}

function jobBlock(name, text = workflow) {
  const match = text.match(new RegExp(`^  ${name}:\\r?\\n[\\s\\S]*?(?=^  [\\w-]+:|$(?![\\s\\S]))`, "m"));
  assert.ok(match, `job '${name}' not found`);
  return match[0];
}

// Select one named step before reading its literal run body. A later sibling
// step (including a second run block) must never become part of the shell.
function namedStep(job, name) {
  const text = job.replaceAll("\r\n", "\n");
  const matches = [...text.matchAll(/^ {6}- name: (.+)$/gm)].filter(match => match[1] === name);
  assert.equal(matches.length, 1, `step '${name}' not found or duplicate`);
  const start = matches[0].index;
  const rest = text.slice(start + matches[0][0].length);
  const end = rest.search(/^(?: {6}- | {0,5}\S)/m);
  return text.slice(start, end === -1 ? text.length : start + matches[0][0].length + end);
}

function stepRun(job, name) {
  const step = namedStep(job, name);
  assert.equal([...step.matchAll(/^ {8}run:/gm)].length, 1, `expected one run block for '${name}'`);
  const marker = step.match(/^ {8}run: \|\n/m);
  assert.ok(marker, `literal run block for '${name}' not found`);
  const lines = step.slice(marker.index + marker[0].length).split("\n");
  const body = [];
  for (const line of lines) {
    if (line === "") body.push("");
    else if (line.startsWith("          ")) body.push(line.slice(10));
    else break;
  }
  assert.ok(body.some(line => line.trim()), `empty run block for '${name}'`);
  return body.join("\n");
}

const goStepName = "Test and vet all Account Platform Go modules";
const auditPGStepName = "Audit export finite PostgreSQL rollback and persisted boundaries";

function assertFiniteAuditPGSetup(job) {
  const text = job.replaceAll("\r\n", "\n");
  // This is a single approved ephemeral service, not permission for arbitrary
  // services, production credentials, public ports or persistent volumes.
  const service = text.match(/^ {4}services:\n[\s\S]*?(?=^ {4}\S|$(?![\s\S]))/m);
  assert.ok(service, "isolated audit PostgreSQL service not found");
  assert.equal(service[0].trimEnd(), [
    "    services:", "      audit-export-postgres:", "        image: postgres:16-alpine",
    "        env:", "          POSTGRES_HOST_AUTH_METHOD: trust", "        ports:",
    "          - 127.0.0.1:5432:5432", "        options: >-", "          --health-cmd pg_isready",
    "          --health-interval 10s", "          --health-timeout 5s", "          --health-retries 5",
  ].join("\n"));
  assert.doesNotMatch(job, /secrets\.|continue-on-error:/);
  const pg = namedStep(job, auditPGStepName);
  assert.match(pg, /^ {8}working-directory: account-platform$/m);
  const env = pg.match(/^ {8}env:\n[\s\S]*?(?=^ {8}\S|$(?![\s\S]))/m);
  assert.ok(env, "finite audit test profile not found");
  assert.equal(env[0].trimEnd(), [
    "        env:", "          F17_TEST_DATABASE_URL: postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable",
    "          F17_TEST_DATABASE_PROFILE: github-actions-service",
  ].join("\n"));
  assert.doesNotMatch(job.replace(pg, ""), /DATABASE_URL|DATABASE_PROFILE|F17_TEST_/);
  const defaultStep = namedStep(job, goStepName);
  const jobEnv = text.match(/^ {4}env:\n[\s\S]*?(?=^ {4}\S|$(?![\s\S]))/m);
  assert.ok(jobEnv);
  assert.doesNotMatch(jobEnv[0] + defaultStep, /DATABASE_URL|DATABASE_PROFILE|F17_TEST_/);
  const pgRun = stepRun(job, auditPGStepName);
  assert.match(pgRun, /^set -euo pipefail$/m);
  assert.match(pgRun, /assert terminal and len\(terminal\) > len\(required\)/);
  assert.match(pgRun, /assert all\(event\["Action"\] == "pass" for event in terminal\)/);
  assert.match(pgRun, /assert roots == required/);
  assert.equal([...pgRun.matchAll(/go test -race -json .*?-parallel=1 /g)].length, 2);
}

const bash = process.platform === "win32" && existsSync("C:/Program Files/Git/bin/bash.exe")
  ? "C:/Program Files/Git/bin/bash.exe" : "bash";

test("Account Platform changes cannot bypass the Go gate at any PR or queue stage", () => {
  const paths = filterPaths(workflow, "account_platform");
  for (const path of ["account-platform/**", "scripts/account-platform*", "scripts/check-account-platform*", "scripts/check-token-management-authorization-coverage.mjs", "scripts/tests/token-management-authorization-coverage.test.mjs", ".github/workflows/pr-check.yml", "scripts/tests/pr-check-routing.test.mjs"]) {
    assert.ok(paths.includes(path), `Go routing must cover ${path}`);
  }
  assert.match(workflow, /account_platform: \$\{\{ steps\.filter\.outputs\.account_platform \}\}/);
  const job = jobBlock("account-platform-test");
  assert.match(job, /needs\.changes\.outputs\.account_platform == 'true'/);
  assert.doesNotMatch(job, /draft|pure_release_backflow|trusted_backflow|github\.event_name == 'pull_request'/);
  assert.match(job, /go-version-file: account-platform\/go\.mod/);
  assert.match(job, /GOTOOLCHAIN: local/);
  assert.match(job, /CGO_ENABLED: "1"/);
  const run = stepRun(job, "Verify Token management authorization contracts");
  assert.deepEqual(run.trim().split(/\r?\n/), [
    "node scripts/check-token-management-authorization-coverage.mjs",
    "node --test scripts/tests/token-management-authorization-coverage.test.mjs",
  ]);
  for (const [checkerStatus, formalStatus, expectedOutput, expectedStatus] of [[7, 0, "checker", 7], [0, 8, "checkerformal", 8], [0, 0, "checkerformal", 0]]) {
    const fixture = `node() { if [[ "$1" == scripts/check-token-management-authorization-coverage.mjs ]]; then printf checker; return ${checkerStatus}; else printf formal; return ${formalStatus}; fi; };\n`;
    const result = spawnSync(bash, ["-e", "-c", fixture + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, expectedStatus, result.stderr);
    assert.equal(result.stdout, expectedOutput);
  }
  assertFiniteAuditPGSetup(job);
});

test("Account Platform Node gate executes its contracts and rejects empty or failed runs", () => {
  const job = jobBlock("account-platform-test");
  const install = namedStep(job, "Install Account Platform contract dependencies");
  assert.match(install, /working-directory: account-platform/);
  assert.match(install, /npm ci --ignore-scripts --prefer-offline --no-audit/);
  const nodeStep = namedStep(job, "Account Platform Node contracts");
  assert.match(nodeStep, /working-directory: account-platform/);
  const run = stepRun(job, "Account Platform Node contracts");
  assert.match(run, /node --test --test-reporter=tap test\/contract\/\*\.test\.mjs/);
  const directory = mkdtempSync(join(tmpdir(), "pinvou-node-gate-"));
  try {
    for (const [passed, failed, exit, expected] of [[1, 0, 0, 0], [0, 0, 0, 1], [2, 1, 1, 1], [1, 0, 7, 7]]) {
      const prefix = `node() { printf '# pass ${passed}\\n# fail ${failed}\\n'; return ${exit}; }; export -f node;\n`;
      const result = spawnSync(bash, ["-c", prefix + run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true, env: { ...process.env, RUNNER_TEMP: directory.replaceAll("\\", "/") } });
      assert.equal(result.status, expected, `${passed}/${failed}/${exit}: ${result.stderr}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Go gate executes test and vet for root and future nested modules, and fails on errors", () => {
  const job = jobBlock("account-platform-test");
  const run = stepRun(job, goStepName);
  const directory = mkdtempSync(join(tmpdir(), "pinvou-go-gate-"));
  try {
    for (const module of ["account-platform", "account-platform/tools/kms-recovery", "account-platform/tools/future tool"]) {
      mkdirSync(join(directory, module), { recursive: true });
      writeFileSync(join(directory, module, "go.mod"), "module example.test/fixture\n");
    }
    const prefix = "go() { printf '%s|%s\\n' \"$PWD\" \"$*\"; }; export -f go;\n";
    const result = spawnSync(bash, ["-c", prefix + run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, 0, result.stderr);
    const calls = result.stdout.split("\n").filter((line) => line.includes("|"));
    assert.equal(calls.length, 6, result.stdout);
    for (const module of ["account-platform", "account-platform/tools/kms-recovery", "account-platform/tools/future tool"]) {
      const moduleCalls = calls.filter((line) => line.includes(`/${module}|`));
      assert.equal(moduleCalls.length, 2, result.stdout);
      assert.ok(moduleCalls.some((line) => line.endsWith("|test -race -tags=nomsgpack -mod=readonly -count=1 ./...")));
      assert.ok(moduleCalls.some((line) => line.endsWith("|vet -tags=nomsgpack -mod=readonly ./...")));
    }
    for (const command of ["test", "vet"]) {
      const failed = spawnSync(bash, ["-c", `go() { if [[ "$1" == "${command}" ]]; then return 7; fi; }; export -f go;\n` + run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true });
      assert.equal(failed.status, 7, `a failing go ${command} must fail the gate: ${failed.stderr}`);
      const nestedFailed = spawnSync(bash, ["-c", `go() { if [[ "$PWD" == */"tools/future tool" && "$1" == "${command}" ]]; then return 7; fi; }; export -f go;\n` + run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true });
      assert.equal(nestedFailed.status, 7, `a failing nested-module go ${command} must fail the gate: ${nestedFailed.stderr}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("named Go step extraction excludes later YAML and fails closed on missing or duplicate names", () => {
  const job = jobBlock("account-platform-test");
  const run = stepRun(job, goStepName);
  assert.doesNotMatch(run, /- name:|F17_TEST_DATABASE|f17-audit-schema/);
  assert.equal(stepRun(job.replaceAll("\n", "\r\n"), goStepName), run);
  assert.equal(stepRun(job + "      - name: Future sibling\n        run: |\n          exit 77\n", goStepName), run);
  assert.throws(() => stepRun(job.replace(goStepName, "Renamed Go step"), goStepName));
  assert.throws(() => stepRun(job + `      - name: ${goStepName}\n        run: |\n          exit 77\n`, goStepName));
  const originalStep = namedStep(job, goStepName);
  const foldedStep = originalStep.replace("        run: |", "        run: >-");
  assert.notEqual(foldedStep, originalStep, "folded run mutation must hit the named Go step");
  assert.throws(() => stepRun(job.replace(originalStep, foldedStep), goStepName));
});

test("finite audit PostgreSQL exception rejects unsafe or unrelated service and profile mutations", () => {
  const job = jobBlock("account-platform-test").replaceAll("\r\n", "\n");
  assertFiniteAuditPGSetup(job);
  for (const [before, after] of [
    ["127.0.0.1:5432:5432", "5432:5432"],
    ["postgres:16-alpine", "postgres:latest"],
    ["      audit-export-postgres:", "      unrelated-postgres:"],
    ["    env:\n      GOTOOLCHAIN:", "      another-service:\n        image: postgres:16-alpine\n    env:\n      GOTOOLCHAIN:"],
    ["        ports:", "        volumes:\n          - shared-data:/var/lib/postgresql/data\n        ports:"],
    ["F17_TEST_DATABASE_PROFILE: github-actions-service", "F17_TEST_DATABASE_PROFILE: local-owned"],
    ["postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable", "postgres://postgres@remote.invalid:5432/postgres?sslmode=disable"],
    ["      GOTOOLCHAIN: local", "      F17_TEST_DATABASE_URL: postgres://postgres@127.0.0.1:5432/postgres\n      GOTOOLCHAIN: local"],
    ["        shell: bash", "        env:\n          F17_TEST_DATABASE_PROFILE: github-actions-service\n        shell: bash"],
    ["          POSTGRES_HOST_AUTH_METHOD: trust", "          POSTGRES_PASSWORD: ${{ secrets.DB_PASSWORD }}"],
    ["        working-directory: account-platform", "        continue-on-error: true\n        working-directory: account-platform"],
    ["assert roots == required", "assert roots <= required"],
    ["assert all(event[\"Action\"] == \"pass\" for event in terminal)", "assert terminal"],
    ["assert terminal and len(terminal) > len(required)", "assert True"],
  ]) {
    assert.ok(job.includes(before), `mutation must hit existing contract: ${before}`);
    const original = before === "        shell: bash" ? namedStep(job, goStepName) : job;
    const changed = original.replace(before, after);
    assert.notEqual(changed, original, `mutation must change its exact target: ${before}`);
    const mutatedJob = original === job ? changed : job.replace(original, changed);
    assert.throws(() => assertFiniteAuditPGSetup(mutatedJob), `must reject: ${after}`);
  }
});

test("Keycloak contract gate invokes the existing suite and propagates failure", () => {
  const run = stepRun(jobBlock("account-platform-test"), "Keycloak provisioning and theme contracts");
  const prefix = "python3() { printf '%s|%s\\n' \"$PINVOU_TEST_BASH\" \"$*\"; }; export -f python3;\n";
  const result = spawnSync(bash, ["-c", prefix + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
  assert.equal(result.status, 0, result.stderr);
  const calls = result.stdout.trim().split("\n");
  assert.equal(calls.length, 1, result.stdout);
  const [shell, script] = calls[0].split("|");
  assert.ok(shell, "the suite must receive an explicit POSIX shell");
  assert.equal(script, "account-platform/deploy/keycloak/test/provision_mobile_auth_test.py");
  const failed = spawnSync(bash, ["-c", "python3() { return 7; }; export -f python3;\n" + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
  assert.equal(failed.status, 7, "a failing Keycloak contract must fail the gate");
});

test("named step fixtures exclude later steps and fail closed on missing names", () => {
  const job = jobBlock("account-platform-test");
  const name = "Test and vet all Account Platform Go modules";
  const later = "      - name: Unrelated fixture\n        run: |\n          exit 99\n";
  assert.equal(stepRun(job + later, name), stepRun(job, name));
  assert.throws(() => stepRun(job, "Missing gate"), /not found/);
});

test("required-gate rejects skipped applicable Go checks and permits unrelated skips", () => {
  const gate = jobBlock("required-gate");
  assert.match(gate, /^      - account-platform-test$/m);
  assert.match(gate, /ACCOUNT_PLATFORM_REQUIRED: \$\{\{ needs\.changes\.outputs\.account_platform \}\}/);
  assert.match(gate, /ACCOUNT_PLATFORM_RESULT: \$\{\{ needs\.account-platform-test\.result \}\}/);
  const run = gate.match(/        run: \|\r?\n([\s\S]*)/)[1].replace(/^ {10}/gm, "");
  const baseEnv = { ...process.env, GITHUB_EVENT_NAME: "pull_request", WINDOWS_RUST_REQUIRED_BY_POLICY: "false" };
  for (const match of gate.matchAll(/^          ([A-Z_]+_RESULT):/gm)) baseEnv[match[1]] = "success";
  for (const event of ["pull_request", "merge_group"]) {
    for (const applicable of ["true", "false"]) {
      for (const status of ["success", "skipped", "failure", "cancelled", ""]) {
        const result = spawnSync(bash, ["-c", run], { encoding: "utf8", timeout: 30_000, windowsHide: true, env: { ...baseEnv, GITHUB_EVENT_NAME: event, ACCOUNT_PLATFORM_REQUIRED: applicable, ACCOUNT_PLATFORM_RESULT: status } });
        const expectedPass = status === "success" || (applicable === "false" && status === "skipped");
        assert.equal(result.status, expectedPass ? 0 : 1, `${event}/${applicable}/${status}: ${result.stderr}`);
      }
    }
  }
});

test("rust_code filter covers plain Rust source changes", () => {
  const paths = filterPaths(workflow, "rust_code");
  assert.ok(paths.includes("**/*.rs"), "rust_code must match '*.rs' files");
});

test("cargo-shear gate routes on rust_code so orphan .rs files cannot bypass it", () => {
  // cargo-shear detects unlinked source files as well as unused dependencies,
  // so gating it only on rust_dependencies lets an orphan .rs file added
  // without Cargo metadata changes skip the gate entirely.
  for (const stepName of ["Install cargo-shear", "cargo shear (hard gate, both workspaces)"]) {
    const condition = stepCondition(workflow, stepName);
    assert.match(
      condition,
      /needs\.changes\.outputs\.rust_code == 'true'/,
      `${stepName} must be gated on rust_code (covers *.rs-only changes)`,
    );
  }
});

test("dependency-only gates (cargo-deny) stay on rust_dependencies", () => {
  // cargo-deny inspects the dependency graph only; keep it on the narrower
  // filter so plain .rs changes do not pay the install cost.
  const condition = stepCondition(workflow, "cargo deny check (hard gate)");
  assert.match(condition, /needs\.changes\.outputs\.rust_dependencies == 'true'/);
  assert.doesNotMatch(condition, /rust_code/);
});

test("the required workflow ignores PR edits; the title gate owns `edited`", () => {
  // Review finding on #501: with `edited` in pr-check.yml's types, a
  // body-only edit still creates skipped check runs named after the required
  // contexts (commit-message, required-gate, ...), and the checks API/UI
  // surfaces the latest check run per name — the skipped runs replaced the
  // green gate results (run 34818389619). Title validation therefore lives
  // in the dedicated pr-title-check.yml, and the required workflow must
  // never receive `edited` again.
  const prCheckActions = pullRequestTypes(workflow);
  assert.ok(
    !prCheckActions.includes("edited"),
    "pr-check.yml must not subscribe to edited (skipped runs shadow required contexts)",
  );
  const titleActions = pullRequestTypes(titleWorkflow);
  for (const expected of ["opened", "synchronize", "reopened", "edited"]) {
    assert.ok(titleActions.includes(expected), `pr-title-check.yml types must include ${expected}`);
  }
});

test("the pr-title context name cannot collide with pr-check.yml job names", () => {
  // The dedicated workflow fixes the shadowing only while its context stays
  // unique; pin the job name and guard it against later renames on either
  // side (job ids and explicit job names at 2-/4-space indent).
  assert.match(titleWorkflow, /^    name: pr-title$/m, "title gate context must stay `pr-title`");
  // Quoted keys are legal YAML, and GitHub names the check run after the job
  // id when no explicit name is set — a bare-word-only scan let `"pr-title":`
  // recreate the shadowing (review finding on #501). Strip quotes on both
  // scans, tolerate CRLF checkouts, and fail closed: an empty job-id scan
  // would pass the includes() check vacuously.
  const prCheckKeys = [
    ...workflow.matchAll(/^ {2}"?([\w-]+)"?:\r?\n/gm),
  ].map((match) => match[1]);
  assert.ok(prCheckKeys.length > 0, "job-id scan matched nothing");
  const prCheckJobNames = [
    ...workflow.matchAll(/^ {4}name: (.+)$/gm),
  ].map((match) => match[1].trim().replace(/^['"]|['"]$/g, ""));
  for (const names of [prCheckKeys, prCheckJobNames]) {
    assert.ok(
      !names.includes("pr-title"),
      "`pr-title` must stay unique vs pr-check.yml job names",
    );
  }
});

test("title gate revalidates on every subscribed event (no skip condition)", () => {
  // Review finding on #501: a skipped job still creates a check run named
  // `pr-title`, and the checks API surfaces the latest run per name, so a
  // body-only skip would replace the previous red or green result with
  // SKIPPED — and a skipped required check counts as success, clearing a
  // red gate. Every event must therefore run the validator; each run
  // fetches the live title, so the latest result always reflects the
  // current title.
  const jobBlock = titleWorkflow.match(
    /^  pr-title:\r?\n(?:^(?! {2}\S).*\r?\n)*/m,
  );
  assert.ok(jobBlock, "pr-title job block not found");
  assert.doesNotMatch(
    jobBlock[0],
    /^    if:/m,
    "pr-title job must not carry a skip condition (skipped runs shadow the result)",
  );
});

test("all title-gate runs share one cancel-and-replace concurrency group", () => {
  // Every run validates the title it fetched from the API at execution
  // time, so whichever run finishes last holds the freshest verdict; the
  // single PR-keyed group with cancel-and-replace just keeps repeated
  // edits from piling up runs. No edit-kind routing may remain anywhere.
  const concurrency = titleWorkflow.slice(
    titleWorkflow.indexOf("\nconcurrency:"),
    titleWorkflow.indexOf("\njobs:"),
  );
  assert.match(concurrency, /^  cancel-in-progress: true$/m);
  assert.match(
    concurrency,
    /^  group: pr-title-\$\{\{ github\.event\.pull_request\.number \}\}$/m,
    "concurrency group must be the single PR-keyed group",
  );
  assert.doesNotMatch(concurrency, /changes\./, "no edit-kind routing may remain");
  // GitHub lets a job-level `concurrency:` key override the workflow-level
  // group (review finding on #501); ban any indented key so the single
  // PR-keyed cancel-and-replace group cannot be bypassed from inside a job.
  assert.doesNotMatch(
    titleWorkflow,
    /^ {2,}concurrency:/m,
    "a job-level concurrency override would bypass the single PR-keyed group",
  );
});

test("title gate enforces the convention on the live PR title (squash subject)", () => {
  // The squash merge subject is "<PR title> (#N)" and the merge queue never
  // runs commit-message, so the title is validated at PR time. Review
  // finding on #501: GitHub does not guarantee the start order of runs in
  // one concurrency group, so an older `edited` event can run last (and
  // cancel-and-replace a newer run) while carrying a superseded event
  // payload — the event-payload title must therefore never reach the
  // validator. Each run fetches the current title from the API right
  // before validating, through a temp file to avoid injection.
  assert.doesNotMatch(
    titleWorkflow,
    /github\.event\.pull_request\.title/,
    "the event-payload title may be stale; fetch the current title from the API",
  );
  // Bracket notation or toJSON(github.event) would carry the same stale
  // payload past the dot-notation ban above (review finding on #501).
  assert.doesNotMatch(
    titleWorkflow,
    /github\s*\[|toJSON\s*\(/,
    "event-payload access must stay in auditable dot notation",
  );
  assert.match(
    titleWorkflow,
    /^  pull-requests: read$/m,
    "fetching the current PR title requires pull-requests: read",
  );
  // The whole chain must hold: a retry loop around the API fetch, the fetch
  // landing in exactly the file the validator reads (a chain that fetches
  // into /dev/null or validates another file must fail here), and the
  // validator call itself (review finding on #501).
  const chain = titleWorkflow.match(
    /for _ in 1 2 3; do[\s\S]*?gh api "repos\/\$PR_REPO\/pulls\/\$PR_NUMBER" --jq \.title > "\$RUNNER_TEMP\/pr-title"[\s\S]*?python3 scripts\/validate-commit-msg\.py --subject-only "\$RUNNER_TEMP\/pr-title"/,
  );
  assert.ok(
    chain,
    "title gate must retry-fetch the live title and validate exactly that file in subject-only mode",
  );
});
