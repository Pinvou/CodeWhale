import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { existsSync, readFileSync, writeFileSync, mkdtempSync, rmSync, symlinkSync, realpathSync, readlinkSync, lstatSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const root = dirname(dirname(dirname(fileURLToPath(import.meta.url))));
const { isTrustedSystemAlias } = createRequire(import.meta.url)(join(root, 'remote-control-relay/scripts/validate-deploy-hardening.cjs'));
const production = readFileSync(join(root, 'scripts/deploy-remote-relay.sh'), 'utf8');
const testing = readFileSync(join(root, 'scripts/deploy-remote-relay-test.sh'), 'utf8');
const bash = process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : 'bash';
const posixPath = (value) => process.platform === 'win32'
  ? value.replaceAll('\\', '/').replace(/^([A-Za-z]):/, (_, drive) => '/' + drive.toLowerCase()) : value;
function shell(source, env = {}) {
  const result = spawnSync(bash, ['-c', 'set -euo pipefail\n' + source], {
    env: { ...process.env, ...env }, encoding: 'utf8', timeout: 10_000,
  });
  assert.equal(result.error, undefined);
  return result;
}
function fragment(source, pattern) {
  const match = source.match(pattern);
  assert.ok(match, 'actual deployment fragment must exist');
  return match[0];
}

test('actual production confirmation and base gates reject before any operations', () => {
  const confirmation = fragment(production, /^if \[\[ "\$\{PINVOU_CONFIRM_PRODUCTION_DEPLOY[^]*?^fi/m);
  const base = fragment(production, /^if \[\[ "\$BASE_PATH"[^]*?^fi/m);
  for (const value of ['', '0', 'true', '2']) {
    const result = shell(confirmation + '\necho unexpected_operation', { PINVOU_CONFIRM_PRODUCTION_DEPLOY: value });
    assert.equal(result.status, 64);
    assert.equal(result.stdout, '');
  }
  assert.equal(shell(confirmation, { PINVOU_CONFIRM_PRODUCTION_DEPLOY: '1' }).status, 0);
  for (const value of ['', '/other', '/pinvou3/remote-test', '/pinvou3/remote/']) {
    const result = shell(base + '\necho unexpected_operation', { BASE_PATH: value });
    assert.equal(result.status, 64);
    assert.equal(result.stdout, '');
  }
  assert.equal(shell(base, { BASE_PATH: '/pinvou3/remote' }).status, 0);
});

test('system alias exception is Darwin-only and rejects noncanonical or operator links', () => {
  for (const [path, expected] of [['/tmp', '/private/tmp'], ['/var', '/private/var']]) {
    assert.equal(isTrustedSystemAlias(path, 'darwin', expected, expected), true);
    assert.equal(isTrustedSystemAlias(path, 'darwin', expected.slice(1), expected), true);
    for (const platform of ['linux', 'win32']) {
      assert.equal(isTrustedSystemAlias(path, platform, expected, expected), false);
    }
    assert.equal(isTrustedSystemAlias(path, 'darwin', '/operator-link', expected), false);
    assert.equal(isTrustedSystemAlias(path, 'darwin', expected, '/operator-link'), false);
  }
  for (const path of ['/etc', '/operator/tmp', '/tmp/operator-link', '/private/tmp']) {
    assert.equal(isTrustedSystemAlias(path, 'darwin', '/private/tmp', '/private/tmp'), false);
  }
});

test('actual Darwin system temporary roots accept hardening input and reject operator links',
  { skip: process.platform !== 'darwin' }, (t) => {
    const validator = join(root, 'remote-control-relay/scripts/validate-deploy-hardening.cjs');
    const valid = readFileSync(join(root, 'remote-control-relay/10-hardening.conf'), 'utf8');
    for (const alias of ['/tmp', '/var']) {
      assert.equal(lstatSync(alias).isSymbolicLink(), true);
      assert.equal(isTrustedSystemAlias(alias, process.platform, readlinkSync(alias), realpathSync(alias)), true);
    }
    for (const parent of ['/tmp', tmpdir()]) {
      const directory = mkdtempSync(join(parent, 'pinvou-hardening-darwin-'));
      t.after(() => rmSync(directory, { recursive: true, force: true }));
      const input = join(directory, 'operator.conf');
      writeFileSync(input, valid);
      const run = (path) => spawnSync(process.execPath, [validator, path], { encoding: 'utf8', timeout: 10_000 });
      assert.equal(run(input).status, 0);
      const leaf = join(directory, 'linked.conf');
      symlinkSync(input, leaf);
      assert.equal(run(leaf).status, 64);
      const link = join(directory, 'linked-parent');
      symlinkSync(realpathSync(directory), link, 'dir');
      assert.equal(run(join(link, 'operator.conf')).status, 64);
    }
  });

test('actual production baseline failure stops before build, upload, or SSH', () => {
  const gate = fragment(production, /^if ! verify_public baseline; then[^]*?^fi/m);
  assert.ok(production.indexOf(gate) < production.indexOf('node --check'));
  assert.ok(production.indexOf(gate) < production.indexOf('scp -i'));
  const result = shell('VERIFY_ERROR=TEST_ONLY_BASELINE_FAILURE\nverify_public() { return 1; }\n'
    + gate + '\necho unexpected_operation');
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '');
  assert.match(result.stderr, /TEST_ONLY_BASELINE_FAILURE/);
});

test('actual remote health loops retry transient failures and bound permanent failures', (t) => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-health-fragment-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const counter = join(directory, 'calls');
  const bounds = join(directory, 'bad-bound');
  const cases = [
    { source: production, expectedSuccess: 4, expectedFailure: 30 },
    { source: testing, expectedSuccess: 3, expectedFailure: 45 },
  ];
  for (const { source, expectedSuccess, expectedFailure } of cases) {
    const health = fragment(source, /^healthy=0[^]*?(?=^echo "(?:backup=|测试 relay))/m);
    // Execute only the real health/rollback section. All IO boundaries are
    // functions; only a private fixture counter persists across the real curl
    // command substitution. No SSH, network, service or deployed filesystem IO.
    const stubs = [
      'service=fixture.service; port=8788; base_path=/fixture; test_dir=/fixture; backup=/fixture-backup',
      'remote_dir="$FIXTURE_DIR"; code_stage="$FIXTURE_DIR"',
      'curl() { IFS= read -r calls < "$COUNTER_FILE"; calls=$((calls+1)); printf "%s\\n" "$calls" > "$COUNTER_FILE"; [[ " $* " == *" --connect-timeout 3 "* && " $* " == *" --max-time 5 "* ]] || { printf BAD_BOUND > "$BOUND_FILE"; return 98; }; [[ "$FIXTURE_MODE" != permanent && "$calls" -ge 3 ]] || return 1; if [[ "$FIXTURE_MODE" == malformed ]]; then printf invalid-json; elif [[ "$FIXTURE_MODE" == controller-off ]]; then printf \'{"ok":true,"desktop_controller_enabled":false}\'; else printf \'{"ok":true,"desktop_controller_enabled":true}\'; fi; }',
      'node() { if [[ "$1" == -e ]]; then command node "$@"; else printf "route_smoke\\n"; [[ "$FIXTURE_MODE" != route-failure ]]; fi; }',
      'sleep() { :; }',
      'systemctl() { printf "systemctl:%s\\n" "$*"; }',
      '[() { if [[ "$1" == -d || "$1" == -f ]]; then return 0; fi; builtin [ "$@"; }',
      'rm() { :; }; mv() { :; }',
      'cp() { printf "copy:%s\\n" "$*"; }',
      'rollback() { echo rollback; }',
      'trap \'IFS= read -r calls < "$COUNTER_FILE"; printf "calls=%s\\n" "$calls"\' EXIT',
    ].join('\n');
    const modes = source === production
      ? ['transient', 'permanent', 'malformed', 'controller-off', 'route-failure'] : ['transient', 'permanent'];
    for (const mode of modes) {
      writeFileSync(counter, '0\n');
      if (existsSync(bounds)) rmSync(bounds);
      const result = shell(stubs + '\n' + health, { FIXTURE_MODE: mode, FIXTURE_DIR: posixPath(directory),
        COUNTER_FILE: posixPath(counter), BOUND_FILE: posixPath(bounds) });
      assert.equal(result.status, mode === 'transient' ? 0 : 1, result.stderr);
      assert.match(result.stdout, new RegExp('calls=' + (mode === 'transient' || mode === 'route-failure' ? expectedSuccess : expectedFailure) + '\\b'));
      assert.equal(existsSync(bounds), false, 'every curl must retain both request bounds');
      if (source === production) {
        assert.equal(result.stdout.includes('route_smoke'), mode === 'transient' || mode === 'route-failure');
        if (mode !== 'transient') assert.match(result.stdout, /rollback/);
        if (mode === 'route-failure') assert.match(result.stderr, /controller-v1 路由烟测失败/);
      }
      if (mode === 'permanent' && source === testing) {
        assert.match(result.stdout, /copy:-a \/etc\/systemd\/system\/fixture.service.bak \/etc\/systemd\/system\/fixture.service/);
        const copy = result.stdout.indexOf('copy:');
        assert.ok(copy < result.stdout.indexOf('systemctl:daemon-reload'));
        assert.ok(copy < result.stdout.indexOf('systemctl:restart'));
      }
    }
  }
});

test('actual deployment failure handler preserves remote stdout and stderr', () => {
  const handler = fragment(production, /^\)" \|\| \{[^]*?^\}/m);
  const result = shell('deploy_output="$(printf TEST_ONLY_REMOTE_STDOUT; printf TEST_ONLY_REMOTE_STDERR >&2; exit 27\n'
    + handler + '\necho unexpected_operation');
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '');
  assert.match(result.stderr, /TEST_ONLY_REMOTE_STDOUT/);
  assert.match(result.stderr, /TEST_ONLY_REMOTE_STDERR/);
  assert.match(result.stderr, /远程部署步骤失败/);
});

test('actual production SSH and scp argv preserve private key and noninteractive auth', () => {
  // Capture every invocation BEFORE checking the auth flags; filtering by -i
  // would silently drop the very mutation that this test must reject.
  const commands = production.split(/\r?\n/).filter((line) =>
    /^(?:scp\s|deploy_output="\$\(ssh\s|\s+\|\s*ssh\s|\s+ssh\s)/.test(line));
  assert.ok(commands.length >= 8);
  for (const command of commands) {
    const invocation = command.slice(command.search(/\b(?:ssh|scp)\s/))
      .replace(/ <<'REMOTE'.*$/, '');
    const result = shell([
      'SSH_KEY="fixture key"; SERVER=fixture.invalid; RELAY_DIR=/fixture; REMOTE_DIR=/fixture',
      'SERVICE=fixture; STAMP=fixture; backup=/fixture-backup; HARDENING_SNAPSHOT=/fixture-hardening',
      'IPV4_DB_CACHE=/fixture-input; IPV6_DB_CACHE=/fixture-input; IPV4_DB_SHA256=fixture; IPV6_DB_SHA256=fixture',
      ...[...production.matchAll(/^(REMOTE_[A-Z0-9_]+)="[^\n]+"/gm)].map((m) => m[1] + '=/fixture-input'),
      'ssh() { [[ "$1" == -i && "$2" == "fixture key" && "$3" == -o && "$4" == BatchMode=yes ]] || return 98; echo valid_auth; }',
      'scp() { ssh "$@"; }', invocation,
    ].join('\n'));
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout.trim(), 'valid_auth');
  }
});

test('actual test public rollback restores the old unit before restarting and bounds curl', () => {
  const rollback = fragment(testing, /^rollback_relay\(\) \{[^]*?^\}/m);
  const remote = fragment(rollback, /^set -euo pipefail[^]*?(?=^REMOTE$)/m);
  const result = shell([
    'set -- /fixture 8788 /fixture fixture.service',
    '[() { [[ "$1" != "!" ]]; }; rm() { :; }; mv() { :; }; sleep() { :; }',
    'cp() { echo restored_unit; }',
    'systemctl() { printf "systemctl:%s\\n" "$*"; }',
    'curl() { [[ " $* " == *" --connect-timeout 3 "* && " $* " == *" --max-time 5 "* ]]; }',
    remote,
  ].join('\n'));
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /restored_unit/);
  assert.ok(result.stdout.indexOf('restored_unit') < result.stdout.indexOf('systemctl:restart'));
});

test('private hardening input is explicit, validated and uploaded as a snapshot', (t) => {
  const directory = mkdtempSync(join(realpathSync(tmpdir()), 'pinvou-hardening-check-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const validator = join(root, 'remote-control-relay/scripts/validate-deploy-hardening.cjs');
  const template = readFileSync(join(root, 'remote-control-relay/10-hardening.conf'), 'utf8');
  assert.doesNotMatch(template, /(?:\d{1,3}\.){3}\d{1,3}/);
  assert.doesNotMatch(template, /PINVOU_REMOTE_(?:ALLOWED|TRUSTED)_PROXY_IPS=/);
  const valid = template;
  const input = join(directory, 'operator config.conf');
  const run = (path) => spawnSync(process.execPath, [validator, path], { encoding: 'utf8', timeout: 10_000 });
  for (const contents of ['', valid.replace('MAX_ROOMS=2000', 'MAX_ROOMS=9999'),
    valid.replace('ROOM_CREATE_WINDOW_MS=60000', 'ROOM_CREATE_WINDOW_MS=1'),
    valid.replace('WS_CONNECT_WINDOW_MS=60000', 'WS_CONNECT_WINDOW_MS=1'),
    valid.replace(/^Environment=MAX_ROOMS=2000\r?\n/m, ''),
    valid + '\nEnvironment=PINVOU_REMOTE_ALLOWED_PROXY_IPS=192.0.2.20\n',
    valid + '\nEnvironment=PINVOU_REMOTE_TRUSTED_PROXY_IPS=192.0.2.20\n',
    valid + '\nEnvironment=PINVOU_REMOTE_ALLOWED_PROXY_IPS=2001:db8::20\n',
    valid + '\nExecStart=/fixture\n', valid + '\nEnvironment=MAX_ROOMS=2000\n']) {
    writeFileSync(input, contents);
    const result = run(input);
    assert.equal(result.status, 64);
    assert.equal(result.stdout, '');
    assert.equal(result.stderr.trim(), 'deploy_hardening_configuration_invalid');
  }
  for (const path of ['', directory, join(directory, 'absent')]) assert.equal(run(path).status, 64);
  writeFileSync(input, valid);
  assert.equal(run(input).status, 0);
  const link = join(directory, 'linked-parent');
  symlinkSync(directory, link, process.platform === 'win32' ? 'junction' : 'dir');
  assert.equal(run(join(link, 'operator config.conf')).status, 64);
  assert.match(production, /HARDENING_FILE="\$\{PINVOU_REMOTE_HARDENING_FILE:-\}"/);
  assert.doesNotMatch(production, /scp[^\n]*10-hardening\.conf/);
  const check = 'node "$RELAY_DIR/scripts/validate-deploy-hardening.cjs" "$HARDENING_FILE"';
  assert.ok(production.indexOf(check) < production.indexOf('if ! verify_public baseline'));
  assert.match(production, /scp -i "\$SSH_KEY" -o BatchMode=yes "\$HARDENING_SNAPSHOT"/);
  assert.match(production, /node "\$RELAY_DIR\/scripts\/validate-deploy-hardening.cjs" "\$HARDENING_SNAPSHOT"/);
  assert.equal((production.match(/cp -a "\$backup\/10-hardening.conf"/g) ?? []).length, 2);
});

test('actual hardening preflight rejects missing config before network and uploads exact validated content', (t) => {
  const directory = mkdtempSync(join(realpathSync(tmpdir()), 'pinvou-hardening-preflight-'));
  t.after(() => rmSync(directory, { recursive: true, force: true }));
  const input = join(directory, 'operator.conf');
  const template = readFileSync(join(root, 'remote-control-relay/10-hardening.conf'), 'utf8');
  writeFileSync(input, template);
  const preflight = fragment(production, /^node "\$RELAY_DIR\/scripts\/validate-deploy-hardening.cjs" "\$HARDENING_FILE"[^]*?^node "\$RELAY_DIR\/scripts\/validate-deploy-hardening.cjs" "\$HARDENING_SNAPSHOT"$/m);
  const upload = fragment(production, /^scp -i "\$SSH_KEY" -o BatchMode=yes "\$HARDENING_SNAPSHOT"[^\n]*/m);
  const posixPath = (value) => value.replaceAll('\\', '/').replace(/^([A-Za-z]):/, (_, drive) => '/' + drive.toLowerCase());
  // Execute the production fragment with the exact parent Node, even when the
  // child PATH deliberately has no node. No product environment knob is added.
  const node = [
    'fixture_mktemp="$(command -v mktemp)"; fixture_cp="$(command -v cp)"',
    'fixture_cmp="$(command -v cmp)"; fixture_rm="$(command -v rm)"',
    'PATH="$FIXTURE_EMPTY_PATH"; export PATH; hash -r',
    'if command -v node >/dev/null 2>&1; then exit 98; fi',
    'node() { "$FIXTURE_NODE" "$@"; }',
    'mktemp() { "$fixture_mktemp" "$@"; }; cp() { "$fixture_cp" "$@"; }',
    'cmp() { "$fixture_cmp" "$@"; }; rm() { "$fixture_rm" "$@"; }',
  ].join('\n') + '\n';
  for (const path of ['', join(directory, 'missing'), input]) {
    const result = shell(node + preflight + '\nSSH_KEY=fixture; SERVER=fixture.invalid; REMOTE_HARDENING_TMP=/fixture\n'
      + 'scp() { [[ "$1" == -i && "$2" == fixture && "$3" == -o && "$4" == BatchMode=yes ]]; cmp "$5" "$HARDENING_FILE"; echo exact_upload; }\n'
      + upload, { RELAY_DIR: process.platform === 'win32' ? posixPath(join(root, 'remote-control-relay')) : join(root, 'remote-control-relay'),
        HARDENING_FILE: process.platform === 'win32' ? posixPath(path) : path,
        TMPDIR: process.platform === 'win32' ? posixPath(directory) : directory,
        FIXTURE_NODE: process.platform === 'win32' ? posixPath(process.execPath) : process.execPath,
        FIXTURE_EMPTY_PATH: process.platform === 'win32' ? posixPath(directory) : directory });
    assert.equal(result.status, path === input ? 0 : 64, result.stderr);
    assert.equal(result.stdout.trim(), path === input ? 'exact_upload' : '');
  }
});

test('Rust endpoint selection keeps env before persisted profile before production fallback', () => {
  // Source contract only: no new native-runtime execution is claimed here.
  const source = readFileSync(join(root, 'pinvou3-app/src-tauri/src/features/remote_control/manager/mod.rs'), 'utf8');
  const body = fragment(source, /^pub\(crate\) fn active_endpoint_profile\(\)[^]*?^\}/m);
  assert.match(body, /std::env::var\("PINVOU_REMOTE_ENDPOINT_PROFILE"\)/);
  assert.match(body, /endpoint_profile_by_id\(requested.trim\(\)\)/);
  assert.match(body, /persistence::selected_endpoint_profile\(\)/);
  assert.match(body, /endpoint_profile_by_id\(selected.as_str\(\)\)/);
  assert.match(body, /&PRODUCTION_PROFILE\s*\}/);
  assert.ok(body.indexOf('std::env::var') < body.indexOf('persistence::selected_endpoint_profile'));
  assert.equal((body.match(/return profile;/g) ?? []).length, 2);
});
