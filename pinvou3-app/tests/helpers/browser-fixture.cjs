exports.wrapNative = native => new Proxy(native, {
  get(target, key) {
    if (key === 'launch') return options => import('./browser-fixture.mjs').then(module => module.launchBrowser(target.launch.bind(target), options));
    return Reflect.get(target, key, target);
  },
});
