import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

const runbook = await readFile(
  new URL("../../account-platform/deploy/runbooks/f18-hold-recovery-owned-fixture.md", import.meta.url),
  "utf8",
);
const goTest = await readFile(
  new URL("../../account-platform/internal/billing/f18_hold_recovery_integration_test.go", import.meta.url),
  "utf8",
);
const names = [
  "PINVOU_TEST_BILLING_DATABASE_URL", "F18_TEST_DATABASE_PROFILE",
  "F18_TEST_DATABASE_USER", "F18_TEST_DATABASE_PORT", "F18_TEST_DATA_DIRECTORY",
];

function assertDocumentedTemplate(text) {
  const block = text.match(/```text\r?\n([\s\S]*?)\r?\n```/u);
  assert.ok(block, "the documented target template must exist");
  const entries = block[1].split(/\r?\n/u).map(line => {
    const separator = line.indexOf("=");
    assert.ok(separator > 0, "every template line must assign a field");
    return [line.slice(0, separator), line.slice(separator + 1)];
  });
  assert.deepEqual(entries.map(([name]) => name).sort(), [...names].sort(), "exact identity fields, without duplicates or extras");
  const fields = Object.fromEntries(entries);
  assert.equal(fields.F18_TEST_DATABASE_PROFILE, "local-owned");
  assert.equal(fields.F18_TEST_DATABASE_USER, "<owned-user>");
  assert.equal(fields.F18_TEST_DATABASE_PORT, "<port>");
  const rendered = fields.PINVOU_TEST_BILLING_DATABASE_URL
    .replaceAll("<owned-user>", "owned-user").replaceAll("<port>", "55474");
  assert.match(rendered, /^postgres:\/\/owned-user@127\.0\.0\.1:55474\/postgres\?sslmode=disable$/u);
  const url = new URL(rendered);
  assert.equal(url.username, fields.F18_TEST_DATABASE_USER.replace("<owned-user>", "owned-user"));
  assert.equal(url.port, fields.F18_TEST_DATABASE_PORT.replace("<port>", "55474"));
  assert.equal(fields.F18_TEST_DATA_DIRECTORY, "<temp>/pinvou-f18-billing-<uuid>/data");
}

test("F18 runbook distinguishes the target contract from the current named fixture", () => {
  assert.match(runbook, /Target contract — not current runnable configuration/u);
  assert.match(runbook, /#1735 and #1979 track.*implementation.*acceptance/u);
  assert.match(runbook, /not consumed by the current named Go test/u);
  assert.match(runbook, /current test instead skips on an unset URL/u);
  assert.match(runbook, /CI guard checks documented fields and prose/u);
  assert.match(runbook, /no TLS, fallback host, or runtime parameter/u);
  assert.match(runbook, /no non-system roles other than the declared fixture coordinator/u);
  assert.match(runbook, /no non-template databases other than `postgres`/u);
  assert.match(runbook, /stop before DDL/u);
  assert.match(goTest, /raw := os\.Getenv\("PINVOU_TEST_BILLING_DATABASE_URL"\)/u);
  assert.match(goTest, /if raw == ""\s*\{\s*t\.Skip\(/u);
  assert.match(goTest, /admin,\s*e\s*:=\s*pgx\.Connect\(ctx,\s*raw\)/u,
    "revisit current-status documentation when the named test changes its connection path");
  for (const name of names.slice(1)) {
    assert.doesNotMatch(goTest, new RegExp(`os\\.(?:Getenv|LookupEnv)\\(\\s*"${name}"\\s*\\)`),
      `${name} consumption requires revisiting the documented current status`);
  }
});

test("F18 target shape consumes the actual documented identity fields", () => {
  assertDocumentedTemplate(runbook);
});

test("F18 shape rejects mutations of the actual runbook URL and directory", () => {
  for (const [from, to] of [
    ["127.0.0.1", "localhost"], ["127.0.0.1", "192.0.2.1"],
    ["/postgres?", "/deployment?"], ["sslmode=disable", "sslmode=prefer"],
    ["sslmode=disable", "sslmode=disable&application_name=f18"],
    ["sslmode=disable", "sslmode=disable&host=192.0.2.1"],
    ["sslmode=disable", "sslmode=disable&sslmode=prefer"],
    ["sslmode=disable", "sslmode=disable#runtime"],
    ["<uuid>", "00000000-0000-0000-0000-000000000000"],
    ["/data", "/data/extra"],
    ["F18_TEST_DATABASE_PROFILE=local-owned", "F18_TEST_DATABASE_PROFILE=shared"],
  ]) {
    const changed = runbook.replace(from, to);
    assert.notEqual(changed, runbook, `mutation must change the real document: ${from}`);
    assert.throws(() => assertDocumentedTemplate(changed), assert.AssertionError, `reject ${to}`);
  }
});
