import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');

test('observability assets use safe low-cardinality dimensions', async () => {
  const metrics = await readFile(path.join(root, 'internal/platform/httpserver/metrics.go'), 'utf8');
  const relay = await readFile(path.join(root, 'cmd/event-relay/metrics.go'), 'utf8');
  const relayDeployment = await readFile(path.join(root, 'deploy/helm/account-platform/templates/event-relays.yaml'), 'utf8');
  const rules = await readFile(path.join(root, 'deploy/helm/account-platform/templates/prometheus-rules.yaml'), 'utf8');
  const dashboard = JSON.parse(await readFile(path.join(root, 'deploy/dashboards/account-platform-overview.json'), 'utf8'));
  assert.match(metrics, /pinvou_http_requests_total/);
  assert.match(metrics, /pinvou_service_readiness/);
  assert.match(metrics, /ObserveWithExemplar/);
  assert.match(relay, /pinvou_outbox_oldest_pending_age_seconds/);
  assert.match(relay, /pinvou_relay_events_total/);
  assert.match(relayDeployment, /OTEL_EXPORTER_OTLP_ENDPOINT/);
  assert.match(relayDeployment, /path: \/health\/ready/);
  assert.match(rules, /PinvouAccountPlatformOutboxBacklog/);
  assert.equal(dashboard.uid, 'pinvou-account-platform');
  for (const source of [metrics, relay, rules]) {
    assert.doesNotMatch(source, /\[\]string\{[^}]*?(user_id|organization|wallet|order|token|usage_request|qr)/i);
  }
});

test('production tracing requires HTTPS and metrics stay off the public listener', async () => {
  const tracing = await readFile(path.join(root, 'internal/platform/observability/tracing.go'), 'utf8');
  const server = await readFile(path.join(root, 'internal/platform/httpserver/server.go'), 'utf8');
  assert.match(tracing, /production OTLP export requires HTTPS/);
  assert.match(server, /metricsAddress = ":9090"/);
  assert.match(server, /ValidRequestID/);
  assert.doesNotMatch(server, /mux\.Handle\("GET \/metrics"/);
});
