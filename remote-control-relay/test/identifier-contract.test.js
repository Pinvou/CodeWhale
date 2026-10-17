import assert from "node:assert/strict";
import test from "node:test";
import { isControllerIdentifier, isMigrationRequestIdentifier, isLegacyWebEndpointIdentifier } from "../identifier-contract.js";
import { isValidControllerIdentifier } from "../protocol-controller.js";

test("shared identifier propagation preserves A06 full-string matching", () => {
  for (const value of ["a", "ID._:-1", "a".repeat(128)]) {
    assert.equal(isControllerIdentifier(value), true);
    assert.equal(isValidControllerIdentifier(value), true);
  }
  for (const value of ["valid\n", "valid\r", "valid\r\n", "valid~", "_wire", "端点", "", "a".repeat(129), null]) {
    assert.equal(isControllerIdentifier(value), false, JSON.stringify(value));
    assert.equal(isValidControllerIdentifier(value), false, JSON.stringify(value));
  }
  assert.equal(isMigrationRequestIdentifier("admin:01"), true);
  assert.equal(isMigrationRequestIdentifier("admin:01\n"), false);
  for (const value of ["_old", "-old"]) assert.equal(isLegacyWebEndpointIdentifier(value), true);
});
