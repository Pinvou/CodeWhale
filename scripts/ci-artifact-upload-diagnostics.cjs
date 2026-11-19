'use strict';

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
  const elapsed = () => Math.round(performance.now() - started);
  function emit(event) {
    if (lines++ >= 160) return;
    process.stderr.write(`[artifact-transport] ${JSON.stringify({ elapsed_ms: elapsed(), ...event })}\n`);
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
    if (pending.size >= 32 || !String(args[0]).endsWith('.json')) return stream;
    const id = ++serial;
    const record = { id, kind: 'receipt_read', phase: 'created', start_ms: elapsed() };
    pending.set(id, record);
    emit({ id, kind: record.kind, phase: 'created' });
    stream.once('open', () => { record.phase = 'open'; emit({ id, kind: record.kind, phase: 'open' }); });
    stream.once('end', () => { record.phase = 'end'; emit({ id, kind: record.kind, phase: 'end', bytes: stream.bytesRead }); });
    stream.once(errorMonitor, error => emit({ id, kind: record.kind, phase: 'error', code: errorCode(error) }));
    stream.once('close', () => { emit({ id, kind: record.kind, phase: 'closed', bytes: stream.bytesRead }); pending.delete(id); });
    return stream;
  };
  require('node:module').syncBuiltinESMExports();
  emit({ phase: 'enabled', pid: process.pid });
  const pulse = setInterval(() => emit({ phase: 'progress', pending: [...pending.values()].map(r => ({ ...r, waiting_ms: elapsed() - r.start_ms })) }), 10000);
  pulse.unref();
  process.once('beforeExit', () => { clearInterval(pulse); emit({ phase: 'exit', pending: pending.size }); });
}
