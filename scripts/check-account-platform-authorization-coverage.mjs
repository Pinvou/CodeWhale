#!/usr/bin/env node

import { readdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath, pathToFileURL } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const defaultCatalogPath = path.join(repositoryRoot, 'account-platform', 'deploy', 'authorization-entrypoints.json');
const routePattern = /\b([A-Za-z][A-Za-z0-9_]*)\.(GET|POST|PUT|PATCH|DELETE)\("([^"]+)"/g;
const scopePrefixes = Object.freeze({ personal: ['personal.'], platform: ['platform.'], organization: ['org.'], active: ['personal.', 'org.'] });

function routeKey(item) {
  return `${item.source}|${item.receiver}|${item.method}|${item.path}`;
}

function taskKey(item) {
  return `${item.source}|${item.name}`;
}

function eventKey(item) {
  return `${item.source}|${item.durable}|${item.subject}`;
}

function exactInternalPermission(permission) {
  return /^internal\.[a-z0-9][a-z0-9.-]*$/.test(permission) && !permission.includes('*');
}

function stableStrategy(strategy) {
  return /^(protocol|authenticated|domain|workload)\.[a-z0-9][a-z0-9.-]*$/.test(String(strategy || ''));
}

async function source(relative) {
  return readFile(path.join(repositoryRoot, relative), 'utf8');
}

async function productionGoSources(relativeDirectory) {
  const result = [];
  async function walk(relative) {
    const entries = await readdir(path.join(repositoryRoot, relative), { withFileTypes: true });
    for (const entry of entries) {
      const child = `${relative}/${entry.name}`;
      if (entry.isDirectory()) await walk(child);
      else if (entry.isFile() && entry.name.endsWith('.go') && !entry.name.endsWith('_test.go')) result.push(child);
    }
  }
  await walk(relativeDirectory);
  return result;
}

async function knownHumanPermissions() {
  const [migration, authorization] = await Promise.all([
    source('account-platform/migrations/account/000034_three_scope_authorization.up.sql'),
    source('account-platform/migrations/account/000037_recharge_product_permissions.up.sql'),
    source('account-platform/internal/account/authorization.go'),
  ]);
  const result = new Set();
  for (const match of `${migration}\n${authorization}`.matchAll(/["']((?:personal|platform|org)\.[a-z0-9._-]+)["']/g)) result.add(match[1]);
  return result;
}

async function discover(catalog) {
  const routes = [];
  const tasks = [];
  const events = [];
  const textBySource = new Map();
  const productionGo = [...(await productionGoSources('account-platform/internal')), ...(await productionGoSources('account-platform/cmd'))];
  const sources = new Set([
    'account-platform/internal/account/authorization.go',
    ...productionGo.filter((relative) => relative.startsWith('account-platform/cmd/')),
    ...catalog.http_sources,
    ...catalog.tasks.map((item) => item.source),
    ...catalog.events.map((item) => item.source),
    ...catalog.cli.map((item) => item.source),
  ]);
  for (const relative of sources) textBySource.set(relative, await source(relative));
  const httpSources = [];
  for (const relative of productionGo) {
    const text = textBySource.get(relative) || (await source(relative));
    if (!textBySource.has(relative)) textBySource.set(relative, text);
    routePattern.lastIndex = 0;
    if (routePattern.test(text)) httpSources.push(relative);
    routePattern.lastIndex = 0;
  }
  for (const relative of httpSources) {
    const text = textBySource.get(relative);
    for (const match of text.matchAll(routePattern)) {
      const lineStart = text.lastIndexOf('\n', match.index) + 1;
      const lineEnd = text.indexOf('\n', match.index);
      routes.push({ source: relative, receiver: match[1], method: match[2], path: match[3], registration: text.slice(lineStart, lineEnd < 0 ? text.length : lineEnd) });
    }
  }
  for (const [relative, text] of textBySource) {
    for (const match of text.matchAll(/RunBackground\([^,\n]+,\s*"([^"]+)"/g)) tasks.push({ source: relative, name: match[1] });
    for (const match of text.matchAll(/broker\.Consume\([^,\n]+,\s*"([^"]+)",\s*"([^"]+)"/g)) events.push({ source: relative, durable: match[1], subject: match[2] });
  }
  events.push({ source: 'account-platform/cmd/event-relay/main.go', durable: 'source-bound-outbox-relay', subject: 'account|billing|token.>' });
  const cliRoot = path.join(repositoryRoot, 'account-platform', 'cmd');
  const cli = (await readdir(cliRoot, { withFileTypes: true })).filter((entry) => entry.isDirectory()).map((entry) => ({ name: entry.name, source: `account-platform/cmd/${entry.name}/main.go` }));
  return { routes, tasks, events, cli, httpSources, textBySource };
}

function addUnique(map, item, key, label, errors) {
  const value = key(item);
  if (map.has(value)) errors.push(`duplicate_${label}:${value}`);
  map.set(value, item);
}

export function validateCatalog(catalog, discovered, knownPermissions) {
  const errors = [];
  if (catalog.version !== 1) errors.push('authorization_catalog_version_invalid');
  const policies = new Map();
  for (const item of catalog.http_policies || []) addUnique(policies, item, routeKey, 'http_policy', errors);
  const seenPolicies = new Set();
  const registeredHTTPSourceSet = new Set(catalog.http_sources || []);
  const discoveredHTTPSourceSet = new Set(discovered.httpSources);
  for (const relative of discoveredHTTPSourceSet) if (!registeredHTTPSourceSet.has(relative)) errors.push(`unregistered_http_source:${relative}`);
  for (const relative of registeredHTTPSourceSet) if (!discoveredHTTPSourceSet.has(relative)) errors.push(`stale_http_source:${relative}`);
  const constantValues = new Map();
  const authorizationSource = discovered.textBySource.get('account-platform/internal/account/authorization.go') || '';
  for (const match of authorizationSource.matchAll(/\b(Permission[A-Za-z0-9]+)\s*=\s*"([^"]+)"/g)) constantValues.set(match[1], match[2]);

  for (const route of discovered.routes) {
    const key = routeKey(route);
    const workload = route.registration.match(/serviceauth\.Require\("([^"]+)"\)/)?.[1];
    const literalHuman = route.registration.match(/authorizer\.Require\("([^"]+)"\)/)?.[1];
    const constant = route.registration.match(/requirePermission\((Permission[A-Za-z0-9]+)\)/)?.[1];
    if (workload) {
      if (!exactInternalPermission(workload)) errors.push(`invalid_workload_permission:${key}:${workload}`);
      continue;
    }
    const literalAnyArguments = route.registration.match(/authorizer\.RequireAny\(([^)]*)\)/)?.[1] || '';
    const literalAny = [...literalAnyArguments.matchAll(/"([^"]+)"/g)].map((match) => match[1]);
    const constantAnyArguments = route.registration.match(/requireAnyPermission\(([^)]*)\)/)?.[1] || '';
    const constantAny = [...constantAnyArguments.matchAll(/Permission[A-Za-z0-9]+/g)].map((match) => constantValues.get(match[0])).filter(Boolean);
    const inlineHumans = [literalHuman || (constant ? constantValues.get(constant) : ''), ...literalAny, ...constantAny].filter(Boolean);
    if (inlineHumans.length > 0) {
      for (const inlineHuman of inlineHumans) if (!knownPermissions.has(inlineHuman)) errors.push(`unknown_human_permission:${key}:${inlineHuman}`);
      continue;
    }
    const policy = policies.get(key);
    if (!policy) {
      errors.push(`unclassified_http_entrypoint:${key}`);
      continue;
    }
    seenPolicies.add(key);
    if (!stableStrategy(policy.strategy)) errors.push(`unstable_http_strategy:${key}`);
    if (!['public', 'authenticated', 'personal', 'platform', 'organization', 'active'].includes(policy.scope)) errors.push(`invalid_http_scope:${key}:${policy.scope}`);
    const permissions = policy.permissions || [];
    if (['personal', 'platform', 'organization', 'active'].includes(policy.scope) && !policy.strategy.startsWith('domain.approval-policy') && permissions.length === 0) errors.push(`missing_human_permission:${key}`);
    for (const permission of permissions) {
      if (!knownPermissions.has(permission)) errors.push(`unknown_human_permission:${key}:${permission}`);
      const prefixes = scopePrefixes[policy.scope] || [];
      if (!prefixes.some((prefix) => permission.startsWith(prefix))) errors.push(`permission_scope_mismatch:${key}:${policy.scope}:${permission}`);
    }
  }
  for (const key of policies.keys()) if (!seenPolicies.has(key)) errors.push(`stale_http_policy:${key}`);

  const registeredTasks = new Map();
  for (const item of catalog.tasks || []) {
    addUnique(registeredTasks, item, taskKey, 'task', errors);
    if (item.permission !== `internal.job.${item.name}` || !exactInternalPermission(item.permission)) errors.push(`invalid_task_permission:${taskKey(item)}:${item.permission}`);
  }
  const discoveredTasks = new Set(discovered.tasks.map(taskKey));
  for (const key of discoveredTasks) if (!registeredTasks.has(key)) errors.push(`unclassified_task:${key}`);
  for (const key of registeredTasks.keys()) if (!discoveredTasks.has(key)) errors.push(`stale_task_policy:${key}`);

  const registeredEvents = new Map();
  for (const item of catalog.events || []) {
    addUnique(registeredEvents, item, eventKey, 'event', errors);
    if (!exactInternalPermission(item.permission)) errors.push(`invalid_event_permission:${eventKey(item)}:${item.permission}`);
  }
  const discoveredEvents = new Set(discovered.events.map(eventKey));
  for (const key of discoveredEvents) if (!registeredEvents.has(key)) errors.push(`unclassified_event:${key}`);
  for (const key of registeredEvents.keys()) if (!discoveredEvents.has(key)) errors.push(`stale_event_policy:${key}`);

  const registeredCLI = new Map();
  for (const item of catalog.cli || []) {
    addUnique(registeredCLI, item, (value) => value.name, 'cli', errors);
    if (!stableStrategy(item.strategy)) errors.push(`unstable_cli_strategy:${item.name}`);
  }
  const discoveredCLI = new Map(discovered.cli.map((item) => [item.name, item]));
  for (const [name, item] of discoveredCLI) {
    const registered = registeredCLI.get(name);
    if (!registered) errors.push(`unclassified_cli:${name}`);
    else if (registered.source !== item.source) errors.push(`cli_source_mismatch:${name}`);
  }
  for (const name of registeredCLI.keys()) if (!discoveredCLI.has(name)) errors.push(`stale_cli_policy:${name}`);
  return errors;
}

export async function runAuthorizationCoverage(catalogPath = defaultCatalogPath) {
  const catalog = JSON.parse(await readFile(catalogPath, 'utf8'));
  const [discovered, permissions] = await Promise.all([discover(catalog), knownHumanPermissions()]);
  const errors = validateCatalog(catalog, discovered, permissions);
  if (errors.length > 0) throw new Error(errors.join('\n'));
  return { routes: discovered.routes.length, tasks: discovered.tasks.length, events: discovered.events.length, cli: discovered.cli.length };
}

if (process.argv[1] && pathToFileURL(path.resolve(process.argv[1])).href === import.meta.url) {
  try {
    const result = await runAuthorizationCoverage(process.argv[2] ? path.resolve(process.argv[2]) : defaultCatalogPath);
    process.stdout.write(`authorization coverage passed: ${result.routes} HTTP routes, ${result.tasks} tasks, ${result.events} event entries, ${result.cli} CLI commands\n`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
