// Private synchronous storage shared by telemetry and its durable retention authority.
import crypto from 'node:crypto';
import { chmodSync, closeSync, existsSync, lstatSync, openSync, renameSync, rmSync, writeFileSync } from 'node:fs';

export function enforcePrivateFile(path) {
  const metadata = lstatSync(path);
  if (metadata.isSymbolicLink() || !metadata.isFile()) throw new Error('unsafe telemetry storage file');
  chmodSync(path, 0o600);
}

function atomicWritePrivateFile(path, content) {
  if (existsSync(path)) enforcePrivateFile(path);
  const temporary = `${path}.${process.pid}.${crypto.randomBytes(6).toString('hex')}.tmp`;
  let descriptor;
  let created = false;
  try {
    descriptor = openSync(temporary, 'wx', 0o600);
    created = true;
    writeFileSync(descriptor, content);
    closeSync(descriptor);
    descriptor = undefined;
    renameSync(temporary, path);
    enforcePrivateFile(path);
  } finally {
    try {
      if (descriptor !== undefined) closeSync(descriptor);
    } finally {
      // Remove only the temporary file this invocation successfully created.
      if (created) rmSync(temporary, { force:true });
    }
  }
}

export function atomicWriteJson(path, value) {
  atomicWritePrivateFile(path, `${JSON.stringify(value, null, 2)}\n`);
}

export function atomicWriteJsonLines(path, values) {
  atomicWritePrivateFile(path, values.length ? `${values.map(value => JSON.stringify(value)).join('\n')}\n` : '');
}
