import React from 'react';
import { createRoot } from 'react-dom/client';
import PetWindow from '../features/pet/PetWindow.jsx';
import { mountWhenLanguageReady } from '../shared/entry-boot.js';
import { ensureLanguageWithZhFallback, initialSystemLanguage } from '../shared/i18n.js';

const query = new URLSearchParams(window.location.search);

// 桌宠窗口级配置：设为 false 时不渲染右下角缩放手柄。
const PET_WINDOW_CONFIG = Object.freeze({
  allowResize: false,
  scale: 0.5,
  verticalAlignment: query.get('verticalAlignment') === 'top' ? 'top' : 'bottom',
});

document.documentElement.classList.add('pet-window');
// 首帧语言引导:与 app/main.jsx 同口径(惰性 chunk 先装载再渲染,失败降级 zh;
// 双失败时由引导门挂词典无关的静态失败/重试态,见 entry-boot.js)。
const root = createRoot(document.getElementById('root'));
mountWhenLanguageReady({
  load: () => ensureLanguageWithZhFallback(initialSystemLanguage()),
  mount: () => {
    root.render(
      <PetWindow
        allowResize={PET_WINDOW_CONFIG.allowResize}
        configuredScale={PET_WINDOW_CONFIG.scale}
        configuredVerticalAlignment={PET_WINDOW_CONFIG.verticalAlignment}
      />,
    );
  },
});
