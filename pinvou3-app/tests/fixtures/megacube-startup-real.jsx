// Browser entry for the opt-in native startup acceptance; never mounted by the app.
import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import '../../src/styles/tailwind.css';
import { CmsSettingsPanel } from '../../src/features/megacube/CmsSettingsPanel.jsx';
import { ModelGuidancePanel, ModelGuidanceView } from '../../src/features/megacube/ModelGuidancePanel.jsx';
import { dictZh } from '../../src/shared/i18n/zh.js';
import { dictEn } from '../../src/shared/i18n/en.js';
import { dictJa } from '../../src/shared/i18n/ja.js';

const p = new URLSearchParams(location.search),
  t = ({ zh: dictZh, en: dictEn, ja: dictJa })[p.get('lang') || 'zh'];
const device = { isCube: true }, h = React.createElement;

createRoot(document.getElementById('root')).render(
  p.get('mode') === 'connection-preview'
    ? h(ModelGuidanceView, {
      copy: t.uiMegacube.guidance,
      snapshot: { phase: 'connectionUnavailable', selectedModelName: 'Selected local model', canTry: false },
      onOpenModelStore() {}, onOpenSettings() {},
    })
    : h(React.Fragment, null,
      h(ModelGuidancePanel, {
        copy: t.uiMegacube.guidance, device, existingUse: true, savedModels: [],
        onOpenSettings() { window.__openedDevice = true; },
        onOpenModelStore() { window.__openedStore = true; },
      }),
      h(CmsSettingsPanel, {
        copy: t.uiMegacube,
        onOpenModelStore() { window.__openedStore = true; },
      }),
    ),
);
