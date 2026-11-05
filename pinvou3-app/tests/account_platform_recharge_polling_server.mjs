// Public import and CLI entry point for the shared recharge fixture.
import { resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { startRechargePollingFixture } from './helpers/recharge-polling-fixture.mjs';

export { startRechargePollingFixture };

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  let startup; let shutdownPromise;
  const failed = reason => { console.error(reason); process.exit(1); };
  const shutdown = () => {
    if (!shutdownPromise) shutdownPromise = Promise.resolve().then(() => startup)
      .then(fixture => fixture.close()).then(() => process.exit(0), failed);
  };
  // Register before the first startup await and retain listeners during the
  // entire close promise, so repeated signals cannot restore a hard exit.
  for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, shutdown);
  process.stdin.on('end', shutdown);
  process.stdin.resume();
  startup = startRechargePollingFixture();
  try {
    const fixture = await startup;
    if (shutdownPromise) await shutdownPromise;
    else console.log(JSON.stringify({ url: fixture.url, scope: 'Actual React and BFF, loopback only' }));
  } catch (reason) { if (!shutdownPromise) failed(reason); }
}
