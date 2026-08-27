import { cpSync, existsSync, mkdirSync, readdirSync } from 'node:fs';
import { extname, join, resolve } from 'node:path';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const sourceRoot = resolve(import.meta.dirname, 'src');
const staticExtensions = new Set([
  '.avif', '.gif', '.ico', '.jpeg', '.jpg', '.png', '.svg', '.webp',
]);
const staticScripts = new Set([
  'features/attachments/attachment-drop-controller.js',
  'features/personas/personas-i18n.js',
  'features/updater/update-notice-logic.js',
  'platform/tauri/bridge.js',
  'platform/web/bootstrap.js',
  'platform/web/bridge.js',
  'platform/web/host-file-picker.js',
  'platform/web/access-policy.json',
  'shared/bridge-messages.js',
  'vendor/marked.min.js',
  'vendor/purify.min.js',
  'vendor/tailwind.js',
]);
const staticScriptPrefixes = ['platform/tauri/bridge/', 'platform/web/bridge/'];

function normalizeWebBasePath(value) {
  let raw = String(value || '/pinvou3/remote').trim();
  try {
    if (/^https?:\/\//i.test(raw)) raw = new URL(raw).pathname;
  } catch {}
  const trimmed = raw.replace(/^\/+|\/+$/g, '');
  return trimmed ? `/${trimmed}/` : '/';
}

function copyRuntimeAssets() {
  let outputRoot;
  return {
    name: 'pinvou-copy-runtime-assets',
    apply: 'build',
    configResolved(config) {
      outputRoot = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      const visit = (dir) => {
        for (const entry of readdirSync(dir, { withFileTypes: true })) {
          const source = join(dir, entry.name);
          if (entry.isDirectory()) {
            visit(source);
            continue;
          }
          const relative = source.slice(sourceRoot.length + 1).replaceAll('\\', '/');
          const isRuntimeScript = staticScripts.has(relative) || staticScriptPrefixes.some(prefix => relative.startsWith(prefix));
          if (!staticExtensions.has(extname(entry.name).toLowerCase()) && !isRuntimeScript) continue;
          const target = join(outputRoot, relative);
          mkdirSync(resolve(target, '..'), { recursive: true });
          cpSync(source, target);
        }
      };
      if (existsSync(sourceRoot)) visit(sourceRoot);
    },
  };
}

function preserveAccountPortalExtensions() {
  const extensions = {
    'admin.html': {
      head: [
        '<script type="module" crossorigin src="/assets/registration-repair.js"></script>',
        '<script type="module" crossorigin src="/assets/role-bindings.js"></script>',
        '<script type="module" crossorigin src="/assets/approval-requests.js"></script>',
        '<script type="module" crossorigin src="/assets/pricing-admin.js"></script>',
        '<script type="module" crossorigin src="/assets/product-admin.js"></script>',
        '<link rel="stylesheet" crossorigin href="/assets/registration-repair.css">',
        '<link rel="stylesheet" crossorigin href="/assets/role-bindings.css">',
        '<link rel="stylesheet" crossorigin href="/assets/approval-requests.css">',
        '<link rel="stylesheet" crossorigin href="/assets/pricing-admin.css">',
        '<link rel="stylesheet" crossorigin href="/assets/product-admin.css">',
      ],
      body: [
        '<div id="registration-repair-root"></div>',
        '<div id="role-bindings-root"></div>',
        '<div id="approval-requests-root"></div>',
        '<div id="pricing-admin-root"></div>',
        '<div id="product-admin-root"></div>',
      ],
    },
  };
  return {
    name: 'pinvou-preserve-account-portal-extensions',
    apply: 'build',
    enforce: 'post',
    transformIndexHtml(html, context) {
      const filename = String(context.filename || '').replaceAll('\\', '/').split('/').pop();
      const extension = extensions[filename];
      if (!extension) return html;
      return html
        .replace('</head>', `  ${extension.head.join('\n  ')}\n</head>`)
        .replace('</body>', `  ${extension.body.join('\n  ')}\n</body>`);
    },
  };
}

export default defineConfig(({ mode }) => {
  const webBuild = mode === 'web';
  const accountPortalBuild = mode === 'account-portal';
  return {
  root: 'src',
  // The Relay and Vite build intentionally share one deployment variable;
  // each side only normalizes the trailing slash for its own router contract.
  base: webBuild ? normalizeWebBasePath(process.env.PINVOU_REMOTE_PUBLIC_BASE_PATH) : '/',
  publicDir: false,
  server: {
    host: process.env.PINVOU3_UI_DEV_HOST || '127.0.0.1',
    port: Number(process.env.PINVOU3_UI_DEV_PORT || 1420),
    strictPort: true,
  },
  plugins: [react(), ...(accountPortalBuild ? [preserveAccountPortalExtensions()] : [copyRuntimeAssets()])],
  build: {
    outDir: accountPortalBuild ? '../../account-platform/internal/portal/static' : webBuild ? '../../remote-control-relay/web/dist' : '../dist',
    // The Portal directory also contains independently maintained approval,
    // recovery, and role-lifecycle assets. Stable Vite outputs overwrite their
    // own names without deleting those sibling entry points.
    emptyOutDir: !accountPortalBuild,
    rolldownOptions: {
      input: accountPortalBuild
        ? { account: resolve(sourceRoot, 'account.html'), admin: resolve(sourceRoot, 'admin.html') }
        : webBuild
        ? { main: resolve(sourceRoot, 'index.html') }
        : {
            main: resolve(sourceRoot, 'index.html'),
            pet: resolve(sourceRoot, 'pet.html'),
            reader: resolve(sourceRoot, 'reader.html'),
          },
      output: accountPortalBuild
        ? {
            entryFileNames: 'assets/[name].js',
            chunkFileNames: 'assets/[name].js',
            assetFileNames: 'assets/[name][extname]',
          }
        : undefined,
    },
  },
  };
});
