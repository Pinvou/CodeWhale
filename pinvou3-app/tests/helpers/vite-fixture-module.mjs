import path from 'node:path';

const normalize = (value) => path.posix.normalize(value.split('?', 1)[0].replaceAll('\\', '/'));

/**
 * Compare Vite's forward-slash module IDs with native fixture filenames.
 */
export function matchesViteModule(moduleId, filename) {
  return normalize(moduleId) === normalize(filename);
}
