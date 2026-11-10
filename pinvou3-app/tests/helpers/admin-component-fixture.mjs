import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import React from 'react';
import domino from '@mixmark-io/domino';
import { transformWithOxc } from 'vite';

const requireHere = createRequire(import.meta.url);
const compiled = new Map();
const sourceRoot = new URL('../../src/features/account-platform/', import.meta.url);

// Compile the actual components in memory, preserving their imports and handlers.
// CSS and native browser geometry/focus are outside this in-process DOM fixture.
async function compile(url, privateExport = '') {
  const key = url.href + '#' + privateExport;
  if (compiled.has(key)) return compiled.get(key);
  let source = (await readFile(url, 'utf8')).replace(/^import ['"][^'"]+\.css['"];?\r?$/gm, '');
  for (const match of [...source.matchAll(/from (['"])([^'"]+)\1/g)]) {
    const relative = match[2].startsWith('.');
    const target = relative ? new URL(match[2], url) : pathToFileURL(requireHere.resolve(match[2]));
    const resolved = relative && target.pathname.endsWith('.jsx') ? await compile(target) : target.href;
    source = source.replace(match[0], () => 'from ' + JSON.stringify(resolved));
  }
  if (privateExport) source += '\nexport { ' + privateExport + ' };\n';
  const { code } = await transformWithOxc(source, url.pathname, { jsx: { runtime: 'classic' }, sourcemap: false });
  const result = 'data:text/javascript;base64,' + Buffer.from(code).toString('base64');
  compiled.set(key, result);
  return result;
}

export async function adminComponent(file, privateExport = '') {
  return import(await compile(new URL(file, sourceRoot), privateExport));
}

export async function adminDOM(t) {
  const window = domino.createWindow('<!doctype html><html><body><main id="root"></main></body></html>');
  const globals = { window, document: window.document, navigator: window.navigator, Node: window.Node, HTMLElement: window.HTMLElement, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(globals).map(key => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  for (const [key, value] of Object.entries(globals)) Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
  const { createRoot } = await import('react-dom/client');
  const root = createRoot(window.document.querySelector('#root'));
  t.after(async () => {
    try { await React.act(async () => root.unmount()); }
    finally {
      for (const [key, descriptor] of previous) {
        if (descriptor) Object.defineProperty(globalThis, key, descriptor);
        else delete globalThis[key];
      }
    }
  });
  const event = (node, name) => React.act(async () => { assert.ok(node); node.dispatchEvent(new window.Event(name, { bubbles: true, cancelable: true })); });
  return {
    document: window.document,
    render: (Component, props) => React.act(async () => root.render(React.createElement(Component, props))),
    click: node => event(node, 'click'),
    submit: node => event(node, 'submit'),
    // Domino has no native input editing pipeline: invoke the installed React
    // onChange with a real DOM target. The product state/reducer is not mocked.
    change: (node, value) => React.act(async () => {
      assert.ok(node);
      node.value = value;
      const key = Object.keys(node).find(candidate => candidate.startsWith('__reactProps$'));
      assert.ok(key && node[key].onChange);
      node[key].onChange({ target: node });
    }),
  };
}
