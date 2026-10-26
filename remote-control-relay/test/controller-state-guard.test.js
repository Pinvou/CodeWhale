import assert from "node:assert/strict";
import * as fs from "node:fs/promises";
import { fork, spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";
import { DatabaseSync } from "node:sqlite";
import { RelayStateStore } from "../relay-state-store.js";
import { RELAY_CONTROLLER_GUARD_FILENAME, RelayStateGuard } from "../relay-state-guard.js";

const childFile = fileURLToPath(new URL("./fixtures/controller-guard-child.mjs", import.meta.url));
const security = process.platform === "win32" ? {
  protectPath: async () => {}, verifyPath: async () => {}, verifyAncestors: async () => {},
} : {};
async function expectBootstrapFault(child, phase) {
  const expected = { "before-guard-protect": 81, "before-seed-commit": 82, "after-seed-commit": 83 }[phase];
  assert.ok(expected, "known bootstrap fault phase required");
  const { code, signal } = await child.exited;
  assert.equal(signal, null, "bootstrap fault must exit without a signal");
  assert.equal(code, expected, `child did not reach the ${phase} fault boundary`);
}
function windowsCreatorMetadata(target) {
  const script = `
$ErrorActionPreference = 'Stop'
$identity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$current = $identity.User.Value
$defaultOwner = $identity.Owner.Value
$acl = Microsoft.PowerShell.Security\\Get-Acl -LiteralPath $env:PINVOU_GUARD_PROBE
$owner = $acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value
function OwnerClass([string]$sid) {
  if ($sid -eq $current) { return 'current' }
  if ($sid -eq 'S-1-5-32-544') { return 'administrators' }
  if ($sid -eq 'S-1-5-18') { return 'system' }
  return 'other'
}
[Console]::Out.Write(([pscustomobject]@{
  token_owner = (OwnerClass $defaultOwner)
  file_owner = (OwnerClass $owner)
  matches_creator_default = ($owner -eq $defaultOwner)
  protected = $acl.AreAccessRulesProtected
} | Microsoft.PowerShell.Utility\\ConvertTo-Json -Compress))
`;
  const systemRoot = process.env.SystemRoot || process.env.WINDIR;
  assert.ok(systemRoot);
  const environment = { PINVOU_GUARD_PROBE: target };
  for (const key of ["SystemRoot", "WINDIR", "PATH", "PATHEXT", "TEMP", "TMP"]) {
    if (process.env[key] !== undefined) environment[key] = process.env[key];
  }
  const result = spawnSync(join(systemRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
    ["-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", Buffer.from(script, "utf16le").toString("base64")],
    { env: environment, encoding: "utf8", windowsHide: true, timeout: 15000, maxBuffer: 4096 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, "native creator metadata inspection failed");
  const metadata = JSON.parse(result.stdout);
  for (const key of ["token_owner", "file_owner"]) assert.ok(["current", "administrators", "system", "other"].includes(metadata[key]));
  assert.equal(typeof metadata.matches_creator_default, "boolean");
  assert.equal(typeof metadata.protected, "boolean");
  return metadata;
}
async function fixture(t) {
  const directory = await fs.realpath(await fs.mkdtemp(join(tmpdir(), "pinvou-guard-test-")));
  const statePath = join(directory, "controller", "state.json");
  const stores = [], children = [];
  t.after(async () => {
    for (const child of children) {
      if (child.exitCode === null) {
        await new Promise(resolve => { child.once("exit", resolve); child.kill(); });
      }
    }
    for (const store of stores) await store.close();
    // This path is a newly created private fixture, never a link to a checkout.
    await fs.rm(directory, { recursive: true, force: true });
  });
  const options = { statePath, legacyStatePath: join(directory, "legacy.json"),
    processIdentity: async pid => {
      try { process.kill(pid, 0); } catch { return null; }
      return `guard-test-process:${pid}`;
    }, ...security };
  async function open(overrides = {}) {
    const store = await RelayStateStore.open({ ...options, ...overrides });
    stores.push(store);
    return store;
  }
  function child(mode = "normal") {
    const process = fork(childFile, [statePath, mode], {
      execArgv: [], stdio: ["ignore", "ignore", "pipe", "ipc"], windowsHide: true,
      env: Object.fromEntries(Object.entries(globalThis.process.env).filter(([key]) => !key.startsWith("PINVOU_"))),
    });
    children.push(process);
    const exited = new Promise(resolve => process.once("exit", (code, signal) => resolve({ code, signal })));
    let failure = "";
    process.stderr.on("data", chunk => { failure += chunk; });
    const messages = [], waiters = [];
    process.on("message", message => {
      if (waiters.length) waiters.shift()(message);
      else messages.push(message);
    });
    async function next() {
      if (messages.length) return messages.shift();
      return new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error(`child timeout: ${failure}`)), 30000);
        waiters.push(message => { clearTimeout(timer); resolve(message); });
      });
    }
    return { process, next, exited, send: message => process.send(message) };
  }
  return { directory, statePath, options, open, child };
}

test("controller guard excludes another process before a stale reclaimer may rename", async t => {
  const f = await fixture(t);
  await (await f.open()).close();
  const dead = { schema: "pinvou-relay-controller-state-lock", version: 1,
    pid: 500000000, process_identity: "guard-test-dead-process", owner_id: "a".repeat(24) };
  await fs.writeFile(`${f.statePath}.lock`, `${JSON.stringify(dead)}\n`, { mode: 0o600 });
  const first = f.child("pause-reclaim");
  assert.equal((await first.next()).event, "before-reclaim");
  const contender = f.child();
  assert.deepEqual(await contender.next(), { event: "error", code: "relay_state_locked" });
  first.send("resume");
  assert.equal((await first.next()).event, "ready");
  first.send("commit");
  assert.equal((await first.next()).snapshot.store_revision, "1");
  const third = f.child();
  assert.deepEqual(await third.next(), { event: "error", code: "relay_state_locked" });
  first.send("close");
  assert.equal((await first.next()).event, "closed");
  const successor = await f.open();
  assert.deepEqual(successor.readNamespace("guard_test").value, { commits: 1 });
  await successor.compareAndSwap("guard_test", "1", { commits: 2 });
  await successor.close();
  assert.equal((await f.open()).readNamespace("guard_test").store_revision, "2");
});

test("controller guard OS lock releases on process exit and preserves next commit", async t => {
  const f = await fixture(t), owner = f.child();
  assert.equal((await owner.next()).event, "ready");
  owner.send("commit");
  assert.equal((await owner.next()).snapshot.store_revision, "1");
  const exited = new Promise(resolve => owner.process.once("exit", resolve));
  owner.send("exit");
  await exited;
  const next = await f.open();
  assert.equal(next.readNamespace("guard_test").value.commits, 1);
  await next.compareAndSwap("guard_test", "1", { commits: 2 });
  await next.close();
  assert.equal((await f.open()).readNamespace("guard_test").value.commits, 2);
});

test("controller guard reserves same-process opens before asynchronous directory preparation", async t => {
  const f = await fixture(t);
  let unblock, started;
  const arrived = new Promise(resolve => { started = resolve; });
  const barrier = new Promise(resolve => { unblock = resolve; });
  const first = f.open({ fs: { ...fs, async mkdir(...args) { started(); await barrier; return fs.mkdir(...args); } } });
  await arrived;
  try { await assert.rejects(f.open(), { code: "relay_state_locked" }); }
  finally { unblock(); }
  await (await first).close();
  await (await f.open()).close();
});

test("controller guard stays fixed and initialized and is never ordinarily opened by a contender", async t => {
  const f = await fixture(t), first = await f.open();
  const guardPath = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const before = await fs.lstat(guardPath, { bigint: true });
  assert.ok(before.size >= 512n && before.size <= 65536n);
  const contender = f.child();
  assert.deepEqual(await contender.next(), { event: "error", code: "relay_state_locked" });
  await first.close();
  let ordinaryOpens = 0;
  const second = await f.open({ fs: { ...fs, async open(path, ...args) {
    if (path === guardPath) { ordinaryOpens++; assert.equal(args[0], "wx"); }
    return fs.open(path, ...args);
  } } });
  assert.equal(ordinaryOpens, 0, "only SQLite may open even an existing guard inode");
  await second.close();
  const after = await fs.lstat(guardPath, { bigint: true });
  assert.deepEqual([after.dev, after.ino, after.size, after.nlink], [before.dev, before.ino, before.size, 1n]);
  assert.deepEqual((await fs.readdir(dirname(guardPath))).sort(),
    [RELAY_CONTROLLER_GUARD_FILENAME, ".pinvou-controller-state-directory", "state.json", "state.json.bak"].sort());
});

test("controller guard never ordinarily opens its inode even on first creation", async t => {
  const f = await fixture(t), guardPath = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const store = await f.open({ fs: { ...fs, async open(target, ...args) {
    assert.notEqual(target, guardPath, "a creator fd must never later close over another worker's SQLite lock");
    return fs.open(target, ...args);
  } } });
  await store.compareAndSwap("guard_test", "0", { commits: 1 });
  await store.close();
  assert.equal((await f.open()).readNamespace("guard_test").value.commits, 1);
});

test("controller guard rejects malformed, linked and reserved guard identities without touching state", async t => {
  const f = await fixture(t);
  await (await f.open()).close();
  const guardPath = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const primary = await fs.readFile(f.statePath);
  const guard = await fs.readFile(guardPath); // No connection remains in this process.
  await fs.writeFile(guardPath, "not-empty");
  await assert.rejects(f.open(), { code: "relay_state_guard_unavailable" });
  assert.deepEqual(await fs.readFile(f.statePath), primary);
  assert.equal(await fs.readFile(guardPath, "utf8"), "not-empty");
  await fs.writeFile(guardPath, guard);
  const alias = join(f.directory, "guard-alias");
  await fs.link(guardPath, alias);
  await assert.rejects(f.open(), { code: "relay_state_guard_unsafe" });
  await fs.unlink(alias);
  await (await f.open()).close();
  assert.throws(() => new RelayStateStore({ ...f.options, statePath: guardPath }),
    { code: "relay_state_filename_reserved" });
});

test("controller guard missing builtin fails before filesystem preparation", () => {
  const module = new URL("../relay-state-store.js", import.meta.url).href;
  const code = `const {RelayStateStore}=await import(${JSON.stringify(module)});
    try {await RelayStateStore.open({statePath:'${process.platform === "win32" ? "C:/" : "/"}unopened-controller/state.json',
      legacyStatePath:'${process.platform === "win32" ? "C:/" : "/"}unopened-legacy.json',
      fs:new Proxy({}, {get(){throw Error('filesystem touched')}})});process.exit(2)}
    catch(e){if(e.code!=='relay_state_lock_runtime_unavailable')throw e}`;
  const result = spawnSync(process.execPath, ["--no-experimental-sqlite", "--input-type=module", "-e", code],
    { encoding: "utf8", windowsHide: true, timeout: 10000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
});

test("controller guard close failure retains resource until retry without deleting fixed file", async () => {
  let closes = 0, held = true;
  class Database {
    isOpen = true;
    isTransaction = false;
    exec() { this.isTransaction = true; }
    prepare(query) {
      const rows = {
        "PRAGMA application_id": { application_id: 0x50564c32 },
        "PRAGMA user_version": { user_version: 2 },
        "PRAGMA journal_mode": { journal_mode: "delete" },
        "PRAGMA page_count": { page_count: 1 },
        "PRAGMA page_size": { page_size: 4096 },
        "SELECT count(*) AS count FROM sqlite_schema": { count: 0 },
      };
      return { get: () => rows[query] };
    }
    close() { closes++; if (closes === 1) throw Error("close interrupted"); this.isOpen = false; held = false; }
  }
  const stat = { isFile: () => true, isSymbolicLink: () => false, size: 4096n, nlink: 1n, dev: 1n, ino: 2n };
  const guard = new RelayStateGuard({ path: "/fixture/guard", Database,
    fs: { open: async () => { throw Object.assign(Error("exists"), { code: "EEXIST" }); },
      lstat: async path => {
        if (path.endsWith("-journal")) throw Object.assign(Error("missing"), { code: "ENOENT" });
        return stat;
      } },
    protect: async () => {}, verify: async () => {}, assertAncestors: async () => {}, syncDirectory: async () => {},
    fail: code => { throw Object.assign(Error(code), { code }); } });
  await guard.acquire();
  await assert.rejects(guard.close(), /close interrupted/u);
  assert.equal(held, true);
  await guard.close();
  assert.equal(held, false);
  assert.equal(closes, 2);
});

test("controller guard native exit before protecting its new inode recovers through CAS and restart", async t => {
  const f = await fixture(t), child = f.child("native-exit-before-guard-protect");
  await expectBootstrapFault(child, "before-guard-protect");
  const path = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const before = await fs.lstat(path, { bigint: true });
  assert.equal(before.size, 0n);
  await assert.rejects(fs.stat(f.statePath), { code: "ENOENT" });
  await assert.rejects(fs.stat(`${path}-journal`), { code: "ENOENT" });
  if (process.platform === "win32") {
    const metadata = windowsCreatorMetadata(path);
    t.diagnostic(`windows_guard_empty_creator ${JSON.stringify(metadata)}`);
    assert.equal(metadata.matches_creator_default, true);
    assert.notEqual(metadata.token_owner, "other");
  }
  const native = { protectPath: undefined, verifyPath: undefined, verifyAncestors: undefined };
  const restored = await f.open(native);
  await restored.compareAndSwap("guard_test", "0", { commits: 1 });
  await restored.close();
  const after = await fs.lstat(path, { bigint: true });
  assert.deepEqual([after.dev, after.ino], [before.dev, before.ino]);
  if (process.platform === "win32") {
    const metadata = windowsCreatorMetadata(path);
    assert.equal(metadata.file_owner, "current");
    assert.equal(metadata.protected, true);
  }
  const restarted = await f.open(native);
  assert.equal(restarted.readNamespace("guard_test").value.commits, 1);
  await restarted.close();
});

// This Unix metadata contract is collected by Linux's full relay suite. The
// native Windows tools gate requires every collected case to execute, without
// substituting a skipped Unix case for Windows ACL qualification.
if (process.platform !== "win32") test("controller guard bootstrap recovery only removes default POSIX read bits", async t => {
  for (const mode of [0o644, 0o640, 0o604, 0o620, 0o704]) {
    const f = await fixture(t);
    await (await f.open()).close();
    const path = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
    const state = await fs.readFile(f.statePath);
    await fs.writeFile(path, ""); // No SQLite connection remains in this process.
    await fs.chmod(path, mode);
    if ([0o644, 0o640, 0o604].includes(mode)) {
      const store = await f.open();
      await store.close();
      assert.equal((await fs.stat(path)).mode & 0o7777, 0o600);
    } else {
      await assert.rejects(f.open(), { code: "relay_state_permissions_unsafe" });
      assert.equal((await fs.stat(path)).mode & 0o7777, mode);
      assert.equal((await fs.stat(path)).size, 0);
    }
    assert.deepEqual(await fs.readFile(f.statePath), state);
  }
});

test("controller guard refuses changed identity before bootstrap permission repair", async () => {
  let stats = 0, protects = 0, bootstrapChecks = 0, verifies = 0;
  const metadata = ino => ({ isFile: () => true, isSymbolicLink: () => false,
    size: 0n, nlink: 1n, dev: 1n, ino });
  class Database {
    isOpen = true;
    close() { this.isOpen = false; }
  }
  const guard = new RelayStateGuard({ path: "/fixture/guard", Database,
    fs: { lstat: async path => {
      if (path.endsWith("-journal")) throw Object.assign(Error("missing"), { code: "ENOENT" });
      return metadata(++stats <= 2 ? 1n : 2n);
    } }, protect: async () => { protects++; },
    verify: async () => {
      if (++verifies > 1) throw Object.assign(Error("permissions"), { code: "relay_state_permissions_unsafe" });
    }, verifyBootstrap: async () => { bootstrapChecks++; },
    assertAncestors: async () => {}, syncDirectory: async () => {},
    fail: code => { throw Object.assign(Error(code), { code }); },
  });
  try { await assert.rejects(guard.acquire(), { code: "relay_state_guard_identity_changed" }); }
  finally { await guard.close(); }
  assert.equal(protects, 0);
  assert.equal(bootstrapChecks, 0);
  assert.equal(verifies, 1);
});

test("controller guard bootstrap recovery never tightens a nonempty unprotected protocol", async t => {
  const f = await fixture(t), child = f.child("native-exit-before-guard-protect");
  await expectBootstrapFault(child, "before-guard-protect");
  const path = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const database = new DatabaseSync(path);
  database.exec("PRAGMA application_id=7; PRAGMA user_version=99;");
  database.close();
  if (process.platform !== "win32") await fs.chmod(path, 0o644);
  const bytes = await fs.readFile(path), before = await fs.lstat(path, { bigint: true });
  const native = { protectPath: undefined, verifyPath: undefined, verifyAncestors: undefined };
  const code = process.platform === "win32" ? "relay_state_acl_unverified" : "relay_state_permissions_unsafe";
  await assert.rejects(f.open(native), { code });
  await assert.rejects(f.open(native), { code }); // Refusal is persistent, not an implicit repair.
  assert.deepEqual(await fs.readFile(path), bytes);
  const after = await fs.lstat(path, { bigint: true });
  assert.deepEqual([after.dev, after.ino, after.mode], [before.dev, before.ino, before.mode]);
  await assert.rejects(fs.stat(f.statePath), { code: "ENOENT" });
});

for (const phase of ["before", "after"]) {
  test(`controller guard native bootstrap exit ${phase} seed commit recovers before first JSON access`, async t => {
    const f = await fixture(t), child = f.child(`native-exit-${phase}-seed-commit`);
    await expectBootstrapFault(child, `${phase}-seed-commit`);
    await assert.rejects(fs.stat(f.statePath), { code: "ENOENT" });
    const guardPath = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
    const identity = await fs.lstat(guardPath, { bigint: true });
    if (phase === "before") {
      assert.equal(identity.size, 0n);
      assert.equal((await fs.stat(`${guardPath}-journal`)).size, 512);
      if (process.platform === "win32") {
        const metadata = windowsCreatorMetadata(`${guardPath}-journal`);
        t.diagnostic(`windows_guard_journal_creator ${JSON.stringify(metadata)}`);
        assert.equal(metadata.matches_creator_default, true);
        assert.notEqual(metadata.token_owner, "other");
      }
    } else {
      assert.ok(identity.size > 0n);
      await assert.rejects(fs.stat(`${guardPath}-journal`), { code: "ENOENT" });
    }
    // Restore through the real default permission adapters, including the
    // SQLite-created journal's inherited Windows ACL. No manual file cleanup.
    const native = { protectPath: undefined, verifyPath: undefined, verifyAncestors: undefined };
    const restored = await f.open(native);
    assert.equal(restored.readNamespace("guard_test").store_revision, "0");
    await restored.compareAndSwap("guard_test", "0", { commits: 1 });
    await restored.close();
    const after = await fs.lstat(guardPath, { bigint: true });
    assert.deepEqual([after.dev, after.ino], [identity.dev, identity.ino]);
    await assert.rejects(fs.stat(`${guardPath}-journal`), { code: "ENOENT" });
    const restarted = await f.open(native);
    assert.equal(restarted.readNamespace("guard_test").value.commits, 1);
    await restarted.close();
  });
}

test("controller guard fault qualification rejects an early open failure", async t => {
  const f = await fixture(t), child = f.child("native-fail-before-seed-commit");
  await assert.rejects(expectBootstrapFault(child, "before-seed-commit"), {
    code: "ERR_ASSERTION", actual: 1, expected: 82,
  });
  assert.equal((await child.next()).event, "error");
  await assert.rejects(fs.stat(f.statePath), { code: "ENOENT" });
});

test("controller guard refuses unknown protocol and unrelated journal without changing JSON", async t => {
  const f = await fixture(t);
  await (await f.open()).close();
  const guardPath = join(dirname(f.statePath), RELAY_CONTROLLER_GUARD_FILENAME);
  const db = new DatabaseSync(guardPath);
  db.exec("PRAGMA user_version=99"); db.close();
  const before = await fs.readFile(guardPath), primary = await fs.readFile(f.statePath);
  await assert.rejects(f.open(), { code: "relay_state_guard_unsupported" });
  assert.deepEqual(await fs.readFile(guardPath), before);
  assert.deepEqual(await fs.readFile(f.statePath), primary);
  await fs.writeFile(join(dirname(f.statePath), "foreign-journal"), "foreign-preserved");
  await assert.rejects(f.open(), { code: "relay_state_directory_not_dedicated" });
  assert.equal(await fs.readFile(join(dirname(f.statePath), "foreign-journal"), "utf8"), "foreign-preserved");
});

test("controller guard retained after abandoned open before old owner publication retries cleanup", async t => {
  const f = await fixture(t), original = DatabaseSync.prototype.close;
  let closes = 0;
  DatabaseSync.prototype.close = function() {
    if (++closes === 1) throw Error("guard close interrupted before release");
    return original.call(this);
  };
  try {
    await assert.rejects(f.open({ fs: { ...fs, async link() { throw Object.assign(Error("fixture publish failure"), { code: "EIO" }); } } }),
      error => error.code === "relay_state_unavailable" && error.releaseError?.message === "guard close interrupted before release");
    const contender = f.child();
    assert.deepEqual(await contender.next(), { event: "error", code: "relay_state_locked" });
    const recovered = await f.open();
    assert.ok(closes >= 2);
    await recovered.compareAndSwap("guard_test", "0", { commits: 1 });
    await recovered.close();
    assert.equal((await f.open()).readNamespace("guard_test").value.commits, 1);
  } finally { DatabaseSync.prototype.close = original; }
});

test("controller guard late close after actual OS unlock cannot remove a successor's old owner", async t => {
  const f = await fixture(t), owner = await f.open(), original = DatabaseSync.prototype.close;
  let interrupted = false;
  DatabaseSync.prototype.close = function() {
    const result = original.call(this);
    if (!interrupted) { interrupted = true; throw Error("guard close interrupted after release"); }
    return result;
  };
  try {
    await assert.rejects(owner.close(), /guard close interrupted after release/u);
    const successor = f.child();
    assert.equal((await successor.next()).event, "ready");
    await owner.close();
    successor.send("commit");
    assert.equal((await successor.next()).snapshot.store_revision, "1");
    successor.send("close");
    assert.equal((await successor.next()).event, "closed");
    assert.equal((await f.open()).readNamespace("guard_test").value.commits, 1);
  } finally { DatabaseSync.prototype.close = original; }
});
