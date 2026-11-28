// exec + transport safety tests.
import { test } from "node:test";
import assert from "node:assert/strict";
import { run, runOk, ExecError, have, trim } from "../src/exec.mjs";
import { safeRemotePath, b64, localExec } from "../src/transport.mjs";

test("run captures stdout/stderr and exit codes without a shell", async () => {
  const r = await run("node", ["-e", "console.log('hello'); console.error('boo')"]);
  assert.equal(r.code, 0);
  assert.equal(r.stdout.trim(), "hello");
  assert.match(r.stderr, /boo/);
});

test("run reports missing executables as code -1 with ENOENT, never throws", async () => {
  const r = await run("definitely-not-a-real-tool-xyz", ["--version"]);
  assert.equal(r.code, -1);
  assert.match(r.stderr, /ENOENT/);
});

test("runOk throws ExecError on non-zero exit and includes stderr", async () => {
  await assert.rejects(() => runOk("node", ["-e", "console.error('reason-here'); process.exit(3)"]), (e) => {
    assert.ok(e instanceof ExecError);
    assert.match(e.message, /exited 3/);
    assert.match(e.message, /reason-here/);
    return true;
  });
});

test("run enforces timeouts", async () => {
  const r = await run("node", ["-e", "setInterval(()=>{},1000)"], { timeoutMs: 300 });
  assert.equal(r.timedOut, true);
});

test("run appends a byte-accurate truncation marker for CJK output past maxBuffer", async () => {
  // 50 CJK chars = 150 UTF-8 bytes against a 100-byte cap: the marker must
  // fire and the kept prefix must stay within cap bytes.
  const r = await run("node", ["-e", "process.stdout.write('\\u6c49'.repeat(50))"], { maxBuffer: 100 });
  assert.equal(r.code, 0);
  assert.match(r.stdout, /\n…\[stdout truncated at 100 byte capture limit\]$/);
  const kept = r.stdout.replace(/\n…\[stdout truncated at 100 byte capture limit\]$/, "");
  assert.ok(
    Buffer.byteLength(kept, "utf8") <= 100,
    `kept prefix should be <= 100 bytes, got ${Buffer.byteLength(kept, "utf8")}`,
  );
  // Byte-truthful kept prefix: the cut drops the trailing partial sequence
  // (floor(100/3) = 33 whole chars = 99 bytes), so no replacement character
  // that was never in the output may appear, however the pipe chunked the
  // stream.
  assert.ok(!kept.includes("\uFFFD"), "kept prefix must not contain a fabricated replacement char");
  assert.equal(kept, "\u6c49".repeat(33));
});

test("run does not mark truncation when output fits or exactly fills maxBuffer", async () => {
  // 10 CJK chars = 30 UTF-8 bytes: under a 100-byte cap, and exactly at a
  // 30-byte cap, nothing is dropped so no marker may appear.
  for (const maxBuffer of [100, 30]) {
    const r = await run("node", ["-e", "process.stdout.write('\\u6c49'.repeat(10))"], { maxBuffer });
    assert.equal(r.code, 0);
    assert.equal(r.stdout, "\u6c49".repeat(10));
    assert.ok(!r.stdout.includes("truncated"), `no marker expected at cap ${maxBuffer}`);
  }
});

test("spawn errors surface with their own line, outside the capture cap", async () => {
  // The 'error' diagnostic is carried outside the capped stderr storage so it
  // can never be cut away by the cap (an error arriving after an over-cap
  // stderr would otherwise leave only the truncation marker). A spawn failure
  // is the deterministically reproducible error event; the prefix pins the
  // dedicated-channel mechanism.
  const r = await run("definitely-not-a-real-tool-xyz", ["--version"], { maxBuffer: 10 });
  assert.equal(r.code, -1);
  assert.match(r.stderr, /^spawn error: /);
  assert.match(r.stderr, /ENOENT/);
});

test("multi-byte sequence split across pipe chunks decodes without fabrication", async () => {
  // One 3-byte char written as 2 bytes + 1 byte across separate pipe chunks
  // (child-side delay forces distinct data events): concat-then-decode-once
  // must yield the character; per-chunk decodes would fabricate a replacement
  // character at the internal join and at the trailing partial sequence.
  const r = await run(
    "node",
    ["-e", "process.stdout.write(Buffer.from([0xe4,0xb8])); setTimeout(() => process.stdout.write(Buffer.from([0xad])), 40)"],
    { maxBuffer: 100 },
  );
  assert.equal(r.code, 0);
  assert.equal(r.stdout, "\u4e2d");
  assert.ok(!r.stdout.includes("\uFFFD"), "no replacement character may be fabricated at a chunk join");
});

test("have() detects real and missing tools", async () => {
  assert.equal(await have("node"), true);
  assert.equal(await have("definitely-not-a-real-tool-xyz"), false);
});

test("safeRemotePath blocks traversal, metacharacters, and absolute escapes", () => {
  assert.equal(safeRemotePath(".codewhale-cu/agent/agent.mjs"), ".codewhale-cu/agent/agent.mjs");
  for (const bad of ["../../etc/passwd", "/etc/passwd", "a;rm -rf /", "a b", "$(id)", "a\nb", "a'b", ".codewhale-cu/../escape"]) {
    assert.throws(() => safeRemotePath(bad), ExecError, `should reject: ${bad}`);
  }
});

test("b64 round-trips JSON payloads", () => {
  const obj = { tool: "screenshot", args: { region: [0, 0, 10, 10] } };
  assert.deepEqual(JSON.parse(Buffer.from(b64(obj), "base64").toString("utf8")), obj);
});

test("localExec provides run/runOk/tmpFile", async () => {
  const ex = localExec();
  const r = await ex.run("echo", ["hi"]);
  assert.equal(r.code, 0);
  const f = ex.tmpFile("cu-test-");
  assert.ok(typeof f === "string");
});
