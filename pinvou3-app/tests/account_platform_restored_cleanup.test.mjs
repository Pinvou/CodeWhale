import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { parse } from 'acorn';
import { FixtureScope } from './helpers/fixture-scope.mjs';

// Execute the real entry's catch/finally with in-memory resources. No browser,
// server or fixture directory is created, including the failure cases.
const source = readFileSync(new URL('./account_platform_restored_ui_smoke.mjs', import.meta.url), 'utf8');
const boundaries = parse(source, { ecmaVersion: 'latest', sourceType: 'module' }).body.filter(node => node.type === 'TryStatement');
assert.equal(boundaries.length, 1, 'the restored smoke has one outer lifetime boundary');
const boundary = boundaries[0];
const AsyncFunction = Object.getPrototypeOf(async function () {}).constructor;
const runTail = new AsyncFunction('fixtureScope', 'browser', 'server', 'console', 'process', 'primary', `try { if (primary) throw primary; }${source.slice(boundary.block.end, boundary.end)}`);

function fixture({ browserFailure, serverFailure, browserCreated = true } = {}) {
  const scope = new FixtureScope(), calls = [], logged = [], processState = {};
  const server = {}, browser = browserCreated ? {} : undefined;
  for (const [kind, owned, failure] of [['server', server, serverFailure], ...(browser ? [['browser', browser, browserFailure]] : [])]) {
    owned.close = scope.register({ kind, close: async () => { calls.push(`${kind}:close`); if (failure) throw failure; }, verify: () => { calls.push(`${kind}:verify`); return true; }, remove: () => calls.push(`${kind}:remove`) });
  }
  return { scope, browser, server, calls, logged, processState, run: primary => runTail(scope, browser, server, { error: error => logged.push(error) }, processState, primary) };
}

test('restored entry attempts server shutdown when browser acknowledgement fails and never removes files', async () => {
  const failure = new Error('browser close was not acknowledged');
  const value = fixture({ browserFailure: failure });
  await assert.rejects(value.run(), error => error instanceof AggregateError && error.errors.includes(failure));
  assert.deepEqual(value.calls, ['browser:close', 'server:close', 'server:verify']);
  await assert.rejects(value.run(), AggregateError);
  assert.deepEqual(value.calls, ['browser:close', 'server:close', 'server:verify'], 'repeated finalization does not repeat side effects');
});

test('restored entry preserves the business failure and every shutdown failure', async () => {
  const primary = new Error('business assertion failed'), browserFailure = new Error('browser failed'), serverFailure = new Error('server failed');
  const value = fixture({ browserFailure, serverFailure });
  await assert.rejects(value.run(primary), error => error instanceof AggregateError && error.errors[0] === browserFailure && error.errors[1] === serverFailure);
  assert.deepEqual(value.logged, [primary]);
  assert.equal(value.processState.exitCode, 1);
  assert.deepEqual(value.calls, ['browser:close', 'server:close']);
});

test('restored entry removes owned files only after all close acknowledgements', async () => {
  const value = fixture();
  await value.run();
  assert.deepEqual(value.calls, ['browser:close', 'browser:verify', 'server:close', 'server:verify', 'browser:remove', 'server:remove']);
  assert.deepEqual(value.logged, []);
  assert.equal(value.processState.exitCode, undefined);
});

test('restored entry retains its original failure after successful resource retirement', async () => {
  const primary = new Error('business assertion failed'), value = fixture();
  await value.run(primary);
  assert.deepEqual(value.logged, [primary]);
  assert.equal(value.processState.exitCode, 1);
  assert.equal(value.calls.filter(call => call.endsWith(':remove')).length, 2);
});

test('restored entry guards listener setup and retires the server when listening fails', async () => {
  const listen = boundary.block.body.find(node => node.type === 'ExpressionStatement' && node.expression.type === 'AwaitExpression' && node.expression.argument.type === 'CallExpression' && node.expression.argument.callee.object?.name === 'server' && node.expression.argument.callee.property?.name === 'listen');
  assert.ok(listen, 'server.listen is inside the actual lifetime boundary');
  const runSetup = new AsyncFunction('fixtureScope', 'server', 'console', 'process', `try { ${source.slice(listen.start, listen.end)} }${source.slice(boundary.block.end, boundary.end)}`);
  const failure = new Error('listener setup failed'), value = fixture({ browserCreated: false });
  value.server.listen = async () => { throw failure; };
  await runSetup(value.scope, value.server, { error: error => value.logged.push(error) }, value.processState);
  assert.deepEqual(value.logged, [failure]);
  assert.equal(value.processState.exitCode, 1);
  assert.deepEqual(value.calls, ['server:close', 'server:verify', 'server:remove']);
});
