import test from 'node:test';
import assert from 'node:assert/strict';
import { chmodSync, existsSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';

const root = resolve(import.meta.dirname, '../../..');
const bash = process.platform === 'win32' ? 'C:/Program Files/Git/bin/bash.exe' : '/bin/bash';
const executable = (dir, name, body) => { const path = join(dir, name); writeFileSync(path, `#!/usr/bin/env bash\nset -euo pipefail\n${body}`); chmodSync(path, 0o755); return path; };
const shellPath = path => process.platform === 'win32' ? `/${path[0].toLowerCase()}${path.slice(2).replaceAll('\\', '/')}` : path;
const envFor = (bin, state, extra = {}) => ({ ...process.env, MOCK_BIN: shellPath(bin), NODE_BIN: process.execPath.replaceAll('\\', '/'), MOCK_STATE: shellPath(state), ...extra });
const run = (script, args, options) => execFileSync(bash, ['--noprofile', '--norc', '-c', 'export PATH="$MOCK_BIN:/usr/bin:/bin"; exec "$@"', 'runbook', script, ...args], { timeout: 90_000, killSignal: 'SIGKILL', ...options });

test('Kubernetes runbook executes Lease lifecycle and restores false after phase2 Helm failure', { timeout: 120_000 }, (t) => {
  if (!existsSync(bash)) return;
  const temp = mkdtempSync(join(tmpdir(), 'lineage-k8s-')); const bin = join(temp, 'bin'); const state = join(temp, 'state');
  t.after(() => rmSync(temp, { recursive: true, force: true }));
  execFileSync(bash, ['-lc', `mkdir -p '${bin}' '${state}'`], { timeout: 10_000, killSignal: 'SIGKILL' });
  executable(bin, 'node', 'exec "$NODE_BIN" "$@"');
  executable(bin, 'sleep', 'exit 0');
  executable(bin, 'timeout', 'while [[ "$1" == --* || "$1" =~ ^[0-9]+$ ]]; do shift; done; exec "$@"');
  const row = 'uid-1 repo@sha256:abc pod-1\n';
  const hash = createHash('sha256').update(row).digest('hex');
  executable(bin, 'kubectl', String.raw`
a="$*"
echo "$a" >>"$MOCK_STATE/kubectl.log"
if [[ "$a" == *"create configmap"*"lineage-time"* ]]; then exit 0; fi
if [[ "$a" == *"get configmap"*"lineage-time"* ]]; then echo '2030-01-01T00:00:00Z'; exit 0; fi
if [[ "$a" == *"delete configmap"*"lineage-time"* ]]; then exit 0; fi
if [[ "$a" == *"create -f - -o json"* ]]; then input=$(cat); INPUT="$input" node -e 'const x=JSON.parse(process.env.INPUT);x.metadata.uid="lease-uid";x.metadata.resourceVersion="1";console.log(JSON.stringify(x))' | tee "$MOCK_STATE/lease.json"; exit 0; fi
if [[ "$a" == *"replace -f - -o json"* ]]; then input=$(cat); if [[ "${'$'}{MOCK_RENEW_CONFLICT:-}" == 1 ]]; then INPUT="$input" node -e 'const fs=require("fs"),p=process.env.MOCK_STATE+"/lease.json",x=JSON.parse(process.env.INPUT);x.metadata.resourceVersion=String(Number(x.metadata.resourceVersion)+1);x.spec.holderIdentity="other";fs.writeFileSync(p,JSON.stringify(x))'; exit 1; fi; INPUT="$input" node -e 'const fs=require("fs"),p=process.env.MOCK_STATE+"/lease.json",x=JSON.parse(process.env.INPUT);x.metadata.resourceVersion=String(Number(x.metadata.resourceVersion)+1);fs.writeFileSync(p,JSON.stringify(x));console.log(JSON.stringify(x))'; exit 0; fi
if [[ "$a" == *"get lease"* ]]; then [[ -f "$MOCK_STATE/lease.json" ]] || exit 1; if [[ "$a" == *"metadata.uid"* ]]; then node -e 'console.log(require(process.argv[1]).metadata.uid)' "$MOCK_STATE/lease.json"; elif [[ "$a" == *"resourceVersion"* ]]; then node -e 'console.log(require(process.argv[1]).metadata.resourceVersion)' "$MOCK_STATE/lease.json"; elif [[ "$a" == *"holderIdentity"* ]]; then node -e 'console.log(require(process.argv[1]).spec.holderIdentity)' "$MOCK_STATE/lease.json"; else cat "$MOCK_STATE/lease.json"; fi; exit 0; fi
if [[ "$a" == *"delete lease"* ]]; then rm -f "$MOCK_STATE/lease.json"; exit 0; fi
if [[ "$a" == *"proxy --port=0"* ]]; then echo $$ >"$MOCK_STATE/proxy.pid"; trap 'rm -f "$MOCK_STATE/proxy.pid"' EXIT; echo 'Starting to serve on 127.0.0.1:19091'; /usr/bin/sleep 0.2; exit 0; fi
if [[ "$a" == *"get deployment"*"jsonpath={range"* ]]; then echo token-portal-deployment; exit 0; fi
if [[ "$a" == *"get deployment token-portal-deployment"*"-o json" ]]; then
  if [[ "${'$'}{MOCK_DEPLOYMENT_ENV:-}" == valueFrom ]]; then env='[{"name":"PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED","valueFrom":{"secretKeyRef":{"name":"flag","key":"enabled"}}}]'
  elif [[ "${'$'}{MOCK_DEPLOYMENT_ENV:-}" == duplicate ]]; then env='[{"name":"PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED","value":"false"},{"name":"PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED","value":"true"}]'
  else value=false; [[ "${'$'}{MOCK_FLAG_TRUE:-}" == 1 ]] && value=true; env="[{\"name\":\"PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED\",\"value\":\"$value\"}]"; fi
  printf '{"spec":{"template":{"spec":{"containers":[{"name":"token-portal","env":%s}]}}}}\n' "$env"; exit 0
fi
if [[ "$a" == *"get deployment token-portal-deployment"* ]]; then echo 1; exit 0; fi
if [[ "$a" == *"get configmap"*"phase1"* ]]; then
  [[ "${'$'}{MOCK_PHASE1_ABSENT:-}" == 1 && "$a" != *"-o "* ]] && exit 1
  case "$a" in *phase1-digest*) echo sha256:abc;; *capable*) echo true;; *enabled*) echo false;; *completed-at*) echo 2020-01-01T00:00:00Z;; *phase2-not-before*) echo 1577837400;; *phase1-pod-set-sha256*) echo '${hash}';; *metadata.creationTimestamp*) echo 2020-01-01T00:00:00Z;; *metadata.uid*) echo generation-1;; esac; exit 0
fi
if [[ "$a" == *"get pods"*"metadata.uid"* ]]; then exit 0; fi
if [[ "$a" == *"get pods"*"-o json"* ]]; then echo '{"items":[{"metadata":{"uid":"uid-1","name":"pod-1"},"status":{"conditions":[{"type":"Ready","status":"True"}],"containerStatuses":[{"name":"token-portal","imageID":"repo@sha256:abc"}]}}]}'; exit 0; fi
if [[ "$a" == *"port-forward"* ]]; then echo $$ >"$MOCK_STATE/port-forward.pid"; trap 'rm -f "$MOCK_STATE/port-forward.pid"' EXIT; echo 'Forwarding from 127.0.0.1:19090 -> 8080'; /usr/bin/sleep 0.2; exit 0; fi
if [[ "$a" == *"scale deployment"* || "$a" == *"wait --for=delete"* || "$a" == *"delete configmap"* || "$a" == *"create configmap"* ]]; then exit 0; fi
echo "unexpected kubectl: $a" >&2; exit 90
`);
  executable(bin, 'helm', String.raw`
echo "$*" >>"$MOCK_STATE/helm.log"
[[ "$1" == status ]] && exit 0
if [[ "$*" == *"sessionLineageLogoutEnabled=true"* ]]; then
  if [[ "${'$'}{MOCK_FAIL_TRUE:-}" == 1 && ! -f "$MOCK_STATE/failed" ]]; then touch "$MOCK_STATE/failed"; exit 41; fi
  echo true >"$MOCK_STATE/enabled"
else echo false >"$MOCK_STATE/enabled"; fi
`);
  executable(bin, 'curl', String.raw`
if [[ "$*" == *"-X DELETE"* ]]; then
  body=''; previous=''; for argument in "$@"; do [[ "$previous" == --data ]] && body="$argument"; previous="$argument"; done
  BODY="$body" node -e 'const fs=require("fs"),p=process.env.MOCK_STATE+"/lease.json",l=JSON.parse(fs.readFileSync(p)),b=JSON.parse(process.env.BODY);if(b.preconditions.uid!==l.metadata.uid||b.preconditions.resourceVersion!==l.metadata.resourceVersion)process.exit(1);fs.writeFileSync(process.env.MOCK_STATE+"/delete-body.json",JSON.stringify(b))'
  if [[ "${'$'}{MOCK_REPLACE_LOCK:-}" == 1 ]]; then node -e 'const fs=require("fs"),p=process.env.MOCK_STATE+"/lease.json",x=JSON.parse(fs.readFileSync(p));x.metadata.resourceVersion=String(Number(x.metadata.resourceVersion)+1);x.spec.holderIdentity="other";fs.writeFileSync(p,JSON.stringify(x))'; exit 22; fi
  rm -f "$MOCK_STATE/lease.json"; echo '{}'; exit 0
fi
enabled=$(cat "$MOCK_STATE/enabled"); [[ "${'$'}{MOCK_FLAG_TRUE:-}" == 1 ]] && enabled=true; printf '{"schema_version":1,"capable":true,"enabled":%s}\n' "$enabled"
`);
  executable(bin, 'flock', 'exit 0'); writeFileSync(join(state, 'enabled'), 'false');
  const args = [resolve(root, 'account-platform/deploy/runbooks/session-lineage-rollout.sh'), 'phase2', 'ns', 'rel', 'repo', 'tag', 'sha256:abc'];
  try { run(args[0], args.slice(1), { cwd: root, env: envFor(bin, state), stdio: 'pipe' }); } catch (error) {
    error.message += `\nhelm=${existsSync(join(state, 'helm.log')) ? readFileSync(join(state, 'helm.log'), 'utf8') : 'missing'}\nkubectl=${readFileSync(join(state, 'kubectl.log'), 'utf8')}\nlease=${existsSync(join(state, 'lease.json')) ? readFileSync(join(state, 'lease.json'), 'utf8') : 'missing'}`;
    throw error;
  }
  assert.equal(existsSync(join(state, 'lease.json')), false);
  const deleteBody = JSON.parse(readFileSync(join(state, 'delete-body.json'), 'utf8'));
  assert.equal(deleteBody.preconditions.uid, 'lease-uid'); assert.ok(Number(deleteBody.preconditions.resourceVersion) > 1);
  assert.equal(existsSync(join(state, 'proxy.pid')), false); assert.equal(existsSync(join(state, 'port-forward.pid')), false);
  writeFileSync(join(state, 'enabled'), 'true'); writeFileSync(join(state, 'helm.log'), '');
  assert.throws(() => run(args[0], ['phase1', ...args.slice(2)], { cwd: root, env: envFor(bin, state, { MOCK_PHASE1_ABSENT: '1', MOCK_FLAG_TRUE: '1' }), stdio: 'pipe' }));
  assert.doesNotMatch(readFileSync(join(state, 'helm.log'), 'utf8'), /upgrade/);
  for (const invalidEnv of ['valueFrom', 'duplicate']) {
    writeFileSync(join(state, 'helm.log'), '');
    assert.throws(() => run(args[0], ['phase1', ...args.slice(2)], { cwd: root, env: envFor(bin, state, { MOCK_PHASE1_ABSENT: '1', MOCK_DEPLOYMENT_ENV: invalidEnv }), stdio: 'pipe' }));
    assert.doesNotMatch(readFileSync(join(state, 'helm.log'), 'utf8'), /upgrade/);
  }
  writeFileSync(join(state, 'enabled'), 'false');
  writeFileSync(join(state, 'enabled'), 'false'); writeFileSync(join(state, 'helm.log'), '');
  assert.throws(() => run(args[0], args.slice(1), { cwd: root, env: envFor(bin, state, { MOCK_REPLACE_LOCK: '1' }), stdio: 'pipe' }));
  assert.ok(Number(JSON.parse(readFileSync(join(state, 'lease.json'), 'utf8')).metadata.resourceVersion) > 1);
  rmSync(join(state, 'lease.json'), { force: true });
  assert.throws(() => run(args[0], args.slice(1), { cwd: root, env: envFor(bin, state, { MOCK_RENEW_CONFLICT: '1' }), stdio: 'pipe' }));
  assert.equal(JSON.parse(readFileSync(join(state, 'lease.json'), 'utf8')).spec.holderIdentity, 'other');
  rmSync(join(state, 'lease.json'), { force: true });
  assert.match(readFileSync(join(state, 'helm.log'), 'utf8'), /upgrade rel[\s\S]*--reuse-values[\s\S]*sessionLineageLogoutEnabled=true/);
  writeFileSync(join(state, 'enabled'), 'false'); rmSync(join(state, 'failed'), { force: true }); writeFileSync(join(state, 'helm.log'), '');
  assert.throws(() => run(args[0], args.slice(1), { cwd: root, env: envFor(bin, state, { MOCK_FAIL_TRUE: '1' }), stdio: 'pipe' }));
  const calls = readFileSync(join(state, 'helm.log'), 'utf8');
  assert.match(calls, /sessionLineageLogoutEnabled=true[\s\S]*sessionLineageLogoutEnabled=false/);
  assert.equal(existsSync(join(state, 'lease.json')), false);
});

test('Compose runbook verifies RepoDigests, persists phase evidence, and restores baseline on phase1 failure', { timeout: 120_000 }, (t) => {
  if (!existsSync(bash)) return;
  const temp = mkdtempSync(join(tmpdir(), 'lineage-compose-')); const bin = join(temp, 'bin'); const state = join(temp, 'state'); const evidence = join(temp, 'evidence');
  t.after(() => rmSync(temp, { recursive: true, force: true }));
  execFileSync(bash, ['-lc', `mkdir -p '${bin}' '${state}' '${evidence}'`], { timeout: 10_000, killSignal: 'SIGKILL' });
  executable(bin, 'node', 'exec "$NODE_BIN" "$@"');
  executable(bin, 'timeout', 'while [[ "$1" == --* || "$1" =~ ^[0-9]+$ ]]; do shift; done; exec "$@"');
  executable(bin, 'flock', 'exit 0');
  executable(bin, 'ln', 'if [[ "${MOCK_LN_RACE:-}" == 1 ]]; then printf attacker >"$2"; exit 1; fi; exec /usr/bin/ln "$@"');
  executable(bin, 'docker', String.raw`
a="$*"
if [[ "$a" == image\ inspect* ]]; then [[ "${'$'}{MOCK_BAD_DIGEST:-}" == 1 ]] && echo '[]' || printf '["%s"]\n' "$3"; exit 0; fi
if [[ "$a" == compose*"ps -q --all token-portal"* ]]; then echo cid; exit 0; fi
if [[ "$a" == inspect\ cid* ]]; then if [[ "$a" == *Config.Env* ]]; then enabled=$(cat "$MOCK_STATE/enabled"); printf '["PORTAL_SESSION_LINEAGE_LOGOUT_ENABLED=%s"]\n' "$enabled"; else cat "$MOCK_STATE/container"; fi; exit 0; fi
if [[ "$a" == create* ]]; then echo clock; exit 0; fi
if [[ "$a" == inspect\ clock* ]]; then echo 2030-01-01T00:00:00Z; exit 0; fi
if [[ "$a" == rm\ clock* ]]; then exit 0; fi
if [[ "$a" == compose*" stop token-portal"* ]]; then awk '{$4="false";print}' "$MOCK_STATE/container" >"$MOCK_STATE/x"; mv "$MOCK_STATE/x" "$MOCK_STATE/container"; exit 0; fi
if [[ "$a" == compose*" up -d"* ]]; then
  echo "$TOKEN_PORTAL_IMAGE" >>"$MOCK_STATE/up.log"
  if [[ "${'$'}{MOCK_UP_FAIL_ONCE:-}" == 1 && ! -f "$MOCK_STATE/up-failed" ]]; then touch "$MOCK_STATE/up-failed"; exit 42; fi
  [[ "${'$'}{MOCK_UP_ALWAYS_FAIL:-}" == 1 ]] && exit 43
  enabled=false; [[ "$a" == *compose.session-lineage-enabled.yaml* ]] && enabled=true
  printf 'cid %s sha256:image-id 2020-01-01T00:00:00Z true\n' "$TOKEN_PORTAL_IMAGE" >"$MOCK_STATE/container"; echo "$enabled" >"$MOCK_STATE/enabled"; exit 0
fi
echo "unexpected docker: $a" >&2; exit 91
`);
  executable(bin, 'curl', 'enabled=$(cat "$MOCK_STATE/enabled"); printf \'{"capable":true,"enabled":%s}\\n\' "$enabled"');
  writeFileSync(join(state, 'container'), 'cid old:mutable sha256:old-image-id 2020-01-01T00:00:00Z true\n'); writeFileSync(join(state, 'enabled'), 'false');
  const script = resolve(root, 'account-platform/deploy/runbooks/session-lineage-compose-rollout.sh');
  const common = { cwd: root, env: envFor(bin, state, { TOKEN_PORTAL_BIND_PORT: '8080' }), stdio: 'pipe' };
  const evidenceArg = shellPath(evidence);
  assert.throws(() => run(script, ['phase1', 'repo@sha256:new', evidenceArg], { ...common, env: { ...common.env, MOCK_UP_FAIL_ONCE: '1' } }));
  assert.match(readFileSync(join(state, 'up.log'), 'utf8'), /sha256:old-image-id/);
  assert.equal(existsSync(join(evidence, 'phase1')), false);
  assert.throws(() => run(script, ['phase1', 'repo@sha256:new', evidenceArg], { ...common, env: { ...common.env, MOCK_LN_RACE: '1' } }));
  assert.equal(readFileSync(join(evidence, 'phase1'), 'utf8'), 'attacker'); rmSync(join(evidence, 'phase1'));
  rmSync(join(state, 'up-failed'), { force: true });
  assert.throws(() => run(script, ['phase1', 'repo@sha256:new', evidenceArg], { ...common, env: { ...common.env, MOCK_UP_ALWAYS_FAIL: '1' } }));
  writeFileSync(join(state, 'enabled'), 'true'); const beforeTrue = readFileSync(join(state, 'up.log'), 'utf8');
  assert.throws(() => run(script, ['phase1', 'repo@sha256:new', evidenceArg], common));
  assert.equal(readFileSync(join(state, 'up.log'), 'utf8'), beforeTrue);
  writeFileSync(join(state, 'enabled'), 'false');
  assert.throws(() => run(script, ['phase1', 'repo@sha256:new', evidenceArg], { ...common, env: { ...common.env, MOCK_BAD_DIGEST: '1' } }));
  run(script, ['phase1', 'repo@sha256:new', evidenceArg], common);
  run(script, ['phase2', 'repo@sha256:new', evidenceArg], common);
  assert.equal(existsSync(join(evidence, 'activation')), true);
  assert.match(readFileSync(join(state, 'container'), 'utf8'), /repo@sha256:new[\s\S]*true/);
});
