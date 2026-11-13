import assert from 'node:assert/strict';

/** Keep the real DeviceManagerView mounted while its pending removal changes locale. */
export async function exercisePendingDeviceRemovalLocale(page) {
  await page.reload();
  await page.evaluate(async () => {
    const fixture = window.deviceRemovalLocale = { attempts: 0 };
    window.__TAURI__ = { core: { invoke: async command => {
      if (command === 'model_store_availability') return { available: false };
      throw new Error(`Unexpected native command: ${command}`);
    } } };
    window.TauriBridge = {
      available: true,
      monitor: { startMonitorPolling() {}, stopMonitorPolling() {} },
      device: {
        startDevicePolling() {}, stopDevicePolling() {},
        pollDeviceServices: async () => [], fetchExternalModels: async () => ({}),
        removeExternalModel: () => new Promise((_resolve, reject) => {
          fixture.attempts += 1;
          fixture.reject = reject;
        }),
      },
    };
    /* eslint-disable import-x/no-unresolved, n/no-missing-import */
    const reactModule = await import('/@id/react');
    const { createElement } = reactModule.default || reactModule;
    const client = await import('/@id/react-dom/client');
    const { DeviceManagerView } = await import('/features/device-manager/DeviceManagerView.jsx');
    const { dictZh } = await import('/shared/i18n/zh.js');
    const { dictEn } = await import('/shared/i18n/en.js');
    /* eslint-enable import-x/no-unresolved, n/no-missing-import */
    const createRoot = client.createRoot || client.default?.createRoot;
    const root = createRoot(document.getElementById('root'));
    const bs = { savedModels: [], externalModels: { warnings: [], models: [{
      id: 'guarded-locale', name: 'guarded-locale', kind: 'llamacpp', model: 'guard-model',
      base_url: 'http://127.0.0.1:8888/v1', weights_path: '/tmp/guarded-locale',
      registered: true, source: 'registry', status: 'available',
    }] } };
    fixture.render = language => root.render(createElement(DeviceManagerView, {
      theme: 'light', t: language === 'en' ? dictEn : dictZh, bs,
      onOpenModelStore() {}, onOpenChat() {},
    }));
    fixture.unmount = () => root.unmount();
    fixture.enBusy = dictEn.uiDeviceMgr.removeFailed(dictEn.uiDeviceMgr.weightRemovalBusy);
    fixture.zhBusy = dictZh.uiDeviceMgr.weightRemovalBusy;
    fixture.enTitle = dictEn.uiDeviceMgr.modelsTitle;
    fixture.render('zh');
  });
  await page.waitForSelector('[data-testid="device-manager-remove"]');
  await page.click('[data-testid="device-manager-remove"]');
  await page.waitForFunction(() => {
    const confirm = document.querySelector('[data-testid="device-manager-remove-confirm"]');
    return !!confirm && !confirm.disabled;
  });
  await page.click('[data-testid="device-manager-delete-weights"]');
  await page.click('[data-testid="device-manager-remove-confirm"]');
  await page.waitForFunction(() => window.deviceRemovalLocale.attempts === 1);
  await page.evaluate(() => window.deviceRemovalLocale.render('en'));
  await page.waitForFunction(() => (document.body.innerText || '').includes(window.deviceRemovalLocale.enTitle));
  await page.evaluate(() => window.deviceRemovalLocale.reject(new Error('model_store_weight_removal_busy')));
  await page.waitForFunction(() => {
    const toast = document.querySelector('[data-testid="model-store-toast"]');
    return (toast?.textContent || '').includes(window.deviceRemovalLocale.enBusy);
  });
  const outcome = await page.evaluate(() => ({
    toast: document.querySelector('[data-testid="model-store-toast"]')?.textContent || '',
    expected: window.deviceRemovalLocale.enBusy, old: window.deviceRemovalLocale.zhBusy,
    attempts: window.deviceRemovalLocale.attempts,
    weightsStillSelected: !!document.querySelector('[data-testid="device-manager-delete-weights"]')?.checked,
    retryEnabled: !document.querySelector('[data-testid="device-manager-remove-confirm"]')?.disabled,
  }));
  assert.equal(outcome.toast, outcome.expected, 'pending removal uses the current mounted catalog');
  assert.ok(!outcome.toast.includes(outcome.old), 'the old request closure cannot select the old locale');
  assert.equal(outcome.attempts, 1, 'locale rerender must not repeat the mutation');
  assert.equal(outcome.weightsStillSelected, true, 'the same mounted removal confirmation keeps its selection');
  assert.equal(outcome.retryEnabled, true, 'rejection releases busy state for retry');
  await page.evaluate(() => window.deviceRemovalLocale.unmount());
  console.log('PASS: same mounted DeviceManagerView localizes a pending removal with its new catalog and preserves retry state');
}
