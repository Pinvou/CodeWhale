#!/usr/bin/env node
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  '/snap/bin/chromium',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
].find(candidate => fs.existsSync(candidate));

if (!chrome) {
  console.error('SKIP: 未找到 Chrome/Edge，可通过 CHROME 指定');
  process.exit(2);
}

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-team-relay-action-'));
let browser;
let vite;

function assert(condition, message, detail) {
  if (!condition) throw new Error(`${message}${detail ? `: ${JSON.stringify(detail)}` : ''}`);
}

try {
  vite = await createServer({
    root: path.join(appRoot, 'src'),
    configFile: false,
    appType: 'mpa',
    logLevel: 'error',
    plugins: [{
      name: 'team-relay-action-smoke-page',
      configureServer(server) {
        server.middlewares.use('/team-relay-action-smoke.html', (_request, response) => {
          response.setHeader('content-type', 'text/html; charset=utf-8');
          response.end(`<!doctype html><html><body><div id="root"></div><script>
            window.$RefreshReg$ = () => {};
            window.$RefreshSig$ = () => type => type;
            window.__vite_plugin_react_preamble_installed__ = true;
          </script></body></html>`);
        });
      },
    }, react()],
    server: { host: '127.0.0.1', port: 0, strictPort: false },
  });
  await vite.listen();
  const port = vite.httpServer.address().port;

  browser = await puppeteer.launch({
    executablePath: chrome,
    headless: 'new',
    userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check'],
  });
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${port}/team-relay-action-smoke.html`, { waitUntil: 'domcontentloaded' });

  await page.evaluate(async () => {
    document.head.innerHTML = '';
    document.body.innerHTML = '<div id="root"></div>';
    const reactModule = await import('/@id/react');
    const React = reactModule.default || reactModule;
    const reactDomClient = await import('/@id/react-dom/client');
    const createRoot = reactDomClient.createRoot || reactDomClient.default?.createRoot;
    const { TeamRelayView } = await import('/features/team-relay/TeamRelayView.jsx');
    const { dict } = await import('/shared/i18n.js');
    const now = new Date().toISOString();
    const me = { id: 'member-me', display_name: '测试成员', email: 'member@example.com' };
    const team = { id: 'team-1', name: '测试团队' };
    const bootstrap = {
      protocol_version: '0.2',
      team,
      me,
      members: [
        { ...me, role: 'owner', confirmed_transfers_supported: true },
        { id: 'member-other', display_name: '协作成员', role: 'member', confirmed_transfers_supported: true },
      ],
      threads: [{
        thread: {
          id: 'thread-1',
          title: 'TEST',
          phase: 'active',
          revision: 3,
          initiator_id: me.id,
          owner_id: me.id,
          current_actor_id: me.id,
          pending_sender_id: null,
          pending_recipient_id: null,
          pending_transfer_kind: null,
          final_recipient_id: null,
          created_at: now,
          updated_at: now,
        },
        actions: [{
          id: 'action-1',
          sequence: 1,
          kind: 'assign',
          actor_id: me.id,
          recipient_id: me.id,
          message: '处理测试任务',
          attachment_ids: [],
          delivery: {
            schema_version: 1,
            artifact_role: 'input',
            subtype: '',
            expected_next: '',
            open_questions: '',
            git_references: [],
          },
          created_at: now,
        }],
        preference: { pinned: false, hidden: false },
        unread_count: 0,
      }],
    };
    const snapshot = { bootstrap, teams: [team], attachments: {} };
    window.__teamRelayActionCalls = [];
    window.PinvouPlatform = { kind: 'tauri' };
    window.TauriBridge = {
      state: {
        get(domain) {
          if (domain === 'platform') {
            return { platformCapabilities: { loaded: true, teamRelaySupported: true } };
          }
          return {
            teamRelay: {
              status: { configured: true, team_id: team.id },
              snapshot,
              loading: false,
              mutating: false,
              taskWorkspaces: {},
            },
          };
        },
        subscribeMany: () => () => {},
      },
      teamRelay: {
        load: async () => {},
        taskWorkspaceStatus: async () => null,
        listDraftSourceSessions: async () => [],
        readDraftSourceSession: async () => null,
        pickDraftAttachments: async () => [],
        mountDraftMaterials: async () => [],
        pickAgentWorkspace: async () => null,
        refresh: async () => {},
        markThreadRead: async () => {},
        createAction: async (threadId, input) => {
          window.__teamRelayActionCalls.push({ threadId, input });
          return {};
        },
      },
    };
    createRoot(document.getElementById('root')).render(
      React.createElement(TeamRelayView, { theme: 'light', t: dict.zh }),
    );
  });

  await page.waitForFunction(() => document.body.innerText.includes('TEST'));
  await page.evaluate(() => {
    const button = [...document.querySelectorAll('button')]
      .find(candidate => candidate.textContent.includes('更新进展'));
    button?.click();
  });
  await page.waitForSelector('[role="dialog"] .team-relay-prompt-card textarea');
  await page.type('[role="dialog"] .team-relay-prompt-card textarea', '已完成关键路径验证');
  await page.evaluate(() => {
    const button = [...document.querySelectorAll('[role="dialog"] button')]
      .find(candidate => candidate.textContent.includes('发布进展'));
    button?.click();
  });
  await page.waitForFunction(() => window.__teamRelayActionCalls.length === 1);
  await page.waitForFunction(() => !document.querySelector('[role="dialog"]'));

  const calls = await page.evaluate(() => window.__teamRelayActionCalls);
  assert(calls.length === 1, '发布进展应只调用一次 Bridge', calls);
  assert(calls[0].threadId === 'thread-1', '发布进展的接力 ID 错误', calls[0]);
  assert(calls[0].input.kind === 'note', '发布进展应创建 note action', calls[0]);
  assert(calls[0].input.message === '已完成关键路径验证', '发布进展正文错误', calls[0]);
  assert(calls[0].input.delivery.subtype === 'progress', '发布进展缺少 progress subtype', calls[0]);
  assert(calls[0].input.expected_revision === 3, '发布进展 revision 错误', calls[0]);
  assert(pageErrors.length === 0, '发布进展不应产生页面异常', pageErrors);
  console.log('team relay action smoke: ok');
} finally {
  await browser?.close();
  await vite?.close();
  fs.rmSync(profile, { recursive: true, force: true });
}
