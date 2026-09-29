// 让 `node --test` 能直接 import 应用里的 .jsx（用于渲染级组件测试，见
// tests/model_store_task_panel_render.test.mjs）。
//
// Node 自己解析不了 JSX，也解析不了 Vite 的资源导入（.svg/.png），这里两件一起办：
//   - .jsx 交给 **Vite 自己导出的** `transformWithOxc`。刻意用这个公开出口，不去抓
//     vite 内部的 rolldown/esbuild：vite 是 package.json 里声明的依赖，真实构建走的
//     也是同一个转换器，测试里的语义因此跟构建产物一致；而抓传递依赖既过不了
//     eslint 的 n/no-extraneous-import，入口也会随 vite 大版本换来换去。
//   - 资源导入换成「导出自身文件名」的桩，写法对齐 tests/model_brand_icon.test.mjs。
//
// 为什么要先整棵图预转换、而不是在 load 钩子里现转：`registerHooks` 的钩子必须是
// **同步**的，而 `transformWithOxc` 是 async。异步钩子（module.register）能等它，
// 但那个 API 在本仓库的 eslint node 版本门槛下还算 experimental，会直接报错。
// 所以顺序反过来：先按相对导入把用到的 .jsx 全部转好放进缓存，钩子只做同步查表。
import fs from 'node:fs';
import { registerHooks } from 'node:module';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { transformWithOxc } from 'vite';

const ASSET = /\.(?:svg|png)(?:\?.*)?$/;
/** 绝对路径 -> 已转换好的 ESM 源码。 */
const transformed = new Map();

/** 抓 `from '…'` / `import '…'` 里的相对说明符。只认相对路径：裸包名不会是 .jsx。 */
const IMPORT_RE = /(?:from|import)\s*['"](\.[^'"]*)['"]/g;

/**
 * 把一个 .jsx 及它（递归）相对导入到的 .jsx 全部转换好存进缓存。
 * @param {string} entry - 入口 .jsx 的绝对路径。
 * @returns {Promise<void>} 转完即 resolve。
 */
async function transformGraph(entry) {
  if (transformed.has(entry)) return;
  const source = await fs.promises.readFile(entry, 'utf8');
  const { code } = await transformWithOxc(source, entry);
  transformed.set(entry, code);
  const dir = path.dirname(entry);
  for (const [, specifier] of source.matchAll(IMPORT_RE)) {
    if (!specifier.endsWith('.jsx')) continue;
    await transformGraph(path.resolve(dir, specifier));
  }
}

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (!ASSET.test(specifier)) return nextResolve(specifier, context);
    return { url: new URL(specifier, context.parentURL).href, format: 'module', shortCircuit: true };
  },
  load(url, context, nextLoad) {
    if (ASSET.test(url)) {
      const filename = decodeURIComponent(url.split('/').pop());
      return { format: 'module', source: `export default ${JSON.stringify(filename)};`, shortCircuit: true };
    }
    if (!url.startsWith('file:') || !url.endsWith('.jsx')) return nextLoad(url, context);
    const code = transformed.get(fileURLToPath(url));
    // 没进缓存说明 loadJsx 的相对导入扫描漏了这个文件。宁可在这里大声失败，也不要
    // 退回 nextLoad —— 那会变成一句指向 Node 内部的 JSX 语法错误，看不出真正原因。
    if (code === undefined) throw new Error(`这个 .jsx 没有被预转换，检查导入扫描：${url}`);
    return { format: 'module', source: code, shortCircuit: true };
  },
});

/**
 * 预转换并 import 一个 .jsx 模块。
 * @param {string} specifier - 相对说明符，比如 '../src/features/x/Y.jsx'。
 * @param {string} parentUrl - 调用方的 import.meta.url。
 * @returns {Promise<object>} 模块命名空间。
 */
export async function loadJsx(specifier, parentUrl) {
  const entry = fileURLToPath(new URL(specifier, parentUrl));
  await transformGraph(entry);
  return import(pathToFileURL(entry).href);
}
