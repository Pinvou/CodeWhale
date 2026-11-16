import { fixtureScope, ownedDirectory } from './fixture-scope.mjs';
import { confirmedChildStopper } from './browser-wrapper-fixture-cleanup.mjs';


export async function launchBrowser(launchNative, options = {}) {
  const directory = options.userDataDir ? null : ownedDirectory('chrome');
  let browser;
  let stop;
  let release;
  try { release = fixtureScope.register({
    kind: 'browser',
    close: async () => { if (stop) await stop(); },
    verify: async () => !browser?.connected,
    remove: () => directory?.remove(),
  }); } catch (primary) {
    try { directory?.remove(); } catch (cleanup) {
      throw new AggregateError([primary, cleanup], 'Browser fixture registration and directory retirement failed', { cause: cleanup });
    }
    throw primary;
  }
  try {
    browser = await launchNative({ ...options, ...(directory ? { userDataDir: directory.path } : {}) });
    const nativeClose = browser.close.bind(browser);
    const child = browser.process();
    if (!child) throw new Error('fixture browser process handle is unavailable');
    // Every owned handle needs an observed close. Exit codes cannot prove
    // stdio retirement, and an unobserved historical close fails closed.
    stop = confirmedChildStopper(child, nativeClose);
    browser.close = release;
    return browser;
  } catch (primary) {
    try { await release(); } catch (cleanup) { console.error('Browser fixture cleanup failed', cleanup); }
    throw primary;
  }
}
