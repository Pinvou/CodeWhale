import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import postcss from 'postcss';
import tailwindcss from 'tailwindcss';

import { runAudit } from '../scripts/audit-compat.mjs';
import { staticRuntimeScripts } from '../vite.config.mjs';

// WebView compatibility contract: the desktop minimum is macOS 11
// (WKWebView = Safari 14.0). scripts/audit-compat.mjs parses every
// verbatim-copied runtime script and HTML inline script at the Safari 14
// syntax ceiling and flags post-14 runtime APIs (guarded calls may carry a
// `safari14-ok` line marker). The dist layer is audited by `npm run
// audit:compat` after a build; this test pins the always-available layers.
const testRoot = path.dirname(fileURLToPath(import.meta.url));
const appRoot = path.resolve(testRoot, '..');
const readSrc = (...parts) => fs.readFileSync(path.join(appRoot, 'src', ...parts), 'utf8');
const { browserslistToTargets, transform: transformCss } = createRequire(import.meta.url)('lightningcss');

test('static runtime scripts and HTML inline scripts stay within the Safari 14 baseline', () => {
  // A non-existent dist dir keeps the test hermetic: it must pass in CI
  // without a prior UI build and must not depend on a developer's stale dist.
  const violations = runAudit({ distDir: path.join(appRoot, '.compat-test-no-dist') });
  assert.deepEqual(violations, []);
});

test('legacy polyfills load before app modules in every entry', () => {
  const entries = [
    ['index.html', /app\/main\.jsx/],
    ['reader.html', /app\/reader-main\.jsx/],
    ['pet.html', /app\/pet-main\.jsx/],
  ];
  for (const [entry, ...anchors] of entries) {
    const html = readSrc(entry);
    const polyfill = html.search(/legacy-polyfills\.js/);
    assert.ok(polyfill > 0, `${entry} must load shared/legacy-polyfills.js`);
    for (const anchor of anchors) {
      const at = html.search(anchor);
      assert.ok(at > polyfill, `${entry}: legacy-polyfills.js must precede ${anchor}`);
    }
  }
});

test('vendored browser runtimes stay retired and the polyfill ships as a static script', () => {
  assert.ok(staticRuntimeScripts.has('shared/legacy-polyfills.js'));
  assert.ok(!staticRuntimeScripts.has('vendor/tailwind.js'));
  assert.ok(!staticRuntimeScripts.has('vendor/marked.min.js'));
  assert.ok(!staticRuntimeScripts.has('vendor/purify.min.js'));
  assert.ok(!fs.existsSync(path.join(appRoot, 'src/vendor/marked.min.js')));
  assert.ok(!fs.existsSync(path.join(appRoot, 'src/vendor/purify.min.js')));
  assert.ok(!fs.existsSync(path.join(appRoot, 'src/vendor/tailwind.js')));
  // The retired vendor scripts must not be reintroduced through index.html
  // either — a stray tag would 404 on every startup.
  assert.ok(!/vendor\/marked\.min\.js/.test(readSrc('index.html')));
  assert.ok(!/vendor\/purify\.min\.js/.test(readSrc('index.html')));
  for (const entry of ['index.html', 'reader.html']) {
    assert.ok(!/vendor\/tailwind\.js/.test(readSrc(entry)));
    assert.ok(!/tailwind\.config/.test(readSrc(entry)));
  }
});

test('the auditor flags violations under minifier-shaped syntax (complete walker contract)', () => {
  // Adversarial fixtures in the exact shapes a minifier emits for dist
  // chunks: sequence expressions, parameter default/pattern positions, and
  // RegExp constructor calls. An earlier hand-maintained child-key table
  // skipped these node types entirely, so a lookbehind hidden behind `(0, ...)`
  // sailed through the audit. Every fixture below MUST be reported.
  const distDir = fs.mkdtempSync(path.join(os.tmpdir(), 'compat-audit-adv-'));
  try {
    fs.mkdirSync(path.join(distDir, 'assets'), { recursive: true });
    fs.writeFileSync(path.join(distDir, 'assets', 'adv.js'), [
      'const x = (0, /(?<=a)b/);',
      'function f(y = /(?<=a)b/) {}',
      'new RegExp("(?<=a)b");',
      'new RegExp(src, "v");',
      'const z = (arr.findLast(n => n), 1);',
      'function g(w = [1, 2].findLastIndex(() => 0)) {}',
      'const s = (0, structuredClone({}));',
    ].join('\n'));
    const violations = runAudit({ distDir });
    const expected = [
      [1, 'lookbehind assertion'],
      [2, 'lookbehind assertion'],
      [3, 'lookbehind assertion'],
      [4, 'regex flag "v"'],
      [5, '.findLast() invocation'],
      [6, '.findLastIndex() invocation'],
      [7, 'structuredClone'],
    ];
    for (const [line, needle] of expected) {
      assert.ok(
        violations.some(v => v.startsWith(`dist:adv.js:${line}:`) && v.includes(needle)),
        `line ${line} must be reported (${needle}); got: ${JSON.stringify(violations)}`,
      );
    }
  } finally {
    fs.rmSync(distDir, { recursive: true, force: true });
  }
});

test('build-time tailwind emits inset utilities as physical properties for Safari 14.0', async () => {
  const result = await postcss([
    tailwindcss({
      content: [{ raw: '<div class="fixed inset-0"></div>', extension: 'html' }],
      corePlugins: { preflight: false },
    }),
  ]).process('@tailwind utilities;', { from: undefined });
  const compiled = transformCss({
    filename: 'tailwind.css',
    code: Buffer.from(result.css),
    targets: browserslistToTargets(['safari 14']),
    minify: true,
  }).code.toString();
  assert.match(compiled, /\.inset-0\{top:0;bottom:0;left:0;right:0\}/);
  assert.doesNotMatch(compiled, /\binset:0/);
});
