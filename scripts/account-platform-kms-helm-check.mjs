// Validate rendered native KMS isolation, not the cluster's actual identities or CNI.
const image = 'quay.io/openbao/openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641';
const names = ['token-gateway', 'token-credential-portal'];
const component = item => item?.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'];
const environment = c => Object.fromEntries((c.env || []).map(v => [v.name, v.value ?? v.valueFrom]));
function check(value, code) { if (!value) throw new Error(`model_token_deployment_kms_${code}`); }
const exact = (actual, expected) => JSON.stringify(actual) === JSON.stringify(expected);
function mount(c, name, target, readonly) {
  const found = (c.volumeMounts || []).filter(v => v.name === name);
  check(found.length === 1 && found[0].mountPath === target && Boolean(found[0].readOnly) === readonly
    && !found[0].subPath && !found[0].subPathExpr, 'mount_unsafe');
}
export function checkHelmKMS(items) {
  const workloads = items.filter(v => v.kind === 'Deployment' && names.includes(component(v)));
  if (!workloads.some(w => environment(w.spec.template.spec.containers[0]).TOKEN_SECRET_PROVIDER === 'vault-transit')) return;
  check(workloads.length >= 1 && workloads.every(w => environment(w.spec.template.spec.containers[0]).TOKEN_SECRET_PROVIDER === 'vault-transit'), 'provider_mismatch');
  const bootstraps = []; const origins = []; const keys = []; const caNames = [];
  for (const w of workloads) {
    const pod = w.spec.template.spec; const app = pod.containers[0]; const env = environment(app);
    check(Object.keys(env).length === (app.env || []).length, 'native_identity_invalid');
    check(pod.containers.length === 2 && !pod.initContainers?.length && !pod.ephemeralContainers?.length
      && !pod.hostNetwork && !pod.hostPID && !pod.hostIPC && !pod.shareProcessNamespace, 'runtime_isolation');
    const agent = pod.containers[1];
    check(agent.name === 'kms-agent' && agent.image === image && exact(agent.command, ['/usr/bin/bao'])
      && exact(agent.args, ['agent', '-config=/bao/config/agent.hcl']) && !agent.env?.length && !agent.envFrom?.length
      && !agent.ports?.length && !agent.lifecycle, 'agent_invalid');
    check(pod.securityContext.runAsUser === 65532 && pod.securityContext.runAsGroup === 65532
      && pod.securityContext.fsGroup === 65532 && pod.automountServiceAccountToken === false, 'runtime_isolation');
    for (const c of [app, agent]) check(c.securityContext?.readOnlyRootFilesystem === true
      && c.securityContext.allowPrivilegeEscalation === false && !c.securityContext.privileged
      && exact(c.securityContext.capabilities?.drop, ['ALL']) && !c.securityContext.capabilities?.add?.length, 'runtime_isolation');
    check(agent.securityContext.runAsUser === 65532 && agent.securityContext.runAsGroup === 65532, 'agent_invalid');
    check(!app.envFrom?.length && !env.TOKEN_SECRET_PROVIDER_TOKEN && !env.TOKEN_LOCAL_KEK_BASE64
      && env.TOKEN_SECRET_PROVIDER_TOKEN_FILE === '/var/run/secrets/pinvou/kms/token'
      && env.TOKEN_SECRET_PROVIDER_CA_FILE === '/var/run/secrets/pinvou/kms-ca/ca.crt', 'native_identity_invalid');
    let url; try { url = new URL(env.TOKEN_SECRET_PROVIDER_URL); } catch { check(false, 'endpoint_invalid'); }
    check(url.protocol === 'https:' && (env.TOKEN_SECRET_PROVIDER_URL === url.origin || env.TOKEN_SECRET_PROVIDER_URL === `${url.origin}/`)
      && ['', '8200'].includes(url.port), 'endpoint_invalid');
    check(typeof env.TOKEN_SECRET_KEY_ID === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$/.test(env.TOKEN_SECRET_KEY_ID), 'key_invalid');
    origins.push(url.origin); keys.push(env.TOKEN_SECRET_KEY_ID);
    mount(app, 'kms-token', '/var/run/secrets/pinvou/kms', true); mount(app, 'kms-ca', '/var/run/secrets/pinvou/kms-ca', true);
    for (const v of app.volumeMounts || []) {
      if (['kms-token', 'kms-ca'].includes(v.name)) continue;
      check(!['/var/run/secrets/pinvou/kms', '/var/run/secrets/pinvou/kms-ca'].some(target =>
        v.mountPath === '/' || v.mountPath === target || target.startsWith(`${v.mountPath}/`) || v.mountPath.startsWith(`${target}/`)), 'mount_shadowed');
    }
    check(!(app.volumeMounts || []).some(v => ['kms-bootstrap', 'kms-agent-config', 'kms-agent-tmp'].includes(v.name)), 'identity_leaked');
    check(agent.volumeMounts.length === 5, 'identity_leaked');
    for (const [name, target, readonly] of [['kms-bootstrap', '/bao/bootstrap', true], ['kms-ca', '/bao/trust', true],
      ['kms-agent-config', '/bao/config', true], ['kms-token', '/bao/token', false], ['kms-agent-tmp', '/tmp', false]]) mount(agent, name, target, readonly);
    const volumes = Object.fromEntries(pod.volumes.map(v => [v.name, v]));
    check(Object.keys(volumes).length === pod.volumes.length, 'mount_unsafe');
    for (const [name, size] of [['kms-token', '1Mi'], ['kms-agent-tmp', '16Mi']]) check(exact(volumes[name], { name, emptyDir: { medium: 'Memory', sizeLimit: size } }), 'mount_unsafe');
    const bootstrap = volumes['kms-bootstrap']?.secret;
    check(bootstrap?.secretName && bootstrap.defaultMode === 288 && exact(bootstrap.items, [{ key: 'role-id', path: 'role-id' }, { key: 'secret-id', path: 'secret-id' }]), 'bootstrap_invalid');
    bootstraps.push(bootstrap.secretName);
    const ca = volumes['kms-ca']?.configMap;
    check(ca?.name && exact(ca.items, [{ key: 'ca.crt', path: 'ca.crt' }]), 'ca_invalid'); caNames.push(ca.name);
    check(volumes['kms-agent-config']?.configMap?.name, 'agent_invalid');
    // No alternate volume alias may expose an AppRole to the application.
    check(pod.volumes.filter(v => v.secret?.secretName === bootstrap.secretName).length === 1, 'identity_leaked');
  }
  check(new Set(bootstraps).size === bootstraps.length && new Set(origins).size === 1 && new Set(keys).size === 1 && new Set(caNames).size === 1, 'shared_identity');
  for (const item of items) {
    if (!item.spec?.template?.spec) continue;
    const pod = item.spec.template.spec;
    for (const c of [...(pod.containers || []), ...(pod.initContainers || []), ...(pod.ephemeralContainers || [])]) {
      check(!(c.env || []).some(e => bootstraps.includes(e.valueFrom?.secretKeyRef?.name))
        && !(c.envFrom || []).some(e => bootstraps.includes(e.secretRef?.name)), 'identity_leaked');
    }
    for (const volume of pod.volumes || []) {
      check(!(volume.projected?.sources || []).some(s => bootstraps.includes(s.secret?.name)), 'identity_leaked');
      if (volume.secret && bootstraps.includes(volume.secret.secretName)) {
        check(workloads.includes(item) && volume.name === 'kms-bootstrap', 'identity_leaked');
      }
    }
  }
}
