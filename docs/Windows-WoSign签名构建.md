# Windows WoSign 签名构建

Windows NSIS 发布构建通过项目内的 `wosigncodecmd.exe` 完成签名，
同时要求同目录存在配套的 `wosigncode.exe`，不依赖服务器上的外部工具目录。
两个工具仅供构建阶段使用，没有配置为 Tauri bundle 资源，不会随应用安装包发布。

## 签名参数

代码签名证书 SHA1 指纹和 UKey 密码保存在仓库根目录下被 Git 忽略的
`scripts/.builtin-secrets.env` 中，分别使用 `PINVOU3_WOSIGN_THUMBPRINT` 和
`PINVOU3_WOSIGN_PASSWORD`。可复制 `scripts/.builtin-secrets.env.example` 后填写真值。

可选环境变量：

- `PINVOU3_WOSIGN_TIMESTAMP_URL`：RFC 3161 时间戳地址，默认使用 `http://timestamp.digicert.com`。

签名工具路径不可通过环境变量覆盖，构建始终使用项目内与 `sign.ps1` 同目录的
`wosigncodecmd.exe` 和 `wosigncode.exe`，并以该目录作为沃通命令的工作目录。
签名命令保留 `/isf` 以跳过已有签名，并继续通过 `/tr` 请求 RFC3161 时间戳。
沃通命令成功后，脚本使用 Windows SDK 的 SignTool 按 Authenticode 默认策略验证全部签名、证书链和
RFC 3161 时间戳；SignTool 的失败或警告都会终止构建。随后脚本读取文件内嵌的签名者证书，并要求其
SHA1 指纹与配置指纹一致。构建机必须安装 Windows SDK Signing Tools，或将 `signtool.exe` 加入 `PATH`。
Tauri 的自定义签名命令从 `src-tauri` 目录执行，因此配置中的脚本路径必须写为
`packaging/windows/signing/wosign/sign.ps1`，不得再次添加 `src-tauri/` 前缀。

证书或 UKey 密码变化后，只需修改构建机器上的私密配置文件，不应将真值写回签名脚本或提交到 Git。

## 预签名资源处理

安装包中的第三方运行时二进制由其供应方提前签名。构建阶段先将这些资源收集到
`target/release/nsis-stage/resources`，再由生成的 NSIS hook 直接写入安装目录；它们不进入
Tauri 的 `bundle.resources`，因此不会被 Tauri 逐个调用签名工具，也不会因 `/isf` 被误当成
本次首方签名成功。

主程序、卸载程序和最终 NSIS 安装器仍由 Tauri 调用沃通脚本签名，并执行上述签名者指纹校验。
这种边界使两类文件各自保持原有签名：第三方资源保留供应方签名，首方产物统一使用项目配置证书。
卸载时，生成的 NSIS hook 只删除 staging 清单中的文件，并按从深到浅的顺序尝试移除空目录；如果用户或
其他组件在这些目录中新增了文件，目录会被保留，不会递归删除额外内容。

## 构建命令

本地完整编译并生成未签名的 NSIS 测试安装包：

```powershell
npm --prefix pinvou3-app run build:nsis:local
```

该命令只会在构建前清理 `pinvou3-app/src-tauri/target/release/bundle/nsis`，避免旧版本、旧架构或失败构建遗留的安装包被误认为本轮产物；构建成功后会校验并打印本轮新生成的安装包路径、大小和修改时间。
它不加载沃通签名配置，也不启用 `dev-tools` 调试二进制；生成的安装包仅供本地验证，Windows 可能显示“未知发布者”，不得作为正式发布包。

直接编译并生成 NSIS 安装器：

```powershell
npm run build:nsis
```

使用已完成的 NSIS staging 生成安装器：

```powershell
npm run bundle:nsis
```

两个命令都会先执行 `check:wosign`，签名参数无效、命令行工具或配套工具缺失时立即终止。Tauri 在写入 NSIS bundle 类型后调用同一签名脚本，依次签署主程序、NSIS 卸载程序和最终安装器。构建已启用 Tauri verbose 日志；若签名失败，Jenkins 日志会显示签名阶段、目标文件、沃通输出和退出码。
