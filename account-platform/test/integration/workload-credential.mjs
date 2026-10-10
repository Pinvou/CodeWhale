import { openSync, closeSync, fstatSync, readFileSync } from 'node:fs';
import path from 'node:path';

// The real services verify issuer, audience and permissions. This runner only
// reads an operator-provided, short-lived credential; it never creates an issuer
// or treats a syntactically valid JWT as authenticated evidence.
export function readTestWorkloadCredential(environment = process.env) {
  const file = environment.PINVOU_TEST_WORKLOAD_CREDENTIAL_FILE;
  if (typeof file !== 'string' || !path.isAbsolute(file)) throw new Error('TEST_WORKLOAD_CREDENTIAL_FILE_REQUIRED');
  let fd;
  try {
    fd = openSync(file, 'r');
    const stat = fstatSync(fd);
    if (!stat.isFile() || stat.size < 1 || stat.size > 64 * 1024) throw new Error('invalid file');
    const token = readFileSync(fd, 'utf8').trim();
    if (!/^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$/.test(token)) throw new Error('invalid credential');
    return token;
  } catch {
    throw new Error('TEST_WORKLOAD_CREDENTIAL_FILE_INVALID');
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
}
