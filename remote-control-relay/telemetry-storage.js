// Private synchronous storage shared by telemetry and its durable retention authority.
import crypto from 'node:crypto';
import { chmodSync, closeSync, existsSync, lstatSync, openSync, renameSync, rmSync, writeFileSync } from 'node:fs';

// Online startup and offline reconciliation must agree on whether an inventory
// is trustworthy before either is allowed to remove durable subject facts.
export function validDeviceInventory(saved) {
  if (!saved || typeof saved !== 'object' || Array.isArray(saved) || !Array.isArray(saved.devices)) return false;
  const ids = new Set(), hardwareHashes = new Set();
  return saved.devices.every(device => {
    if (!device || typeof device !== 'object' || Array.isArray(device)
      || ![device.device_id, device.hardware_hash, device.device_token].every(value => typeof value === 'string' && value.length > 0)
      || ids.has(device.device_id) || hardwareHashes.has(device.hardware_hash)) return false;
    ids.add(device.device_id); hardwareHashes.add(device.hardware_hash);
    return true;
  });
}

export function enforcePrivateFile(path) {
  const metadata = lstatSync(path);
  if (metadata.isSymbolicLink() || !metadata.isFile()) throw new Error('unsafe telemetry storage file');
  chmodSync(path, 0o600);
}

export function atomicWritePrivateFile(path, content) {
  if (existsSync(path)) enforcePrivateFile(path);
  const temporary = `${path}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`;
  let descriptor;
  let created = false;
  let failure;
  try {
    descriptor = openSync(temporary, 'wx', 0o600);
    created = true;
    writeFileSync(descriptor, content);
    closeSync(descriptor);
    descriptor = undefined;
    renameSync(temporary, path);
    enforcePrivateFile(path);
  } catch (error) {
    failure = error;
    throw error;
  } finally {
    try {
      if (descriptor !== undefined) closeSync(descriptor);
    } catch (error) {
      // Some close failures have already closed the fd. Cleanup's EBADF must
      // not replace the original write/close error that explains the failure.
      if (!failure) throw error;
    } finally {
      // Remove only the temporary file this invocation successfully created.
      try { if (created) rmSync(temporary, { force:true }); }
      catch (error) { if (!failure) throw error; }
    }
  }
}

export function atomicWriteJson(path, value) {
  atomicWritePrivateFile(path, `${JSON.stringify(value, null, 2)}\n`);
}

export function atomicWriteJsonLines(path, values) {
  atomicWritePrivateFile(path, values.length ? `${values.map(value => JSON.stringify(value)).join('\n')}\n` : '');
}
