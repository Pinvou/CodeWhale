import { readFileSync } from 'node:fs';

// A Windows cancel/claim rename may deny opening the old filename before it
// disappears. It is not an acknowledged request and must not be dispatched.
export function readPendingHostRequest(file, read = readFileSync) {
  try {
    return read(file, 'utf8');
  } catch (error) {
    if (['ENOENT', 'EPERM', 'EBUSY'].includes(error?.code)) return null;
    throw error;
  }
}

// Observe close from spawn time: an exit signal, or a resolved stop callback,
// does not prove that the process and its stdio have actually closed.
export function confirmedChildStopper(child, stop, { graceMs = 5_000, closeMs = 2_000 } = {}) {
  let closed = false;
  let resolveClosed;
  const closing = new Promise(resolve => { resolveClosed = resolve; });
  child.once('close', () => { closed = true; resolveClosed(); });
  return async () => {
    if (closed) return;
    await stop(child, graceMs);
    let timer;
    try {
      await Promise.race([
        closing,
        new Promise((resolve, reject) => {
          timer = setTimeout(() => reject(new Error('owned browser wrapper close was not confirmed')), closeMs);
        }),
      ]);
    } finally {
      clearTimeout(timer);
    }
  };
}

export function confirmedProcessExited(pid, probe = process.kill) {
  try {
    probe(pid, 0);
    return false;
  } catch (error) {
    if (error?.code === 'ESRCH') return true;
    throw error;
  }
}

// closeHost must first stop its poller, then may report its recorded failure.
// Preserve that failure while still closing every owned child and server.
export async function cleanupBrowserFixture({ closeHost, stopChildren, closeServer, removeFiles }) {
  const errors = [];
  const attempt = async cleanup => {
    try { await cleanup(); return true; }
    catch (error) { errors.push(error); return false; }
  };
  await attempt(closeHost);
  let childrenStopped = true;
  for (const stop of stopChildren) {
    if (!await attempt(stop)) childrenStopped = false;
  }
  const serverClosed = await attempt(closeServer);
  if (childrenStopped && serverClosed) await attempt(removeFiles);
  if (errors.length) throw new AggregateError(errors, 'browser fixture cleanup failed');
}
