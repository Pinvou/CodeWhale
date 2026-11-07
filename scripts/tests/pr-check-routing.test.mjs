// Routing contract tests for .github/workflows/pr-check.yml and
// .github/workflows/pr-title-check.yml.
//
// Guards against gate-routing regressions that paths-filter YAML makes easy:
// a lint step advertised as a hard gate can be silently bypassed when its
// path filter is narrower than the changes it claims to cover.
import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { existsSync, mkdtempSync, mkdirSync, writeFileSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, matchesGlob } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

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
  const block = text.match(new RegExp(`^ {6}- name: ${escaped}\\r?\\n((?:(?! {6}- ).*\\r?\\n)*)`, "m"));
  const match = block?.[1].match(/^\s*if: \$\{\{ (.*?) \}\}/m);
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

function assertAccountFailurePolicy(job) {
  assert.doesNotMatch(job, /secrets\./);
  let mandatory = job;
  // Only these reviewed, bounded transport steps are advisory. Remove their
  // complete named blocks before applying the original job-wide failure ban;
  // a test, cleanup, unnamed sibling or job-level exception remains forbidden.
  for (const [name, id] of [
    ["Financial parallel diagnostics", "financial_diagnostics_upload"],
    ["Account test resource lifecycle receipts", "account_receipts_upload"],
  ]) {
    const diagnostic = namedStep(job, name);
    assert.match(diagnostic, new RegExp(`^ {8}id: ${id}$`, "m"));
    assert.match(diagnostic, /^ {8}uses: actions\/upload-artifact@v7$/m);
    assert.match(diagnostic, /^ {8}timeout-minutes: 3$/m);
    assert.match(diagnostic, /^ {8}continue-on-error: true$/m);
    assert.doesNotMatch(diagnostic, /^ {8}(?:run|env):/m);
    mandatory = mandatory.replace(diagnostic, "");
  }
  assert.doesNotMatch(mandatory, /continue-on-error:/);
}



const goStepName = "Test and vet all Account Platform Go modules";
const auditPGStepName = "Audit export finite PostgreSQL rollback and persisted boundaries";
const existingBrokerStepName = "Existing broker compatibility on owned PostgreSQL and TLS NATS";
const pgPreflightStepName = "Verify account CI PostgreSQL service isolation";
const pgStopStepName = "Stop owned temporary PostgreSQL services";

function pgIdentityEnvironment(service) {
  return [
    `PINVOU_ACCOUNT_CI_PG_PORT: \${{ steps.account-pg.outputs.${service}_port }}`,
    `PINVOU_ACCOUNT_CI_PG_SYSTEM_ID: \${{ steps.account-pg.outputs.${service}_system }}`,
    `PINVOU_ACCOUNT_CI_PG_DIRECTORY: \${{ steps.account-pg.outputs.${service}_directory }}`,
  ];
}

function assertFinancialDiagnosticExemption(job) {
  const text = job.replaceAll("\r\n", "\n");
  const diagnostic = namedStep(text, "Financial parallel diagnostics");
  const withoutComments = diagnostic.replace(/^ *#.*\n/gm, "").trimEnd();
  assert.equal(withoutComments, [
    "      - name: Financial parallel diagnostics",
    "        id: financial_diagnostics_upload",
    "        if: ${{ always() }}",
    "        timeout-minutes: 3",
    "        continue-on-error: true",
    "        uses: actions/upload-artifact@v7",
    "        with:",
    "          name: account-financial-parallel-${{ github.run_attempt }}",
    "          path: ${{ runner.temp }}/financial-parallel-*/",
    "          if-no-files-found: ignore",
    "          retention-days: 7",
  ].join("\n"));
  assert.doesNotMatch(text, /secrets\./);
  assert.doesNotMatch(text.replace(diagnostic, "").replace(namedStep(text, "Account test resource lifecycle receipts"), ""), /continue-on-error:/,
    "only the exact diagnostic upload step may be advisory");
}

test("financial diagnostic exception rejects business and job exemptions", () => {
  const job = jobBlock("account-platform-test");
  assertFinancialDiagnosticExemption(job);
  const diagnostic = namedStep(job, "Financial parallel diagnostics");
  const withoutExemption = job.replace(diagnostic,
    diagnostic.replace("        continue-on-error: true\n", ""));
  const mutants = [
    job.replace("  account-platform-test:\n", "  account-platform-test:\n    continue-on-error: true\n"),
    job.replace("uses: actions/upload-artifact@v7", "run: exit 37"),
    job.replace("uses: actions/upload-artifact@v7", "uses: actions/upload-artifact@v6"),
    job + "      - run: exit 37\n        continue-on-error: true\n",
  ];
  for (const [first, rest] of [
    ["run: exit 37", "        name: Required business command\n"],
    ["id: business_guard", '        name: Required business command\n        "run": exit 37\n'],
    ["if: ${{ always() }}", "        run: exit 37\n"],
    ["env:", "          FIXTURE: yes\n        run: exit 37\n"],
  ]) {
    mutants.push(withoutExemption.replace("      - name: Stop owned temporary PostgreSQL services\n",
      `      - ${first}\n${rest}        continue-on-error: true\n\n      - name: Stop owned temporary PostgreSQL services\n`));
  }
  for (const mutant of mutants) {
    assert.notEqual(mutant, job);
    assert.throws(() => assertFinancialDiagnosticExemption(mutant));
  }
  assertFinancialDiagnosticExemption(job);
});

// Match the Python gate policy across the complete workflow. Account-only
// validation cannot detect a job/step exception added to a different leg.
function assertWorkflowFailurePolicy(workflowText) {
  const text = workflowText.replaceAll("\r\n", "\n").replace(/^ *#.*\n/gm, "");
  const financialJob = jobBlock("account-platform-test", text);
  assertFinancialDiagnosticExemption(financialJob);
  const financial = namedStep(financialJob, "Financial parallel diagnostics");
  const windows = namedStep(jobBlock("windows-rust-test", text), "Windows 测试二进制导入诊断");
  assert.equal(windows.trimEnd(), [
    "      - name: Windows 测试二进制导入诊断",
    "        if: ${{ matrix.phase == 'regression' }}",
    "        continue-on-error: true",
    "        shell: pwsh",
    '        run: python scripts/ci-windows-imports-diagnose.py "$env:PINVOU3_TEST_EXE"',
  ].join("\n"));
  let remainder = text.replace(financial, "").replace(windows, "");
  {
    const receipt = namedStep(jobBlock("frontend-test", text), "Test resource lifecycle receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Test resource lifecycle receipts",
      "        id: frontend_receipts_upload",
      "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: frontend-resource-lifecycle-${{ github.run_attempt }}",
      "          path: ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  {
    const receipt = namedStep(jobBlock("account-platform-test", text), "Account test resource lifecycle receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Account test resource lifecycle receipts",
      "        id: account_receipts_upload",
      "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: account-resource-lifecycle-${{ github.run_attempt }}",
      "          path: |",
      "            ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
      "            ${{ runner.temp }}/account-platform-deployment-contracts-*/resources/",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  // Explicit YAML keys may put comments/newlines before their value indicator.
  const keySeparator = String.raw`(?:\s|#[^\n]*(?:\n|$))*:`;
  assert.doesNotMatch(remainder, new RegExp(String.raw`\bcontinue-on-error\s*['"]?` + keySeparator),
    "only the four exact reviewed diagnostic steps may be advisory in any job");
  // YAML double-quoted keys decode numeric escapes and escaped line breaks.
  // Inspect each possible quoted key, including inside flow mappings. A
  // lookahead avoids a quote in unrelated shell text hiding a later real key.
  for (const match of remainder.matchAll(new RegExp(String.raw`(?="((?:[^"\\]|\\[\s\S])*)"` + keySeparator + ")", "g"))) {
    const key = match[1].replace(/\\(?:x([\da-fA-F]{2})|u([\da-fA-F]{4})|U([\da-fA-F]{8})|(\n[ \t]*)|([\s\S]))/g,
      (escape, hex, shortUnicode, longUnicode, continuation, character) => {
        const digits = hex ?? shortUnicode ?? longUnicode;
        if (digits !== undefined) return String.fromCodePoint(Number.parseInt(digits, 16));
        if (continuation !== undefined) return "";
        // All other YAML escapes produce characters outside this ASCII key.
        // Consume escaped backslashes once, without decoding their result again.
        return character === "\\" ? "\\" : escape;
      });
    assert.notEqual(key, "continue-on-error", "escaped YAML keys cannot grant an advisory exception");
  }
  // Alias mapping keys would need resolution against a separate scalar. This
  // finite workflow contract requires literal keys rather than guessing one.
  assert.doesNotMatch(remainder, new RegExp(String.raw`\*[^\s\[\]{},:]+` + keySeparator),
    "alias mapping keys require an explicit failure-policy contract review");
}

test("all workflow legs reject unreviewed job and step failure exemptions", () => {
  const original = workflow.replaceAll("\r\n", "\n");
  assertWorkflowFailurePolicy(original);
  const windows = namedStep(jobBlock("windows-rust-test", original), "Windows 测试二进制导入诊断");
  const withoutWindowsExemption = original.replace(windows,
    windows.replace("        continue-on-error: true\n", ""));
  const mutants = [
    original.replace("  frontend-test:\n", "  frontend-test:\n    continue-on-error: true\n"),
    original.replace("  frontend-test:\n", "  frontend-test:\n    'continue-on-error': true\n"),
    original.replace("  windows-rust-test:\n", "  windows-rust-test:\n    continue-on-error: true\n"),
    original.replace(windows, windows + "      - run: exit 37\n        continue-on-error: true\n"),
    original.replace(windows, windows + "      - {run: exit 37, 'continue-on-error': true}\n"),
    withoutWindowsExemption.replace("  frontend-test:\n", "  frontend-test:\n    continue-on-error: true\n"),
    original.replace(windows, windows.replace("scripts/ci-windows-imports-diagnose.py", "scripts/not-the-reviewed-diagnostic.py")),
    original.replace("  frontend-test:\n", '  frontend-test:\n    "continue\\u002don-error": true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    "\\u0063ontinue-on-error": true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    "continue\\x2don-error": true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    "continue\\U0000002don-error": true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    ? "continue\\\n      -on-error"\n    : true\n'),
    original.replace(windows, windows + '      - {run: exit 37, "continue\\u002don-error": true}\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    env:\n      FAILURE_KEY: &advisory_key "continue\\u002don-error"\n    *advisory_key: true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    env:\n      FAILURE_KEY: &advisory_key "continue\\u002don-error"\n    ? *advisory_key # label\n    : true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    env:\n      FAILURE_KEY: &advisory#key "continue\\u002don-error"\n    *advisory#key : true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    ? "continue\\u002don-error" # label\n    : true\n'),
    original.replace("  frontend-test:\n", '  frontend-test:\n    ? continue-on-error # label\n    : true\n'),
  ];
  for (const [jobName, stepName] of [
    ["frontend-test", "Test resource lifecycle receipts"],
    ["account-platform-test", "Account test resource lifecycle receipts"],
  ]) {
    const receipt = namedStep(jobBlock(jobName, original), stepName);
    const withoutReceiptExemption = original.replace(receipt,
      receipt.replace("        continue-on-error: true\n", ""));
    mutants.push(
      withoutReceiptExemption.replace("  frontend-test:\n", "  frontend-test:\n    continue-on-error: true\n"),
      original.replace(receipt, receipt.replace("uses: actions/upload-artifact@v7", "run: exit 37")),
      original.replace(receipt, receipt.replace("timeout-minutes: 3\n", "")),
    );
    for (const [first, rest] of [
      ["run: exit 37", "        name: Required business command\n"],
      ["id: business_guard", '        name: Required business command\n        "run": exit 37\n'],
      ["if: ${{ always() }}", "        run: exit 37\n"],
      ["env:", "          FIXTURE: yes\n        run: exit 37\n"],
    ]) {
      mutants.push(withoutReceiptExemption.replace(receipt.replace("        continue-on-error: true\n", ""),
        `${receipt.replace("        continue-on-error: true\n", "")}      - ${first}\n${rest}        continue-on-error: true\n`));
    }
  }
  for (const mutant of mutants) {
    assert.notEqual(mutant, original, "the counterexample must change the actual workflow");
    assert.throws(() => assertWorkflowFailurePolicy(mutant));
  }
  assertWorkflowFailurePolicy(original);
});

function assertFiniteAuditPGSetup(job) {
  const text = job.replaceAll("\r\n", "\n");
  // These are exactly six approved ephemeral services, not permission for arbitrary
  // services, production credentials, public ports or persistent volumes.
  const service = text.match(/^ {4}services:\n[\s\S]*?(?=^ {4}\S|$(?![\s\S]))/m);
  assert.ok(service, "isolated audit PostgreSQL service not found");
  assert.equal(service[0].trimEnd(), [
    "    services:",
    "      audit-export-postgres:",
    "        image: mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea",
    "        env:",
    "          POSTGRES_HOST_AUTH_METHOD: trust",
    "          POSTGRES_USER: postgres",
    "          POSTGRES_DB: postgres",
    "        ports:",
    '          - "127.0.0.1::5432"',
    "        options: >-",
    "          --rm",
    "          --health-cmd \"pg_isready -U postgres -d postgres\"",
    "          --health-interval 10s",
    "          --health-timeout 5s",
    "          --health-retries 5",
    "      existing-broker-postgres:",
    "        # Dedicated disposable PG18.4, matching the supported production-history",
    "        # contract. The acceptance creates and removes only its own actors/DBs.",
    "        image: postgres:18.4-alpine@sha256:9a8afca54e7861fd90fab5fdf4c42477a6b1cb7d293595148e674e0a3181de15",
    "        env:",
    "          POSTGRES_HOST_AUTH_METHOD: trust",
    "          POSTGRES_DB: postgres",
    "        ports:",
    '          - "127.0.0.1::5432"',
    "        options: >-",
    "          --rm",
    "          --health-cmd \"pg_isready -U postgres -d postgres\"",
    "          --health-interval 5s",
    "          --health-timeout 5s",
    "          --health-retries 12",
    ...["financial-postgres", "financial-token-postgres", "financial-billing-0-postgres", "financial-billing-1-postgres"].flatMap(name => [
      `      ${name}:`,
    "        # Same immutable PostgreSQL image already used by team-relay-hub-test.",
    "        # Disposable runner only; trust is never a deployment configuration.",
    "        image: mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea",
    "        env:",
    "          POSTGRES_HOST_AUTH_METHOD: trust",
    "          POSTGRES_DB: postgres",
    "        ports:",
    '          - "127.0.0.1::5432"',
    "        options: >-",
    "          --rm",
    "          --cpus 2",
    "          --memory 1g",
    "          --memory-swap 1g",
    "          --health-cmd \"pg_isready -U postgres -d postgres\"",
    "          --health-interval 5s",
    "          --health-timeout 5s",
    "          --health-retries 12",
    ]),
  ].join("\n"));
  assertAccountFailurePolicy(job);
  // Admit only read-only verification of this job's actual service metadata.
  // Every database consumer must use its identity-checked outputs, never a
  // fixed port or another job's endpoint on the shared runner host.
  const preflight = namedStep(text, pgPreflightStepName);
  assert.equal(preflight.trimEnd(), [
    `      - name: ${pgPreflightStepName}`,
    "        id: account-pg", "        env:",
    "          ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}",
    "        run: |",
    "          python3 -m unittest discover -s scripts/tests -p test_account_ci_postgres.py",
    "          python3 scripts/account-ci-postgres.py",
  ].join("\n"));
  for (const name of [existingBrokerStepName, auditPGStepName,
    "Token financial contracts on isolated PostgreSQL", "Account closure contracts on isolated PostgreSQL"]) {
    assert.ok(text.indexOf(preflight) < text.indexOf(namedStep(text, name)),
      `service identity verification must precede ${name}`);
  }
  const pg = namedStep(text, auditPGStepName);
  assert.match(pg, /^ {8}working-directory: account-platform$/m);
  assert.doesNotMatch(pg, /^ {8}(?:if|continue-on-error):/m,
    "finite audit PostgreSQL gate must execute unconditionally and fail closed");
  const env = pg.match(/^ {8}env:\n[\s\S]*?(?=^ {8}\S|$(?![\s\S]))/m);
  assert.ok(env, "finite audit test profile not found");
  assert.equal(env[0].trimEnd(), [
    "        env:", "          F17_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.audit_url }}",
    ...pgIdentityEnvironment("audit").map(value => `          ${value}`),
    "          F17_TEST_DATABASE_PROFILE: github-actions-service",
    "          F17_TEST_SERVICE_PORT: ${{ steps.account-pg.outputs.audit_port }}",
  ].join("\n"));
  // F16's two separately bounded suites use only the other ephemeral service.
  // Validate each whole step before removing it from the audit-only env ban;
  // arbitrary env, commands, conditions and extra steps remain rejected.
  const stop = namedStep(text, pgStopStepName);
  assert.equal(stop.trimEnd(), [
    `      - name: ${pgStopStepName}`,
    "        if: ${{ always() }}", "        env:",
    "          ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}",
    "        run: |",
    "          python3 scripts/account-ci-postgres.py --stop-owned-services",
  ].join("\n"));
  for (const name of [existingBrokerStepName, auditPGStepName,
    "Token financial contracts on isolated PostgreSQL", "Account closure contracts on isolated PostgreSQL"]) {
    assert.ok(text.indexOf(namedStep(text, name)) < text.indexOf(stop), "stop must follow database consumers");
  }
  let outsidePGSteps = text.replace(pg, "").replace(preflight, "").replace(stop, "");
  // Admit only this bounded acceptance step before enforcing the job-wide ban
  // on incidental database inputs. The complete step is fixed deliberately.
  const existingBroker = namedStep(text, existingBrokerStepName);
  assert.equal(existingBroker.trimEnd(), [
    `      - name: ${existingBrokerStepName}`,
    "        timeout-minutes: 12", "        shell: bash", "        env:",
    "          PINVOU_TEST_SCHEMA_DATABASE_URL: ${{ steps.account-pg.outputs.broker_url }}",
    ...pgIdentityEnvironment("broker").map(value => `          ${value}`),
    "        run: |", "          set -euo pipefail",
    "          # Extract only the pinned public server executable. The test itself",
    "          # starts a private loopback broker with its own TLS keys and store.",
    '          fixture_container="$(docker create --network none nats@sha256:ad7a43eb7e3337c3c38ce5d784d1461791f95f730f252d2b25eee699752a0ca3)"',
    `          trap 'docker rm "$fixture_container" >/dev/null' EXIT`,
    '          docker cp "$fixture_container:/nats-server" "$RUNNER_TEMP/pinvou-test-nats-server" 2>/dev/null || \\',
    '            docker cp "$fixture_container:/usr/local/bin/nats-server" "$RUNNER_TEMP/pinvou-test-nats-server"',
    '          printf \'%s  %s\\n\' 2d0dc83e0547185a6820377114215d6a7c645f607aeb31728fffaf3d84e77c10 "$RUNNER_TEMP/pinvou-test-nats-server" | sha256sum --check --strict',
    '          chmod 0700 "$RUNNER_TEMP/pinvou-test-nats-server"',
    '          export PINVOU_TEST_NATS_SERVER_BINARY="$RUNNER_TEMP/pinvou-test-nats-server"',
    "          npm --prefix account-platform run test:existing-broker",
  ].join("\n"));
  outsidePGSteps = outsidePGSteps.replace(existingBroker, "");
  for (const [name, environment, commands] of [
    ["Token financial contracts on isolated PostgreSQL", [
      "ACCOUNT_FINANCIAL_SERVICES: ${{ steps.account-pg.outputs.financial_services }}",
    ], ["set -euo pipefail", "python3 -m unittest discover -s scripts/tests -p test_token_financial_postgres.py", "python3 -m unittest discover -s scripts/tests -p test_financial_parallel.py", "python3 -m unittest discover -s scripts/tests -p test_account_financial_families_postgres.py", "python3 -m unittest discover -s scripts/tests -p test_go_test_contract.py", 'python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/financial-contracts.json" -- python3 scripts/test-financial-parallel.py']],
    ["Account closure contracts on isolated PostgreSQL", [
      "PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.financial_url }}",
      ...pgIdentityEnvironment("financial"),
      "PINVOU_ACCOUNT_CLOSURE_TEST_ARTIFACT_DIR: ${{ runner.temp }}/account-closure-postgres",
    ], ["set -euo pipefail", "python3 -m unittest discover -s scripts/tests -p test_account_closure_postgres.py", "python3 scripts/test-account-closure-postgres.py"]],
  ]) {
    const step = namedStep(text, name);
    assert.equal(step.trimEnd(), [
      `      - name: ${name}`, "        shell: bash", "        env:",
      ...environment.map(value => `          ${value}`), "        run: |",
      ...commands.map(value => `          ${value}`),
    ].join("\n"));
    outsidePGSteps = outsidePGSteps.replace(step, "");
  }
  assert.doesNotMatch(outsidePGSteps, /DATABASE_URL|DATABASE_PROFILE|F17_TEST_|PINVOU_ACCOUNT_CI_PG_|ACCOUNT_CI_SERVICES|ACCOUNT_FINANCIAL_SERVICES/);
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

test("container CI uses immutable official cache images without losing either build or runtime gates", () => {
  const relay = jobBlock("team-relay-hub-test");
  assert.match(relay, /image: mirror\.gcr\.io\/library\/postgres@sha256:[a-f0-9]{64}/);
  assert.match(relay, /--health-cmd "pg_isready/);
  const consoleJob = jobBlock("update-release-console-test");
  const step = namedStep(consoleJob, "发布台容器与 Compose 配置校验");
  const selectedMatch = step.match(/PINVOU_UPDATE_NODE_IMAGE: (mirror\.gcr\.io\/library\/node@sha256:[a-f0-9]{64})/);
  assert.ok(selectedMatch, "the real container step must select an immutable image");
  const selected = selectedMatch[1];
  assert.match(step, /if: \$\{\{ matrix\.node-version == '20' \}\}/);
  assert.match(step, /^ {8}run: bash scripts\/ci-update-release-console-container\.sh$/m);
  assert.doesNotMatch(step, /continue-on-error|\|\| true/);
  const cleanupStep = namedStep(consoleJob, "清理发布台容器");
  const cleanupSelection = cleanupStep.match(/PINVOU_UPDATE_NODE_IMAGE: (mirror\.gcr\.io\/library\/node@sha256:[a-f0-9]{64})/);
  assert.ok(cleanupSelection, "cleanup must receive an immutable image selection");
  assert.equal(cleanupSelection[1], selected, "execution and cleanup must receive the same selected image");
  assert.match(cleanupStep, /if: \$\{\{ always\(\) && matrix\.node-version == '20' \}\}/);
  assert.match(cleanupStep, /^ {8}run: bash scripts\/ci-update-release-console-container\.sh --cleanup$/m);

  const scriptPath = fileURLToPath(new URL("../ci-update-release-console-container.sh", import.meta.url));
  const script = readFileSync(scriptPath, "utf8");
  assert.match(script, /for attempt in \{1\.\.30\}/);
  assert.match(script, /exit 1/);
  assert.match(script, /Authorization: Bearer \$PINVOU_UPDATE_ADMIN_TOKEN/);
  assert.doesNotMatch(script, /\/var\/www\/pinvou3/);
  const directory = mkdtempSync(join(tmpdir(), "pinvou-container-routing-"));
  const shellPath = value => value.replaceAll("\\", "/");
  const prefix = `
    docker() {
      printf '%s|%s\\n' "$PINVOU_UPDATE_NODE_IMAGE" "$*" >> "$LOG"
      if [[ "$1" == build && "$BUILD_STATUS" != 0 ]]; then return "$BUILD_STATUS"; fi
      if [[ "$*" == *'port console 8792'* ]]; then printf '%s\\n' '127.0.0.1:43781'; fi
      return 0
    }
    curl() { printf 'CURL|%s\\n' "$*" >> "$LOG"; }
    python3() { command "$CI_PYTHON" "$@"; }
    source "$CI_SCRIPT"
  `;
  try {
    for (const status of [0, 7]) {
      const log = join(directory, `calls-${status}`);
      const result = spawnSync(bash, ["-e", "-c", prefix], {
        cwd: fileURLToPath(new URL("../../", import.meta.url)), encoding: "utf8", timeout: 30_000, windowsHide: true,
        env: { ...process.env, LOG: shellPath(log), RUNNER_TEMP: shellPath(directory), GITHUB_RUN_ID: `710${status}`, GITHUB_RUN_ATTEMPT: "1",
          CI_SCRIPT: shellPath(scriptPath), CI_PYTHON: shellPath(process.env.PYTHON || "python3"), BUILD_STATUS: String(status),
          PINVOU_UPDATE_NODE_IMAGE: selected, PINVOU_UPDATE_ADMIN_TOKEN: "ci-only-synthetic-routing-token" },
      });
      assert.equal(result.status, status, result.stderr || String(result.error || ""));
      const calls = readFileSync(log, "utf8").trim().split("\n");
      assert.ok(calls.some(line => line.includes(`build --build-arg NODE_IMAGE=${selected} --file`)), "direct build must receive the selected base");
      assert.ok(calls.some(line => line.includes("down --volumes --remove-orphans")), "success and build failure must clean their job project");
      if (status === 0) {
        assert.ok(calls.some(line => line.startsWith(selected + "|compose ") && line.includes("up --detach --build")), "Compose must inherit the selected base");
        assert.ok(calls.some(line => /mirror\.gcr\.io\/library\/nginx@sha256:[a-f0-9]{64} nginx -t/.test(line)));
        assert.ok(calls.some(line => line.startsWith("CURL|") && line.includes("http://127.0.0.1:43781/pinvou3/release-console/healthz")));
        assert.ok(calls.some(line => line.startsWith("CURL|") && line.includes("Authorization: Bearer ci-only-synthetic-routing-token") && line.includes("/api/status")));
      } else {
        assert.ok(!calls.some(line => line.includes("up --detach --build") || line.startsWith("CURL|")), "build failure must precede runtime requests");
      }
      assert.deepEqual(readdirSync(directory).filter(name => name.startsWith("release-console.") || name.endsWith(".path")), [], "the real cleanup must remove temporary state");
    }
  } finally {
    assert.ok(directory.startsWith(join(tmpdir(), "pinvou-container-routing-")));
    rmSync(directory, { recursive: true, force: true });
  }
});

test("each independently changed Account Platform translation input triggers its release gate", () => {
  const paths = filterPaths(workflow, "account_platform");
  // These are independent single-file changes, not a combined PR whose other
  // account-platform/** changes could conceal a missing UI/catalog route.
  const inputs = [
    "pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx",
    "pinvou3-app/src/features/account-platform/AccountPlatformView.jsx",
    "pinvou3-app/src/features/account-platform/AdminPlatformView.jsx",
    "pinvou3-app/src/app/account-main.jsx",
    "pinvou3-app/src/app/admin-main.jsx",
    "pinvou3-app/src/app/portal-session-handoff-main.js",
    "pinvou3-app/src/portal-session-handoff.html",
    "pinvou3-app/src/shared/account-platform-i18n.js",
    "pinvou3-app/src/shared/admin-platform-i18n.js",
    "pinvou3-app/src/shared/supported-locales.js",
    "pinvou3-app/src/shared/i18n.js",
  ];
  for (const input of inputs) {
    assert.ok(paths.some(pattern => pattern === input ||
      (pattern.endsWith("/**") && input.startsWith(pattern.slice(0, -2)))),
    `a sole change to ${input} must trigger Account Platform contracts`);
  }
  const contracts = JSON.parse(readFileSync(new URL("../../account-platform/package.json", import.meta.url), "utf8"));
  assert.equal(contracts.scripts["test:contracts"], "node --test test/contract/*.test.mjs");
  const releaseGate = readFileSync(new URL("../../account-platform/test/contract/i18n-release-gate.test.mjs", import.meta.url), "utf8");
  assert.match(releaseGate, /check-account-platform-i18n\.mjs/);
});

test("Account Go routing skips desktop notes and generated assets but retains mixed changes", () => {
  const paths = filterPaths(workflow, "account_platform");
  assert.match(workflow, /predicate-quantifier: some-with-excludes/);
  const required = changed => changed.some(file =>
    paths.some(pattern => !pattern.startsWith("!") && matchesGlob(file, pattern)) &&
    !paths.some(pattern => pattern.startsWith("!") && matchesGlob(file, pattern.slice(1))));
  const desktop = [
    "account-platform/docs/draft-model-confirmation.md",
    "pinvou3-app/src/platform/tauri/bridge/chat.js",
    "pinvou3-app/src/platform/tauri/bridge/sessions.js",
    "pinvou3-app/tests/session_nav_race.test.mjs",
  ];
  const assets = [
    "account-platform/internal/portal/static/account.html",
    "account-platform/internal/portal/static/assets/admin-old.js",
    "account-platform/internal/portal/static/assets/admin-new.js",
  ];
  assert.equal(required(desktop), false);
  assert.equal(required(assets), false);
  for (const file of [
    "account-platform/internal/billing/payment_admission.go",
    "account-platform/internal/portal/static.go",
    "account-platform/go.mod",
    "account-platform/go.sum",
    "account-platform/migrations/billing/new.sql",
    "account-platform/deploy/helm/account-platform/values.yaml",
    "account-platform/contracts/openapi/new.yaml",
    "account-platform/test/contract/new.test.mjs",
    "account-platform/docs/requirements.md",
    "account-platform/docs/new-policy.md",
    "scripts/test-token-financial-postgres.py",
    ".github/workflows/pr-check.yml",
    "scripts/tests/pr-check-routing.test.mjs",
  ]) {
    assert.equal(required([file]), true, file);
    assert.equal(required([...desktop, ...assets, file]), true, `mixed ${file}`);
  }
  const frontend = filterPaths(workflow, "frontend");
  for (const file of assets) {
    assert.ok(frontend.some(pattern => !pattern.startsWith("!") && matchesGlob(file, pattern)), file);
  }
  const gate = jobBlock("required-gate");
  assert.match(gate, /ACCOUNT_PLATFORM_REQUIRED: \$\{\{ needs\.changes\.outputs\.account_platform \}\}/);
  assert.match(gate, /if \[\[ "\$ACCOUNT_PLATFORM_REQUIRED" == "true" && "\$ACCOUNT_PLATFORM_RESULT" != success \]\]/);
});

test("Account Platform changes cannot bypass the Go gate at any PR or queue stage", () => {
  const paths = filterPaths(workflow, "account_platform");
  for (const path of ["scripts/check-account-platform-authorization-coverage.mjs", "scripts/tests/account-platform-authorization-coverage.test.mjs", "scripts/tests/test_postgres_backup.py", "scripts/tests/test_postgres_backup_job.py", "scripts/tests/test_user_realm.py"]) {
    assert.ok(paths.includes(path), `authorization changes must trigger the Go gate: ${path}`);
  }
  for (const path of ["account-platform/**", "scripts/account-platform*", "scripts/check-account-platform*", "scripts/check-token-management-authorization-coverage.mjs", "scripts/tests/token-management-authorization-coverage.test.mjs", "scripts/tests/token-management-phase-coverage.test.mjs", ".github/workflows/pr-check.yml", "scripts/tests/pr-check-routing.test.mjs"]) {
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
    "node --test scripts/tests/token-management-phase-coverage.test.mjs",
  ]);
  for (const [checkerStatus, formalStatus, phaseStatus, expectedOutput, expectedStatus] of [
    [7, 0, 0, "checker", 7],
    [0, 8, 0, "checkerformal", 8],
    [0, 0, 9, "checkerformalphase", 9],
    [0, 0, 0, "checkerformalphase", 0],
  ]) {
    const fixture = `node() { case "$*" in
      scripts/check-token-management-authorization-coverage.mjs) printf checker; return ${checkerStatus};;
      '--test scripts/tests/token-management-authorization-coverage.test.mjs') printf formal; return ${formalStatus};;
      '--test scripts/tests/token-management-phase-coverage.test.mjs') printf phase; return ${phaseStatus};;
      *) printf unexpected >&2; return 99;;
    esac; };\n`;
    const result = spawnSync(bash, ["-e", "-c", fixture + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, expectedStatus, result.stderr);
    assert.equal(result.stdout, expectedOutput);
  }
  assertAccountFailurePolicy(job);
});

test("diagnostic delivery cannot make Account tests or resource retirement advisory", () => {
  const job = jobBlock("account-platform-test").replaceAll("\r\n", "\n");
  assertAccountFailurePolicy(job);
  for (const name of [goStepName, "Confirm Account test resource retirement"]) {
    assert.throws(() => assertAccountFailurePolicy(job.replace(
      `      - name: ${name}\n`, `      - name: ${name}\n        continue-on-error: true\n`,
    )));
  }
  for (const [before, after] of [
    ["        timeout-minutes: 3\n", ""],
    ["        uses: actions/upload-artifact@v7", "        uses: actions/upload-artifact@main"],
    ["    name: account-platform-test\n", "    name: account-platform-test\n    continue-on-error: true\n"],
  ]) {
    assert.ok(job.includes(before));
    assert.throws(() => assertAccountFailurePolicy(job.replace(before, after)));
  }
});

test("restored operator unit gates reject zero, skipped, missing and failed evidence without operations", () => {
  for (const [step, command] of [
    ["Account Platform optional backup tool contracts", "test:postgres-backup-tools"],
    ["Account Platform offline user realm contracts", "test:user-realm-tools"],
  ]) {
    const run = stepRun(jobBlock("account-platform-test"), step);
    assert.ok(run.includes(`npm --prefix account-platform run ${command}`));
    assert.doesNotMatch(run, /docker|systemctl|account-platform-postgres-backup-job\.py/);
    const directory = mkdtempSync(join(tmpdir(), "pinvou-backup-ci-"));
    try {
      for (const [output, status, expected] of [
        ["Ran 34 tests in 0.1s\n\nOK\n", 0, 0],
        ["Ran 0 tests in 0.1s\n\nOK\n", 0, 1],
        ["Ran 34 tests in 0.1s\n\nOK (skipped=1)\n", 0, 1],
        ["", 0, 1],
        ["Ran 34 tests in 0.1s\n\nFAILED (failures=1)\n", 0, 1],
        ["Ran 34 tests in 0.1s\n\nOK\n", 7, 7],
      ]) {
        const prefix = `npm() { printf '%s' '${output}'; return ${status}; };\n`;
        const result = spawnSync(bash, ["-c", prefix + run], {
          cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true,
          env: { ...process.env, RUNNER_TEMP: directory.replaceAll("\\", "/") },
        });
        assert.equal(result.status, expected, `${status}/${output}: ${result.stderr}`);
      }
    } finally {
      assert.ok(directory.startsWith(join(tmpdir(), "pinvou-backup-ci-")));
      rmSync(directory, { recursive: true, force: true });
    }
  }
});

test("whole Account Platform authorization failures block the CI gate", () => {
  const job = jobBlock("account-platform-test");
  const run = stepRun(job, "Verify Account Platform authorization contracts");
  assert.deepEqual(run.trim().split(/\r?\n/), [
    "node scripts/check-account-platform-authorization-coverage.mjs",
    "node --test scripts/tests/account-platform-authorization-coverage.test.mjs",
  ]);
  for (const [checkerStatus, formalStatus, expectedOutput, expectedStatus] of [[7, 0, "checker", 7], [0, 8, "checkerformal", 8], [0, 0, "checkerformal", 0]]) {
    const fixture = `node() { if [[ "$1" == scripts/check-account-platform-authorization-coverage.mjs ]]; then printf checker; return ${checkerStatus}; else printf formal; return ${formalStatus}; fi; };\n`;
    const result = spawnSync(bash, ["-e", "-c", fixture + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, expectedStatus, result.stderr);
    assert.equal(result.stdout, expectedOutput);
  }
  assertFiniteAuditPGSetup(job);
});

test("Account Platform Node gate executes its contracts and rejects empty, failed or skipped runs", () => {
  const job = jobBlock("account-platform-test");
  const install = namedStep(job, "Install Account Platform contract dependencies");
  assert.match(install, /working-directory: account-platform/);
  assert.match(install, /npm ci --ignore-scripts --prefer-offline --no-audit/);
  const nodeStep = namedStep(job, "Account Platform Node contracts");
  assert.match(nodeStep, /working-directory: account-platform/);
  assert.match(nodeStep, /PINVOU_TEST_KMS_HELM: "1"/);
  assert.match(nodeStep, /PINVOU_TEST_KMS_COMPOSE: "1"/);
  const run = stepRun(job, "Account Platform Node contracts");
  assert.match(run, /node --test --test-reporter=tap test\/contract\/\*\.test\.mjs/);
  const directory = mkdtempSync(join(tmpdir(), "pinvou-node-gate-"));
  try {
    for (const [passed, failed, skipped, exit, expected] of [
      [1, 0, 0, 0, 0], [0, 0, 0, 0, 1], [2, 1, 0, 0, 1],
      [2, 1, 0, 1, 1], [1, 0, 0, 7, 7], [1, 0, 1, 0, 1], [1, 0, null, 0, 1],
    ]) {
      const skipSummary = skipped === null ? "" : `# skipped ${skipped}\\n`;
      const prefix = `node() { printf '# pass ${passed}\\n# fail ${failed}\\n${skipSummary}'; return ${exit}; }; export -f node;\n`;
      const result = spawnSync(bash, ["-c", prefix + run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true, env: { ...process.env, RUNNER_TEMP: directory.replaceAll("\\", "/") } });
      assert.equal(result.status, expected, `${passed}/${failed}/${skipped}/${exit}: ${result.stderr}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("offline render gate pins tool downloads, hashes and exported binaries", () => {
  const job = jobBlock("account-platform-test");
  const install = namedStep(job, "Install pinned offline-render tools");
  assert.match(install, /helm-v3\.16\.4-linux-amd64\.tar\.gz/);
  assert.match(install, /docker\/compose\/releases\/download\/v5\.4\.0\/docker-compose-linux-x86_64/);
  assert.match(install, /prometheus\/releases\/download\/v3\.13\.5\/prometheus-3\.13\.5\.linux-amd64\.tar\.gz/);
  assert.equal((install.match(/sha256sum --check --strict/g) ?? []).length, 3);
  for (const variable of [
    "PINVOU_TEST_KMS_HELM_BINARY",
    "PINVOU_TEST_KMS_COMPOSE_BINARY",
    "PINVOU_TEST_PROMTOOL_BINARY",
  ]) {
    assert.match(install, new RegExp(`${variable}=`));
    assert.match(install, /GITHUB_ENV/);
  }
  const render = namedStep(job, "Account Platform rendered Prometheus rule scenarios");
  assert.match(render, /PINVOU_TEST_KMS_HELM_BINARY/);
  assert.match(render, /PINVOU_TEST_PROMTOOL_BINARY/);
  const assertPinned = candidate => {
    const step = namedStep(candidate, "Install pinned offline-render tools");
    assert.equal((step.match(/sha256sum --check --strict/g) ?? []).length, 3);
    for (const variable of ["PINVOU_TEST_KMS_HELM_BINARY", "PINVOU_TEST_KMS_COMPOSE_BINARY", "PINVOU_TEST_PROMTOOL_BINARY"]) {
      assert.match(step, new RegExp(`${variable}=`));
      assert.match(step, /GITHUB_ENV/);
    }
  };
  assertPinned(job);
  for (const candidate of [
    job.replace(install, "", 1),
    job.replace(install, install.replace("sha256sum --check --strict", "sha256sum --check", 1), 1),
    job.replace(install, install.replace("PINVOU_TEST_KMS_COMPOSE_BINARY", "PINVOU_TEST_KMS_COMPOSE_BINARY_REMOVED", 1), 1),
  ]) {
    assert.throws(() => {
      assertPinned(candidate);
    });
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
    ['"127.0.0.1::5432"', '"5432"'],
    ['"127.0.0.1::5432"', '"0.0.0.0::5432"'],
    ['"127.0.0.1::5432"', '"127.0.0.1:5432:5432"'],
    ["          --rm\n", ""],
    ["mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea", "mirror.gcr.io/library/postgres@sha256:0000000000000000000000000000000000000000000000000000000000000000"],
    ["      audit-export-postgres:", "      unrelated-postgres:"],
    ["    env:\n      GOTOOLCHAIN:", "      another-service:\n        image: postgres:16-alpine\n    env:\n      GOTOOLCHAIN:"],
    ["        ports:", "        volumes:\n          - shared-data:/var/lib/postgresql/data\n        ports:"],
    ["F17_TEST_DATABASE_PROFILE: github-actions-service", "F17_TEST_DATABASE_PROFILE: local-owned"],
    ["F17_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.audit_url }}", "F17_TEST_DATABASE_URL: postgres://postgres@remote.invalid:5432/postgres?sslmode=disable"],
    ["PINVOU_TEST_SCHEMA_DATABASE_URL: ${{ steps.account-pg.outputs.broker_url }}", "PINVOU_TEST_SCHEMA_DATABASE_URL: postgres://postgres@remote.invalid:5434/postgres?sslmode=disable"],
    ["npm --prefix account-platform run test:existing-broker", "npm --prefix account-platform run test:existing-broker || true"],
    ["ACCOUNT_FINANCIAL_SERVICES: ${{ steps.account-pg.outputs.financial_services }}", "ACCOUNT_FINANCIAL_SERVICES: ${{ toJSON(needs.other.outputs.services) }}"],
    ["PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.financial_url }}", "PINVOU_ACCOUNT_CLOSURE_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.audit_url }}"],
    ["ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}", "ACCOUNT_CI_SERVICES: ${{ toJSON(needs.other.outputs.services) }}"],
    ["        id: account-pg", "        id: unrelated-pg"],
    ["python3 scripts/account-ci-postgres.py", "python3 scripts/account-ci-postgres.py || true"],
    ["python3 -m unittest discover -s scripts/tests -p test_account_ci_postgres.py", "true"],
    ["python3 scripts/test-financial-parallel.py", "python3 scripts/test-financial-parallel.py || true"],
    ['python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/financial-contracts.json" -- ', ""],
    ["          python3 -m unittest discover -s scripts/tests -p test_account_financial_families_postgres.py\n", ""],
    ["          python3 -m unittest discover -s scripts/tests -p test_go_test_contract.py\n", ""],
    ["python3 scripts/test-account-closure-postgres.py", "python3 scripts/test-account-closure-postgres.py || true"],
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
    const changed = original.replace(before, () => after);
    assert.notEqual(changed, original, `mutation must change its exact target: ${before}`);
    const mutatedJob = original === job ? changed : job.replace(original, () => changed);
    assert.throws(() => assertFiniteAuditPGSetup(mutatedJob), `must reject: ${after}`);
  }
  // Every consumer needs all three identity fields from its own service.
  const rejectStepMutation = (name, step, changed, expected) => {
    assert.notEqual(changed, step, `mutation must hit ${name}`);
    // A complete audit step contains the shell regex suffix $'. Returning the
    // payload literally prevents String.replace from expanding it as $' (the
    // original suffix), which would make rejection pass for duplicated steps.
    const mutated = job.replace(step, () => changed);
    assert.equal(namedStep(mutated, name), changed, "mutation must preserve the literal step payload");
    for (const required of [pgPreflightStepName, auditPGStepName, existingBrokerStepName,
      "Token financial contracts on isolated PostgreSQL", "Account closure contracts on isolated PostgreSQL"]) {
      namedStep(mutated, required); // Missing/duplicate steps cannot satisfy a negative control.
    }
    assert.throws(() => assertFiniteAuditPGSetup(mutated), expected);
  };
  for (const [name, service] of [[auditPGStepName, "audit"], [existingBrokerStepName, "broker"],
    ["Account closure contracts on isolated PostgreSQL", "financial"]]) {
    const step = namedStep(job, name);
    const miswired = step.replace(`outputs.${service}_url`, "outputs.unrelated_url");
    assert.notEqual(miswired, step, "URL mutation must hit the selected consumer");
    rejectStepMutation(name, step, miswired);
    for (const entry of pgIdentityEnvironment(service)) {
      for (const replacement of ["", entry.replace(`outputs.${service}_`, "outputs.unrelated_")]) {
        rejectStepMutation(name, step,
          step.replace(`          ${entry}\n`, () => replacement ? `          ${replacement}\n` : ""));
      }
    }
    rejectStepMutation(name, step,
      step.replace("        run:", () => "        if: ${{ false }}\n        run:"),
      name === auditPGStepName ? /finite audit PostgreSQL gate must execute unconditionally/ : undefined);
  }
  const financial = namedStep(job, "Token financial contracts on isolated PostgreSQL");
  for (const replacement of ["", "ACCOUNT_FINANCIAL_SERVICES: ${{ steps.account-pg.outputs.unrelated }}"]) {
    rejectStepMutation("Token financial contracts on isolated PostgreSQL", financial,
      financial.replace("          ACCOUNT_FINANCIAL_SERVICES: ${{ steps.account-pg.outputs.financial_services }}\n",
        () => replacement ? `          ${replacement}\n` : ""));
  }
  rejectStepMutation("Token financial contracts on isolated PostgreSQL", financial,
    financial.replace("        run:", () => "        if: ${{ false }}\n        run:"));
  const preflight = namedStep(job, pgPreflightStepName);
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(preflight, "")));
  rejectStepMutation(pgPreflightStepName, preflight,
    preflight.replace("        run:", () => "        if: ${{ false }}\n        run:"));
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(preflight, "") + preflight));
  assert.throws(() => assertFiniteAuditPGSetup(job.replace("      GOTOOLCHAIN: local",
    "      PINVOU_ACCOUNT_CI_PG_PORT: ${{ steps.account-pg.outputs.audit_port }}\n      GOTOOLCHAIN: local")));
});

test("PostgreSQL service preflight propagates admission test and verifier failures", () => {
  const run = stepRun(jobBlock("account-platform-test"), pgPreflightStepName);
  for (const [guard, verifier, expected, calls] of [
    [7, 0, 7, "guard"], [0, 8, 8, "guardverifier"], [0, 0, 0, "guardverifier"],
  ]) {
    const prefix = `python3() { if [[ "$1" == -m ]]; then printf guard; return ${guard}; else printf verifier; return ${verifier}; fi; };\n`;
    const result = spawnSync(bash, ["-e", "-c", prefix + run],
      { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, expected, result.stderr);
    assert.equal(result.stdout, calls);
  }
});

test("owned PostgreSQL stop is always scheduled and propagates failure", () => {
  const job = jobBlock("account-platform-test");
  assertFiniteAuditPGSetup(job);
  const stop = namedStep(job, pgStopStepName);
  for (const replacement of ["        if: ${{ false }}", "        if: ${{ success() }}"]) {
    assert.throws(() => assertFiniteAuditPGSetup(job.replace(stop, () => stop.replace("        if: ${{ always() }}", replacement))));
  }
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(stop, "")));
  const audit = namedStep(job, auditPGStepName);
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(audit, "") + audit), /stop must follow database consumers/);
  const run = stepRun(job, pgStopStepName);
  for (const exit of [0, 7]) {
    const result = spawnSync(bash, ["-e", "-c", `python3() { return ${exit}; };\n` + run],
      { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, exit);
  }
});

test("existing broker acceptance is required and propagates fixture failures", () => {
  const job = jobBlock("account-platform-test");
  assertFiniteAuditPGSetup(job);
  const run = stepRun(job, existingBrokerStepName);
  const prefix = `docker() { case "$1" in create) printf 'fixture-container\\n';; cp|rm) return 0;; *) return 8;; esac; };\n` +
    `sha256sum() { cat >/dev/null; return 0; }; chmod() { return 0; };\n` +
    `npm() { [[ "$*" == '--prefix account-platform run test:existing-broker' ]] || return 9; return "\${TEST_NPM_EXIT:-0}"; };\n`;
  for (const exit of [0, 7]) {
    const result = spawnSync(bash, ["-c", prefix + run], { encoding: "utf8", timeout: 30_000, windowsHide: true, env: { ...process.env, RUNNER_TEMP: tmpdir().replaceAll("\\", "/"), TEST_NPM_EXIT: String(exit) } });
    assert.equal(result.status, exit, `acceptance exit ${exit} must propagate: ${result.stderr}`);
  }
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(namedStep(job, existingBrokerStepName), "")));
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

test("Token financial PostgreSQL gate is required and cannot swallow a failed runner", () => {
  const paths = filterPaths(workflow, "account_platform");
  assert.ok(paths.includes("scripts/test-token-financial-postgres.py"));
  assert.ok(paths.includes("scripts/test-financial-parallel.py"));
  assert.ok(paths.includes("scripts/tests/test_financial_parallel.py"));
  assert.ok(paths.includes("scripts/tests/test_token_financial_postgres.py"));
  const job = jobBlock("account-platform-test");
  assert.match(job, /image: mirror\.gcr\.io\/library\/postgres@sha256:[a-f0-9]{64}/);
  const services = job.match(/^    services:\r?\n([\s\S]*?)^    env:/m)?.[1];
  assert.ok(services, "the disposable PostgreSQL service must exist");
  assert.deepEqual([...services.matchAll(/^ {6}([a-z][a-z0-9-]*):\r?$/gm)].map(match => match[1]), ["audit-export-postgres", "existing-broker-postgres", "financial-postgres", "financial-token-postgres", "financial-billing-0-postgres", "financial-billing-1-postgres"]);
  assert.deepEqual([...services.matchAll(/^ {10}- (.+)\r?$/gm)].map(match => match[1]), Array(6).fill('"127.0.0.1::5432"'));
  assertFiniteAuditPGSetup(job);
  const run = stepRun(job, "Token financial contracts on isolated PostgreSQL");
  for (const failedPhase of ["guard", "postgres"]) {
    const mock = `python3() { case "$*" in *unittest*) ${failedPhase === "guard" ? "return 7" : "return 0"};; *) ${failedPhase === "postgres" ? "return 7" : "return 0"};; esac; }; export -f python3;\n`;
    const result = spawnSync(bash, ["-c", mock + run], { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, 7, `${failedPhase} failure must fail the gate: ${result.stderr}`);
  }
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
  for (const match of gate.matchAll(/^          ([A-Z0-9_]+_RESULT):/gm)) baseEnv[match[1]] = "success";
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

test("MCP checks are selected by their inputs and fail closed on unknown routing", () => {
  const paths = filterPaths(workflow, "mcp_contract");
  for (const input of [
    "pinvou3-app/resources/mcp-servers/**",
    "scripts/mcp-server-contract-smoke.py",
    "scripts/tests/test_mcp_schema_contract.py",
    "CodeWhale",
    ".gitmodules",
    ".github/workflows/pr-check.yml",
  ]) assert.ok(paths.includes(input), `MCP input ${input} must select its tests`);

  const job = workflow.split("\n  fast-gate:")[1].split("\n  frontend-test:")[0];
  assert.doesNotMatch(job, /\n    needs: changes\s*\n/);
  const condition = "steps.mcp-filter.outputs.mcp_contract != 'false' || (github.event_name == 'pull_request' && (github.event.pull_request.changed_files == null || github.event.pull_request.changed_files >= 3000))";
  for (const name of [
    "Python MCP server 测试 (mcp-servers/*/test_*.py)",
    "内置 MCP 协议与产物契约",
  ]) {
    assert.equal(stepCondition(job, name), condition);
  }
  const firstIfRemoved = job.replace(`if: \${{ ${condition} }}`, "# condition removed");
  assert.throws(() => stepCondition(firstIfRemoved, "Python MCP server 测试 (mcp-servers/*/test_*.py)"));
});

test("an exact Rust test cache hit skips only the redundant lint restore", () => {
  const job = workflow.split("\n  rust-test:")[1].split("\n  cli-test:")[0];
  const probe = job.split("- name: 检查 Rust test 缓存是否精确命中")[1]
    .split("\n      - name:")[0];
  const testCache = job.split("- name: Cargo test cache")[1]
    .split("\n      - name:")[0];
  for (const setting of ["workspaces: pinvou3-app/src-tauri", "shared-key: rust-test-v2"]) {
    assert.ok(probe.includes(setting) && testCache.includes(setting), `probe and restore must agree on ${setting}`);
  }
  assert.match(probe, /lookup-only: true/);
  assert.match(probe, /save-if: false/);
  assert.equal(stepCondition(job, "恢复 Rust lint 暖缓存"), "steps.rust-test-cache-probe.outputs.cache-hit != 'true'");
  assert.equal(stepCondition(job, "恢复被淘汰缓存的 Rust lint 回退"), "steps.rust-test-cache-probe.outputs.cache-hit == 'true' && steps.rust-test-cache.outputs.cache-hit != 'true'");
  const recovery = job.split("- name: 恢复被淘汰缓存的 Rust lint 回退")[1].split("\n      - name:")[0];
  assert.match(recovery, /uses: Swatinem\/rust-cache@v2/);
  assert.match(recovery, /workspaces: pinvou3-app\/src-tauri/);
  assert.match(recovery, /shared-key: rust-lint-v2/);
  assert.match(recovery, /cache-on-failure: true/);
  assert.match(recovery, /save-if: false/);
  assert.ok(job.indexOf("检查 Rust test 缓存是否精确命中") < job.indexOf("恢复 Rust lint 暖缓存"));
  assert.ok(job.indexOf("恢复 Rust lint 暖缓存") < job.indexOf("Cargo test cache"));
  assert.ok(job.indexOf("Cargo test cache") < job.indexOf("恢复被淘汰缓存的 Rust lint 回退"));
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


const frontendProbeDependencyPaths = [
  "account-platform/package.json",
  "account-platform/package-lock.json",
  "account-platform/contracts/deployment/model-token-release-plan.schema.json",
  "account-platform/contracts/deployment/model-token-release-evidence.schema.json",
  "account-platform/deploy/nginx/credential-edge.example.json",
  "account-platform/internal/portal/credential-handoff.v1.js",
  "account-platform/internal/portal/credential_handoff_hash_generated.go",
  "scripts/account-platform-credential-browser-scope.mjs",
  "scripts/account-platform-credential-origin-probe.mjs",
  "scripts/account-platform-credential-edge.mjs",
  "scripts/account-platform-model-token-release-plan.mjs",
];
const frontendProbeInstallStep = "Install Account Platform probe dependencies";
const frontendLogicStep = "前端逻辑测试 (pet 全套 + diff parser + markdown)";

function assertFrontendProbeDependencies(text) {
  const job = jobBlock("frontend-test", text).replaceAll("\r\n", "\n");
  assert.match(job, /^ {8}working-directory: pinvou3-app$/m);
  const install = namedStep(job, frontendProbeInstallStep);
  assert.match(install, /^ {8}run: npm --prefix \.\.\/account-platform ci --ignore-scripts --prefer-offline --no-audit$/m);
  assert.doesNotMatch(install, /^ {8}(?:if|continue-on-error|working-directory):/m,
    "The logic test loads AP dependencies on Draft too, in the inherited app cwd");
  assert.ok(job.indexOf(install) < job.indexOf(namedStep(job, frontendLogicStep)),
    "AP tools must be installed before npm test module discovery");
  const setup = job.match(/^ {6}- uses: actions\/setup-node@\S+\n[\s\S]*?(?=^ {6}- |$(?![\s\S]))/m);
  assert.ok(setup, "frontend setup-node block not found");
  assert.match(setup[0], /^ {12}account-platform\/package-lock\.json$/m);
  const paths = filterPaths(text, "frontend");
  for (const path of frontendProbeDependencyPaths) {
    assert.ok(paths.includes(path), `${path} must route the frontend module-loading gate`);
  }
}

test("frontend probe tool dependencies are installed before logic tests on Draft and Ready", () => {
  assertFrontendProbeDependencies(workflow);
});

test("frontend probe dependency guard rejects incomplete or conditional installation and missing imports", () => {
  const job = jobBlock("frontend-test");
  const install = namedStep(job, frontendProbeInstallStep);
  const variants = [
    job.replace(install, ""),
    job.replace(install, install.replace("        run:", "        if: ${{ github.event.pull_request.draft == false }}\n        run:")),
    job.replace(install, install.replace("        run:", "        continue-on-error: true\n        run:")),
    job.replace(install, install.replace("../account-platform", "../../account-platform")),
    job.replace(install, "") + install,
    job.replace(/^ {12}account-platform\/package-lock\.json\r?\n/m, ""),
  ];
  for (const variant of variants) {
    assert.throws(() => assertFrontendProbeDependencies(workflow.replace(job, variant)));
  }
  for (const path of frontendProbeDependencyPaths) {
    assert.throws(() => assertFrontendProbeDependencies(
      workflow.replace(`              - '${path}'`, "")), `missing route ${path}`);
  }
});

const handoffGenerator = "scripts/build-credential-handoff.mjs";
const documentBaselineChecker = "scripts/check-recharge-product-document-baseline.mjs";
const generatedInputsStep = "Verify Account Platform generated inputs and document baseline";
const handoffDeploymentStep = "Verify credential handoff built deployment boundary";

// These simple path patterns use the same inclusion/exclusion semantics as the
// workflow's some-with-excludes. Each input is a separate changed-file set.
function inputSelects(text, filter, input) {
  const patterns = filterPaths(text, filter);
  return patterns.some(pattern => !pattern.startsWith("!") && matchesGlob(input, pattern))
    && !patterns.some(pattern => pattern.startsWith("!") && matchesGlob(input, pattern.slice(1)));
}

function assertAccountPlatformInputChecks(text) {
  assert.match(text, /predicate-quantifier: some-with-excludes/);
  for (const input of [handoffGenerator, documentBaselineChecker]) {
    assert.ok(inputSelects(text, "account_platform", input), `${input} must select Account Platform checks`);
  }
  assert.ok(inputSelects(text, "frontend", handoffGenerator), "handoff deployment checker needs freshly built Portal assets");
  const account = jobBlock("account-platform-test", text);
  const generated = namedStep(account, generatedInputsStep);
  assert.match(generated, /^ {8}shell: bash$/m);
  assert.doesNotMatch(generated, /^ {8}(?:if|continue-on-error|working-directory|env):/m);
  assert.deepEqual(stepRun(account, generatedInputsStep).trim().split(/\r?\n/), [
    "set -euo pipefail", `node ${handoffGenerator} --check`, `node ${documentBaselineChecker}`,
  ]);
  const frontend = jobBlock("frontend-test", text);
  const deployment = namedStep(frontend, handoffDeploymentStep);
  assert.match(deployment, /^ {8}shell: bash$/m);
  assert.doesNotMatch(deployment, /^ {8}(?:if|continue-on-error|working-directory|env):/m);
  assert.deepEqual(stepRun(frontend, handoffDeploymentStep).trim().split(/\r?\n/), [
    "set -euo pipefail", `node ../${handoffGenerator} --check-deployment`,
  ]);
  const portalBuild = namedStep(frontend, "Verify embedded Account Portal assets are current");
  assert.doesNotMatch(portalBuild, /^ {8}(?:if|continue-on-error):/m);
  assert.match(stepRun(frontend, "Verify embedded Account Portal assets are current"), /^npm run build:account-portal$/m);
  assert.ok(frontend.indexOf(portalBuild)
    < frontend.indexOf(deployment), "deployment check must follow the actual Portal build");
}

test("Account Platform generator/checker inputs reach exact applicable CI legs", () => {
  assertAccountPlatformInputChecks(workflow);
  assert.equal(inputSelects(workflow, "frontend", documentBaselineChecker), false,
    "document-only policy checker does not need a browser build");
  for (const input of [
    "account-platform/internal/portal/credential-handoff.v1.js",
    "account-platform/internal/portal/credential_handoff_hash_generated.go",
  ]) {
    assert.ok(inputSelects(workflow, "account_platform", input));
    assert.ok(inputSelects(workflow, "frontend", input));
  }
});

test("Account Platform generator/checker CI guard rejects old routes and bypassed checks", () => {
  assertAccountPlatformInputChecks(workflow);
  const account = jobBlock("account-platform-test");
  const frontend = jobBlock("frontend-test");
  const generated = namedStep(account, generatedInputsStep);
  const deployment = namedStep(frontend, handoffDeploymentStep);
  const portalBuild = namedStep(frontend, "Verify embedded Account Portal assets are current");
  const variants = [
    workflow.replaceAll(`              - '${handoffGenerator}'\r\n`, "").replaceAll(`              - '${handoffGenerator}'\n`, ""),
    workflow.replaceAll(`              - '${documentBaselineChecker}'\r\n`, "").replaceAll(`              - '${documentBaselineChecker}'\n`, ""),
    workflow.replace(generated, ""), workflow.replace(deployment, ""),
    workflow.replace(`node ${handoffGenerator} --check`, `node ${handoffGenerator}`),
    workflow.replace(`node ${documentBaselineChecker}`, "true"),
    workflow.replace(`node ../${handoffGenerator} --check-deployment`, "true"),
    workflow.replace(generated, generated.replace("        run:", "        if: ${{ github.event.pull_request.draft == false }}\n        run:")),
    workflow.replace(deployment, deployment.replace("        run:", "        continue-on-error: true\n        run:")),
    workflow.replace("npm run build:account-portal", "true"),
    workflow.replace(portalBuild, portalBuild.replace("        run:", "        if: ${{ github.event.pull_request.draft == false }}\n        run:")),
    workflow.replace(frontend, frontend.replace(deployment, "").replace("      - name: Vite build", `${deployment}      - name: Vite build`)),
  ];
  for (const variant of variants) assert.throws(() => assertAccountPlatformInputChecks(variant));
});

test("Account Platform generator/checker steps run real inputs and propagate failures", () => {
  const directory = mkdtempSync(join(tmpdir(), "pinvou-generated-ci-"));
  try {
    const put = (relative, body) => {
      const parts = relative.split("/"); parts.pop();
      mkdirSync(join(directory, ...parts), { recursive: true });
      writeFileSync(join(directory, relative), body);
    };
    for (const script of [handoffGenerator, documentBaselineChecker]) {
      put(script, readFileSync(new URL(`../../${script}`, import.meta.url)));
    }
    const source = "(() => { /* controlled public handoff input */ })();\n";
    put("account-platform/internal/portal/credential-handoff.v1.js", source);
    const generatedPath = "account-platform/internal/portal/credential_handoff_hash_generated.go";
    // Generate only inside this owned fixture. The workflow below must use the
    // read-only check mode and must reject subsequent controlled output drift.
    const prepare = spawnSync(process.execPath, [join(directory, handoffGenerator)],
      { cwd: directory, encoding: "utf8", timeout: 10_000, windowsHide: true });
    assert.equal(prepare.status, 0, prepare.stderr);
    for (const name of ["requirements", "architecture", "implementation-tasks", "implementation-status"]) {
      put(`account-platform/docs/${name}.md`, "Catalog facts are supplied by the runtime authority.\n");
    }
    put("account-platform/internal/portal/static/index.html", "<!doctype html><title>controlled public fixture</title>\n");
    mkdirSync(join(directory, "pinvou3-app"));
    const executable = process.execPath.replaceAll("\\", "/").replaceAll("'", "'\\''");
    const prefix = `node() { '${executable}' "$@"; };\n`;
    const execute = (job, name, cwd = directory) => spawnSync(bash,
      ["--noprofile", "--norc", "-c", prefix + stepRun(jobBlock(job), name)],
      { cwd, encoding: "utf8", timeout: 30_000, windowsHide: true });
    const clean = execute("account-platform-test", generatedInputsStep);
    assert.equal(clean.status, 0, clean.stderr);
    assert.match(clean.stdout, /Recharge product document baseline passed/);
    const generated = readFileSync(join(directory, generatedPath));
    put(generatedPath, "// stale controlled artifact\n");
    const stale = execute("account-platform-test", generatedInputsStep);
    assert.equal(stale.status, 1); assert.match(stale.stderr, /handoff script digest is stale/);
    assert.doesNotMatch(stale.stdout, /baseline passed/);
    put(generatedPath, generated);
    put("account-platform/docs/requirements.md", "售价为 10 元\n");
    const catalog = execute("account-platform-test", generatedInputsStep);
    assert.equal(catalog.status, 1); assert.match(catalog.stderr, /baseline contains runtime catalog facts/);
    const cleanDeployment = execute("frontend-test", handoffDeploymentStep, join(directory, "pinvou3-app"));
    assert.equal(cleanDeployment.status, 0, cleanDeployment.stderr);
    put("account-platform/internal/portal/static/unsafe.js", "navigator.serviceWorker.register('/controlled-worker.js');\n");
    const worker = execute("frontend-test", handoffDeploymentStep, join(directory, "pinvou3-app"));
    assert.equal(worker.status, 1); assert.match(worker.stderr, /Service Worker registration may cover/);
  } finally {
    assert.ok(directory.startsWith(join(tmpdir(), "pinvou-generated-ci-")));
    rmSync(directory, { recursive: true, force: true });
  }
});


test("Account Platform task tests run for their own inputs without bypassing production CLI preflights", () => {
  for (const input of [
    "scripts/check-account-platform-tasks.test.mjs",
    "scripts/check-account-platform-tasks.mjs",
    "scripts/account-platform.mjs",
    "scripts/account-platform-dev-nats.mjs",
    "scripts/account-platform-dev-nats-storage.mjs",
    "scripts/account-platform-dev-nats-runner.mjs",
    "account-platform/test/fixtures/development-nats.mjs",
    "account-platform/docs/implementation-status.md",
    "account-platform/docs/implementation-tasks.md",
    "account-platform/docs/requirements.md",
  ]) assert.ok(inputSelects(workflow, "account_platform", input), `${input} alone must select the task contracts`);
  const job = jobBlock("account-platform-test");
  const name = "Verify Account Platform task contracts";
  assert.doesNotMatch(namedStep(job, name), /^ {8}(?:if|continue-on-error|working-directory|env):/m);
  assert.equal(stepRun(job, name).trim(), "node --test scripts/check-account-platform-tasks.test.mjs");
});

test("NewAPI TLS deployment contracts are discovered by the Account-only CI dependency boundary", () => {
  const relative = "account-platform/test/contract/newapi-tls-deployment.test.mjs";
  assert.ok(inputSelects(workflow, "account_platform", relative));
  assert.ok(inputSelects(workflow, "account_platform", "account-platform/deploy/production/newapi.yaml.template"));
  const directory = new URL("../../account-platform/", import.meta.url);
  const contracts = JSON.parse(readFileSync(new URL("package.json", directory), "utf8"));
  const command = contracts.scripts["test:contracts"];
  assert.equal(command, "node --test test/contract/*.test.mjs");
  const glob = command.split(" ").at(-1);
  const discovered = readdirSync(new URL("test/contract/", directory))
    .map(name => `test/contract/${name}`).filter(path => matchesGlob(path, glob));
  assert.ok(discovered.includes(relative.slice("account-platform/".length)),
    "the NewAPI TLS contract must be an actual file matched by the package and CI glob");
  const job = jobBlock("account-platform-test");
  const install = namedStep(job, "Install Account Platform contract dependencies");
  const nodeStep = namedStep(job, "Account Platform Node contracts");
  assert.match(install, /^ {8}working-directory: account-platform$/m);
  assert.match(install, /^ {8}run: npm ci --ignore-scripts --prefer-offline --no-audit$/m);
  assert.doesNotMatch(install, /^ {8}(?:if|continue-on-error):/m);
  assert.ok(job.indexOf(install) < job.indexOf(nodeStep));
  assert.match(nodeStep, /^ {8}working-directory: account-platform$/m);
  assert.doesNotMatch(nodeStep, /^ {8}(?:if|continue-on-error):/m);
  assert.match(stepRun(job, "Account Platform Node contracts"),
    /^node --test --test-reporter=tap test\/contract\/\*\.test\.mjs \| tee /m);
  assert.ok(contracts.devDependencies.yaml, "YAML parser belongs to the installed Account package");
  const source = readFileSync(new URL(`../../${relative}`, import.meta.url), "utf8");
  assert.match(source, /createRequire\(new URL\('\.\.\/\.\.\/package\.json', import\.meta\.url\)\)/);
  assert.doesNotMatch(source, /pinvou3-app/);
});

test("credential origin browser matrix runs after browser preparation without accepting skips", () => {
  const job = jobBlock("frontend-test");
  assert.match(job, /defaults:\r?\n {6}run:\r?\n {8}working-directory: pinvou3-app/);
  const setup = namedStep(job, "准备浏览器");
  const name = "Credential origin real browser scope matrix";
  const gate = namedStep(job, name);
  assert.ok(job.indexOf(setup) < job.indexOf(gate), "browser matrix must follow CHROME preparation");
  const ready = "github.event_name == 'merge_group' || github.event.pull_request.draft == false";
  assert.equal(stepCondition(job, "准备浏览器"), ready);
  assert.equal(stepCondition(job, name), ready);
  assert.doesNotMatch(gate, /^ {8}(?:continue-on-error|working-directory|env):/m);
  assert.match(gate, /^ {8}shell: bash$/m);
  const run = stepRun(job, name);
  assert.match(run, /^set -euo pipefail$/m);
  assert.match(run, /^test -x "\$\{CHROME:\?browser preparation must provide CHROME\}"$/m);
  assert.match(run, /^python3 \.\.\/scripts\/ci-fixture-process\.py --receipt "\$PINVOU_CI_RESOURCE_LEDGER\/credential-origin\.json" -- node --test --test-reporter=tap tests\/credential_origin_scope_probe\.test\.mjs \| tee "\$report"$/m);
  assert.match(run, /^grep -Eq '\^# pass \[1-9\]\[0-9\]\*\$' "\$report"$/m);
  assert.match(run, /^grep -Fxq '# fail 0' "\$report"$/m);
  assert.match(run, /^grep -Fxq '# skipped 0' "\$report"$/m);
});


test("credential origin browser matrix shell rejects failed, empty or skipped results", () => {
  const run = stepRun(jobBlock("frontend-test"), "Credential origin real browser scope matrix");
  const directory = mkdtempSync(join(tmpdir(), "pinvou-credential-matrix-gate-"));
  try {
    // Controlled Node output exercises the exact workflow shell, not Chromium.
    for (const [passed, failed, skipped, exit, expected] of [
      [2, 0, 0, 0, 0], [0, 0, 0, 0, 1], [1, 0, 1, 0, 1],
      [2, 1, 0, 0, 1], [2, 0, 0, 7, 7], [2, 0, null, 0, 1],
    ]) {
      const summary = `# pass ${passed}\n# fail ${failed}\n${skipped === null ? "" : `# skipped ${skipped}\n`}`;
      const prefix = `CHROME="$(command -v bash)";
        python3() {
          [[ "$1" == ../scripts/ci-fixture-process.py && "$2" == --receipt && "$3" == "$PINVOU_CI_RESOURCE_LEDGER/credential-origin.json" && "$4" == -- ]] || return 99;
          shift 4; "$@";
        }
        node() { printf '%s' '${summary}'; return ${exit}; };\n`;
      const result = spawnSync(bash, ["-c", prefix + run], {
        cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true,
        env: { ...process.env, RUNNER_TEMP: directory.replaceAll("\\", "/"), PINVOU_CI_RESOURCE_LEDGER: directory.replaceAll("\\", "/") },
      });
      assert.equal(result.status, expected, `${passed}/${failed}/${skipped}/${exit}: ${result.stderr}`);
    }
    for (const prefix of ['unset CHROME;\n', 'CHROME="/missing-credential-browser";\n']) {
      const result = spawnSync(bash, ["-c", prefix + run], {
        cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true,
        env: { ...process.env, RUNNER_TEMP: directory.replaceAll("\\", "/") },
      });
      assert.notEqual(result.status, 0, "missing/non-executable CHROME must reject the gate");
    }
  } finally {
    assert.ok(directory.startsWith(join(tmpdir(), "pinvou-credential-matrix-gate-")));
    rmSync(directory, { recursive: true, force: true });
  }
});
