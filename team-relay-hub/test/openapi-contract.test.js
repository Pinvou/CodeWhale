import assert from "node:assert/strict";
import { readdir, readFile } from "node:fs/promises";
import test from "node:test";

test("OpenAPI publishes every literal Hub error code", async () => {
  const sourceDirectory = new URL("../src/", import.meta.url);
  const runtimeCodes = new Set();
  for (const entry of await readdir(sourceDirectory, { withFileTypes: true })) {
    if (!entry.isFile() || !entry.name.endsWith(".js")) continue;
    const source = await readFile(new URL(entry.name, sourceDirectory), "utf8");
    for (const match of source.matchAll(
      /new AppError\(\s*\d+\s*,\s*["']([a-z0-9_]+)["']/g,
    )) {
      runtimeCodes.add(match[1]);
    }
  }

  const document = await readFile(
    new URL("../../docs/team-relay/openapi.yaml", import.meta.url),
    "utf8",
  );
  const schemaStart = document.indexOf("    ErrorDetail:");
  const schemaEnd = document.indexOf("        message:", schemaStart);
  assert.ok(schemaStart >= 0 && schemaEnd > schemaStart, "ErrorDetail schema must exist");
  const declaredCodes = new Set(
    [...document.slice(schemaStart, schemaEnd).matchAll(/^\s+- ([a-z0-9_]+)$/gm)]
      .map(match => match[1]),
  );
  assert.deepEqual(
    [...runtimeCodes].filter(code => !declaredCodes.has(code)).sort(),
    [],
    "literal Hub error codes must stay valid under ErrorDetail.code",
  );
});
