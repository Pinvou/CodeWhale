# Keycloak 主题本地浏览器回归

安装项目现有 puppeteer-core 依赖，并使用独立安装的 Microsoft Edge。Windows 显式门禁：

```powershell
npm --prefix account-platform run test:browser-login-theme:windows
```

该入口将两个 `PINVOU_REQUIRE_PASSWORD_*` 开关固定为 1；缺 Edge 或依赖失败，不会 skip。可用 `PINVOU_PASSWORD_BROWSER_EXECUTABLE` 明确选择绝对 Edge 路径，不能指向真实用户 profile。测试始终创建并清理自己的临时 profile，不读取已登录浏览器数据、不访问真实站点、不操作系统剪贴板。仅当前 FTL 的密码控件投影与 theme.properties 指定的实际 hash JS/CSS在本地 headless 浏览器执行，不启动 FreeMarker、Keycloak、Portal 或 OIDC 服务，也不证明真实账号登录。

通用显式浏览器入口：`npm --prefix account-platform run test:browser-login-theme`。没有浏览器或 puppeteer-core 时会明确 skip；要求执行时设置 `PINVOU_REQUIRE_PASSWORD_BROWSER_TEST=1`，Edge视觉要求设置 `PINVOU_REQUIRE_PASSWORD_EDGE_VISUAL_TEST=1`。任何 skip 都不能称浏览器已验证。此文件位于 `test/browser/`，不进入现有 Linux contract glob 或改变其零 skip 门禁。

覆盖当前 hash资产/FTL控件、实际键盘输入/显式镜像/blur、未知初值与脚本替换、Unicode/IME、reset和maxlength微任务竞态；Edge还比较输入尾部像素以检测重复原生眼睛。截图仅在内存，不保存完整密码画面。测试只使用公开虚构文本；不读取系统剪贴板，所以不声称执行原历史用例的真实剪贴板复制/粘贴场景。React账号页面与生产OIDC服务另有各自合同，不用本fixture替代。

如项目决定在 Windows CI 执行，可在已安装 Edge、Node 和现有 npm 依赖的显式 job 中调用上述 `:windows` 命令；无需改变现有 required Linux 链。该建议未自动增加或批准新的 required job；应把实际 job/head 运行结果作为后续证据。

可复用 Windows job 的最小命令（已有受信 checkout、Node >=22 和 Edge 是前置条件）：

```powershell
npm --prefix pinvou3-app ci --ignore-scripts
npm --prefix account-platform run test:browser-login-theme:windows
```

`puppeteer-core` 使用当前 app lockfile 的既有依赖；不会借此下载浏览器。Windows入口核对3根+5子项准确成功且零skip；缺任一命名子项、重复证据或零执行都失败。`test/contract/login-theme-browser-runner.test.mjs` 只用合成TAP验证该门禁，不访问浏览器，其通过数不得计为浏览器验证。
