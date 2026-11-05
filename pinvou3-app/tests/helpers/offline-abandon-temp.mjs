import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

export function offlineAbandonTempBase(platform, inheritedTemp) {
  // Chrome appends its singleton directory/socket beneath TMPDIR on Linux.
  return platform === 'linux' ? '/tmp' : inheritedTemp;
}

export function createOfflineAbandonTempRoot() {
  return fs.mkdtempSync(path.join(offlineAbandonTempBase(process.platform, os.tmpdir()), 'pva-'));
}
