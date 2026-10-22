import fs from 'node:fs';
import path from 'node:path';

export async function cleanupUserCodeLookupFixture({ browser, vite, profile, tmpRoot, remove = fs.rmSync }) {
  const errors = [];
  for (const resource of [browser, vite]) {
    try { if (resource) await resource.close(); } catch (error) { errors.push(error); }
  }
  if (path.dirname(profile) !== path.resolve(tmpRoot) || !path.basename(profile).startsWith('pinvou-code-lookup-')) {
    errors.push(new Error('unowned browser profile cleanup refused'));
  } else {
    try { remove(profile, { recursive: true, force: true }); } catch (error) { errors.push(error); }
  }
  return errors;
}

// Called after finally: cleanup failures cannot hide the original UI assertion.
export function finishUserCodeLookupFixture({ failed, primaryFailure, cleanupErrors, report = console.error }) {
  if (failed) {
    if (cleanupErrors.length) report(new AggregateError(cleanupErrors, 'user-code fixture cleanup failed'));
    throw primaryFailure;
  }
  if (cleanupErrors.length) throw new AggregateError(cleanupErrors, 'user-code fixture cleanup failed');
}
