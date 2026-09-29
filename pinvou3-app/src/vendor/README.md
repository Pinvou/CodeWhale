# vendor/ — 本地化前端依赖（离线/内网可用）

前端第三方库现已统一通过 npm 锁定版本，并由 Vite 打包或在构建期编译；运行时不依赖 CDN，也不再加载经典 vendor 脚本。

Tailwind CSS 3.4.17 于 2026-09 由 Play CDN runtime 迁移为 PostCSS 构建期依赖。应用启动不再下载、解析和执行 407 KB 的运行时编译器，也不会在浏览器中扫描 DOM 后生成样式。`tailwind.config.cjs` 固定源码扫描范围和 `darkMode: 'class'`，`tests/compat_audit.test.mjs` 验证 Safari 14.0 所需的 `inset-*` 物理属性输出。

marked 和 DOMPurify 也已迁移为 npm 依赖，并由 Vite 按 Safari 14 目标转译。若未来确需增加必须在应用模块前加载的经典脚本，须在此登记版本、SHA-256、许可证和用途；`tests/vendor_asset_integrity.test.js` 会拒绝未登记的 `.js` 文件。

完整第三方归因见仓库根目录 `THIRD_PARTY_NOTICES.md`。
