// Validate the rendered NATS credential preparation boundary, not live K8s
// ownership, broker trust, ACLs or the separately provisioned JetStream inputs.
const component = item => item?.spec?.template?.metadata?.labels?.['app.kubernetes.io/component'];
const env = container => Object.fromEntries((container.env || []).map(v => [v.name, v.value]));
const equal = (a, b) => JSON.stringify(a) === JSON.stringify(b);
function check(value, code) { if (!value) throw new Error(`model_token_deployment_nats_${code}`); }
function mount(container, name, target, readonly) {
  const found = (container.volumeMounts || []).filter(v => v.name === name);
  check(found.length === 1 && equal(found[0], { name, mountPath: target, ...(readonly ? { readOnly: true } : {}) }), 'mount');
}
export function checkHelmNATSCredentials(items) {
  const governed = items.some(v => v.kind === 'Deployment'
    && ['token-command-executor', 'token-credential-portal'].includes(component(v)));
  for (const item of items.filter(v => v.kind === 'Deployment')) {
    const name = component(item), pod = item.spec.template.spec, main = pod.containers[0], values = env(main);
    const clients = name === 'account-event-relay' ? ['account-event-relay', 'account-approval-relay']
      : name === 'billing-event-relay' ? ['billing-event-relay']
        : name === 'billing-service' ? ['billing-command-executor']
          : name === 'token-command-executor' ? ['token-command-executor']
            : ['account-service', 'token-gateway'].includes(name)
              && (governed || values.NATS_CREDENTIALS_FILE) ? [name] : [];
    if (!clients.length) continue;
    check(values.NATS_CREDENTIALS_FILE === `/run/pinvou/nats/private/${clients[0]}.creds`, 'runtime_path');
    if (name === 'account-event-relay') check(values.APPROVAL_RELAY_NATS_CREDENTIALS_FILE === '/run/pinvou/nats/private/account-approval-relay.creds', 'runtime_path');
    check(pod.securityContext?.runAsNonRoot === true && pod.securityContext.runAsUser === 65532
      && pod.securityContext.runAsGroup === 65532 && pod.securityContext.fsGroup === 65532
      && (main.securityContext?.runAsUser ?? pod.securityContext.runAsUser) === 65532
      && (main.securityContext?.runAsGroup ?? pod.securityContext.runAsGroup) === 65532
      && (main.securityContext?.runAsNonRoot ?? pod.securityContext.runAsNonRoot) === true
      && pod.automountServiceAccountToken === false && !pod.hostNetwork && !pod.hostPID && !pod.hostIPC
      && !pod.shareProcessNamespace && !pod.ephemeralContainers?.length, 'runtime_identity');
    const volumes = Object.fromEntries((pod.volumes || []).map(v => [v.name, v]));
    check(Object.keys(volumes).length === pod.volumes.length
      && equal(volumes['nats-private'], { name: 'nats-private', emptyDir: { medium: 'Memory', sizeLimit: '1Mi' } }), 'private_volume');
    mount(main, 'nats-private', '/run/pinvou/nats', true);
    const inits = pod.initContainers || [];
    check(inits.length === clients.length, 'init_scope');
    const sourceNames = clients.map(client => `nats-source-${client}`);
    for (const client of clients) {
      const init = inits.find(c => c.name === `prepare-nats-${client}`);
      check(init && init.image === main.image && init.imagePullPolicy === main.imagePullPolicy
        && equal(init.command, ['/service']) && equal(init.args, ['prepare-nats-credentials', client])
        && !init.env?.length && !init.envFrom && !init.ports?.length && !init.lifecycle
        && !init.readinessProbe && !init.livenessProbe && !init.startupProbe, 'init_scope');
      const security = init.securityContext;
      check(security?.runAsNonRoot === true && security.runAsUser === 65532 && security.runAsGroup === 65532
        && security.allowPrivilegeEscalation === false && security.readOnlyRootFilesystem === true
        && !security.privileged && equal(security.capabilities?.drop, ['ALL']) && !security.capabilities?.add?.length, 'init_identity');
      check(init.volumeMounts?.length === 2, 'init_scope');
      mount(init, `nats-source-${client}`, '/var/run/bootstrap/pinvou/nats', true);
      mount(init, 'nats-private', '/run/pinvou/nats', false);
      const sourceVolume = volumes[`nats-source-${client}`], source = sourceVolume?.secret;
      check(source?.secretName && source.defaultMode === 288 && !source.optional
        && Object.keys(sourceVolume).sort().join(',') === 'name,secret'
        && equal(source.items, [{ key: `${client}.creds`, path: `${client}.creds` }]), 'source_scope');
      // No volume alias can turn the same credential Secret into an unfiltered
      // projection, or expose another copy of its NATS key to the application.
      for (const volume of pod.volumes) if (volume.secret?.secretName === source.secretName && !sourceNames.includes(volume.name)) {
        check(volume.secret.items?.length && !volume.secret.items.some(v => v.key.endsWith('.creds')), 'source_scope');
      }
      for (const volume of pod.volumes) for (const projection of volume.projected?.sources || []) {
        if (projection.secret?.name === source.secretName) check(projection.secret.items?.length
          && !projection.secret.items.some(v => v.key.endsWith('.creds')), 'source_scope');
      }
    }
    for (const container of pod.containers) for (const v of container.volumeMounts || []) {
      check(!sourceNames.includes(v.name) && (v.name === 'nats-private'
        ? container === main && v.readOnly === true
        : !(v.mountPath === '/' || v.mountPath === '/run' || v.mountPath === '/run/pinvou' || v.mountPath.startsWith('/run/pinvou/nats')
          || v.mountPath.startsWith('/var/run/bootstrap/pinvou/nats'))), 'source_leaked');
    }
  }
}
