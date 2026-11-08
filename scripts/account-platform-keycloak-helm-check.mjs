import { isMainModule } from './account-platform-cli-entry.mjs';
import { parseHelmRender, readGateFile, requireGate as check, stableJSON } from './account-platform-production-render.mjs';
const same = (a, b) => stableJSON(a) === stableJSON(b);

// Preserve the original rendered health/session/token-proxy contract. This is a
// config check; it does not contact Keycloak or install an ingress controller.
export function verifyRenderedKeycloak(source) {
  const resources = typeof source === 'string' ? parseHelmRender(source) : source;
  const found = resources.filter(r => r.kind === 'Deployment' && r.metadata?.name?.endsWith('-keycloak'));
  check(found.length === 1, 'keycloak_deployment');
  const containers = found[0].spec?.template?.spec?.containers?.filter(c => c.name === 'keycloak');
  check(containers?.length === 1, 'keycloak_container');
  const container = containers[0];
  check(container.ports?.some(p => p.name === 'management' && p.containerPort === 9000)
    && same(container.startupProbe?.httpGet, { path: '/health/started', port: 'management', scheme: 'HTTP' })
    && same(container.readinessProbe?.httpGet, { path: '/health/ready', port: 'management', scheme: 'HTTP' })
    && same(container.livenessProbe?.httpGet, { path: '/health/live', port: 'management', scheme: 'HTTP' }), 'keycloak_health');
  check(['requests', 'limits'].every(bound => ['cpu', 'memory'].every(resource =>
    container.resources?.[bound]?.[resource] !== undefined)), 'keycloak_resources');
  const ingresses = resources.filter(r => r.kind === 'Ingress');
  const sticky = ingresses.filter(r => r.metadata.name.endsWith('-keycloak-session'));
  check(sticky.length === 1 && sticky[0].spec?.ingressClassName === 'nginx', 'keycloak_sticky');
  const session = sticky[0];
  check(same(session.metadata.annotations, {
    'nginx.ingress.kubernetes.io/affinity': 'cookie',
    'nginx.ingress.kubernetes.io/affinity-mode': 'persistent',
    'nginx.ingress.kubernetes.io/session-cookie-name': 'pinvou-keycloak-route',
    'nginx.ingress.kubernetes.io/session-cookie-path': '/',
    'nginx.ingress.kubernetes.io/session-cookie-samesite': 'Lax',
    'nginx.ingress.kubernetes.io/session-cookie-secure': 'true',
    'nginx.ingress.kubernetes.io/session-cookie-change-on-failure': 'true',
  }) && session.spec.rules?.length === 1, 'keycloak_sticky');
  const paths = session.spec.rules[0].http?.paths;
  check(same(paths?.map(p => [p.path, p.pathType]), [['/realms', 'Prefix'], ['/resources', 'Prefix']])
    && paths.every(p => p.backend?.service?.name === found[0].metadata.name && p.backend.service.port?.number === 8080), 'keycloak_session_scope');
  const tokenPath = '/realms/pinvou/protocol/openid-connect/token';
  const proxy = ingresses.flatMap(ingress => (ingress.spec?.rules || []).filter(rule =>
    rule.http?.paths?.some(path => path.path === tokenPath)).map(rule => ({ ingress, rule })));
  check(proxy.length === 1 && proxy[0].ingress !== session && proxy[0].ingress.spec.ingressClassName === 'nginx'
    && proxy[0].rule.host === session.spec.rules[0].host
    && same(proxy[0].rule.http.paths.map(p => [p.path, p.pathType]), [[tokenPath, 'Exact']])
    && proxy[0].rule.http.paths[0].backend?.service?.name === found[0].metadata.name.replace(/-keycloak$/, '-identity-service')
    && proxy[0].rule.http.paths[0].backend.service.port?.number === 8080
    && proxy[0].ingress.metadata.annotations?.['nginx.ingress.kubernetes.io/affinity'] === undefined, 'keycloak_token_proxy');
  return { decision: 'KEYCLOAK_RENDER_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' };
}
if (isMainModule(import.meta.url)) {
  try {
    check(process.argv.length === 3, 'usage');
    process.stdout.write(JSON.stringify(verifyRenderedKeycloak(readGateFile(process.argv[2]))) + '\n');
  } catch (error) {
    process.stderr.write((/^production_render_[a-z_]+$/.test(error?.message) ? error.message : 'production_render_input_rejected') + '\n');
    process.exitCode = 1;
  }
}
