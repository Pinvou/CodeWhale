import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";

const runbook = await readFile(
  new URL("../../account-platform/deploy/runbooks/f18-hold-recovery-owned-fixture.md", import.meta.url),
  "utf8",
);

const uuid = "11111111-1111-4111-8111-111111111111";
const ownedDirectory = `<temp>/pinvou-f18-billing-${uuid}/data`;
const databaseURL = "postgres://owned-user@127.0.0.1:55474/postgres?sslmode=disable";

test("F18 runbook pins every owned PostgreSQL identity input", () => {
  for (const name of [
    "PINVOU_TEST_BILLING_DATABASE_URL",
    "F18_TEST_DATABASE_PROFILE",
    "F18_TEST_DATABASE_USER",
    "F18_TEST_DATABASE_PORT",
    "F18_TEST_DATA_DIRECTORY",
  ]) {
    assert.match(runbook, new RegExp(`\\b${name}=`), `missing ${name}`);
  }
  assert.match(runbook, /F18_TEST_DATABASE_PROFILE=local-owned/);
  assert.match(runbook, /127\.0\.0\.1:<port>\/postgres\?sslmode=disable/);
  assert.match(runbook, /pinvou-f18-billing-<uuid>\/data/);
  assert.match(runbook, /no TLS, fallback host, or runtime parameter/);
  assert.match(runbook, /stops before DDL/);
  assert.match(runbook, /conditional skip/);
});

test("F18 owned fixture shape accepts only canonical loopback UUID paths", () => {
  const urlShape = /^postgres:\/\/[^@/]+@127\.0\.0\.1:\d{4,5}\/postgres\?sslmode=disable$/;
  const directoryShape = /^<temp>\/pinvou-f18-billing-[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\/data$/;
  assert.match(databaseURL, urlShape);
  assert.match(ownedDirectory, directoryShape);
  for (const invalid of [
    "postgres://owned-user@localhost:55474/postgres?sslmode=disable",
    "postgres://owned-user@192.0.2.1:55474/postgres?sslmode=disable",
    "postgres://owned-user@127.0.0.1:55474/deployment?sslmode=disable",
    "postgres://owned-user@127.0.0.1:55474/postgres?sslmode=prefer",
  ]) {
    assert.doesNotMatch(invalid, urlShape);
  }
  assert.doesNotMatch(ownedDirectory.replace(uuid, "00000000-0000-0000-0000-000000000000"), directoryShape);
  assert.doesNotMatch(ownedDirectory.replace(uuid, "11111111-1111-5111-8111-111111111111"), directoryShape);
});
