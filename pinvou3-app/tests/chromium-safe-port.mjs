export const chromiumUnsafePorts = new Set([
  1, 7, 9, 11, 13, 15, 17, 19, 20, 21, 22, 23, 25, 37, 42, 43, 53, 69, 77, 79, 87, 95,
  101, 102, 103, 104, 109, 110, 111, 113, 115, 117, 119, 123, 135, 137, 139, 143, 161, 179,
  389, 427, 465, 512, 513, 514, 515, 526, 530, 531, 532, 540, 548, 554, 556, 563, 587, 601,
  636, 989, 990, 993, 995, 1719, 1720, 1723, 2049, 3659, 4045, 4190, 5060, 5061, 6000, 6566,
  6665, 6666, 6667, 6668, 6669, 6697, 10080,
]);

function bindEphemeral(server, host) {
  return new Promise((resolve, reject) => {
    const onError = (error) => {
      server.off('listening', onListening);
      reject(error);
    };
    const onListening = () => {
      server.off('error', onError);
      resolve(server.address());
    };
    server.once('error', onError);
    server.once('listening', onListening);
    server.listen(0, host);
  });
}

function closeServer(server) {
  return new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
}

export async function listenOnChromiumSafePort(server, options = {}) {
  const host = options.host || '127.0.0.1';
  const bind = options.bind || (() => bindEphemeral(server, host));
  const close = options.close || (() => closeServer(server));
  for (;;) {
    const address = await bind();
    if (!address || typeof address === 'string' || !Number.isInteger(address.port)) throw new Error('HTTP server did not expose an ephemeral TCP port');
    if (!chromiumUnsafePorts.has(address.port)) return { host, port: address.port, origin: `http://${host}:${address.port}` };
    await close();
  }
}
