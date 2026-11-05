import { realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

// Node may resolve the module URL while argv still names a linked release.
// A different entry belongs to an importer; importing must not run this CLI.
export function isMainModule(moduleURL, entryPath = process.argv[1]) {
  if (!entryPath) return false;
  try {
    return realpathSync(entryPath) === realpathSync(fileURLToPath(moduleURL));
  } catch {
    return false;
  }
}
