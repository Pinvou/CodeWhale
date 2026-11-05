#!/usr/bin/env node

import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const platformRoot = path.join(repositoryRoot, 'account-platform');

const serviceMains = [
  'cmd/account-service/main.go',
  'cmd/billing-service/main.go',
  'cmd/identity-service/main.go',
  'cmd/mock-newapi/main.go',
  'cmd/payment-simulator/main.go',
  'cmd/token-gateway/main.go',
  'cmd/token-portal/main.go',
];

async function goFiles(directory) {
  const entries = await readdir(directory, { withFileTypes:true });
  const nested = await Promise.all(entries.map(async entry => {
    const target = path.join(directory, entry.name);
    if (entry.isDirectory()) return goFiles(target);
    return entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go') ? [target] : [];
  }));
  return nested.flat();
}

export function checkObservabilitySource(relative, source) {
  const violations = [];
  if (relative !== 'internal/platform/observability/logging.go' && /slog\.NewJSONHandler\s*\(/.test(source)) {
    violations.push(`${relative}: bypasses the safe structured logger`);
  }
  if (/slog\.(?:Info|Warn|Error|Debug)(?:Context)?\s*\([\s\S]{0,500}?"(?:password|token|cookie|secret|request_body|response_body|payload|model_body)"\s*,/i.test(source)) {
    violations.push(`${relative}: attempts to attach a protected log field`);
  }
  if (/logger\.(?:Info|Warn|Error|Debug)\s*\([\s\S]{0,500}?"(?:password|token|cookie|secret|request_body|response_body|payload|model_body)"\s*,/i.test(source)) {
    violations.push(`${relative}: attempts to attach a protected logger field`);
  }
  if (/api\.WriteError\([^\n]*\.Error\(\)/.test(source)) {
    violations.push(`${relative}: returns an unclassified internal error to a client`);
  }
  if (/(?:Stop|Reschedule)Reconciliation\([^\n]*\.Error\(\)/.test(source)) {
    violations.push(`${relative}: persists an unclassified provider error`);
  }
  if (/\.RecordError\s*\(/.test(source)) {
    violations.push(`${relative}: records an unclassified error message in a trace`);
  }
  if (/otelhttp\.New(?:Handler|Transport)\s*\(/.test(source)) {
    violations.push(`${relative}: enables uncontrolled HTTP trace attributes`);
  }
  return violations;
}

export async function checkRepository() {
  const violations = [];
  for (const relative of serviceMains) {
    const source = await readFile(path.join(platformRoot, relative), 'utf8');
    if (!source.includes('httpserver.ConfigureProcessLogging(')) {
      violations.push(`${relative}: service does not configure the shared structured logger before startup`);
    }
  }
  for (const file of await goFiles(platformRoot)) {
    const relative = path.relative(platformRoot, file).replaceAll('\\', '/');
    violations.push(...checkObservabilitySource(relative, await readFile(file, 'utf8')));
  }
  const server = await readFile(path.join(platformRoot, 'internal/platform/httpserver/server.go'), 'utf8');
  const client = await readFile(path.join(platformRoot, 'internal/platform/observability/client.go'), 'utf8');
  const nats = await readFile(path.join(platformRoot, 'internal/platform/events/nats.go'), 'utf8');
  const relayTemplate = await readFile(path.join(platformRoot, 'deploy/helm/account-platform/templates/event-relays.yaml'), 'utf8');
  if (!server.includes('ValidRequestID') || !server.includes('X-Request-ID')) violations.push('HTTP request ID validation is not enabled');
  if (!client.includes('clone.Header.Set("X-Request-ID"')) violations.push('outbound HTTP request ID propagation is missing');
  if (!nats.includes('ContextForTrace') || !nats.includes('messaging.consume')) violations.push('NATS trace restoration is missing');
  if (!relayTemplate.includes('path: /health/ready')) violations.push('event relay readiness is not dependency-aware');
  if (violations.length > 0) throw new Error(`Account Platform observability gate failed:\n${violations.join('\n')}`);
}

if (path.resolve(process.argv[1] || '') === fileURLToPath(import.meta.url)) {
  await checkRepository();
  process.stdout.write('Account Platform observability gate passed.\n');
}
