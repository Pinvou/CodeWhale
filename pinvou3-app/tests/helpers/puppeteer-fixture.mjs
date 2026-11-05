import puppeteer from 'puppeteer-core';
import { launchBrowser } from './browser-fixture.mjs';
export const launch = options => launchBrowser(puppeteer.launch.bind(puppeteer), options);
const methods = new WeakMap();
export default new Proxy(puppeteer, {
  get(target, key) {
    if (key === 'launch') return launch;
    const value = Reflect.get(target, key, target);
    if (typeof value !== 'function') return value;
    if (!methods.has(value)) methods.set(value, value.bind(target));
    return methods.get(value);
  },
});
