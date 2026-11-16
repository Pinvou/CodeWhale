import fs from 'node:fs';
import path from 'node:path';

const SECRET = String.raw`(?:Bearer\s+[^\s"'<>]+|(?:password|passwd|token|secret|authorization|cookie|api[_-]?key)\s*[:=]\s*["']?[^\s"'<>]+|(?:gh[pousr]_|github_pat_|sk-)[A-Za-z0-9_-]{12,}|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)`;
const PRIVATE_HOST = /^(?:10\.|192\.168\.|172\.(?:1[6-9]|2\d|3[01])\.|169\.254\.|\[?(?:f[cd][0-9a-f]{2}|fe[89ab][0-9a-f]):)/i;
const LIMITS = Object.freeze({ browsers: 32, bytes: 768 * 1024, events: 128, pages: 3, dom: 96 * 1024, image: 256 * 1024, captureMs: 1500 });

export function diagnosticUrl(value) {
  try {
    const url = new URL(value);
    if (!['http:', 'https:', 'ws:', 'wss:'].includes(url.protocol)) return `[${url.protocol.replace(':', '') || 'unknown'}]`;
    const host = PRIVATE_HOST.test(url.hostname) ? '[private-host]' : url.host;
    // Never persist URL credentials, query parameters or fragments.
    return `${url.protocol}//${host}${url.pathname.replace(new RegExp(SECRET, 'gi'), '[redacted]')}`;
  } catch { return '[unavailable-url]'; }
}

export function diagnosticText(value, limit = 1600) {
  return String(value ?? '')
    .replace(/(?:https?|wss?):\/\/[^\s"'<>]+/gi, diagnosticUrl)
    .replace(new RegExp(SECRET, 'gi'), '[redacted]')
    .slice(0, limit);
}

function bounded(task, milliseconds) {
  let timer;
  return Promise.race([
    Promise.resolve().then(task),
    new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('diagnostic capture deadline')), milliseconds); }),
  ]).finally(() => clearTimeout(timer));
}

function plainDirectory(directory) {
  const info = fs.lstatSync(directory);
  if (!info.isDirectory() || info.isSymbolicLink()) throw new Error('diagnostic directory unavailable');
}

export function createDiagnosticWriter(ledger, limits = LIMITS) {
  if (!ledger) return null;
  try {
    plainDirectory(ledger);
    const root = path.join(ledger, 'browser-diagnostics');
    fs.mkdirSync(root, { recursive: true, mode: 0o700 });
    plainDirectory(root);
    // Atomic directory reservation bounds the whole ledger across Node workers,
    // including interrupted captures. Never remove another worker's evidence.
    let directory;
    for (let slot = 1; slot <= (limits.browsers ?? LIMITS.browsers); slot++) {
      const candidate = path.join(root, `browser-${slot}`);
      try { fs.mkdirSync(candidate, { mode: 0o700 }); directory = candidate; break; }
      catch (error) { if (error.code !== 'EEXIST') throw error; }
    }
    if (!directory) return null;
    let bytes = 0, events = 0, omitted = 0;
    const write = (name, content, append = false, summary = false) => {
      plainDirectory(ledger); plainDirectory(root); plainDirectory(directory);
      const data = Buffer.isBuffer(content) ? content : Buffer.from(content);
      if (bytes + data.length > limits.bytes - (summary ? 0 : Math.min(4096, limits.bytes / 8))) { omitted++; return false; }
      const target = path.join(directory, name);
      if (fs.existsSync(target) && fs.lstatSync(target).isSymbolicLink()) throw new Error('symbolic diagnostic file');
      const flags = fs.constants.O_WRONLY | fs.constants.O_CREAT | (append ? fs.constants.O_APPEND : fs.constants.O_EXCL) | (fs.constants.O_NOFOLLOW || 0);
      const descriptor = fs.openSync(target, flags, 0o600);
      try { fs.writeFileSync(descriptor, data); } finally { fs.closeSync(descriptor); }
      bytes += data.length;
      return true;
    };
    return {
      directory,
      write(name, content) { try { return write(name, content); } catch { omitted++; return false; } },
      record(event) {
        if (events++ >= limits.events) { omitted++; return; }
        try { write('events.ndjson', JSON.stringify({ t: new Date().toISOString(), ...event }) + '\n', true); } catch { omitted++; }
      },
      finish(extra = {}) {
        // Diagnostic data deliberately has a different format from mandatory
        // lifecycle *.json receipts. The existing job artifact uploads both.
        try { write('summary.txt', JSON.stringify({ schema: 1, capture: 'last-state-before-close', bytes, events: Math.min(events, limits.events), omitted, ...extra }, null, 2) + '\n', false, true); } catch { /* preserve cleanup and the original test result */ }
      },
    };
  } catch { return null; }
}

// Runs in the page, immediately before its existing close. It neither reads
// cookies/storage nor returns input values or inline application scripts.
export function snapshotDocument({ maxCharacters, secretPattern, maskForScreenshot = true }) {
  // Navigation may finish between page.url() and evaluate(). Check again in
  // the document before reading or masking anything from the new origin.
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(location.hostname) || !['http:', 'https:'].includes(location.protocol)) return null;
  const clone = document.documentElement.cloneNode(true);
  for (const element of clone.querySelectorAll('script, style, noscript, iframe, object, embed')) element.remove();
  for (const element of [clone, ...clone.querySelectorAll('*')]) {
    for (const attribute of [...element.attributes]) {
      if (/^on|password|token|secret|authorization|cookie|api.?key/i.test(attribute.name) || attribute.name === 'value') element.removeAttribute(attribute.name);
      else if (['href', 'src', 'action', 'poster'].includes(attribute.name)) {
        try { const url = new URL(attribute.value, location.href); url.username = ''; url.password = ''; url.search = ''; url.hash = ''; element.setAttribute(attribute.name, url.href); }
        catch { element.removeAttribute(attribute.name); }
      }
    }
    if (element.matches('textarea, [contenteditable], [data-sensitive], [data-private]')) element.textContent = '[redacted]';
  }
  // Mask the real page only after assertions have finished, for the bounded
  // last-state screenshot. A failed mask means no screenshot is taken.
  if (maskForScreenshot) {
    const style = document.createElement('style');
    style.textContent = 'input,textarea,iframe,object,embed,[contenteditable],[data-sensitive],[data-private]{visibility:hidden!important}';
    document.head?.appendChild(style);
    const walker = document.createTreeWalker(document.body || document.documentElement, NodeFilter.SHOW_TEXT);
    let node, count = 0;
    while ((node = walker.nextNode()) && count++ < 5000) node.data = node.data.replace(new RegExp(secretPattern, 'gi'), '[redacted]');
  }
  return {
    url: location.href, title: document.title, readyState: document.readyState,
    elements: document.body?.querySelectorAll('*').length || 0,
    scripts: [...document.scripts].slice(-12).map(node => ({ src: node.src, type: node.type })),
    resources: performance.getEntriesByType('resource').slice(-12).map(entry => ({ url: entry.name, type: entry.initiatorType, durationMs: Math.round(entry.duration) })),
    html: clone.outerHTML.slice(0, maxCharacters),
  };
}

export function observeBrowser(browser, { ledger = process.env.PINVOU_CI_RESOURCE_LEDGER, limits = LIMITS, emit = line => process.stderr.write(line + '\n') } = {}) {
  if (!ledger || typeof browser.on !== 'function') return { async close() {} };
  const writer = createDiagnosticWriter(ledger, limits) || { record() {}, write() {}, finish() {} };
  const pages = new Map();
  const contexts = new WeakSet();
  let pageNumber = 0, closing, failureCount = 0;
  const recent = [], critical = [];
  const record = event => {
    const value = { t: new Date().toISOString(), ...event };
    recent.push(value);
    if (recent.length > 32) recent.shift();
    // Hundreds of successful Vite imports can displace the failed import
    // from both the timeline and the bounded event file before a wait fails.
    if (['requestfailed', 'pageerror', 'http-error'].includes(value.kind) || value.kind === 'console' && value.level === 'error') {
      critical.push(value);
      if (critical.length > 32) critical.shift();
    }
    writer.record(value);
  };
  const listen = (page, event, callback) => page.on(event, (...args) => {
    try { callback(...args); } catch { record({ kind: 'event-unavailable', event }); }
  });
  const capture = async (page, state, failure) => {
    if (!failure && (state.captured || state.number > limits.pages)) return;
    if (!failure) state.captured = true;
    try {
      const address = new URL(page.url());
      if (!['127.0.0.1', 'localhost', '[::1]'].includes(address.hostname) || !['http:', 'https:'].includes(address.protocol)) {
        record({ kind: 'snapshot-skipped', page: state.number, reason: 'not a loopback fixture', url: diagnosticUrl(page.url()) });
        return;
      }
      const snapshot = await page.evaluate(snapshotDocument, { maxCharacters: limits.dom, secretPattern: SECRET, maskForScreenshot: !failure });
      if (!snapshot) return;
      const capturedAddress = new URL(snapshot.url);
      if (!['127.0.0.1', 'localhost', '[::1]'].includes(capturedAddress.hostname) || !['http:', 'https:'].includes(capturedAddress.protocol)) return;
      const metadata = {
        kind: 'page-state', page: state.number, phase: failure ? 'wait-rejected' : 'before-close',
        url: diagnosticUrl(snapshot.url), title: diagnosticText(snapshot.title), readyState: snapshot.readyState,
        elements: snapshot.elements,
        scripts: snapshot.scripts?.map(item => ({ src: diagnosticUrl(item.src), type: diagnosticText(item.type, 32) })),
        resources: snapshot.resources?.map(item => ({ url: diagnosticUrl(item.url), type: diagnosticText(item.type, 32), durationMs: item.durationMs })),
      };
      record(metadata);
      const prefix = failure ? `failure-${failure.number}-page-${state.number}` : `page-${state.number}`;
      writer.write(`${prefix}.dom.txt`, diagnosticText(snapshot.html, limits.dom));
      if (failure) return metadata;
      if (typeof page.screenshot === 'function') {
        const image = await page.screenshot({ type: 'jpeg', quality: 35, fullPage: false });
        if (image.length <= limits.image) writer.write(`page-${state.number}.jpg`, image);
        else record({ kind: 'screenshot-skipped', page: state.number, reason: 'image byte limit' });
      }
    } catch { record({ kind: 'snapshot-unavailable', page: state.number }); }
  };
  const attach = page => {
    if (!page || pages.has(page) || typeof page.on !== 'function') return;
    const state = { number: ++pageNumber, captured: false, pending: new Map() };
    pages.set(page, state);
    listen(page, 'framenavigated', frame => {
      if (!frame.parentFrame()) record({ kind: 'navigation', page: state.number, url: diagnosticUrl(frame.url()) });
    });
    for (const event of ['domcontentloaded', 'load']) listen(page, event, () => record({ kind: event, page: state.number }));
    listen(page, 'request', request => {
      if (state.pending.size >= 32) state.pending.delete(state.pending.keys().next().value);
      state.pending.set(request, { url: diagnosticUrl(request.url()), type: diagnosticText(request.resourceType(), 32), started: Date.now() });
    });
    listen(page, 'requestfinished', request => {
      const item = state.pending.get(request);
      if (item) record({ kind: 'request-finished', page: state.number, url: item.url, type: item.type, durationMs: Date.now() - item.started });
      state.pending.delete(request);
    });
    listen(page, 'console', message => record({ kind: 'console', page: state.number, level: diagnosticText(message.type(), 32), text: diagnosticText(message.text()) }));
    listen(page, 'pageerror', error => record({ kind: 'pageerror', page: state.number, text: diagnosticText(error?.stack || error?.message) }));
    listen(page, 'requestfailed', request => {
      state.pending.delete(request);
      record({ kind: 'requestfailed', page: state.number, url: diagnosticUrl(request.url()), method: diagnosticText(request.method(), 16), resource: diagnosticText(request.resourceType(), 32), error: diagnosticText(request.failure()?.errorText) });
    });
    listen(page, 'response', response => {
      if (response.status() >= 400) record({ kind: 'http-error', page: state.number, status: response.status(), url: diagnosticUrl(response.url()) });
    });
    // Rejected waits are observed before a test catches/retries or navigates
    // away. Never log function source/arguments, mutate the page, or replace
    // the original error. Stderr survives a failed artifact upload.
    for (const method of ['goto', 'reload', 'waitForSelector', 'waitForFunction', 'waitForNavigation']) {
      if (typeof page[method] !== 'function') continue;
      const native = page[method].bind(page);
      page[method] = async (...args) => {
        try { return await native(...args); }
        catch (error) {
          if (failureCount < limits.pages) {
            const failure = { number: ++failureCount, method, error: diagnosticText(error?.message), page: state.number };
            const errors = critical.filter(event => event.page === state.number).slice(-8);
            try {
              const pending = [...state.pending.values()].map(item => ({ url: item.url, type: item.type, ageMs: Date.now() - item.started }));
              const snapshot = await bounded(() => capture(page, state, failure), limits.captureMs);
              const value = { ...failure, snapshot, pending, critical: errors, recent: recent.slice(-12) };
              writer.write(`failure-${failure.number}.txt`, JSON.stringify(value));
              const compact = { ...value, pending: pending.slice(0, 8), recent: recent.slice(-8) };
              const shorten = (key, item) => typeof item === 'string' ? item.slice(0, 256) : item;
              let line = JSON.stringify(compact, shorten);
              if (Buffer.byteLength(line) > 14_000) line = JSON.stringify({ ...failure, snapshot: { readyState: snapshot?.readyState, elements: snapshot?.elements }, pending: pending.slice(0, 3), critical: errors, recent: recent.slice(-3) }, shorten);
              emit('[browser-fixture-diagnostic] ' + line);
            } catch {
              try { emit('[browser-fixture-diagnostic] ' + JSON.stringify({ ...failure, snapshot: 'unavailable', critical: errors })); } catch { /* preserve the original rejection */ }
            }
          }
          throw error;
        }
      };
    }
    const close = page.close.bind(page);
    page.close = async (...args) => {
      try { await bounded(() => capture(page, state), limits.captureMs); } catch { record({ kind: 'capture-timeout', page: state.number }); }
      return close(...args);
    };
  };
  const wrapContext = context => {
    if (!context || contexts.has(context)) return context;
    contexts.add(context);
    if (typeof context.newPage === 'function') {
      const native = context.newPage.bind(context);
      context.newPage = async (...args) => { const page = await native(...args); attach(page); return page; };
    }
    if (typeof context.close === 'function') {
      const native = context.close.bind(context);
      context.close = async (...args) => {
        try {
          await bounded(async () => {
            const list = await context.pages();
            await Promise.all(list.map(page => { attach(page); return capture(page, pages.get(page)); }));
          }, limits.captureMs);
        } catch { record({ kind: 'context-capture-unavailable' }); }
        return native(...args);
      };
    }
    return context;
  };
  if (typeof browser.newPage === 'function') {
    const native = browser.newPage.bind(browser);
    browser.newPage = async (...args) => { const page = await native(...args); attach(page); return page; };
  }
  if (typeof browser.createBrowserContext === 'function') {
    const native = browser.createBrowserContext.bind(browser);
    browser.createBrowserContext = async (...args) => wrapContext(await native(...args));
  }
  browser.browserContexts?.().forEach(wrapContext);
  browser.on('targetcreated', target => { Promise.resolve().then(() => target.page()).then(attach).catch(() => {}); });
  Promise.resolve().then(() => browser.pages?.()).then(list => list?.forEach(attach)).catch(() => {});
  return {
    close() {
      return closing ||= (async () => {
        try { await bounded(() => Promise.all([...pages].map(([page, state]) => capture(page, state))), limits.captureMs); }
        catch { record({ kind: 'capture-timeout' }); }
        writer.finish({ pages: pageNumber, pageLimit: limits.pages });
      })();
    },
  };
}
