export function assertTestSummary(output) {
  const counts = {};
  for (const name of ['tests', 'pass', 'fail', 'cancelled', 'skipped', 'todo']) {
    const matches = [...output.matchAll(new RegExp(`^# ${name} (\\d+)\\s*$`, 'gm'))];
    if (matches.length !== 1) throw new Error(`Missing or ambiguous TAP summary: ${name}`);
    counts[name] = Number(matches[0][1]);
  }
  if (counts.tests < 1 || counts.pass !== counts.tests
    || counts.fail || counts.cancelled || counts.skipped || counts.todo) {
    throw new Error('Windows relay tests must execute, pass, and never skip');
  }
}
