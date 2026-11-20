'use strict';

function preloadOptions(existing, filename) {
  if (/[\r\n]/.test(existing || '') || /[\r\n"]/.test(filename)) throw new Error('Invalid multiline or quoted Node options path');
  // NODE_OPTIONS has its own backslash-escape parser, including on Windows.
  // Forward separators preserve drive/UNC paths and quoting preserves spaces.
  return `${existing || ''} --require ${JSON.stringify(filename.replaceAll('\\', '/'))}`.trim();
}
module.exports = { preloadOptions };
if (require.main === module && process.argv[2] === '--configure-env') {
  const options = preloadOptions(process.env.NODE_OPTIONS, __filename);
  require('node:fs').appendFileSync(process.env.GITHUB_ENV, `PINVOU_CI_UPLOAD_NODE_OPTIONS=${options}\n`, 'utf8');
}

// Opt-in passive diagnostics for the artifact action, never its signed URL,
// headers, payload, local paths or raw error messages. No retries or deadlines.
if (process.env.PINVOU_CI_ARTIFACT_DIAGNOSTICS === '1') {
  const http = require('node:http');
  const https = require('node:https');
  const fs = require('node:fs');
  const { errorMonitor } = require('node:events');
  const started = performance.now();
  const pending = new Map();
  let serial = 0;
  let lines = 0;
  const receiptReads = { created: 0, opened: 0, ended: 0, closed: 0, bytes: 0, errors: 0, active: 0 };
  const elapsed = () => Math.round(performance.now() - started);
  function emit(event) {
    if (lines++ >= 160) return;
    try { fs.writeSync(2, `[artifact-transport] ${JSON.stringify({ elapsed_ms: elapsed(), ...event })}\n`); }
    catch { /* Closed diagnostic output cannot change the transfer verdict. */ }
  }
  function hostLabel(host) {
    host = String(host || '').toLowerCase();
    if (host === 'localhost' || host === '127.0.0.1' || host === '::1') return 'loopback';
    return /(^|\.)(github\.com|githubusercontent\.com|blob\.core\.windows\.net)$/.test(host) ? host : 'other';
  }
  function errorCode(error) {
    return /^[A-Z][A-Z0-9_]{0,63}$/.test(error?.code || '') ? error.code : 'unclassified';
  }
  function requestHost(args) {
    const first = args[0];
    if (typeof first === 'string' || first instanceof URL) {
      try { return hostLabel(new URL(first).hostname); } catch { return 'other'; }
    }
    return hostLabel(first?.hostname || first?.host);
  }
  for (const module of [http, https]) {
    const original = module.request;
    module.request = function (...args) {
      const request = original.apply(this, args);
      if (pending.size >= 32) return request;
      const id = ++serial;
      const record = { id, kind: 'request', host: requestHost(args), phase: 'created', start_ms: elapsed() };
      pending.set(id, record);
      const phase = (name, extra = {}) => { record.phase = name; emit({ id, kind: record.kind, host: record.host, phase: name, ...extra }); };
      phase('created');
      request.once('socket', socket => {
        phase('socket');
        socket.once('lookup', error => phase('dns', { code: error ? errorCode(error) : 'OK' }));
        socket.once('connect', () => phase('tcp_connected'));
        socket.once('secureConnect', () => phase('tls_connected', { authorized: socket.authorized }));
      });
      request.once('finish', () => phase('request_body_finished'));
      request.once('response', response => {
        phase('response_headers', { status: response.statusCode });
        response.once('end', () => phase('response_end'));
      });
      request.once(errorMonitor, error => phase('error', { code: errorCode(error) }));
      request.once('close', () => { phase('closed'); pending.delete(id); });
      return request;
    };
  }
  const originalRead = fs.createReadStream;
  fs.createReadStream = function (...args) {
    const stream = originalRead.apply(this, args);
    if (!String(args[0]).endsWith('.json')) return stream;
    receiptReads.created++; receiptReads.active++;
    // A large atomic ledger must not exhaust the network diagnostic budget
    // before its first request. Keep all read totals, detail only four files.
    const detailed = receiptReads.created <= 4 && pending.size < 32;
    const id = detailed ? ++serial : null;
    const record = { id, kind: 'receipt_read', phase: 'created', start_ms: elapsed() };
    const phase = (name, extra = {}) => { record.phase = name; if (detailed) emit({ id, kind: record.kind, phase: name, ...extra }); };
    if (detailed) pending.set(id, record);
    phase('created');
    stream.once('open', () => { receiptReads.opened++; phase('open'); });
    stream.once('end', () => { receiptReads.ended++; phase('end', { bytes: stream.bytesRead }); });
    stream.once(errorMonitor, error => { receiptReads.errors++; emit({ id, kind: record.kind, phase: 'error', code: errorCode(error) }); });
    stream.once('close', () => {
      receiptReads.closed++; receiptReads.active--; receiptReads.bytes += stream.bytesRead;
      phase('closed', { bytes: stream.bytesRead });
      if (detailed) pending.delete(id);
    });
    return stream;
  };
  require('node:module').syncBuiltinESMExports();
  emit({ phase: 'enabled', pid: process.pid });
  const pulse = setInterval(() => emit({ phase: 'progress', receipt_reads: receiptReads, pending: [...pending.values()].map(r => ({ ...r, waiting_ms: elapsed() - r.start_ms })) }), 10000);
  pulse.unref();
  process.once('beforeExit', () => { clearInterval(pulse); emit({ phase: 'exit', receipt_reads: receiptReads, pending: pending.size }); });
}
