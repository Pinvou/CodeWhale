import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import test from 'node:test';

const root = new URL('../../', import.meta.url);
async function productionGo(directory) {
  const files = [];
  for (const entry of await readdir(new URL(directory, root), { withFileTypes: true })) {
    const path = `${directory}${entry.name}`;
    if (entry.isDirectory()) files.push(...await productionGo(`${path}/`));
    else if (entry.isFile() && path.endsWith('.go') && !path.endsWith('_test.go')) files.push(path);
  }
  return files;
}

test('all command and internal production sources retain readiness-only compatibility wiring', async () => {
  const files = [...await productionGo('cmd/'), ...await productionGo('internal/')];
  const wrappers = [], manifests = [];
  for (const path of files.sort()) {
    const source = await readFile(new URL(path, root), 'utf8');
    // Include method values as well as direct calls; the check does not depend
    // on the local policy variable's name or FromEnv's argument spelling.
    for (const match of source.matchAll(/\.\s*(Wrap|WrapExcept)\b/g)) {
      wrappers.push({ path, method: match[1] });
    }
    if (/\bclientcompat\s*\.\s*FromEnv\s*\(/.test(source)) manifests.push(path);
  }
  assert.deepEqual(wrappers, [{ path: 'internal/platform/clientcompat/policy.go', method: 'Wrap' }],
    'a new wrapper reference needs an explicit route/scope contract review');
  assert.deepEqual(manifests, ['cmd/token-gateway/personal_execution.go']);
  const source = await readFile(new URL(manifests[0], root), 'utf8');
  const binding = source.match(/\b(\w+),\s*\w+\s*:?=\s*clientcompat\s*\.\s*FromEnv\s*\(/);
  assert.ok(binding, 'readiness must construct the client compatibility policy');
  assert.match(source, new RegExp(`Compatibility:\\s*${binding[1]}\\s*\\.\\s*Manifest\\b`));
});

test('authoritative documents distinguish current readiness from unmounted HTTP policy', async () => {
  for (const path of ['docs/architecture.md', 'docs/implementation-status.md', 'docs/client-compatibility-scope.md']) {
    const source = await readFile(new URL(path, root), 'utf8');
    assert.match(source, /readiness/);
    assert.match(source, /未(?:挂载|装配)/);
    assert.match(source, /#1954/);
    assert.doesNotMatch(source, /四个对外服务由同一客户端兼容策略包裹|四服务共用版本清单与响应头|四服务返回契约\/最小版本\/Sunset头/);
  }
});
