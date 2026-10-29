import { dirname } from "node:path";

// A fixed SQLite format header is an OS-backed mutex, never an authorization DB.
// Never read/copy/hash this inode with ordinary file IO: on POSIX a close in
// another thread can release this process's SQLite locks. Metadata is sufficient
// and SQLite APIs verify its format, including for unsuccessful contenders.
export const RELAY_CONTROLLER_GUARD_FILENAME = ".pinvou-controller-state.guard";
export const RELAY_CONTROLLER_GUARD_JOURNAL = `${RELAY_CONTROLLER_GUARD_FILENAME}-journal`;
export const RELAY_CONTROLLER_LOCK_PROTOCOL = 2;
const APPLICATION_ID = 0x50564c32; // PVL2; no business tables or authorization data.
const MAX_GUARD_BYTES = 64 * 1024;

export async function requireControllerLockRuntime() {
  if (Number(process.versions.node.split(".")[0]) < 24) {
    const error = new Error("relay_state_lock_runtime_unavailable");
    error.code = error.message;
    throw error;
  }
  try {
    const { DatabaseSync } = await import("node:sqlite");
    return DatabaseSync;
  } catch (cause) {
    const error = new Error("relay_state_lock_runtime_unavailable", { cause });
    error.code = error.message;
    throw error;
  }
}

export class RelayStateGuard {
  #path;
  #fs;
  #protect;
  #verify;
  #verifyJournal;
  #verifyBootstrap;
  #assertAncestors;
  #syncDirectory;
  #fail;
  #Database;
  #database = null;
  #identity = null;
  #readySize = null;

  constructor({ path, fs, protect, verify, verifyJournal = verify, verifyBootstrap, assertAncestors, syncDirectory, fail, Database }) {
    this.#path = path;
    this.#fs = fs;
    this.#protect = protect;
    this.#verify = verify;
    this.#verifyJournal = verifyJournal;
    this.#verifyBootstrap = verifyBootstrap;
    this.#assertAncestors = assertAncestors;
    this.#syncDirectory = syncDirectory;
    this.#fail = fail;
    this.#Database = Database;
  }

  async #metadata({ finishBootstrap = false } = {}) {
    await this.#assertAncestors();
    const stat = await this.#fs.lstat(this.#path, { bigint: true });
    if (!stat.isFile() || stat.isSymbolicLink() || stat.size > BigInt(MAX_GUARD_BYTES) || stat.nlink !== 1n
      || typeof stat.dev !== "bigint" || typeof stat.ino !== "bigint"
      || (stat.dev === 0n && stat.ino === 0n)) this.#fail("relay_state_guard_unsafe");
    if (this.#readySize !== null && stat.size !== BigInt(this.#readySize)) {
      this.#fail("relay_state_guard_unsafe");
    }
    if (this.#identity && (this.#identity.dev !== stat.dev || this.#identity.ino !== stat.ino)) {
      this.#fail("relay_state_guard_identity_changed");
    }
    try { await this.#verify(this.#path, "file"); }
    catch (error) {
      // SQLite may have created the empty inode just before an opener exited,
      // leaving its safe inherited ACL/default read bits not yet tightened.
      // Only this no-journal, owned empty bootstrap may finish protection.
      if (!finishBootstrap || !this.#verifyBootstrap || stat.size !== 0n
        || !["relay_state_permissions_unsafe", "relay_state_acl_unverified"].includes(error?.code)
        || await this.#journalMetadata()) throw error;
      await this.#verifyBootstrap(this.#path, stat);
      const current = await this.#fs.lstat(this.#path, { bigint: true });
      if (!current.isFile() || current.isSymbolicLink() || current.nlink !== 1n || current.size !== 0n
        || current.dev !== stat.dev || current.ino !== stat.ino) this.#fail("relay_state_guard_identity_changed");
      this.#identity ??= { dev: stat.dev, ino: stat.ino };
      await this.#protect(this.#path, "file");
      return this.#metadata(); // Recheck identity, permissions and ancestors.
    }
    if (this.#identity && (this.#identity.dev !== stat.dev || this.#identity.ino !== stat.ino)) {
      this.#fail("relay_state_guard_identity_changed");
    }
    return stat;
  }

  async #journalMetadata() {
    const path = `${this.#path}-journal`;
    let stat;
    try { stat = await this.#fs.lstat(path, { bigint: true }); }
    catch (error) { if (error?.code === "ENOENT") return false; throw error; }
    if (!stat.isFile() || stat.isSymbolicLink() || stat.nlink !== 1n || stat.size > BigInt(MAX_GUARD_BYTES)) {
      this.#fail("relay_state_guard_unsafe");
    }
    // SQLite owns this one exact rollback sidecar. Its native inherited ACL is
    // allowed only when it grants no principals outside the private directory.
    await this.#verifyJournal(path, "file");
    return true;
  }

  #format() {
    const get = query => this.#database.prepare(query).get();
    return {
      app: get("PRAGMA application_id").application_id,
      version: get("PRAGMA user_version").user_version,
      mode: get("PRAGMA journal_mode").journal_mode,
      pages: get("PRAGMA page_count").page_count,
      pageSize: get("PRAGMA page_size").page_size,
      schemaRows: get("SELECT count(*) AS count FROM sqlite_schema").count,
    };
  }

  #assertFormat(format) {
    if (format.app !== APPLICATION_ID || format.version !== RELAY_CONTROLLER_LOCK_PROTOCOL
      || format.mode !== "delete" || format.pages !== 1 || format.schemaRows !== 0
      || !Number.isInteger(format.pageSize) || format.pageSize < 512 || format.pageSize > MAX_GUARD_BYTES
      || (format.pageSize & (format.pageSize - 1)) !== 0) {
      this.#fail("relay_state_guard_unsupported");
    }
  }

  async acquire() {
    let exists = true;
    try { await this.#fs.lstat(this.#path, { bigint: true }); }
    catch (error) { if (error?.code === "ENOENT") exists = false; else throw error; }
    if (!exists && await this.#journalMetadata()) this.#fail("relay_state_guard_unsafe");
    if (exists) {
      const before = await this.#metadata({ finishBootstrap: true });
      this.#identity = { dev: before.dev, ino: before.ino };
      await this.#journalMetadata();
    } else {
      await this.#assertAncestors();
    }
    try {
      // SQLite alone opens/creates this inode, even during bootstrap. A creator's
      // ordinary wx fd could otherwise close after another worker acquired a
      // SQLite lock, releasing that worker's process-wide POSIX locks.
      // The private parent protects it from birth. Check the current object,
      // rather than treating an earlier missing-path observation as permission
      // to protect an object another initializer may already have completed.
      this.#database = new this.#Database(this.#path, { timeout: 0, allowExtension: false });
      const opened = await this.#metadata({ finishBootstrap: true });
      this.#identity ??= { dev: opened.dev, ino: opened.ino };
      if (!exists) await this.#syncDirectory(dirname(this.#path));
      await this.#journalMetadata();
      this.#database.exec("PRAGMA synchronous=FULL");
      this.#database.exec("BEGIN EXCLUSIVE");
      const format = this.#format();
      const active = await this.#metadata();
      if (active.size === 0n && format.app === 0 && format.version === 0 && format.mode === "delete"
        && format.pages <= 1 && format.schemaRows === 0) {
        // SQLite may recover/discard its own exact bootstrap rollback journal.
        // Commit only this non-authorizing header, then reacquire the lifetime
        // mutex before inspecting the old owner or any JSON authorization state.
        this.#database.exec(`PRAGMA application_id=${APPLICATION_ID}; PRAGMA user_version=${RELAY_CONTROLLER_LOCK_PROTOCOL}; COMMIT; BEGIN EXCLUSIVE;`);
      }
      const readyFormat = this.#format();
      this.#assertFormat(readyFormat);
      this.#readySize = readyFormat.pageSize;
    } catch (error) {
      if (typeof error?.code === "string" && error.code.startsWith("relay_state_")) throw error;
      if (error?.errcode === 5 || error?.errcode === 6) this.#fail("relay_state_locked", error);
      this.#fail("relay_state_guard_unavailable", error);
    }
    await this.#metadata();
    if (await this.#journalMetadata()) this.#fail("relay_state_guard_unexpected_journal");
  }

  async assertIdentity() {
    if (!this.#database?.isOpen || !this.#database.isTransaction) {
      this.#fail("relay_state_guard_unavailable");
    }
    await this.#metadata();
    if (await this.#journalMetadata()) this.#fail("relay_state_guard_unexpected_journal");
  }

  async close() {
    // close ends the transaction without a separate unlock/close window. If a
    // close throws after completing, retries must not touch an old JSON owner.
    if (this.#database) {
      try {
        this.#database.close();
      } finally {
        if (!this.#database.isOpen) this.#database = null;
      }
    }
    // The guard remains in place, even on failed open. Never unlink it.
  }
}
