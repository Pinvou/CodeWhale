import { setTimeout as sleep } from 'node:timers/promises';

const staticTypes = new Set(['document', 'script', 'stylesheet', 'image', 'font']);

// Host-network CI browsers also observe other jobs' Docker interface changes.
// Recover only an interrupted loopback fixture GET, before running case actions;
// never replay application writes or retry assertions, HTTP errors or JS errors.
export async function navigateFixture(page, url, ready, {
  attempts = 3,
  settleMs = 250,
  warn = message => console.warn(message),
  waitUntil = 'load',
} = {}) {
  const origin = new URL(url);
  if (origin.protocol !== 'http:' || origin.hostname !== '127.0.0.1') {
    throw new Error('fixture navigation requires an explicit loopback HTTP origin');
  }
  if (!Number.isSafeInteger(attempts) || attempts < 1 || attempts > 3) {
    throw new Error('fixture navigation attempts must be between one and three');
  }
  for (let attempt = 1; attempt <= attempts; attempt++) {
    const controller = new AbortController();
    let networkError;
    let interrupt;
    const interrupted = new Promise((_, reject) => { interrupt = reject; });
    const onFailed = request => {
      if (request.failure()?.errorText !== 'net::ERR_NETWORK_CHANGED'
          || request.method() !== 'GET' || !staticTypes.has(request.resourceType())) return;
      if (new URL(request.url()).origin !== origin.origin) return;
      networkError = new Error('loopback fixture GET interrupted by a network change');
      interrupt(networkError);
      controller.abort(networkError);
    };
    page.on('requestfailed', onFailed);
    try {
      await Promise.race([
        (async () => {
          await page.goto(url, { waitUntil });
          await ready(controller.signal);
        })(),
        interrupted,
      ]);
      return;
    } catch (error) {
      if (!networkError || attempt === attempts) throw error;
      warn(`Loopback fixture network changed; restarting page load (${attempt}/${attempts})`);
    } finally {
      page.off('requestfailed', onFailed);
      controller.abort();
    }
    await sleep(settleMs);
  }
}
