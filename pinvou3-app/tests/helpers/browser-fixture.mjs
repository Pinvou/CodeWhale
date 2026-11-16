import { fixtureScope, ownedDirectory } from './fixture-scope.mjs';
import { confirmedChildStopper } from './browser-wrapper-fixture-cleanup.mjs';
import { observeBrowser } from './browser-diagnostics.mjs';


export async function launchBrowser(launchNative, options = {}, { scope = fixtureScope, createDirectory = ownedDirectory } = {}) {
  const directory = options.userDataDir ? null : createDirectory('chrome');
  let browser;
  let stop;
  let release;
  let diagnostics;
  try { release = scope.register({
    kind: 'browser',
    close: async () => {
      // A rejected launch may already have spawned a child without returning
      // its handle. Only the outer process supervisor can retire that scope.
      if (!stop) throw new Error('fixture browser shutdown is unconfirmed: no child lifecycle handle');
      // Capture before native close destroys the page; diagnostics may not
      // replace a test failure or prevent mandatory child retirement.
      try { await diagnostics?.close(); } catch { /* best-effort evidence only */ }
      await stop();
    },
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
    // Every owned handle needs an observed close, even after exit.
    // exitCode/nativeClose cannot prove stdio retirement; a missed
    // acknowledgement or unobserved historical close fails closed.
    stop = confirmedChildStopper(child, nativeClose);
    try { diagnostics = observeBrowser(browser); } catch { /* preserve native launch/cleanup behavior */ }
    browser.close = release;
    return browser;
  } catch (primary) {
    try { await release(); } catch (cleanup) { console.error('Browser fixture cleanup failed', cleanup); }
    throw primary;
  }
}
