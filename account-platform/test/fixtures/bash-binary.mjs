import { existsSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const probeMarker = 'pinvou-controlled-bash';

// Test infrastructure only. Probe the executable: a Windows WSL launcher may
// exist while Bash cannot start, and must not count as a usable shell.
export function findTestBash({
  platform = process.platform,
  env = process.env,
  exists = existsSync,
  run = spawnSync,
} = {}) {
  const usable = binary => {
    const result = run(binary, ['-c', `test -n "$BASH_VERSION" && printf '%s' '${probeMarker}'`], {
      env, encoding: 'utf8', timeout: 15_000,
    });
    return !result.error && result.status === 0 && result.stdout === probeMarker;
  };
  if (Object.hasOwn(env, 'PINVOU_TEST_BASH_BINARY')) {
    const binary = env.PINVOU_TEST_BASH_BINARY.trim();
    if (!binary || !usable(binary)) throw new Error('PINVOU_TEST_BASH_BINARY must name a runnable Bash executable');
    return binary;
  }
  const candidates = platform === 'win32'
    ? ['C:/Program Files/Git/bin/bash.exe', 'C:/Program Files/Git/usr/bin/bash.exe'].filter(exists).concat('bash')
    : ['/bin/bash'];
  for (const binary of candidates) if (usable(binary)) return binary;
  throw new Error('Bash is required for the controlled interpolation contract; set PINVOU_TEST_BASH_BINARY to a runnable Bash executable');
}
