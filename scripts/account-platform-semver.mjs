// SemVer 2.0.0 precedence: https://semver.org/spec/v2.0.0.html.
// Keep decimal identifiers as strings: their size is not limited by Number.
const syntax = /^([0-9]+)\.([0-9]+)\.([0-9]+)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/;
const numeric = /^[0-9]+$/;

export function parseSemVer(value) {
  const match = typeof value === 'string' ? syntax.exec(value) : null;
  // JS's $ may match before a final newline; require the entire original input.
  if (!match || match[0] !== value) throw new Error('release_gate_version_invalid');
  const core = match.slice(1, 4);
  const prerelease = match[4] === undefined ? [] : match[4].split('.');
  if (core.some(part => part.length > 1 && part[0] === '0')
    || prerelease.some(part => numeric.test(part) && part.length > 1 && part[0] === '0')) {
    throw new Error('release_gate_version_invalid');
  }
  return { raw: value, core, prerelease, build: match[5] === undefined ? [] : match[5].split('.') };
}

function compareDecimal(left, right) {
  if (left.length !== right.length) return left.length < right.length ? -1 : 1;
  return left === right ? 0 : left < right ? -1 : 1;
}

export function compareSemVer(left, right) {
  const a = parseSemVer(left);
  const b = parseSemVer(right);
  for (let index = 0; index < 3; index++) {
    const result = compareDecimal(a.core[index], b.core[index]);
    if (result !== 0) return result;
  }
  if (a.prerelease.length === 0 || b.prerelease.length === 0) {
    return a.prerelease.length === b.prerelease.length ? 0 : a.prerelease.length === 0 ? 1 : -1;
  }
  const common = Math.min(a.prerelease.length, b.prerelease.length);
  for (let index = 0; index < common; index++) {
    const x = a.prerelease[index];
    const y = b.prerelease[index];
    if (x === y) continue;
    const xNumeric = numeric.test(x);
    const yNumeric = numeric.test(y);
    if (xNumeric !== yNumeric) return xNumeric ? -1 : 1;
    if (xNumeric) return compareDecimal(x, y);
    return x < y ? -1 : 1;
  }
  return a.prerelease.length === b.prerelease.length ? 0 : a.prerelease.length < b.prerelease.length ? -1 : 1;
}
