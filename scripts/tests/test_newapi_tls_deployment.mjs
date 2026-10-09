import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { createRequire } from 'node:module'
import { test } from 'node:test'

const require = createRequire(new URL('../../pinvou3-app/package.json', import.meta.url))
const { parseAllDocuments } = require('yaml')
const root = new URL('../../', import.meta.url)
const read = (path) => readFileSync(new URL(path, root), 'utf8')
const docs = parseAllDocuments(read('account-platform/deploy/production/newapi.yaml.template'))
for (const doc of docs) assert.equal(doc.errors.length, 0)
const [deployment, service, policy] = docs.map((doc) => doc.toJSON())
const pod = deployment.spec.template.spec
const tls = pod.containers.find((container) => container.name === 'newapi-tls')
const backend = pod.containers.find((container) => container.name === 'new-api')

test('same pod isolates the HTTP backend and exposes only private TLS', () => {
  assert.equal(pod.containers.length, 2)
  assert.equal(pod.hostNetwork, undefined)
  assert.equal(backend.ports, undefined)
  assert.equal(backend.env.find((item) => item.name === 'PORT').value, '8080')
  assert.equal(service.spec.type, 'ClusterIP')
  assert.deepEqual(service.spec.ports, [{ name: 'https', port: 8443, targetPort: 'https', protocol: 'TCP' }])
  assert.deepEqual(service.spec.selector, deployment.spec.selector.matchLabels)
  assert.deepEqual(deployment.spec.template.metadata.labels, service.spec.selector)
  assert.equal(tls.ports[0].hostPort, undefined)
})

test('current PKI private key mounts only in TLS and app uses secret references', () => {
  assert.equal(pod.automountServiceAccountToken, false)
  assert.equal(pod.securityContext.runAsNonRoot, true)
  for (const container of pod.containers) {
    assert.equal(container.securityContext.readOnlyRootFilesystem, true)
    assert.equal(container.securityContext.allowPrivilegeEscalation, false)
    assert.deepEqual(container.securityContext.capabilities.drop, ['ALL'])
  }
  assert.equal(backend.volumeMounts.some((mount) => mount.name === 'newapi-tls'), false)
  assert.deepEqual(tls.volumeMounts, [{ name: 'newapi-tls', mountPath: '/run/pinvou/newapi-tls', readOnly: true }])
  const secret = pod.volumes.find((volume) => volume.name === 'newapi-tls').secret
  assert.equal(secret.defaultMode, 0o440)
  assert.deepEqual(secret.items, [{ key: 'server.crt', path: 'server.crt' }, { key: 'server.key', path: 'server.key' }])
  for (const name of ['SQL_DSN', 'REDIS_CONN_STRING', 'SESSION_SECRET', 'CRYPTO_SECRET']) {
    const item = backend.env.find((entry) => entry.name === name)
    assert.equal(item.value, undefined)
    assert.equal(item.valueFrom.secretKeyRef.key, name)
  }
  assert.match(backend.image, /@sha256:[a-f0-9]{64}$/)
  assert.equal(tls.image, '__NEWAPI_TLS_IMAGE_AT_DIGEST__')
})

test('network boundary denies HTTP and permits only explicit client and supplier facts', () => {
  assert.deepEqual(policy.spec.podSelector.matchLabels, deployment.spec.selector.matchLabels)
  assert.deepEqual(policy.spec.policyTypes, ['Ingress', 'Egress'])
  assert.deepEqual(policy.spec.ingress, [{ from: [
    { ipBlock: { cidr: '__TOKEN_GATEWAY_CIDR__' } },
    { ipBlock: { cidr: '__CREDENTIAL_PORTAL_CIDR__' } },
  ], ports: [{ protocol: 'TCP', port: 8443 }] }])
  assert.deepEqual(policy.spec.egress.slice(1).map((rule) => rule.ports[0].port), [5432, 6379, 443])
  assert.deepEqual(policy.spec.egress.slice(1).map((rule) => rule.to[0].ipBlock.cidr), ['__POSTGRES_CIDR__', '__NEWAPI_REDIS_CIDR__', '__MODEL_PROVIDER_CIDR__'])
  assert.equal(JSON.stringify(policy).includes('0.0.0.0/0'), false)
  assert.equal(JSON.stringify(policy).includes('8080'), false)
})

test('existing Docker CLI and static probes are delivered with current files', () => {
  assert.match(read('account-platform/Dockerfile'), /token-command-executor\|newapi-tls\)/)
  assert.match(read('account-platform/cmd/newapi-tls/main.go'), /NEWAPI_TLS_CERT_FILE/)
  assert.match(read('account-platform/cmd/newapi-tls/main.go'), /NEWAPI_TLS_KEY_FILE/)
  for (const [name, path] of [['readinessProbe', '/health/ready'], ['livenessProbe', '/health/live']]) {
    assert.deepEqual(tls[name].httpGet, { path, port: 'https', scheme: 'HTTPS' })
  }
  assert.match(read('account-platform/deploy/production/README.models.md'), /README.newapi.md/)
  assert.match(read('account-platform/deploy/production/README.newapi.md'), /kubectl apply --dry-run=server/)
})
