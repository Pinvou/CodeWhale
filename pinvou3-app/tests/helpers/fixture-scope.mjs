import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { randomUUID } from 'node:crypto';

// Test resources only. Download/build caches never belong to this scope.
export class FixtureScope {
  #resources = [];
  #closing;
  constructor({ ledger } = {}) {
    this.ledger = ledger;
  }
  register({ kind, id = randomUUID(), close, verify = async () => true, remove = async () => {} }) {
    if (this.#closing) throw new Error('fixture scope is already closing');
    if (typeof id !== 'string' || typeof kind !== 'string' || !/^[a-zA-Z0-9_-]{1,80}$/.test(id) || !/^[a-z][a-z0-9-]{0,40}$/.test(kind) || typeof close !== 'function') {
      throw new Error('invalid fixture resource registration');
    }
    const resource = { schema: 1, kind, id, state: 'registered', close, verify, remove };
    const record = () => {
      if (!this.ledger) return;
      const directory = fs.lstatSync(this.ledger);
      if (!directory.isDirectory() || directory.isSymbolicLink()) throw new Error('fixture ledger is not an owned directory');
      const target = path.join(this.ledger, `${id}.json`);
      const temporary = `${target}.${randomUUID()}.tmp`;
      fs.writeFileSync(temporary, JSON.stringify({ schema: 1, kind, id, state: resource.state, pid: process.pid }), { flag: 'wx' });
      fs.renameSync(temporary, target);
    };
    record();
    let closing;
    let removing;
    const closeOnly = () => closing ||= (async () => {
      try {
        let timer;
        try {
          await Promise.race([Promise.resolve().then(close), new Promise((resolve, reject) => {
            timer = setTimeout(() => reject(new Error(`fixture ${kind} close deadline exceeded`)), 10_000);
          })]);
        } finally { clearTimeout(timer); }
        if (!await verify()) throw new Error(`fixture ${kind} shutdown was not confirmed`);
        resource.state = 'closed';
        record();
      } catch (error) {
        resource.state = 'unconfirmed';
        try { record(); } catch (receiptError) {
          throw new AggregateError([error, receiptError], 'fixture close and receipt recording failed', { cause: receiptError });
        }
        throw error;
      }
    })();
    const destroy = () => removing ||= (async () => {
      await closeOnly();
      try {
        await remove();
        resource.state = 'released';
        record();
      } catch (error) {
        resource.state = 'unconfirmed';
        try { record(); } catch (receiptError) {
          throw new AggregateError([error, receiptError], 'fixture removal and receipt recording failed', { cause: receiptError });
        }
        throw error;
      }
    })();
    const release = async () => { await closeOnly(); await destroy(); };
    this.#resources.push({ closeOnly, destroy });
    return release;
  }
  close() {
    return this.#closing ||= (async () => {
      const failures = [];
      for (const resource of [...this.#resources].reverse()) {
        try { await resource.closeOnly(); } catch (error) {
          // A later resource can stall until the signal watchdog exits. Keep
          // this failure visible now, before the final aggregate is available.
          console.error(error);
          failures.push(error);
        }
      }
      if (failures.length) throw new AggregateError(failures, 'fixture scope did not become clean');
      for (const resource of [...this.#resources].reverse()) {
        try { await resource.destroy(); } catch (error) {
          console.error(error);
          failures.push(error);
        }
      }
      if (failures.length) throw new AggregateError(failures, 'fixture files could not be retired');
    })();
  }
}

export const fixtureScope = new FixtureScope({ ledger: process.env.PINVOU_CI_RESOURCE_LEDGER });

export function ownedDirectory(label) {
  const base = process.env.PINVOU_CI_FIXTURE_ROOT || os.tmpdir();
  const root = fs.mkdtempSync(path.join(base, `pinvou-${label}-`));
  const identity = fs.lstatSync(root, { bigint: true });
  return {
    path: root,
    remove() {
      const current = fs.lstatSync(root, { bigint: true });
      if (!current.isDirectory() || current.isSymbolicLink() || current.dev !== identity.dev || current.ino !== identity.ino) {
        throw new Error('owned fixture directory identity changed');
      }
      fs.rmSync(root, { recursive: true });
    },
  };
}

// The supervisor handles killed processes. These hooks cover cooperative
// cancellation and ordinary completion without replacing the primary failure.
let stopping;
let signalDeadline;
// Vite's own SIGTERM callback awaits server.close() before calling process.exit.
// During shared shutdown it must join the whole scope, not only its own lease.
export function pendingFixtureShutdown() { return stopping; }
async function stop(signal) {
  if (signal && !signalDeadline) {
    signalDeadline = setTimeout(() => {
      console.error('fixture signal cleanup deadline exceeded; unfinished resource receipts retained');
      process.exit(process.exitCode || 1);
    }, 30_000);
  }
  stopping ||= fixtureScope.close().catch(error => {
    console.error(error);
    process.exitCode = process.exitCode || 1;
  });
  await stopping;
  if (signal) {
    clearTimeout(signalDeadline);
    // exitCode alone cannot stop a listener left alive by a failed close.
    // A nonzero primary failure and unfinished receipts remain authoritative;
    // only the owning supervisor may retire any surviving child processes.
    process.exit(process.exitCode || (signal === 'SIGINT' ? 130 : 143));
  }
}
process.once('beforeExit', () => stop());
process.once('SIGINT', () => stop('SIGINT'));
process.once('SIGTERM', () => stop('SIGTERM'));
