import { fixtureScope } from './fixture-scope.mjs';

// Preserve Node's synchronous close initiation and callback API. The lease
// additionally acknowledges completion and records it for the job postcheck.
export function ownServer(server, kind = 'http') {
  const nativeClose = server.close.bind(server);
  const nativeListen = server.listen.bind(server);
  let closing;
  let closeError;
  const close = () => closing ||= new Promise((resolve, reject) => {
    nativeClose(error => {
      closeError = error;
      if (error && error.code !== 'ERR_SERVER_NOT_RUNNING') reject(error);
      else resolve();
    });
  });
  const release = fixtureScope.register({ kind, close, verify: () => !server.listening });
  server.close = callback => {
    close();
    release().then(() => callback?.(closeError), error => {
      if (callback) callback(error);
      else server.emit('error', error);
    });
    return server;
  };
  server.listen = (...args) => {
    if (closing) throw new Error('a retired fixture server cannot be reused');
    return nativeListen(...args);
  };
  return server;
}
