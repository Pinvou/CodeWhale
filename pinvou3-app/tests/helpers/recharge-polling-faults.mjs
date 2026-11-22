// Only dependency boundaries fail: the fixture's allocation, close, signal,
// directory checks and error projection always execute their real code.
// Preload this module only in the owned lifecycle child process.
import fs from 'node:fs';
import http from 'node:http';
import { resolve, sep } from 'node:path';
import { syncBuiltinESMExports } from 'node:module';
const owner = resolve(process.env.FIXTURE_LIFECYCLE_OWNER);
const mode = process.env.FIXTURE_LIFECYCLE_MODE;
const inProjection = value => {
  const path = resolve(String(value));
  return path.startsWith(owner + sep) && path.includes('pinvou-public-tok03-app-');
};
for (const [name, targetIndex, fault] of [['cpSync', 1, 'copy-cp'], ['mkdirSync', 0, 'copy-mkdir'], ['copyFileSync', 1, 'copy-file']]) {
  const original = fs[name];
  fs[name] = (...args) => {
    if ((mode === fault || (mode === 'copy-and-remove' && name === 'cpSync')) && inProjection(args[targetIndex]))
      throw Object.assign(new Error('INJECTED_COPY_FAILURE'), { code: 'INJECTED_COPY_FAILURE' });
    return original(...args);
  };
}
const remove = fs.rmSync;
fs.rmSync = (...args) => {
  if ((mode === 'remove' || mode === 'copy-and-remove') && inProjection(args[0]))
    throw Object.assign(new Error('INJECTED_REMOVE_FAILURE'), { code: 'INJECTED_REMOVE_FAILURE' });
  return remove(...args);
};
const create = http.createServer;
http.createServer = (...args) => {
  const server = create(...args);
  if (mode === 'startup-signal') {
    const listen = server.listen;
    server.listen = (...listenArgs) => {
      // Fix the signal between entering listen and announcing readiness.
      process.nextTick(() => process.emit('SIGTERM'));
      setTimeout(() => listen.apply(server, listenArgs), 40);
      return server;
    };
  }
  if (mode === 'repeat-signal' || mode === 'os-repeat-signal') {
    const close = server.close;
    server.close = callback => close.call(server, reason => setTimeout(() => callback?.(reason), 80));
  }
  return server;
};
if (mode === 'repeat-signal') process.on('message', message => {
  if (message !== 'signal') return;
  process.emit('SIGTERM');
  process.send?.({ signalListenersAfterFirst: process.listenerCount('SIGTERM') });
  setTimeout(() => process.emit('SIGTERM'), 5);
});
syncBuiltinESMExports();
