# 前端目录边界

- `app/`：主窗口、宠物窗口等应用入口和顶层视图编排。
- `features/`：按业务功能组织页面、状态、接口包装和功能内组件。
- `platform/`：Tauri、浏览器等宿主平台适配；不得包含具体业务页面。
- `components/`、`hooks/`、`shared/`：至少被两个功能复用的公共能力。
- `assets/`、`avatars/`、`brand-icons/`、`file-icons/`、`styles/`、`vendor/`：静态资源、全局样式和离线第三方运行时。

依赖方向为 `app -> features -> platform/shared`。功能模块不得反向引用 `app/`；前端不得通过 `navigator.userAgent` 扩散新的平台分支，新增平台能力应由 Tauri bridge 返回语义化 capability。

`platform/tauri/bridge.js` 是兼容门面，继续提供稳定的全局 `window.TauriBridge` 接口；功能实现位于 `platform/tauri/bridge/` 下的独立模块，由门面注入共享状态和最小依赖。拆分模块不得自行创建第二份全局状态，现有 invoke 命令名和公开方法名必须保持兼容。

`platform/web/bridge.js` 只负责 Web transport 的状态与命令实现；`platform/web/bridge/domain-adapter.js` 在其后加载，把内部扁平接口收口为与桌面端一致的领域 API。React 不得调用扁平兼容方法，桌面/Web 公共 API 及明确的平台例外由 Bridge 契约测试锁定。

共享 Markdown 展示入口负责隔离解析、高亮及净化异常：正常内容仍走既有净化管线，异常时只返回完整转义的原文，不交付中间 HTML、不重试另一解析器，也不禁用后续正常渲染。桥接委托层使用同一转义兜底，使消息和回合的状态收尾不依赖渲染成功。`renderMarkdownMarkup` 是未经最终净化的中间接口，不得直接用于 DOM 展示。

操作系统差异由 Rust 命令 `get_platform_capabilities` 返回语义化能力（例如是否展示 MegaCube、是否支持超级权限设置、依赖安装方式），React 功能代码只消费 capability，不解析 WebView 的 user agent。

上述边界由仓库根目录的 `scripts/architecture-guard.py` 检查。历史违规记录在
`scripts/architecture-baseline.json`，只允许减少，不允许新增或扩大；规则和本地运行
方式见 `docs/architecture-guard.md`。
