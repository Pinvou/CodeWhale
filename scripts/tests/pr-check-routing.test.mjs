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
import { runInNewContext } from "node:vm";

const workflow = await readFile(
  new URL("../../.github/workflows/pr-check.yml", import.meta.url),
  "utf8",
);
const titleWorkflow = await readFile(
  new URL("../../.github/workflows/pr-title-check.yml", import.meta.url),
  "utf8",
);

test("browser wrapper helper-only changes select both native runtime gates", () => {
  const helpers = [
    "pinvou3-app/tests/helpers/ndjson-child-driver.mjs",
    "pinvou3-app/tests/helpers/browser-wrapper-fixture-cleanup.mjs",
    "pinvou3-app/tests/helpers/browser-wrapper-held-result.mjs",
    "pinvou3-app/tests/helpers/browser-wrapper-host-fixture.mjs",
  ];
  for (const gate of ["windows_codex", "acp_runtime"]) {
    // Comments inside a filter must not hide later patterns from these
    // positive/negative assertions.
    const paths = filterPaths(workflow.replace(/^ {14}#.*\r?\n/gm, ""), gate);
    const selected = file => paths.some(pattern => !pattern.startsWith("!") && matchesGlob(file, pattern))
      && !paths.some(pattern => pattern.startsWith("!") && matchesGlob(file, pattern.slice(1)));
    for (const helper of helpers) assert.equal(selected(helper), true, `${gate}: ${helper}`);
    for (const unrelated of ["docs/unrelated.md", "pinvou3-app/tests/helpers/unrelated-fixture.mjs"]) {
      assert.equal(selected(unrelated), false, `${gate}: ${unrelated}`);
    }
    for (const helper of helpers) {
      const narrowed = paths.filter(pattern => pattern !== helper);
      assert.equal(narrowed.some(pattern => !pattern.startsWith("!") && matchesGlob(helper, pattern)), false,
        `${gate} negative must detect removal of ${helper}`);
    }
  }
});

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
const goGatePhase = "PINVOU_GO_GATE_PHASE";

// Only fixed fixture markers and process metadata enter this diagnostic. Do
// not dump the inherited environment, arbitrary commands or native error text.
function goGateDiagnostic(result, elapsedMs) {
  const phases = (result.stderr || "").split(/\r?\n/)
    .filter(line => /^PINVOU_GO_GATE_PHASE\|(bash-start|fixture-ready|gate-complete|shell-exit:[0-9]+|go:(test|vet):(begin|complete))$/.test(line));
  const token = value => typeof value === "string" && /^[A-Z0-9_]+$/.test(value) ? value : null;
  return JSON.stringify({ status: result.status ?? null, signal: token(result.signal),
    errorCode: token(result.error?.code), elapsedMs: Math.round(elapsedMs), phases });
}

function runGoGateFixture(prefix, run, directory) {
  const script = `printf '${goGatePhase}|bash-start\\n' >&2\n`
    + `trap 'fixture_status=$?; printf "${goGatePhase}|shell-exit:%s\\n" "$fixture_status" >&2' EXIT\n`
    + prefix + `printf '${goGatePhase}|fixture-ready\\n' >&2\n`
    + run + `\nprintf '${goGatePhase}|gate-complete\\n' >&2\n`;
  const started = performance.now();
  const result = spawnSync(bash, ["-c", script], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true });
  return { ...result, diagnostic: goGateDiagnostic(result, performance.now() - started) };
}

const auditPGStepName = "Audit export finite PostgreSQL rollback and persisted boundaries";
const existingBrokerStepName = "Existing broker compatibility on owned PostgreSQL and TLS NATS";
const tokenOutboxStepName = "Token outbox publication on owned PostgreSQL and TLS NATS";
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
  const diagnostic = namedStep(text, "Financial group lifecycle receipts");
  const withoutComments = diagnostic.replace(/^ *#.*\n/gm, "").trimEnd();
  assert.equal(withoutComments, [
    "      - name: Financial group lifecycle receipts",
    "        id: financial_receipts_upload",
    "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
    "        timeout-minutes: 3",
    "        continue-on-error: true",
    "        uses: actions/upload-artifact@v7",
    "        with:",
    "          name: account-financial-${{ matrix.group }}-resource-lifecycle-${{ github.run_attempt }}",
    "          path: ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
    "          if-no-files-found: error",
    "          retention-days: 7",
  ].join("\n"));
  assert.doesNotMatch(text, /secrets\./);
  assert.doesNotMatch(text.replace(diagnostic, ""), /continue-on-error:/,
    "only the exact diagnostic upload step may be advisory");
}

test("financial diagnostic exception rejects business and job exemptions", () => {
  const job = jobBlock("account-financial-test");
  assertFinancialDiagnosticExemption(job);
  const diagnostic = namedStep(job, "Financial group lifecycle receipts");
  const withoutExemption = job.replace(diagnostic,
    diagnostic.replace("        continue-on-error: true\n", ""));
  const mutants = [
    job.replace("  account-financial-test:\n", "  account-financial-test:\n    continue-on-error: true\n"),
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
  const financialJob = jobBlock("account-financial-test", text);
  assertFinancialDiagnosticExemption(financialJob);
  const financial = namedStep(financialJob, "Financial group lifecycle receipts");
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
    const receipt = namedStep(jobBlock("required-gate", text), "PR test success proof transport");
    assert.equal(receipt.trimEnd(), [
      "      - name: PR test success proof transport",
      "        if: ${{ success() && steps.success_proof.outputs.directory != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: ci-success-proof-${{ github.run_attempt }}",
      "          path: ${{ steps.success_proof.outputs.directory }}/proof.json",
      "          if-no-files-found: error",
      "          retention-days: 2",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  {
    const receipt = namedStep(jobBlock("windows-relay-tools-test", text), "Preserve Windows relay tools process receipt");
    assert.equal(receipt.trimEnd(), [
      "      - name: Preserve Windows relay tools process receipt",
      "        if: always()",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: windows-relay-tools-exit-${{ github.run_id }}-${{ github.run_attempt }}",
      "          path: ${{ runner.temp }}/windows-relay-tools-process.json",
      "          if-no-files-found: warn",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  {
    const receipt = namedStep(jobBlock("fast-gate", text), "Gate test resource lifecycle receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Gate test resource lifecycle receipts",
      "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: gate-resource-lifecycle-${{ github.run_attempt }}",
      "          path: ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  for (const [job, name, id, artifact] of [
    ["frontend-test", "Test resource lifecycle receipts", "frontend_receipts_upload", "frontend-resource-lifecycle-${{ github.run_attempt }}"],
    ["frontend-browser-test", "Browser shard resource lifecycle receipts", "browser_receipts_upload", "frontend-browser-${{ matrix.shard }}-resource-lifecycle-${{ github.run_attempt }}"],
  ]) {
    const receipt = namedStep(jobBlock(job, text), name);
    assert.equal(receipt.trimEnd(), [
      "      - name: " + name,
      "        id: " + id,
      "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: " + artifact,
      "          path: ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  for (const [stepName, artifact, prefix] of [
    ["Preserve browser fixture exit receipts", "windows-browser-exit", "windows-browser-*"],
    ["Preserve ACP fixture exit receipt", "windows-acp-exit", "windows-acp"],
  ]) {
    const receipt = namedStep(jobBlock("windows-codex-runtime-test", text), stepName);
    assert.equal(receipt.trimEnd(), [
      "      - name: " + stepName,
      "        if: always()",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: " + artifact + "-${{ github.run_id }}-${{ github.run_attempt }}",
      "          retention-days: 7",
      "          if-no-files-found: warn",
      "          path: |",
      "            ${{ runner.temp }}/" + prefix + "-${{ github.run_id }}-${{ github.run_attempt }}.json",
      "            ${{ runner.temp }}/" + prefix + "-${{ github.run_id }}-${{ github.run_attempt }}-resources/*.json",
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
  {
    const receipt = namedStep(jobBlock("account-closure-test", text), "Account closure resource lifecycle receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Account closure resource lifecycle receipts",
      "        id: account_receipts_upload",
      "        if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: account-closure-resource-lifecycle-${{ github.run_attempt }}",
      "          path: |",
      "            ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  // The primary transport failure is provisional only while the exact
  // mandatory recovery reducer still protects this artifact and every build.
  assertFrontendUploadRecovery(text);
  remainder = remainder.replace(namedStep(jobBlock("frontend-setup", text), frontendUploadStep), "");
  {
    const receipt = namedStep(jobBlock("windows-rust-test", text), "Windows atomic lifecycle receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Windows atomic lifecycle receipts",
      "        id: windows_atomic_receipts_upload",
      "        if: ${{ always() && matrix.phase == 'regression' && env.PINVOU_CI_RESOURCE_LEDGER != '' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: windows-atomic-resources-${{ matrix.phase }}-${{ github.run_attempt }}",
      "          path: |",
      "            ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/**/*.json",
      "            ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/atomic-expected.txt",
      "            ${{ env.PINVOU_CI_RESOURCE_LEDGER }}/ledger.txt",
      "          if-no-files-found: error",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  {
    const receipt = namedStep(jobBlock("release-contract-test", text), "Preserve Windows release contract process receipts");
    assert.equal(receipt.trimEnd(), [
      "      - name: Preserve Windows release contract process receipts",
      "        id: windows_release_contract_receipts",
      "        if: ${{ always() && runner.os == 'Windows' }}",
      "        timeout-minutes: 3",
      "        continue-on-error: true",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: windows-release-contract-resources-${{ github.run_id }}-${{ github.run_attempt }}",
      "          path: |",
      "            ${{ runner.temp }}/remote-native-exit-${{ github.run_id }}-${{ github.run_attempt }}.json",
      "            ${{ runner.temp }}/remote-native-exit-${{ github.run_id }}-${{ github.run_attempt }}-resources/**/*.json",
      "            ${{ runner.temp }}/remote-evidence-contract-${{ github.run_id }}-${{ github.run_attempt }}.json",
      "            ${{ runner.temp }}/remote-evidence-contract-${{ github.run_id }}-${{ github.run_attempt }}-resources/**/*.json",
      "            ${{ runner.temp }}/remote-fixture-process-${{ github.run_id }}-${{ github.run_attempt }}.json",
      "            ${{ runner.temp }}/remote-fixture-process-${{ github.run_id }}-${{ github.run_attempt }}-resources/**/*.json",
      "          if-no-files-found: warn",
      "          retention-days: 7",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  {
    const receipt = namedStep(jobBlock("relay-test", text), "保留 Relay 进程作用域原始回执");
    assert.equal(receipt.trimEnd(), [
      "      - name: 保留 Relay 进程作用域原始回执",
      "        if: always()",
      "        continue-on-error: true",
      "        timeout-minutes: 3",
      "        uses: actions/upload-artifact@v7",
      "        with:",
      "          name: relay-process-${{ github.run_id }}-${{ github.run_attempt }}",
      "          path: |",
      "            ${{ runner.temp }}/relay-process-${{ github.run_id }}-${{ github.run_attempt }}.json",
      "            ${{ runner.temp }}/relay-process-${{ github.run_id }}-${{ github.run_attempt }}-resources/",
      "          retention-days: 7",
      "          if-no-files-found: warn",
    ].join("\n"));
    remainder = remainder.replace(receipt, "");
  }
  // Explicit YAML keys may put comments/newlines before their value indicator.
  const keySeparator = String.raw`(?:\s|#[^\n]*(?:\n|$))*:`;
  assert.doesNotMatch(remainder, new RegExp(String.raw`\bcontinue-on-error\s*['"]?` + keySeparator),
    "only the exact reviewed diagnostic steps may be advisory in any job");
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
  const relayReceipt = namedStep(jobBlock("windows-relay-tools-test", original), "Preserve Windows relay tools process receipt");
  const withoutWindowsExemption = original.replace(windows,
    windows.replace("        continue-on-error: true\n", ""));
  const mutants = [
    original.replace(relayReceipt, relayReceipt.replace("windows-relay-tools-process.json", "other-process.json")),
    original.replace("      - name: Windows PowerShell 5.1 与实际工具回归（禁止跳过）\n",
      "      - name: Windows PowerShell 5.1 与实际工具回归（禁止跳过）\n        continue-on-error: true\n"),
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
    ["fast-gate", "Gate test resource lifecycle receipts"],
    ["frontend-test", "Test resource lifecycle receipts"],
    ["frontend-browser-test", "Browser shard resource lifecycle receipts"],
    ["account-platform-test", "Account test resource lifecycle receipts"],
    ["account-closure-test", "Account closure resource lifecycle receipts"],
    ["windows-codex-runtime-test", "Preserve browser fixture exit receipts"],
    ["windows-codex-runtime-test", "Preserve ACP fixture exit receipt"],
    ["windows-rust-test", "Windows atomic lifecycle receipts"],
    ["windows-relay-tools-test", "Preserve Windows relay tools process receipt"],
    ["release-contract-test", "Preserve Windows release contract process receipts"],
    ["relay-test", "保留 Relay 进程作用域原始回执"],
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
  const gateReceipt = namedStep(jobBlock("fast-gate", original), "Gate test resource lifecycle receipts");
  for (const [old, replacement] of [
    ["if: ${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}", "if: always()"],
    ["${{ env.PINVOU_CI_RESOURCE_LEDGER }}/", "${{ github.workspace }}/**/*"],
    ["retention-days: 7", "retention-days: 30"],
    ["        uses: actions/upload-artifact@v7", "        env:\n          UNREVIEWED: yes\n        uses: actions/upload-artifact@v7"],
  ]) mutants.push(original.replace(gateReceipt, gateReceipt.replace(old, replacement)));
  mutants.push(original.replace(gateReceipt, gateReceipt + gateReceipt));
  const frontendForGate = namedStep(jobBlock("frontend-test", original), "Test resource lifecycle receipts");
  mutants.push(original.replace(gateReceipt, "").replace(frontendForGate, frontendForGate + gateReceipt));
  const withoutGateExemption = original.replace(gateReceipt, gateReceipt.replace("        continue-on-error: true\n", ""));
  for (const name of ["Gate policy test suite (scripts/tests)", "Confirm gate test resource retirement"]) {
    const mandatory = namedStep(jobBlock("fast-gate", original), name);
    for (const variant of [original, withoutGateExemption]) {
      mutants.push(variant.replace(mandatory, mandatory.replace("      - name: " + name + "\n",
        "      - name: " + name + "\n        continue-on-error: true\n")));
    }
  }
  const releaseReceipt = namedStep(jobBlock("release-contract-test", original), "Preserve Windows release contract process receipts");
  for (const [old, replacement] of [
    ["if: ${{ always() && runner.os == 'Windows' }}", "if: always()"],
    ["${{ runner.temp }}/remote-native-exit-${{ github.run_id }}-${{ github.run_attempt }}.json", "${{ runner.temp }}/**/*"],
    ["retention-days: 7", "retention-days: 30"],
  ]) mutants.push(original.replace(releaseReceipt, releaseReceipt.replace(old, replacement)));
  mutants.push(original.replace(releaseReceipt, releaseReceipt + releaseReceipt));
  const withoutReleaseExemption = original.replace(releaseReceipt, releaseReceipt.replace("        continue-on-error: true\n", ""));
  for (const name of ["Windows 原生命令退出码回归", "Windows 远控发布与安全契约", "Windows 共享进程监督器兼容回归"]) {
    const mandatory = namedStep(jobBlock("release-contract-test", original), name);
    for (const variant of [original, withoutReleaseExemption]) {
      mutants.push(variant.replace(mandatory, mandatory.replace("      - name: " + name + "\n", "      - name: " + name + "\n        continue-on-error: true\n")));
    }
  }
  const atomicReceipt = namedStep(jobBlock("windows-rust-test", original), "Windows atomic lifecycle receipts");
  for (const [old, replacement] of [
    ["id: windows_atomic_receipts_upload", "id: unreviewed_upload"],
    ["if: ${{ always() && matrix.phase == 'regression' && env.PINVOU_CI_RESOURCE_LEDGER != '' }}", "if: always()"],
    ["${{ env.PINVOU_CI_RESOURCE_LEDGER }}/**/*.json", "${{ github.workspace }}/**/*"],
    ["retention-days: 7", "retention-days: 30"],
    ["name: Windows atomic lifecycle receipts", "name: Unreviewed transport"],
    ["        uses: actions/upload-artifact@v7", "        env:\n          UNREVIEWED: yes\n        uses: actions/upload-artifact@v7"],
  ]) {
    mutants.push(original.replace(atomicReceipt, atomicReceipt.replace(old, replacement)));
  }
  mutants.push(original.replace(atomicReceipt, atomicReceipt + atomicReceipt));
  const frontendReceipt = namedStep(jobBlock("frontend-test", original), "Test resource lifecycle receipts");
  mutants.push(original.replace(atomicReceipt, "").replace(frontendReceipt, frontendReceipt + atomicReceipt));
  const withoutAtomicExemption = original.replace(atomicReceipt,
    atomicReceipt.replace("        continue-on-error: true\n", ""));
  for (const name of ["Windows 原子替换状态机回归", "Confirm Windows atomic resource retirement"]) {
    const mandatory = namedStep(jobBlock("windows-rust-test", original), name);
    mutants.push(original.replace(mandatory,
      mandatory.replace("      - name: " + name + "\n", "      - name: " + name + "\n        continue-on-error: true\n")));
    mutants.push(withoutAtomicExemption.replace(mandatory,
      mandatory.replace("      - name: " + name + "\n", "      - name: " + name + "\n        continue-on-error: true\n")));
  }
  for (const mutant of mutants) {
    assert.notEqual(mutant, original, "the counterexample must change the actual workflow");
    assert.throws(() => assertWorkflowFailurePolicy(mutant));
  }
  assertWorkflowFailurePolicy(original);
});

function assertFiniteAuditPGSetup(job) {
  const text = job.replaceAll("\r\n", "\n");
  // These are exactly two approved ephemeral services, not permission for arbitrary
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
    "          python3 scripts/account-ci-postgres.py --profile core",
  ].join("\n"));
  for (const name of [existingBrokerStepName, tokenOutboxStepName, auditPGStepName]) {
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
    "          python3 scripts/account-ci-postgres.py --profile core --stop-owned-services",
  ].join("\n"));
  for (const name of [existingBrokerStepName, tokenOutboxStepName, auditPGStepName]) {
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
  const tokenOutbox = namedStep(text, tokenOutboxStepName);
  assert.equal(tokenOutbox.trimEnd(), [
    `      - name: ${tokenOutboxStepName}`,
    "        timeout-minutes: 4", "        shell: bash", "        env:",
    "          PINVOU_TEST_SCHEMA_DATABASE_URL: ${{ steps.account-pg.outputs.broker_url }}",
    ...pgIdentityEnvironment("broker").map(value => `          ${value}`),
    "          PINVOU_TEST_NATS_SERVER_BINARY: ${{ runner.temp }}/pinvou-test-nats-server",
    "        run: |", "          set -euo pipefail",
    "          # Reuse only the preceding step's pinned executable; the test starts",
    "          # its own TLS broker and applies public Token migrations. The shared",
    "          # supervisor owns the complete Python/Go/NATS process tree, including",
    "          # panic, timeout and cancellation; always-run ledger recovery verifies it.",
    "          python3 -m unittest discover -s scripts/tests -p test_token_outbox_acceptance.py",
    '          python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/token-outbox-process.json" -- npm --prefix account-platform run test:token-outbox',
  ].join("\n"));
  assert.ok(text.indexOf(existingBroker) < text.indexOf(tokenOutbox),
    "Token outbox must follow the pinned NATS executable verification");
  outsidePGSteps = outsidePGSteps.replace(tokenOutbox, "");

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

function assertFiniteFinancialPGSetup(job) {
  const text = job.replaceAll("\r\n", "\n");
  assertFinancialDiagnosticExemption(job);
  assert.match(text, /^    needs: changes$/m);
  assert.match(text, /^    timeout-minutes: 180$/m);
  assert.match(text, /^      GOTOOLCHAIN: local$/m);
  assert.match(text, /^      CGO_ENABLED: "1"$/m);
  assert.match(text, /^      fail-fast: false$/m);
  assert.match(text, /^      max-parallel: 6$/m);
  assert.match(text, /^        group: \[owners, token, billing-0, billing-1, billing-2, billing-3\]$/m);
  const services = text.match(/^    services:\n[\s\S]*?(?=^    \S|$(?![\s\S]))/m);
  assert.ok(services, "isolated financial PostgreSQL service not found");
  assert.equal(services[0].trimEnd(), [
    "    services:", "      financial-postgres:",
    "        # Same immutable PostgreSQL image already used by team-relay-hub-test.",
    "        # Disposable runner only; trust is never a deployment configuration.",
    "        image: mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea",
    "        env:", "          POSTGRES_HOST_AUTH_METHOD: trust", "          POSTGRES_DB: postgres",
    "        ports:", '          - "127.0.0.1::5432"', "        options: >-",
    "          --rm", "          --cpus 2", "          --memory 1g", "          --memory-swap 1g",
    "          --health-cmd \"pg_isready -U postgres -d postgres\"",
    "          --health-interval 5s", "          --health-timeout 5s", "          --health-retries 12",
    ...["account", "wallet"].flatMap(lane => [
      `      financial-owner-${lane}-postgres:`,
      "        # Empty image disables these extra services for the other five Jobs.",
      "        image: ${{ matrix.group == 'owners' && 'mirror.gcr.io/library/postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea' || '' }}",
      "        env:", "          POSTGRES_HOST_AUTH_METHOD: trust", "          POSTGRES_DB: postgres",
      "        ports:", '          - "127.0.0.1::5432"', "        options: >-",
      "          --rm", "          --cpus 1", "          --memory 1g", "          --memory-swap 1g",
      '          --health-cmd "pg_isready -U postgres -d postgres"',
      "          --health-interval 5s", "          --health-timeout 5s", "          --health-retries 12",
    ]),
  ].join("\n"));
  assert.match(text, /^      PINVOU_ACCOUNT_PG_PROFILE: \$\{\{ matrix.group == 'owners' && 'owners' \|\| 'financial' \}\}$/m);
  const guard = namedStep(text, "Verify complete financial CI group selection");
  assert.deepEqual(stepRun(text, "Verify complete financial CI group selection").trim().split("\n"),
    ["set -euo pipefail", "python3 scripts/check-account-financial-ci.py"]);
  assert.doesNotMatch(guard, /^        (?:if|continue-on-error):/m);
  const preflight = namedStep(text, pgPreflightStepName);
  assert.equal(preflight.trimEnd(), [
    `      - name: ${pgPreflightStepName}`, "        id: account-pg", "        env:",
    "          ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}", "        run: |",
    "          set -euo pipefail", '          python3 scripts/account-ci-postgres.py --profile "$PINVOU_ACCOUNT_PG_PROFILE"' ,
  ].join("\n"));
  const execute = namedStep(text, "Run isolated financial group");
  assert.equal(execute.trimEnd(), [
    "      - name: Run isolated financial group", "        shell: bash", "        env:",
    "          ACCOUNT_FINANCIAL_GROUP: ${{ matrix.group }}",
    "          PINVOU_CI_OWNER_PARALLEL: ${{ matrix.group == 'owners' && '1' || '0' }}",
    "          ACCOUNT_FINANCIAL_OWNER_SERVICES: ${{ steps.account-pg.outputs.owners_services }}",
    '          PINVOU_CI_FIXTURE_TEMPLATES: "1"',
    '          PINVOU_CI_FIXTURE_TIMING: "1"',
    "          PINVOU_ACCOUNT_FINANCIAL_TEST_DATABASE_URL: ${{ steps.account-pg.outputs.financial_url }}",
    ...pgIdentityEnvironment("financial").map(value => `          ${value}`),
    "        run: |", "          set -euo pipefail",
    '          python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/financial-process.json" -- python3 scripts/test-token-financial-postgres.py --group "$ACCOUNT_FINANCIAL_GROUP" --verify-fixture-cache',
  ].join("\n"));
  const stop = namedStep(text, pgStopStepName);
  assert.equal(stop.trimEnd(), [
    `      - name: ${pgStopStepName}`, "        if: ${{ always() }}", "        env:",
    "          ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}", "        run: |",
    '          python3 scripts/account-ci-postgres.py --profile "$PINVOU_ACCOUNT_PG_PROFILE" --stop-owned-services' ,
  ].join("\n"));
  const retirement = namedStep(text, "Confirm Account test resource retirement");
  assert.deepEqual([...retirement.matchAll(/^        if: (.+)$/gm)].map(match => match[1]),
    ["${{ always() && env.PINVOU_CI_RESOURCE_LEDGER != '' }}"]);
  assert.deepEqual(stepRun(text, "Confirm Account test resource retirement").trim().split("\n"),
    ["set -euo pipefail", 'python3 scripts/ci-resource-ledger.py --recover --directory "$PINVOU_CI_RESOURCE_LEDGER"']);
  assert.doesNotMatch(retirement, /continue-on-error/);
  const ordered = [guard, preflight, execute, stop, retirement];
  for (let index = 1; index < ordered.length; index += 1) {
    assert.ok(text.indexOf(ordered[index - 1]) < text.indexOf(ordered[index]), "financial guard, execution and retirement order must be retained");
  }
  const outside = ordered.reduce((remaining, step) => remaining.replace(step, ""), text);
  assert.doesNotMatch(outside, /DATABASE_URL|PINVOU_ACCOUNT_CI_PG_|ACCOUNT_CI_SERVICES|ACCOUNT_FINANCIAL_SERVICES/);
}


const bash = process.platform === "win32" && existsSync("C:/Program Files/Git/bin/bash.exe")
  ? "C:/Program Files/Git/bin/bash.exe" : "bash";

test("independent financial services reject identity, profile and retirement mutations", () => {
  const job = jobBlock("account-financial-test");
  assertFiniteFinancialPGSetup(job);
  for (const [before, after] of [
    ['"127.0.0.1::5432"', '"5432:5432"'],
    ["          --rm\n", ""], ["          --cpus 2", "          --cpus 4"],
    ['--profile "$PINVOU_ACCOUNT_PG_PROFILE"', '--profile core'],
    ["matrix.group == 'owners' && 'owners' || 'financial'", "'financial'"],
    ["          --cpus 1", "          --cpus 2"],
    ["matrix.group == 'owners' && 'mirror.gcr.io/library/postgres", "matrix.group != 'owners' && 'mirror.gcr.io/library/postgres"],
    ["outputs.owners_services", "outputs.financial_services"],
    ['ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}', 'ACCOUNT_CI_SERVICES: ${{ toJSON(needs.other.outputs.services) }}'],
    ['ACCOUNT_FINANCIAL_GROUP: ${{ matrix.group }}', 'ACCOUNT_FINANCIAL_GROUP: all'],
    ['outputs.financial_url', 'outputs.unrelated_url'],
    ['outputs.financial_port', 'outputs.unrelated_port'],
    ['outputs.financial_system', 'outputs.unrelated_system'],
    ['outputs.financial_directory', 'outputs.unrelated_directory'],
    ['-- python3 scripts/test-token-financial-postgres.py', '-- python3 scripts/test-token-financial-postgres.py || true'],
    ['python3 scripts/account-ci-postgres.py --profile "$PINVOU_ACCOUNT_PG_PROFILE" --stop-owned-services', 'true'],
    ['python3 scripts/ci-resource-ledger.py --recover', 'true'],
    ["      CGO_ENABLED: \"1\"", "      CGO_ENABLED: \"0\""],
  ]) {
    assert.ok(job.includes(before), `financial mutation must hit ${before}`);
    const mutant = job.replace(before, () => after);
    assert.notEqual(mutant, job);
    assert.throws(() => assertFiniteFinancialPGSetup(mutant));
  }
  for (const name of ["Verify complete financial CI group selection", pgPreflightStepName,
    "Run isolated financial group", pgStopStepName, "Confirm Account test resource retirement"]) {
    const step = namedStep(job, name);
    assert.throws(() => assertFiniteFinancialPGSetup(job.replace(step, "")));
    assert.throws(() => assertFiniteFinancialPGSetup(job.replace(step,
      () => step.replace("        run:", "        if: ${{ false }}\n        run:"))));
  }
  for (const exit of [0, 7]) {
    const result = spawnSync(bash, ["-e", "-c", `python3() { return ${exit}; };\n` + stepRun(job, pgStopStepName)],
      { encoding: "utf8", timeout: 30_000, windowsHide: true });
    assert.equal(result.status, exit);
  }
});

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
    const traceStart = `printf '${goGatePhase}|go:%s:begin\\n' "$1" >&2; `;
    const traceEnd = `printf '${goGatePhase}|go:%s:complete\\n' "$1" >&2; `;
    const prefix = `go() { ${traceStart}printf '%s|%s\\n' "$PWD" "$*"; ${traceEnd}}; export -f go;\n`;
    const result = runGoGateFixture(prefix, run, directory);
    assert.equal(result.status, 0, result.diagnostic);
    const successPhases = JSON.parse(result.diagnostic).phases;
    assert.equal(successPhases.filter(line => line.endsWith(":begin")).length, 6);
    assert.equal(successPhases.filter(line => line.endsWith(":complete")).length, 6);
    assert.ok(successPhases.includes(`${goGatePhase}|gate-complete`));
    assert.equal(successPhases.at(-1), `${goGatePhase}|shell-exit:0`);
    const calls = result.stdout.split("\n").filter((line) => line.includes("|"));
    assert.equal(calls.length, 6, result.stdout);
    for (const module of ["account-platform", "account-platform/tools/kms-recovery", "account-platform/tools/future tool"]) {
      const moduleCalls = calls.filter((line) => line.includes(`/${module}|`));
      assert.equal(moduleCalls.length, 2, result.stdout);
      assert.ok(moduleCalls.some((line) => line.endsWith("|test -race -tags=nomsgpack -mod=readonly -count=1 ./...")));
      assert.ok(moduleCalls.some((line) => line.endsWith("|vet -tags=nomsgpack -mod=readonly ./...")));
    }
    for (const command of ["test", "vet"]) {
      const failed = runGoGateFixture(`go() { ${traceStart}if [[ "$1" == "${command}" ]]; then return 7; fi; ${traceEnd}}; export -f go;\n`, run, directory);
      assert.equal(failed.status, 7, `a failing go ${command} must fail the gate: ${failed.diagnostic}`);
      assert.equal(JSON.parse(failed.diagnostic).phases.at(-1), `${goGatePhase}|shell-exit:7`);
      const nestedFailed = runGoGateFixture(`go() { ${traceStart}if [[ "$PWD" == */"tools/future tool" && "$1" == "${command}" ]]; then return 7; fi; ${traceEnd}}; export -f go;\n`, run, directory);
      assert.equal(nestedFailed.status, 7, `a failing nested-module go ${command} must fail the gate: ${nestedFailed.diagnostic}`);
      assert.equal(JSON.parse(nestedFailed.diagnostic).phases.at(-1), `${goGatePhase}|shell-exit:7`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});

test("Go gate diagnostics retain null exits, error codes and only fixed phase markers", () => {
  const diagnostic = JSON.parse(goGateDiagnostic({ status: null, signal: "SIGTERM", error: { code: "ETIMEDOUT", message: "private native detail" },
    stderr: `${goGatePhase}|bash-start\n${goGatePhase}|fixture-ready\nprivate environment or command\n${goGatePhase}|untrusted detail\n` }, 30_025.3));
  assert.deepEqual(diagnostic, { status: null, signal: "SIGTERM", errorCode: "ETIMEDOUT", elapsedMs: 30_025,
    phases: [`${goGatePhase}|bash-start`, `${goGatePhase}|fixture-ready`] });
  assert.throws(() => assert.equal(diagnostic.status, 0), /null/);
  assert.equal(JSON.parse(goGateDiagnostic({ status: null, error: { code: "ENOENT" } }, 2)).errorCode, "ENOENT");
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
    ["ACCOUNT_CI_SERVICES: ${{ toJSON(job.services) }}", "ACCOUNT_CI_SERVICES: ${{ toJSON(needs.other.outputs.services) }}"],
    ["        id: account-pg", "        id: unrelated-pg"],
    ["python3 scripts/account-ci-postgres.py", "python3 scripts/account-ci-postgres.py || true"],
    ["python3 -m unittest discover -s scripts/tests -p test_account_ci_postgres.py", "true"],
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
    for (const required of [pgPreflightStepName, auditPGStepName, existingBrokerStepName, tokenOutboxStepName]) {
      namedStep(mutated, required); // Missing/duplicate steps cannot satisfy a negative control.
    }
    assert.throws(() => assertFiniteAuditPGSetup(mutated), expected);
  };
  for (const [name, service] of [[auditPGStepName, "audit"], [existingBrokerStepName, "broker"], [tokenOutboxStepName, "broker"]]) {
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
  const tokenOutbox = namedStep(job, tokenOutboxStepName);
  for (const [before, after] of [
    ["timeout-minutes: 4", "timeout-minutes: 40"],
    ["${{ runner.temp }}/pinvou-test-nats-server", "/unverified/nats-server"],
    ["python3 -m unittest discover -s scripts/tests -p test_token_outbox_acceptance.py", "true"],
    ["npm --prefix account-platform run test:token-outbox", "npm --prefix account-platform run test:token-outbox || true"],
    ['python3 scripts/ci-fixture-process.py --receipt "$PINVOU_CI_RESOURCE_LEDGER/token-outbox-process.json" -- ', ""],
  ]) {
    rejectStepMutation(tokenOutboxStepName, tokenOutbox, tokenOutbox.replace(before, () => after));
  }
  assert.throws(() => assertFiniteAuditPGSetup(job.replace(tokenOutbox, "")));
  const existingBroker = namedStep(job, existingBrokerStepName);
  const earlyTokenOutbox = job.replace(tokenOutbox, "").replace(existingBroker, () => tokenOutbox + existingBroker);
  assert.throws(() => assertFiniteAuditPGSetup(earlyTokenOutbox), /Token outbox must follow/);
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
  for (const path of ["scripts/test-token-financial-postgres.py", "scripts/test-financial-parallel.py",
    "scripts/test-financial-owners.py", "scripts/tests/test_financial_owners.py",
    "scripts/tests/test_financial_parallel.py", "scripts/tests/test_token_financial_postgres.py",
    "scripts/check-account-financial-ci.py", "scripts/tests/test_account_financial_ci.py"]) {
    assert.ok(paths.includes(path));
  }
  const job = jobBlock("account-financial-test");
  assertFiniteFinancialPGSetup(job);
  const run = ["Verify complete financial CI group selection", pgPreflightStepName, "Run isolated financial group"]
    .map(name => stepRun(job, name)).join("\n");
  const directory = mkdtempSync(join(tmpdir(), "pinvou-financial-gate-"));
  try {
    for (const failedPhase of ["none", "guard", "postgres", "group"]) {
      const mock = `python3() { case "$*" in *check-account-financial-ci.py*) ${failedPhase === "guard" ? "return 7" : "return 0"};; *account-ci-postgres.py*) ${failedPhase === "postgres" ? "return 7" : "return 0"};; *ci-fixture-process.py*) ${failedPhase === "group" ? "return 7" : "return 0"};; *) return 99;; esac; }; export -f python3;\n`;
      const result = spawnSync(bash, ["-e", "-c", mock + run], {
        encoding: "utf8", timeout: 30_000, windowsHide: true,
        env: { ...process.env, PINVOU_CI_RESOURCE_LEDGER: directory, ACCOUNT_FINANCIAL_GROUP: "token", PINVOU_ACCOUNT_PG_PROFILE: "financial" },
      });
      assert.equal(result.status, failedPhase === "none" ? 0 : 7,
        `${failedPhase} must preserve the selected gate result: ${result.stderr}`);
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
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
  const run = stepRun(gate, "汇总所有适用检查");
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

test('frontend shards are independently scheduled only on applicable Ready and queue trees', () => {
  const browser = jobBlock('frontend-browser-test');
  // Setup 工件化:分片经 needs 消费 frontend-setup 的单次构建,仍必须挂在
  // changes 路由上(否则无前端改动的 PR 会拉起浏览器腿)。
  assert.match(browser, /^    needs: \[changes, frontend-setup\]$/m);
  assert.match(jobBlock('frontend-setup'), /^    needs: changes$/m);
  assert.match(browser, /^      fail-fast: false$/m);
  assert.match(browser, /^      max-parallel: 4$/m);
  assert.match(browser, /^        shard: \[1, 2, 3, 4\]$/m);
  assert.doesNotMatch(browser, /--receipt.*frontend-logic|-- npm test/);
  assert.doesNotMatch(jobBlock('frontend-test'), /run-frontend-smokes\.mjs/);
  // 构建/安装步只允许出现在 frontend-setup(setup ×5 → ×1),分片内不得回流。
  for (const name of ['Vite build', 'Vite web build (remote relay)', 'Verify embedded Account Portal assets are current']) {
    namedStep(jobBlock('frontend-setup'), name);
    assert.throws(() => namedStep(browser, name));
  }
  for (const name of ['Initialize owned test resource ledger', 'Confirm test resource retirement']) namedStep(browser, name);
  for (const name of ['Ready PR 定向浏览器 smoke', 'Merge Queue diff-selected browser smoke']) {
    const run = stepRun(browser, name);
    assert.match(run, /^set -euo pipefail$/m);
    assert.match(run, /run-frontend-smokes\.mjs --selected --shard "\$\{\{ matrix.shard \}\}\/4"/);
    assert.match(run, /run-frontend-smokes\.mjs[^\n]+--isolate-network/);
  }
  const expression = browser.match(/^    if: \$\{\{ (.*) \}\}$/m)[1];
  for (const event of ['pull_request', 'merge_group', 'push']) {
    for (const draft of [true, false]) for (const frontend of ['true', 'false']) {
      for (const release of ['true', 'false']) for (const reused of [true, false]) {
        const actual = runInNewContext(expression, {
          github: { event_name: event, event: { action: 'synchronize', pull_request: { draft } } },
          needs: { 'frontend-setup': { result: 'success' }, changes: { outputs: { frontend, pet: 'false', pure_release_backflow: release,
            reuse_verified: reused ? 'true' : 'false', reused_jobs: '["frontend-browser-test"]' } } },
          cancelled: () => false, fromJSON: JSON.parse, contains: (items, value) => items.includes(value),
        });
        const expected = frontend === 'true' && release !== 'true'
          && (event === 'merge_group' ? !reused : event === 'pull_request' && !draft);
        assert.equal(actual, expected, `${event}/${draft}/${frontend}/${release}/${reused}`);
      }
    }
  }
});

test('required-gate rejects missing, skipped, failed and cancelled applicable frontend groups', () => {
  const gate = jobBlock('required-gate');
  assert.match(gate, /^      - frontend-browser-test$/m);
  assert.match(gate, /FRONTEND_BROWSER_RESULT: \$\{\{ needs.frontend-browser-test.result \}\}/);
  assert.match(gate, /FRONTEND_BROWSER_REQUIRED:.*github.event_name == 'merge_group'.*github.event.pull_request.draft == false/);
  const run = stepRun(gate, '汇总所有适用检查');
  const baseline = { ...process.env, FRONTEND_REQUIRED: 'true', FRONTEND_BROWSER_REQUIRED: 'true' };
  for (const match of gate.matchAll(/^          ([A-Z0-9_]+_RESULT):/gm)) baseline[match[1]] = 'success';
  for (const event of ['pull_request', 'merge_group']) for (const variable of ['FRONTEND_RESULT', 'FRONTEND_BROWSER_RESULT']) {
    for (const status of ['success', 'skipped', 'failure', 'cancelled', '']) {
      const result = spawnSync(bash, ['-c', run], { encoding: 'utf8', timeout: 30_000, windowsHide: true,
        env: { ...baseline, GITHUB_EVENT_NAME: event, [variable]: status } });
      assert.equal(result.status, status === 'success' ? 0 : 1, `${event}/${variable}/${status}: ${result.stderr}`);
    }
  }
  for (const status of ['skipped', 'failure', 'cancelled']) {
    const result = spawnSync(bash, ['-c', run], { encoding: 'utf8', timeout: 30_000, windowsHide: true,
      env: { ...baseline, GITHUB_EVENT_NAME: 'merge_group', CI_REUSE_VERIFIED: 'true',
        CI_REUSE_IDS: 'frontend-browser-test', FRONTEND_BROWSER_RESULT: status } });
    assert.equal(result.status, status === 'skipped' ? 0 : 1, result.stderr);
  }
});

test("queue reuse normalizes only verified skips and never failed executions", () => {
  const gate = jobBlock("required-gate");
  const run = stepRun(gate, "汇总所有适用检查");
  const baseEnv = { ...process.env, WINDOWS_RUST_REQUIRED_BY_POLICY: "false", ACCOUNT_PLATFORM_REQUIRED: "true" };
  for (const match of gate.matchAll(/^          ([A-Z0-9_]+_RESULT):/gm)) baseEnv[match[1]] = "success";
  for (const [event, verified, listed, status, accepted] of [
    ["merge_group", "true", "true", "skipped", true],
    ["merge_group", "false", "true", "skipped", false],
    ["merge_group", "true", "false", "skipped", false],
    ["pull_request", "true", "true", "skipped", false],
    ["merge_group", "true", "true", "failure", false],
    ["merge_group", "true", "true", "cancelled", false],
  ]) {
    const result = spawnSync(bash, ["-c", run], { encoding: "utf8", timeout: 30_000, windowsHide: true,
      env: { ...baseEnv, GITHUB_EVENT_NAME: event, CI_REUSE_VERIFIED: verified,
        CI_REUSE_IDS: listed === "true" ? "account-platform-test" : "", ACCOUNT_PLATFORM_RESULT: status } });
    assert.equal(result.status, accepted ? 0 : 1, `${event}/${verified}/${listed}/${status}: ${result.stderr}`);
  }
  const lookup = namedStep(jobBlock("changes"), "Verify identical-input PR success proofs");
  assert.match(lookup, /if: \$\{\{ github.event_name == 'merge_group' \}\}/);
  assert.match(lookup, /timeout-minutes: 2/);
  assert.match(lookup, /python3 scripts\/ci-test-reuse.py lookup/);
  const productJobs = ["version-consistency", "account-platform-test", "account-financial-test", "frontend-test", "frontend-browser-test", "relay-test",
    "windows-relay-dacl", "windows-relay-tools-test", "team-relay-hub-test", "update-release-console-test",
    "release-contract-test", "knowledge-rust", "rust-lint", "rust-test", "cli-test", "cli-lint",
    "windows-static-updater-contract", "windows-rust-test", "windows-rustup-repair-test", "macos-rust-check",
    "windows-codex-runtime-test", "windows-runtime-test", "macos-codex-runtime-test", "macos-cli-check", "rust-loongarch64-check"];
  for (const name of productJobs) {
    const block = jobBlock(name);
    assert.ok(block.includes("needs.changes.outputs.reuse_verified == 'true'"));
    assert.ok(block.includes(`contains(fromJSON(needs.changes.outputs.reused_jobs || '[]'), '${name}')`));
  }
  const bindings = [...run.matchAll(/"([\w-]+):([A-Z0-9_]+_RESULT)"/g)];
  assert.deepEqual(bindings.map(match => match[1]).sort(), [...productJobs].sort());
  const allReused = { ...baseEnv, GITHUB_EVENT_NAME: "merge_group", CI_REUSE_VERIFIED: "true",
    CI_REUSE_IDS: productJobs.join(","), WINDOWS_RUST_REQUIRED_BY_POLICY: "true",
    RELAY_REQUIRED_BY_POLICY: "true", BACKFLOW_MODE: "full" };
  for (const match of bindings) allReused[match[2]] = "skipped";
  for (const [override, accepted] of [[{}, true], [{ RUST_RESULT: "failure" }, false],
    [{ FAST_GATE_RESULT: "skipped", CI_REUSE_IDS: productJobs.join(",") + ",fast-gate" }, false]]) {
    const result = spawnSync(bash, ["-c", run], { encoding: "utf8", timeout: 30_000, windowsHide: true,
      env: { ...allReused, ...override } });
    assert.equal(result.status, accepted ? 0 : 1, result.stderr);
  }
  const producer = namedStep(gate, "Record successful PR test inputs");
  assert.ok(producer.includes("success() && github.event_name == 'pull_request'"));
  assert.ok(producer.includes("github.event.pull_request.head.repo.full_name == github.repository"));
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

  const job = workflow.split("\n  fast-gate:")[1].split("\n  frontend-setup:")[0];
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
  // Setup 工件化:三次 vite build 在 frontend-setup 一次完成,npm ci 保留在
  // 消费方(网络绑定、CPU 低)。AP probe 依赖安装在 frontend-test,原「安装
  // 先于 npm test」次序契约保持;另钉住消费方 needs 解线。
  const job = jobBlock("frontend-test", text).replaceAll("\r\n", "\n");
  assert.match(job, /^ {8}working-directory: pinvou3-app$/m);
  const install = namedStep(job, frontendProbeInstallStep);
  assert.match(install, /^ {8}run: npm --prefix \.\.\/account-platform ci --ignore-scripts --prefer-offline --no-audit$/m);
  assert.doesNotMatch(install, /^ {8}(?:if|continue-on-error|working-directory):/m,
    "The logic test loads AP dependencies on Draft too, in the inherited app cwd");
  assert.ok(job.indexOf(install) < job.indexOf(namedStep(job, frontendLogicStep)),
    "AP tools must be installed before npm test module discovery");
  assert.match(job, /^ {4}needs: \[changes, frontend-setup\]$/m);
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
  // Setup 工件化:三次 build 在 frontend-setup,AP probe 安装保留在 frontend-test。
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
  // Setup 工件化:Portal build 与 handoff 部署边界检查随构建迁入 frontend-setup
  // (build:account-portal 只跑一次,检查紧随其后);次序契约保持不变。
  const frontend = jobBlock("frontend-setup", text);
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
  // Setup 工件化:变异面与 assertAccountPlatformInputChecks 同步迁到 frontend-setup。
  const frontend = jobBlock("frontend-setup");
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
    const cleanDeployment = execute("frontend-setup", handoffDeploymentStep, join(directory, "pinvou3-app"));
    assert.equal(cleanDeployment.status, 0, cleanDeployment.stderr);
    put("account-platform/internal/portal/static/unsafe.js", "navigator.serviceWorker.register('/controlled-worker.js');\n");
    const worker = execute("frontend-setup", handoffDeploymentStep, join(directory, "pinvou3-app"));
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
  assert.match(run, /^python3 \.\.\/scripts\/ci-fixture-process\.py --receipt "\$PINVOU_CI_RESOURCE_LEDGER\/credential-origin\.json" -- python3 \.\.\/scripts\/ci-browser-network\.py -- node --test --test-reporter=tap tests\/credential_origin_scope_probe\.test\.mjs \| tee "\$report"$/m);
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
          [[ "$5" == python3 && "$6" == ../scripts/ci-browser-network.py && "$7" == -- ]] || return 98;
          shift 7; "$@";
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

test('all Linux frontend test entry points isolate their whole fixture tree after preparation', () => {
  const job = jobBlock('frontend-test');
  const logic = namedStep(job, '前端逻辑测试 (pet 全套 + diff parser + markdown)');
  assert.match(logic, /ci-fixture-process\.py --receipt[^\n]+ -- python3 \.\.\/scripts\/ci-browser-network\.py -- npm test$/m);
  assert.ok(job.indexOf(namedStep(job, 'Verify browser loopback isolation and retirement')) < job.indexOf(namedStep(job, '前端逻辑测试 (pet 全套 + diff parser + markdown)')));
  assert.ok(job.indexOf(namedStep(job, '安装前端依赖')) < job.indexOf(namedStep(job, '前端逻辑测试 (pet 全套 + diff parser + markdown)')));
  for (const id of ['frontend-test', 'frontend-browser-test']) {
    const preflight = namedStep(jobBlock(id), 'Verify browser loopback isolation and retirement');
    assert.match(preflight, /PINVOU_TEST_NETWORK_ISOLATION: "1"/);
    assert.doesNotMatch(preflight, /^ {8}(?:if|continue-on-error):/m);
    assert.match(preflight, /python3 -m unittest discover -s \.\.\/scripts\/tests -p test_ci_browser_network\.py -v/);
  }
});

const frontendUploadStep = "Upload frontend setup bundle";
const frontendUploadRetryStep = "Upload frontend setup bundle (retry)";
const frontendUploadRequiredStep = "Require frontend setup bundle upload";
const uploadPolicyLF = String.fromCharCode(10);

function assertFrontendUploadRecovery(text) {
  const setup = jobBlock("frontend-setup", text);
  const primary = namedStep(setup, frontendUploadStep);
  const retry = namedStep(setup, frontendUploadRetryStep);
  const required = namedStep(setup, frontendUploadRequiredStep);
  const line = (block, value) => block.split(uploadPolicyLF).includes(value);
  const hasContinue = block => block.split(uploadPolicyLF).some(value => value.startsWith("        continue-on-error:"));
  assert.ok(!setup.split(uploadPolicyLF).some(value => value.startsWith("    continue-on-error:")));
  assert.ok(line(primary, "        id: frontend_setup_upload"));
  assert.ok(line(primary, "        continue-on-error: true"));
  assert.equal(stepCondition(setup, frontendUploadStep), "steps.frontend_bundle.outputs.transport == 'github'");
  assert.ok(line(retry, "        id: frontend_setup_upload_retry"));
  assert.equal(stepCondition(setup, frontendUploadRetryStep), "!cancelled() && steps.frontend_setup_upload.outcome == 'failure'");
  assert.ok(!hasContinue(retry));
  for (const step of [primary, retry]) {
    assert.ok(line(step, "        uses: actions/upload-artifact@v7"));
    assert.ok(line(step, "          name: frontend-setup-bundle"));
    assert.ok(line(step, "          path: ${{ runner.temp }}/frontend-setup-bundle.tar.gz"));
    assert.ok(line(step, "          if-no-files-found: error"));
    assert.ok(line(step, "          retention-days: 1"));
  }
  assert.ok(line(retry, "          overwrite: true"));
  assert.equal(stepCondition(setup, frontendUploadRequiredStep), "always() && !cancelled()");
  assert.ok(!hasContinue(required));
  assert.ok(line(required, "        shell: bash"));
  assert.ok(line(required, "          FRONTEND_TRANSPORT: ${{ steps.frontend_bundle.outputs.transport }}"));
  assert.ok(line(required, "          FIRST_UPLOAD_OUTCOME: ${{ steps.frontend_setup_upload.outcome }}"));
  assert.ok(line(required, "          RETRY_UPLOAD_OUTCOME: ${{ steps.frontend_setup_upload_retry.outcome }}"));
  assert.ok(line(required, "          FRONTEND_SETUP_BUNDLE: ${{ runner.temp }}/frontend-setup-bundle.tar.gz"));
  const run = stepRun(setup, frontendUploadRequiredStep);
  assert.ok(run.includes('case "$FRONTEND_TRANSPORT:$FIRST_UPLOAD_OUTCOME:$RETRY_UPLOAD_OUTCOME" in'));
  assert.ok(run.includes("github:success:skipped|github:failure:success) ;;"));
  assert.ok(run.includes("lan:skipped:skipped) ;;"));
  assert.ok(run.includes('[[ ! -s "$FRONTEND_SETUP_BUNDLE" ]]'));
  assert.ok(setup.indexOf(namedStep(setup, "Pack frontend setup bundle")) < setup.indexOf(primary));
  assert.ok(setup.indexOf(primary) < setup.indexOf(retry) && setup.indexOf(retry) < setup.indexOf(required));
  for (const name of ["安装前端依赖", "Vite build", "Verify embedded Account Portal assets are current",
    "Verify credential handoff built deployment boundary", "Vite web build (remote relay)", "Pack frontend setup bundle"]) {
    assert.ok(!hasContinue(namedStep(setup, name)));
  }
}

test("frontend setup upload recovery keeps successful retry and strict artifact gate", () => {
  assertFrontendUploadRecovery(workflow);
});

test("frontend setup upload policy rejects broad retry, swallowed failures and payload drift", () => {
  const setup = jobBlock("frontend-setup");
  const primary = namedStep(setup, frontendUploadStep);
  const retry = namedStep(setup, frontendUploadRetryStep);
  const required = namedStep(setup, frontendUploadRequiredStep);
  const variants = [
    setup.replace(primary, primary.replace("        continue-on-error: true" + uploadPolicyLF, "")),
    setup.replace(primary, primary.replace("id: frontend_setup_upload", "id: unrelated_upload")),
    setup.replace(retry, retry.replace("!cancelled() && steps.frontend_setup_upload.outcome == 'failure'", "failure()")),
    setup.replace(retry, retry.replace("steps.frontend_setup_upload.outcome", "steps.frontend_setup_upload.conclusion")),
    setup.replace(retry, retry.replace("        uses:", "        continue-on-error: true" + uploadPolicyLF + "        uses:")),
    setup.replace(retry, retry.replace("frontend-setup-bundle.tar.gz", "unrelated.tar.gz")),
    setup.replace(primary, primary.replace("if-no-files-found: error", "if-no-files-found: warn")),
    setup.replace(required, ""),
    setup.replace(required, required.replace("always() && !cancelled()", "success()")),
    setup.replace(required, required.replace("        shell:", "        continue-on-error: true" + uploadPolicyLF + "        shell:")),
    setup.replace(required, required.replace("[[ ! -s", "[[ ! -e")),
    setup.replace(required, required.replace("lan:skipped:skipped) ;;", "lan:skipped:skipped) error ;;")),
    setup.replace(primary, primary.replace("if: ${{ steps.frontend_bundle.outputs.transport == 'github' }}" + uploadPolicyLF, "")),
  ];
  for (const variant of variants) assert.throws(() => assertFrontendUploadRecovery(workflow.replace(setup, variant)));
});

test("frontend upload retry observes only primary outcome and preserves cancellation", () => {
  const setup = jobBlock("frontend-setup");
  for (const cancelled of [false, true]) {
    for (const outcome of ["success", "failure", "skipped", "cancelled", "timed_out", ""]) {
      const context = { cancelled: () => cancelled, always: () => true,
        steps: { frontend_setup_upload: { outcome } } };
      assert.equal(runInNewContext(stepCondition(setup, frontendUploadRetryStep), context), !cancelled && outcome === "failure");
      assert.equal(runInNewContext(stepCondition(setup, frontendUploadRequiredStep), context), !cancelled);
    }
  }
});

test("frontend upload final shell accepts exactly recovered outcomes with a nonempty bundle", () => {
  const run = stepRun(jobBlock("frontend-setup"), frontendUploadRequiredStep);
  const directory = mkdtempSync(join(tmpdir(), "frontend-upload-policy-"));
  try {
    const file = join(directory, "frontend-setup-bundle.tar.gz");
    const cases = [["github", "success", "skipped", true], ["github", "failure", "success", true],
      ["lan", "skipped", "skipped", true],
      ["github", "failure", "failure", false], ["github", "failure", "skipped", false], ["github", "failure", "cancelled", false],
      ["github", "cancelled", "skipped", false], ["github", "skipped", "skipped", false], ["github", "success", "success", false],
      ["lan", "success", "skipped", false], ["github", "", "", false]];
    for (const [transport, primary, retry, accepted] of cases) {
      for (const payload of accepted ? ["nonempty", "empty", "missing"] : ["nonempty"]) {
        rmSync(file, { force: true });
        if (payload !== "missing") writeFileSync(file, payload === "empty" ? "" : "fixed bundle");
        const result = spawnSync(bash, ["-c", run], { cwd: directory, encoding: "utf8", timeout: 30_000, windowsHide: true,
          env: { ...process.env, FRONTEND_TRANSPORT: transport, FIRST_UPLOAD_OUTCOME: primary, RETRY_UPLOAD_OUTCOME: retry,
            FRONTEND_SETUP_BUNDLE: "frontend-setup-bundle.tar.gz" } });
        assert.equal(result.status, accepted && payload === "nonempty" ? 0 : 1,
          `${transport}/${primary}/${retry}/${payload}: ${result.error?.message || result.stderr}`);
      }
    }
  } finally {
    rmSync(directory, { recursive: true, force: true });
  }
});
