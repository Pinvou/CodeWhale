import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const probe = fileURLToPath(new URL("./helpers/claude-acp-startup-probe.cjs", import.meta.url));

test("ACP startup observer preserves process APIs, stdin and protocol output without leaking inputs", () => {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "acp-probe-test-"));
  try {
    const entry = path.join(root, "claude-agent-acp", "dist", "index.js");
    fs.mkdirSync(path.dirname(entry), { recursive: true });
    fs.writeFileSync(entry, `
      const assert = require('node:assert/strict');
      const cp = require('node:child_process');
      const before = cp.execFile;
      const symbols = Object.getOwnPropertySymbols(before);
      require(${JSON.stringify(probe)});
      assert.equal(cp.execFile, before);
      assert.deepEqual(Object.getOwnPropertySymbols(cp.execFile), symbols);
      assert.equal(process.stdin.readableFlowing, null);
      let input = '';
      process.stdin.on('data', chunk => { input += chunk; });
      process.stdin.on('end', async () => {
        assert.equal(input, 'private-rpc-body');
        const result = await require('node:util').promisify(cp.execFile)(process.execPath, ['--version']);
        assert.match(result.stdout, /^v/);
        console.log(JSON.stringify({id: 1, ok: true}));
      });
    `);
    const result = spawnSync(process.execPath, [entry, "private-argument"], {
      input: "private-rpc-body", encoding: "utf8", timeout: 10_000,
      env: { ...process.env, ACP_PRIVATE_TEST: "private-environment-value" },
    });
    assert.equal(result.status, 0, result.stderr);
    assert.deepEqual(JSON.parse(result.stdout), { id: 1, ok: true });
    const records = result.stderr.trim().split(/\r?\n/u).map(line => {
      assert.ok(line.startsWith("[acp-startup] "));
      return JSON.parse(line.slice("[acp-startup] ".length));
    });
    assert.ok(records.some(event => event.phase === "preload"));
    assert.ok(records.some(event => event.phase === "stdin-data-listener"));
    assert.ok(records.length <= 48);
    for (const secret of [root, "private-rpc-body", "private-argument", "private-environment-value"]) {
      assert.ok(!result.stderr.includes(secret), "diagnostics only expose fixed tags and numeric metadata");
    }
    for (const record of records) {
      for (const key of ["elapsed_ms", "cpu_ms", "rss_mib"]) assert.ok(Number.isFinite(record[key]));
    }
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
});
