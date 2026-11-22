#!/usr/bin/env node

import { readdir, readFile } from 'node:fs/promises';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath, pathToFileURL } from 'node:url';

const repositoryRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const defaultCatalogPath = path.join(repositoryRoot, 'account-platform', 'deploy', 'authorization-entrypoints.json');
const routePattern = /\b([A-Za-z][A-Za-z0-9_]*)\.(GET|POST|PUT|PATCH|DELETE|Any)\("([^"]+)"/g;
const humanPermissionSources = Object.freeze([
  'account-platform/internal/account/authorization.go',
  'account-platform/internal/account/platform_roles.go',
]);
const scopePrefixes = Object.freeze({ personal: ['personal.'], platform: ['platform.'], organization: ['org.'], active: ['personal.', 'org.'] });
const requiredActionSuffixes = new Map([
  ['account-platform/internal/billing/organization_routes.go|POST|/internal/v1/organizations/:organization_id/quota-grants:batch', '/quota-grants:batch'],
  ['account-platform/internal/token/management_routes.go|POST|/commands:precheck', '/commands:precheck'],
  ['account-platform/internal/token/organization_finance_routes.go|POST|/organizations/:organization_id/account-closed-grants:reclaim-batch', '/account-closed-grants:reclaim-batch'],
]);

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
  // Read the installed phase's permission definitions, not future migration
  // filenames or a runtime database intersection that could hide omissions.
  const directory = 'account-platform/migrations/account';
  const entries = await readdir(path.join(repositoryRoot, directory), { withFileTypes: true });
  const names = entries.filter(entry => entry.isFile() && entry.name.endsWith('.up.sql')).map(entry => `${directory}/${entry.name}`);
  const texts = await Promise.all([...names, ...humanPermissionSources].map(source));
  const permissions = new Set();
  for (const match of texts.join('\n').matchAll(/["']((?:personal|platform|org)\.[a-z0-9._-]+)["']/g)) permissions.add(match[1]);
  return permissions;
}

// The Billing process installs consumers from the closed registry rather than
// string literals. Parse that current declarative shape completely: a new
// dynamic shape must fail coverage instead of silently losing a consumer.
export function goSource(text) {
  const regions = /"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|`[^`]*`|\/\/[^\n]*|\/\*[\s\S]*?\*\//g;
  const blank = part => part.replace(/[^\r\n]/g, ' ');
  return {
    code: text.replace(regions, part => part.startsWith('//') || part.startsWith('/*') ? blank(part) : part),
    masked: text.replace(regions, blank),
  };
}

// Account registers workers before starting the supervised group. Read those
// literal declarations as well as direct RunBackground calls; the dispatcher's
// task.name is not a new registration. This is a closed source grammar, not a
// Go interpreter: unsupported composite names must fail rather than vanish.
export function authorizationTasks(sourceName, sourceText) {
  const { code, masked } = goSource(sourceText);
  const tasks = [];
  const invalid = () => { throw new Error(`unrecognized_background_task:${sourceName}`); };
  // Split only at this expression's outer commas. Comments and string/rune
  // contents are masked at matching offsets, including braces in raw strings.
  function fields(start, end) {
    const result = [];
    const stack = [];
    let field = start;
    const closing = { '(': ')', '[': ']', '{': '}' };
    for (let index = start; index < end; index += 1) {
      const token = masked[index];
      if (closing[token]) stack.push(closing[token]);
      else if (')]}'.includes(token)) {
        if (stack.pop() !== token) invalid();
      } else if (token === ',' && stack.length === 0) {
        if (!code.slice(field, index).trim()) invalid();
        result.push([field, index]);
        field = index + 1;
      }
    }
    if (stack.length) invalid();
    if (code.slice(field, end).trim()) result.push([field, end]);
    return result;
  }
  function declaration(start, end) {
    const parts = fields(start, end).map(([from, to]) => code.slice(from, to).trim());
    if (parts.length !== 2) invalid();
    let name;
    if (/^name\s*:/.test(parts[0]) || /^run\s*:/.test(parts[0])) {
      const keyed = new Map();
      for (const part of parts) {
        const match = part.match(/^(name|run)\s*:\s*([\s\S]+)$/);
        if (!match || keyed.has(match[1])) invalid();
        keyed.set(match[1], match[2].trim());
      }
      if (!keyed.has('name') || !keyed.has('run')) invalid();
      name = keyed.get('name');
    } else {
      [name] = parts;
      if (/^[A-Za-z_]\w*\s*:/.test(parts[1])) invalid();
    }
    // Actual job permission identities use these canonical literal names.
    if (!/^"[a-z][a-z0-9-]*"$/.test(name)) invalid();
    tasks.push({ source: sourceName, name: name.slice(1, -1) });
  }
  // The one dynamic dispatch belongs to this unchanged supervised lifecycle,
  // not an exemption for arbitrary task.name calls elsewhere in the package.
  const dispatcher = [...code.matchAll(/func startAccountBackground\(ctx context\.Context, tasks \[\]accountBackgroundTask\) <-chan struct\{\} \{[\s\S]*?\n\}/g)]
    .filter(match => masked[match.index] !== ' ');
  const expectedDispatcher = `func startAccountBackground(ctx context.Context, tasks []accountBackgroundTask) <-chan struct{} {
    done := make(chan struct{})
    var workers sync.WaitGroup
    workers.Add(len(tasks))
    for _, task := range tasks {
      go func() {
        defer workers.Done()
        observability.RunBackground(ctx, task.name, task.run)
      }()
    }
    go func() {
      workers.Wait()
      close(done)
    }()
    return done
  }`.replace(/\s/g, '');
  for (const match of masked.matchAll(/\b(?:[A-Za-z_]\w*\s*\.\s*)?RunBackground\b/g)) {
    // The function declaration itself registers no worker.
    if (/\bfunc\s*$/.test(masked.slice(0, match.index))) continue;
    const next = match.index + match[0].length;
    const open = next + masked.slice(next).search(/\S/);
    if (masked[open] !== '(') invalid();
    let depth = 1;
    let end = open + 1;
    for (; end < masked.length && depth > 0; end += 1) {
      if (masked[end] === '(') depth += 1;
      if (masked[end] === ')') depth -= 1;
    }
    if (depth) invalid();
    const args = fields(open + 1, end - 1).map(([from, to]) => code.slice(from, to).trim());
    if (args.length !== 3 || match[0].replace(/\s/g, '') !== 'observability.RunBackground') invalid();
    if (/^"[a-z][a-z0-9-]*"$/.test(args[1])) {
      tasks.push({source: sourceName, name: args[1].slice(1, -1)});
    } else if (!(sourceName === 'account-platform/cmd/account-service/background.go'
        && args.map(arg => arg.replace(/\s/g, '')).join(',') === 'ctx,task.name,task.run'
        && dispatcher.length === 1 && dispatcher[0][0].replace(/\s/g, '') === expectedDispatcher
        && match.index >= dispatcher[0].index && end <= dispatcher[0].index + dispatcher[0][0].length)) invalid();
  }
  for (const match of masked.matchAll(/\baccountBackgroundTask\s*\{/g)) {
    const open = match.index + match[0].lastIndexOf('{');
    let depth = 1;
    let end = open + 1;
    for (; end < masked.length && depth > 0; end += 1) {
      if (masked[end] === '{') depth += 1;
      if (masked[end] === '}') depth -= 1;
    }
    if (depth) invalid();
    if (/\[\s*\]\s*$/.test(masked.slice(0, match.index))) {
      for (const [from, to] of fields(open + 1, end - 1)) {
        const item = masked.slice(from, to).trim();
        if (!item.startsWith('{') || !item.endsWith('}')) invalid();
        const first = from + masked.slice(from, to).indexOf('{');
        const last = from + masked.slice(from, to).lastIndexOf('}');
        declaration(first + 1, last);
      }
    } else declaration(open + 1, end - 1);
  }
  // A literal registry is not evidence of its runtime names if callers later
  // overwrite elements, mutate fields, or hand a writable alias to a helper.
  // Account's current list has a closed set of uses: declaration, appending a
  // literal (or configureApprovals' returned list), and supervised startup.
  const registries = new Map();
  for (const match of masked.matchAll(/\b([A-Za-z_]\w*)\s*:=\s*(?:\[\s*\]\s*)?accountBackgroundTask\s*\{/g)) {
    if (registries.has(match[1])) invalid();
    registries.set(match[1], new Set([match.index]));
  }
  const approvals = [...masked.matchAll(/\bapprovalWorkers\s*,\s*err\s*:=\s*configureApprovals\(/g)];
  const producerReferences = [...masked.matchAll(/\bconfigureApprovals\b/g)];
  if (sourceName === 'account-platform/cmd/account-service/main.go') {
    if (approvals.length !== 1 || producerReferences.length !== 1
        || producerReferences[0].index !== approvals[0].index + approvals[0][0].indexOf('configureApprovals')) invalid();
  } else if (sourceName !== 'account-platform/cmd/account-service/approval_runtime.go' && producerReferences.length) invalid();
  if (sourceName === 'account-platform/cmd/account-service/main.go' && approvals.length === 1) {
    registries.set('approvalWorkers', new Set([approvals[0].index]));
  }
  for (const [name, recognized] of registries) {
    for (const match of masked.matchAll(new RegExp(`\\b${name}\\s*=\\s*append\\s*\\(\\s*${name}\\s*,`, 'g'))) {
      if (sourceName === 'account-platform/cmd/account-service/main.go' && name === 'approvalWorkers') invalid();
      const open = match.index + match[0].indexOf('(');
      let depth = 1;
      let end = open + 1;
      for (; end < masked.length && depth > 0; end += 1) {
        if (masked[end] === '(') depth += 1;
        if (masked[end] === ')') depth -= 1;
      }
      if (depth) invalid();
      const args = fields(open + 1, end - 1).map(([from, to]) => code.slice(from, to).trim());
      if (args.length !== 2 || !( /^accountBackgroundTask\s*\{[\s\S]*\}$/.test(args[1])
          || (sourceName === 'account-platform/cmd/account-service/main.go' && name === 'workers'
              && args[1] === 'approvalWorkers...' && registries.has('approvalWorkers')))) invalid();
      recognized.add(match.index);
      recognized.add(open + 1 + masked.slice(open + 1).search(/\S/));
      if (args[1] === 'approvalWorkers...') {
        const second = fields(open + 1, end - 1)[1][0];
        registries.get('approvalWorkers').add(second + masked.slice(second, end - 1).search(/\S/));
      }
    }
    for (const match of masked.matchAll(new RegExp(`\\bstartAccountBackground\\s*\\(\\s*ctx\\s*,\\s*(${name})\\s*\\)`, 'g'))) {
      recognized.add(match.index + match[0].lastIndexOf(name));
    }
  }
  for (const [name, recognized] of registries) {
    if ([...masked.matchAll(new RegExp(`\\b${name}\\b`, 'g'))].some(match => !recognized.has(match.index))) invalid();
  }
  // Production declarations must reach the real main consumer. Keep the
  // supported wiring closed: an unused closure, conditional startup or helper
  // mentioning the same names is not evidence that main starts its registry.
  // Declaration-only parser fixtures outside this production file stay useful.
  if (sourceName === 'account-platform/cmd/account-service/main.go') {
    const mains = [...masked.matchAll(/\bfunc\s+main\s*\(\s*\)\s*\{/g)];
    if (mains.length !== 1 || !registries.has('workers')) invalid();
    const open = mains[0].index + mains[0][0].lastIndexOf('{');
    const blockEnd = start => {
      let depth = 1;
      for (let cursor = start + 1; cursor < masked.length; cursor += 1) {
        if (masked[cursor] === '{') depth += 1;
        if (masked[cursor] === '}' && --depth === 0) return cursor;
      }
      invalid();
    };
    const end = blockEnd(open);
    const closures = [];
    for (const match of masked.matchAll(/\bfunc\b/g)) {
      if (match.index <= open || match.index >= end) continue;
      // Current main has a simple drain closure. Other function signatures
      // need an explicit grammar update, not guessed function boundaries.
      const header = masked.slice(match.index).match(/^func\s*\([^(){}]*\)\s*\{/);
      if (!header) invalid();
      const body = match.index + header[0].lastIndexOf('{');
      closures.push([body, blockEnd(body)]);
    }
    const inMainRegistry = index => index > open && index < end
      && !closures.some(([start, finish]) => index > start && index < finish);
    for (const name of registries.keys()) {
      if ([...masked.matchAll(new RegExp(`\\b${name}\\b`, 'g'))].some(match => !inMainRegistry(match.index))) invalid();
    }
    for (const match of masked.matchAll(/\baccountBackgroundTask\s*\{/g)) {
      const before = masked.slice(0, match.index);
      if (!inMainRegistry(match.index) || !(/\bworkers\s*:=\s*\[\s*\]\s*$/.test(before)
          || /\bworkers\s*=\s*append\s*\(\s*workers\s*,\s*$/.test(before))) invalid();
    }
    const atMainLevel = index => {
      if (index <= open) return false;
      let depth = 1;
      for (let cursor = open + 1; cursor < index; cursor += 1) {
        if (masked[cursor] === '{') depth += 1;
        if (masked[cursor] === '}' && --depth === 0) return false;
      }
      return depth === 1;
    };
    const required = [
      /\bworkers\s*:=\s*\[\s*\]\s*accountBackgroundTask\s*\{/g,
      /\bapprovalWorkers\s*,\s*err\s*:=\s*configureApprovals\(/g,
      /\bworkers\s*=\s*append\s*\(\s*workers\s*,\s*approvalWorkers\s*\.\.\.\s*\)/g,
      /\bworkersDone\s*:=\s*startAccountBackground\s*\(\s*ctx\s*,\s*workers\s*\)/g,
    ].map(pattern => [...masked.matchAll(pattern)]);
    if (required.some(matches => matches.length !== 1 || !atMainLevel(matches[0].index))) invalid();
    if (required.some((matches, index) => index > 0 && matches[0].index <= required[index - 1][0].index)) invalid();
    if ([...masked.matchAll(/\bstartAccountBackground\b/g)].length !== 1) invalid();
    // Starting a slice snapshots its current length. Registrations appended
    // afterwards are declarations but never reach that supervised group.
    const startup = required[3][0].index;
    if ([...masked.matchAll(/\bworkers\s*=\s*append\s*\(/g)].some(match => match.index >= startup)) invalid();
  }
  // The main list's only cross-file producer must return the declared literals
  // themselves. Appending an unknown list or returning a writable temporary
  // would break the provenance promised by the approvalWorkers exception.
  if (sourceName === 'account-platform/cmd/account-service/approval_runtime.go') {
    const producers = [...masked.matchAll(/\bfunc\s+configureApprovals\s*\([^)]*\)\s*\(\s*\[\s*\]\s*accountBackgroundTask\s*,\s*error\s*\)\s*\{/g)];
    if (producers.length !== 1 || producerReferences.length !== 1
        || producerReferences[0].index !== producers[0].index + producers[0][0].indexOf('configureApprovals')) invalid();
    const open = producers[0].index + producers[0][0].lastIndexOf('{');
    let depth = 1;
    let end = open + 1;
    for (; end < masked.length && depth > 0; end += 1) {
      if (masked[end] === '{') depth += 1;
      if (masked[end] === '}') depth -= 1;
    }
    if (depth) invalid();
    // This producer's supported form returns its literal list directly.
    // A nested function's return cannot prove the enclosing producer's result;
    // reject unsupported function scopes rather than count their returns.
    if (/\bfunc\b/.test(masked.slice(open + 1, end - 1))) invalid();
    for (const match of masked.matchAll(/\baccountBackgroundTask\s*\{/g)) {
      if (match.index <= open || match.index >= end
          || !/\breturn\s*\[\s*\]\s*$/.test(masked.slice(open + 1, match.index))) invalid();
    }
    // An error-looking expression may still be nil. Prove the supported error
    // exits: a standard errors.New literal, or the direct err != nil branch.
    const standardErrors = [...masked.matchAll(/\bimport\s*\(/g)].flatMap(match => {
      const begin = match.index + match[0].lastIndexOf('(');
      const finish = masked.indexOf(')', begin + 1);
      if (finish < 0) invalid();
      return code.slice(begin + 1, finish).split('\n').filter(line => line.trim() === '"errors"');
    });
    if (standardErrors.length !== 1) invalid();
    const safeErrorReturns = new Set([...masked.slice(open + 1, end - 1)
      .matchAll(/\bif\s+err\s*!=\s*nil\s*\{\s*return\s+nil\s*,\s*err\s*;?\s*\}/g)]
      .map(match => open + 1 + match.index + match[0].indexOf('return')));
    const errorReferences = new Set();
    let literalReturns = 0;
    for (const match of masked.slice(open + 1, end - 1).matchAll(/\breturn\b/g)) {
      const from = open + 1 + match.index + match[0].length;
      const stack = [];
      const closing = {'(': ')', '[': ']', '{': '}'};
      let to = from;
      for (; to < end - 1; to += 1) {
        const token = masked[to];
        if (stack.length === 0 && (token === '\n' || token === ';')) break;
        if (closing[token]) stack.push(closing[token]);
        else if (')]}'.includes(token) && stack.pop() !== token) invalid();
      }
      if (stack.length) invalid();
      const returnFields = fields(from, to);
      const args = returnFields.map(([start, finish]) => code.slice(start, finish).trim());
      if (args.length !== 2) invalid();
      if (args[0] === 'nil') {
        if (/^errors\.New\("[A-Z][A-Z0-9_]*"\)$/.test(args[1].replace(/\s/g, ''))) {
          const [start, finish] = returnFields[1];
          errorReferences.add(start + masked.slice(start, finish).search(/\S/));
        } else if (!(args[1] === 'err' && safeErrorReturns.has(from - match[0].length))) invalid();
        continue;
      }
      // The success list is the producer's unconditional outer return, not a
      // literal hidden in an optional branch before an empty success return.
      let returnDepth = 1;
      for (let cursor = open + 1; cursor < from - match[0].length; cursor += 1) {
        if (masked[cursor] === '{') returnDepth += 1;
        if (masked[cursor] === '}') returnDepth -= 1;
      }
      if (returnDepth !== 1) invalid();
      if (args[1] !== 'nil' || !/^\[\s*\]\s*accountBackgroundTask\s*\{[\s\S]*\}$/.test(args[0])) invalid();
      literalReturns += 1;
    }
    if (literalReturns !== 1) invalid();
    // A local binding, parameter, or alias named errors must not impersonate
    // the standard constructor whose non-nil result was just relied upon.
    for (const match of masked.slice(producers[0].index, end).matchAll(/\berrors\b/g)) {
      if (!errorReferences.has(producers[0].index + match.index)) invalid();
    }
  }
  // Unknown/zero-value lists and function aliases cannot bypass the literal
  // registry census by entering through the supervised consumer instead.
  for (const match of masked.matchAll(/\bstartAccountBackground\b/g)) {
    if (/\bfunc\s*$/.test(masked.slice(0, match.index))) continue;
    const next = match.index + match[0].length;
    const open = next + masked.slice(next).search(/\S/);
    if (masked[open] !== '(') invalid();
    let depth = 1;
    let end = open + 1;
    for (; end < masked.length && depth > 0; end += 1) {
      if (masked[end] === '(') depth += 1;
      if (masked[end] === ')') depth -= 1;
    }
    if (depth) invalid();
    const args = fields(open + 1, end - 1).map(([from, to]) => code.slice(from, to).trim());
    if (args.length !== 2 || !(registries.has(args[1]) || /^\[\s*\]\s*accountBackgroundTask\s*\{[\s\S]*\}$/.test(args[1]))) invalid();
  }
  return tasks;
}

export function authorizationRoutes(sourceName, sourceText) {
  const { code: text, masked } = goSource(sourceText);
  const routes = [];
  const recognizedSelectors = new Set();
  for (const match of text.matchAll(routePattern)) {
    if (masked[match.index] === ' ') continue;
    recognizedSelectors.add(match.index);
    const lineStart = text.lastIndexOf('\n', match.index) + 1;
    const lineEnd = text.indexOf('\n', match.index);
    const suffix = requiredActionSuffixes.get(`${sourceName}|${match[2]}|${match[3]}`);
    if (suffix) {
      const expression = text.slice(match.index + match[0].length, lineEnd < 0 ? text.length : lineEnd);
      const firstGuard = expression.match(/^\s*,\s*httpserver\s*\.\s*RequirePathSuffix\s*\(\s*"([^"]+)"\s*\)\s*,/);
      const guards = [...goSource(expression).masked.matchAll(/\bhttpserver\s*\.\s*RequirePathSuffix\b/g)];
      if (firstGuard?.[1] !== suffix || guards.length !== 1) {
        throw new Error(`invalid_path_suffix_guard:${sourceName}|${match[2]}|${match[3]}`);
      }
    }
    routes.push({ source: sourceName, receiver: match[1], method: match[2].toUpperCase(), path: match[3], registration: text.slice(lineStart, lineEnd < 0 ? text.length : lineEnd) });
  }
  if ([...masked.matchAll(/\.\s*Any\b/g)].length !== routes.filter(route => route.method === 'ANY').length) {
    throw new Error(`unrecognized_wildcard_http_entrypoint:${sourceName}`);
  }
  // Ordinary registrations must use the same closed literal grammar as the
  // catalog. Account for selectors on actual router receivers, not unrelated
  // HTTP clients or the Engine/RouterGroup forwarding implementation.
  const receivers = new Set(routes.map(route => route.receiver));
  for (const match of masked.matchAll(/\b([A-Za-z][A-Za-z0-9_]*)\s*(?::=|=)\s*httpserver\.NewRouter\s*\(/g)) receivers.add(match[1]);
  for (const match of masked.matchAll(/\b([A-Za-z][A-Za-z0-9_]*)\s+\*httpserver\.(?:Router|RouteGroup)\b/g)) receivers.add(match[1]);
  let changed;
  do {
    changed = false;
    for (const match of masked.matchAll(/\b([A-Za-z][A-Za-z0-9_]*)\s*(?::=|=)\s*([A-Za-z][A-Za-z0-9_]*)\s*\.\s*Group\s*\(/g)) {
      if (receivers.has(match[2]) && !receivers.has(match[1])) { receivers.add(match[1]); changed = true; }
    }
  } while (changed);
  for (const match of masked.matchAll(/\b([A-Za-z][A-Za-z0-9_]*)\s*\.\s*(?:GET|POST|PUT|PATCH|DELETE)\b/g)) {
    if (receivers.has(match[1]) && !recognizedSelectors.has(match.index)) {
      throw new Error(`unrecognized_http_entrypoint:${sourceName}`);
    }
  }
  return routes;
}


// The two dynamic relay catalog rows describe this installed closed shape.
// They are not evidence that a Token relay or an arbitrary new actor exists.
function eventRelayConsumers(sourceName, main, approval) {
  function view(sourceText) {
    const { code, masked } = goSource(sourceText);
    let text = ''; let mask = '';
    for (let index = 0; index < code.length; index += 1) {
      if (!/\s/.test(code[index])) { text += code[index]; mask += masked[index]; }
    }
    return { text, mask };
  }
  function actualFunction(sourceText, name) {
    const source = goSource(sourceText);
    const declarations = [...source.code.matchAll(new RegExp('func ' + name + '\\([^\\n]+\\{([\\s\\S]*?)\\n\\}', 'g'))].filter(match => source.masked[match.index] !== ' ');
    if (declarations.length !== 1) throw new Error(`unrecognized_event_relay:${sourceName}:function`);
    return declarations[0][1];
  }
  const mainView = view(main); const approvalView = view(approval);
  const runView = view(actualFunction(main, 'run'));
  const openApprovalView = view(actualFunction(approval, 'openApprovalChannel'));
  const approvalRunView = view(actualFunction(approval, 'runApprovalRelay'));
  const matches = ({ text, mask }, pattern) => [...text.matchAll(pattern)].filter(match => mask[match.index] !== ' ');
  const exactly = (input, pattern, count = 1) => {
    if (matches(input, pattern).length !== count) throw new Error(`unrecognized_event_relay:${sourceName}:${pattern.source}`);
  };
  exactly(mainView, /source:=strings\.TrimSpace\(environment\("EVENT_SOURCE"\)\)/g);
  exactly(mainView, /ifsource==""\{source="account"\}/g);
  exactly(mainView, /source="account"/g);
  exactly(mainView, /ifsource!="account"&&source!="billing"&&source!="token"\{returnerrors\.New\("ACCOUNT_EVENT_SOURCE_REQUIRED"\)\}/g);
  const references = [...goSource(main).masked.matchAll(/\bsource\b/g)];
  const closed = matches(mainView, /source(?::=strings\.TrimSpace\(environment\("EVENT_SOURCE"\)\)|==""|="account"|!="account"|!="billing"|!="token"|=="account"|=="billing"|=="token"|\+"-outbox-relay:"|\+"-outbox-relay"|\+"-event-relay"|\+"\.>")|newRelayMetrics\(source\)/g);
  if (references.length !== closed.length) throw new Error(`unrecognized_event_relay:${sourceName}:source-references`);
  const brokerConfigs = matches(runView, /events\.NewNATSBroker\(setup,environment\("NATS_URL"\),source\+"-outbox-relay",events\.NATSConfig\{([^{}]*)\}\)/g);
  if (brokerConfigs.length !== 1) throw new Error(`unrecognized_event_relay:${sourceName}:broker-config`);
  exactly(view(brokerConfigs[0][1]), /RequireExplicitAuthentication:true/g);
  exactly(view(brokerConfigs[0][1]), /ExpectedPublicKey:expectedPublicKey/g);
  exactly(runView, /expectedPublicKey,err=identity\.TokenOutboxPublicKey\(time\.Now\(\)\.UTC\(\)\)/g);
  exactly(runView, /events\.NewOwnedPostgresStore\(setup,environment\("DATABASE_URL"\),"token_schema_owner"\)/g);
  exactly(mainView, /RequireExplicitAuthentication:true/g);
  exactly({ text: mainView.mask, mask: mainView.mask }, /events\.NewNATSBroker\(/g);
  exactly({ text: mainView.mask, mask: mainView.mask }, /serviceauth\.ContextForJob\(/g);
  exactly(runView, /relay:=events\.Relay\{Store:store,Broker:broker,WorkerID:source\+"-outbox-relay:"\+uuid\.NewString\(\),BatchSize:\d+,Lease:\d+\*time\.Second,MaxAttempts:\d+,AllowedSubjects:\[\]string\{source\+"\.>"\}\}/g);
  exactly({ text: mainView.mask, mask: mainView.mask }, /events\.Relay\{/g);
  exactly(runView, /relay\.RunOnce\(jobContext\)/g);
  exactly(mainView, /serviceauth\.ContextForJob\(ctx,source\+"-event-relay","OUTBOX_RELAY",jobID\)/g);
  exactly(mainView, /ifsource=="account"\{approval=openApprovalChannel\(setup,environment,logger\)\}/g);
  exactly(mainView, /openApprovalChannel\(/g);
  exactly(approvalView, /account\.NewApprovalRelayStore\(ctx,environment\("APPROVAL_RELAY_DATABASE_URL"\)\)/g);
  const approvalConfigs = matches(openApprovalView, /events\.NewNATSPublishOnlyBroker\(ctx,environment\("NATS_URL"\),"account-approval-relay",events\.NATSConfig\{([^{}]*)\}\)/g);
  if (approvalConfigs.length !== 1) throw new Error(`unrecognized_event_relay:${sourceName}:approval-config`);
  exactly(view(approvalConfigs[0][1]), /RequireExplicitAuthentication:true/g);
  exactly(approvalRunView, /relay\.RunOnce\(jobContext\)/g);
  exactly(approvalView, /RequireExplicitAuthentication:true/g);
  exactly({ text: approvalView.mask, mask: approvalView.mask }, /events\.NewNATSPublishOnlyBroker\(/g);
  exactly({ text: approvalView.mask, mask: approvalView.mask }, /serviceauth\.ContextForJob\(/g);
  exactly(approvalView, /serviceauth\.ContextForJob\(ctx,"account-approval-relay","OUTBOX_RELAY",jobRunID\)/g);
  return [
    { source: sourceName, durable: 'source-bound-outbox-relay', subject: 'account|billing|token.>' },
    { source: sourceName, durable: 'account-approval-relay', subject: 'account.approval-command.authorized.v1' },
  ];
}

export function authorizationConsumers(sourceName, sourceText, registryText, relayApprovalText = '') {
  if (sourceName === 'account-platform/cmd/event-relay/main.go') return eventRelayConsumers(sourceName, sourceText, relayApprovalText);
  const { code: text, masked } = goSource(sourceText);
  const calls = [...text.matchAll(/\bbroker\.ConsumeAuthorization\([^,\n]+,\s*([^,\n]+),/g)].filter(call => masked[call.index] !== ' ');
  const events = [];
  let billingDurables;
  for (const call of calls) {
    let durables;
    if (/^"[^"\n]+"$/.test(call[1])) {
      durables = [JSON.parse(call[1])];
    } else if (call[1] === 'descriptor.Durable') {
      const loops = [...text.matchAll(/for _, descriptor := range approvalconsumer\.BillingCommands\(\) \{([\s\S]*?)\n\t\}/g)].filter(loop => masked[loop.index] !== ' ');
      if (loops.length !== 1 || call.index < loops[0].index || call.index >= loops[0].index + loops[0][0].length) {
        throw new Error(`unrecognized_authorization_consumer:${sourceName}`);
      }
      if (!billingDurables) {
        const { code: registry, masked: registryMasked } = goSource(registryText);
        // Only the declaration, a defensive clone, and by-value range reads
        // belong to this closed registry. Reject every other reference: this
        // covers field/element writes, compound/multiple assignments, built-in
        // mutation and writable aliases without pretending to interpret Go.
        const registryUses = [...registryMasked.matchAll(/\bdescriptors\b/g)];
        const closedUses = [...registryMasked.matchAll(/\bvar\s+descriptors\s*=\s*\[\]Descriptor\s*\{|\bslices\.Clone\(descriptors\)|\bfor _, descriptor := range descriptors \{/g)];
        const declarations = [...registryMasked.matchAll(/\bvar\s+descriptors\s*=\s*\[\]Descriptor\s*\{/g)];
        if (declarations.length !== 1 || registryUses.length !== closedUses.length) throw new Error('unrecognized_billing_consumer_registry');
        const body = registry.match(/var descriptors = \[\]Descriptor\{([\s\S]*?)\n\}/)?.[1];
        const filter = registry.match(/func BillingCommands\(\) \[\]Descriptor \{([\s\S]*?)\n\}/)?.[1];
        if (!body || !filter || !/^result:=make\(\[\]Descriptor,0,\d+\)for_,descriptor:=rangedescriptors\{ifdescriptor.Workload==BillingWorkload\{result=append\(result,descriptor\)\}\}returnresult$/.test(filter.replace(/\s/g, ''))) {
          throw new Error('unrecognized_billing_consumer_registry');
        }
        const constants = new Map([...registry.matchAll(/\b(\w+)\s*=\s*("[^"\n]+")/g)].map(match => [match[1], JSON.parse(match[2])]));
        if (constants.get('AuthorizationSubject') !== 'account.approval-command.authorized.v1' ||
            constants.get('BillingWorkload') !== 'billing-command-executor' ||
            constants.get('TokenWorkload') !== 'token-command-executor') throw new Error('unrecognized_billing_consumer_registry');
        billingDurables = [];
        const allDurables = new Set();
        for (const line of body.trim().split(/\r?\n/)) {
          const row = line.match(/^\s*\{([^{}]+)\},\s*$/);
          if (!row) throw new Error('unrecognized_billing_consumer_descriptor');
          const fields = new Map();
          for (const field of row[1].split(',')) {
            const match = field.trim().match(/^(\w+):\s*("[^"\n]+"|\d+|[A-Za-z]\w*)$/);
            if (!match || fields.has(match[1])) throw new Error('unrecognized_billing_consumer_descriptor');
            const expression = match[2];
            const value = expression.startsWith('"') ? JSON.parse(expression) : /^\d+$/.test(expression) ? Number(expression) : constants.get(expression);
            if (value === undefined) throw new Error('unrecognized_billing_consumer_descriptor');
            fields.set(match[1], value);
          }
          if ([...fields.keys()].sort().join(',') !== 'CommandType,Durable,ExecutorIdentity,PolicyVersion,StartSource,Workload' ||
              typeof fields.get('CommandType') !== 'string' || !Number.isSafeInteger(fields.get('PolicyVersion')) ||
              fields.get('PolicyVersion') <= 0 || typeof fields.get('Durable') !== 'string' ||
              !['billing-command-executor', 'token-command-executor'].includes(fields.get('Workload')) ||
              fields.get('ExecutorIdentity') !== fields.get('Workload') || typeof fields.get('StartSource') !== 'string' ||
              allDurables.has(fields.get('Durable'))) throw new Error('unrecognized_billing_consumer_descriptor');
          allDurables.add(fields.get('Durable'));
          if (fields.get('Workload') === constants.get('BillingWorkload')) billingDurables.push(fields.get('Durable'));
        }
        if (!billingDurables.length) throw new Error('empty_billing_consumer_registry');
      }
      durables = billingDurables;
    } else {
      throw new Error(`unrecognized_authorization_consumer:${sourceName}`);
    }
    for (const durable of durables) events.push({ source: sourceName, durable, subject: 'account.approval-command.authorized.v1' });
  }
  // A selector that escaped the supported call grammar cannot pass silently.
  if ([...masked.matchAll(/\bbroker\s*\.\s*ConsumeAuthorization\b/g)].length !== calls.length) throw new Error(`unrecognized_authorization_consumer:${sourceName}`);
  return events;
}

async function discover(catalog) {
  const routes = [];
  const tasks = [];
  const events = [];
  const textBySource = new Map();
  const productionGo = [...(await productionGoSources('account-platform/internal')), ...(await productionGoSources('account-platform/cmd'))];
  const sources = new Set([
    ...humanPermissionSources,
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
    const discoveredRoutes = authorizationRoutes(relative, text);
    if (discoveredRoutes.length > 0) httpSources.push(relative);
    routes.push(...discoveredRoutes);
  }
  for (const [relative, text] of textBySource) {
    tasks.push(...authorizationTasks(relative, text));
    for (const match of text.matchAll(/broker\.Consume(?:UntilResolved)?\([^,\n]+,\s*"([^"]+)",\s*"([^"]+)"/g)) events.push({ source: relative, durable: match[1], subject: match[2] });
    events.push(...authorizationConsumers(relative, text, textBySource.get('account-platform/internal/platform/approvalconsumer/registry.go'), textBySource.get('account-platform/cmd/event-relay/approval.go')));
  }
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
  const authorizationSource = humanPermissionSources.map(relative => discovered.textBySource.get(relative) || '').join('\n');
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
    // RequireAnyAnd contains two conjunctive sets; coverage checks every named
    // permission without flattening or changing the runtime conjunction.
    const literalAnyArguments = route.registration.match(/authorizer\.RequireAny(?:And)?\(([^)]*)\)/)?.[1] || '';
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
