import * as fs from "node:fs/promises";
import { dirname, join } from "node:path";
import { RelayStateStore } from "../../relay-state-store.js";
import { DatabaseSync } from "node:sqlite";

const statePath = process.argv[2];
const mode = process.argv[3];
const seedMode = mode.replace(/^native-/u, "");
// Distinct exits prove the exact synchronous fault boundary was reached.
// An earlier open failure must never look like a successful fault injection.
const bootstrapFaultExit = {
  "exit-before-guard-protect": 81,
  "exit-before-seed-commit": 82,
  "exit-after-seed-commit": 83,
};
if (seedMode === "exit-before-seed-commit" || seedMode === "exit-after-seed-commit") {
  const execute = DatabaseSync.prototype.exec;
  DatabaseSync.prototype.exec = function(sql) {
    if (sql.startsWith("PRAGMA application_id=")) {
      const initialization = sql.slice(0, sql.indexOf("COMMIT;"));
      execute.call(this, seedMode === "exit-before-seed-commit" ? initialization : `${initialization}COMMIT;`);
      process.exit(bootstrapFaultExit[seedMode]); // No cleanup after the real SQL boundary.
    }
    return execute.call(this, sql);
  };
}
let resume;
const barrier = new Promise(resolve => { resume = resolve; });
process.on("message", message => {
  if (message === "resume") resume();
  if (message === "exit") process.exit(0); // Test real OS release, no store.close.
});
const io = { ...fs, async rename(source, target) {
  if (mode === "pause-reclaim" && source === `${statePath}.lock`) {
    process.send({ event: "before-reclaim" });
    await barrier;
  }
  return fs.rename(source, target);
}, async lstat(target, ...options) {
  if (seedMode === "fail-before-seed-commit" && target.endsWith(".pinvou-controller-state.guard")) {
    throw Object.assign(new Error("injected pre-bootstrap failure"), { code: "EACCES" });
  }
  const metadata = await fs.lstat(target, ...options);
  // The first successful guard stat follows SQLite creation and precedes any
  // protection. This boundary also exists when a restrictive umask means no
  // chmod is needed, unlike a hook on chmod/protect itself.
  if (seedMode === "exit-before-guard-protect" && target.endsWith(".pinvou-controller-state.guard")
    && metadata.isFile() && (metadata.size === 0n || metadata.size === 0)) process.exit(bootstrapFaultExit[seedMode]);
  return metadata;
} };
let store;
try {
  store = await RelayStateStore.open({
    statePath, legacyStatePath: join(dirname(dirname(statePath)), "legacy.json"), fs: io,
    processIdentity: async pid => {
      try { process.kill(pid, 0); } catch { return null; }
      return `guard-test-process:${pid}`;
    },
    ...(process.platform === "win32" && !mode.startsWith("native-") ? {
      protectPath: async () => {}, verifyPath: async () => {}, verifyAncestors: async () => {},
    } : {}),
  });
  process.send({ event: "ready", snapshot: store.readNamespace("guard_test") });
  process.on("message", async message => {
    try {
      if (message === "commit") {
        const snapshot = store.readNamespace("guard_test");
        const value = await store.compareAndSwap("guard_test", snapshot.namespace_revision,
          { commits: (snapshot.value?.commits || 0) + 1 });
        process.send({ event: "committed", snapshot: value });
      }
      if (message === "close") {
        await store.close();
        process.send({ event: "closed" }, () => process.exit(0));
      }
    } catch (error) {
      process.send({ event: "error", code: error.code || "unexpected" });
    }
  });
} catch (error) {
  process.send({ event: "error", code: error.code || "unexpected" }, () => process.exit(1));
}
