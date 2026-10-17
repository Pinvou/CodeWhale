import assert from "node:assert/strict";
import { fork } from "node:child_process";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { fileURLToPath } from "node:url";
import { once } from "node:events";
import { closeClientFromUpstream, normalizeUpstreamClose } from "../websocket-close.js";

for (const scenario of ["abnormal", "valid"]) {
  test(`real gateway child survives ${scenario} upstream close`, { timeout: 15_000 }, async (t) => {
    const root = await mkdtemp(join(tmpdir(), "pinvou-gateway-close-"));
    const child = fork(fileURLToPath(new URL("./fixtures/gateway-close-worker.js", import.meta.url)),
      [scenario, root], { silent: true });
    t.after(async () => {
      if (child.exitCode === null && child.signalCode === null) {
        const exited = once(child, "exit");
        child.kill();
        await exited;
      }
      await rm(root, { recursive: true, force: true });
    });
    let result;
    let errors = "";
    child.on("message", (value) => { result = value; });
    child.stderr.on("data", (value) => { errors = (errors + value.toString()).slice(-4096); });
    child.stdout.resume();
    const exitCode = await new Promise((resolve, reject) => {
      child.once("error", reject);
      child.once("exit", resolve);
    });
    assert.equal(exitCode, 0, errors);
    assert.equal(result?.ok, true);
    assert.equal(result.code, scenario === "abnormal" ? 1011 : 4001);
    assert.equal(result.reasonBytes, scenario === "abnormal" ? 0 : 123);
  });
}

test("upstream close normalization matches ws status boundaries and clips by UTF-8 bytes", () => {
  for (const code of [1000, 1001, 1002, 1003, 1007, 1008, 1009, 1010, 1011, 1012, 1013, 1014, 3000, 4999]) {
    assert.equal(normalizeUpstreamClose(code, Buffer.from("合法")).code, code);
  }
  for (const code of [undefined, NaN, 0, 999, 1004, 1005, 1006, 1015, 2999, 5000, "1000", 1000.5]) {
    assert.equal(normalizeUpstreamClose(code, "").code, 1011);
  }
  // Overlong reasons are defensive unit inputs, NOT legal upstream wire frames.
  for (const reason of ["x".repeat(124), "断".repeat(42), "😀".repeat(32), "x".repeat(122) + "😀"]) {
    const normalized = normalizeUpstreamClose(4001, reason);
    assert.ok(Buffer.byteLength(normalized.reason, "utf8") <= 123);
    assert.equal(Buffer.from(normalized.reason).toString(), normalized.reason);
  }
  assert.equal(normalizeUpstreamClose(4001, "断".repeat(42)).reason, "断".repeat(41));
  assert.equal(normalizeUpstreamClose(4001, "😀".repeat(32)).reason, "😀".repeat(30));
});

test("an unexpected close failure is contained to its client", () => {
  let terminated = 0;
  assert.doesNotThrow(() => closeClientFromUpstream({ readyState: 1,
    close() { throw new TypeError("synthetic transport failure"); }, terminate() { terminated += 1; } }, 1006, Buffer.alloc(0)));
  assert.equal(terminated, 1);
  closeClientFromUpstream({ readyState: 3, close() { assert.fail("closed client must not be closed again"); } }, 1000, "");
});
