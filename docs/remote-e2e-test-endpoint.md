# Web 远控测试端点与 E2E 部署

测试脚本 `scripts/deploy-remote-relay-test.sh` 将本地 Relay 与完整共享 WebUI 部署到隔离测试实例。公开客户端入口是 `https://pinvou.com/pinvou3/remote-test`；SSH 管理入口、登录账号、个人密钥、真实主机与备份定位仅由不入库的运维清单提供。

## 架构与副作用

客户端 → HTTPS 前端代理 → loopback 反向隧道 → 隔离 Relay。生产路由 `/pinvou3/remote`、8787 服务和生产遥测数据目录不被测试脚本替换；但共享代理仍会发生 nginx include/reload、专用低权限隧道账户与 authorized_keys、sshd Match 转发限制，以及对配置 Relay IP 的 fail2ban unban 操作。跨主机不是全局事务，不宣称“生产零改动”。

脚本全权管理的 `relay-tunnel` 是生成的专用 nologin 账户，不是运维登录账号；其 key 只准指定 loopback 端口的 remote forwarding，禁止 shell、PTY、agent/X11 forwarding 与 local forwarding。服务布局是脚本管理的通用测试默认值，不能据此推断真实线上资产。

## 必填受控配置

实际目标 `RELAY_SERVER` / `PROXY_SERVER` 和独立登记值 `EXPECTED_RELAY_SERVER` / `EXPECTED_PROXY_SERVER` 必须全部显式提供并逐项一致；格式为 `<ssh-user>@<literal-ipv4>`。从运维清单独立核对登记值，不能由实际目标派生。部署私钥路径由 `SSH_KEY` 显式提供，不再使用个人默认文件。

跳板非空时 `PROXY_SSH_JUMP` / `EXPECTED_PROXY_SSH_JUMP` 也必须成对提供且一致。空配置、目标不匹配、格式错误均在联网和任何远端变更前拒绝，包括 teardown。

这些变量只在本地环境/secret 管理器中注入，不写入提交、PR、示例值或共享日志。加载配置后从仓库根目录执行：

```bash
bash scripts/deploy-remote-relay-test.sh --validate-only
bash scripts/deploy-remote-relay-test.sh
# 仅启用隔离测试 controller lane；生产不得用 development/TOFU
ENABLE_DESKTOP_CONTROLLER=1 bash scripts/deploy-remote-relay-test.sh
```

测试目录必须精确等于脚本管理的 `/opt/pinvou-remote-relay-test`，端口范围为 1024–65535 且不得是生产 8787，base 必须在 `/pinvou3/remote-test` 内，不能有 shell 元字符或 dot segments。远端删除前重复校验测试目录，不允许把它改成生产路径或宽泛目录。

## 发布与回滚

先构建当前 base 的共享 React UI，运行本地测试，再用 CLI 验证 index、base、startup、module 引用与真实资源边界。完整根目录 JS 模块、manifest、schema 和 Web dist 一起归档；暂存验证使用本次上传的 ESM manifest，缺失资源失败退出。

Relay 先暂存解包、装依赖才替换目录；新版本健康失败或公网验证失败回滚 Relay 侧并非零退出。nginx 与 sshd 分别使用暂存、原子替换、验证和局部回滚；跨主机失败需按阶段排查，可重跑收敛。默认执行 npm test / Web 构建，仅已有对应实际证据时允许显式 skip。

Controller 私钥、HMAC 与状态位于代码目录之外的独立持久目录；0600 文件、0700 目录，重部署复用。已有安装丢身份但仍有状态/公钥痕迹时拒绝重新生成；legacy 身份只作一次性只读迁移。首次生成的公钥可能不等于内置 test pin，须取回实际公钥并成对配置；自动交叉校验仍见 #1507。

客户端显式选 test profile；自建或轮换身份同时配置 key ID/public key，不能以修改内置信任根或删除状态来绕过配对失败。

```bash
PINVOU_REMOTE_ENDPOINT_PROFILE=test ./pinvou3-app/run-dev.sh
```

## 拆除

```bash
bash scripts/deploy-remote-relay-test.sh --teardown
```

仍需全部受控配置和隔离校验。脚本移除测试实例、隧道、代理测试路由和专用账户，并按托管标记清理历史 key；共享代理 nginx 会 reload。持久 controller 身份/状态不删除，以免误换密钥和 tombstone（#1510）；彻底回收须另由运维核对精确目录并授权，不新增自动擦除开关。

## 验证边界

`npm --prefix remote-control-relay test` 覆盖 Relay 与离线部署合同；`node scripts/run-windows-relay-tools-tests.mjs` 在原生 Windows PowerShell 5.1 执行属主、身份与 launcher 三组测试，CI 的 `windows-relay-tools-test` 同样执行且禁止零计数或 skip。健康片段覆盖有界重试/请求超时和恢复旧 unit，不代表完整故障矩阵（#1429）。WebUI 浏览器 smoke 使用 `npm --prefix pinvou3-app run test:webui`。这些检查不执行真实 SSH、生产部署、线上回滚或双 Windows 验收。

真实双机旅程与发布门禁见 [部署说明](Windows双端远程控制服务部署说明.md) 和 [发布与回滚](Windows双端远程控制发布与回滚.md)。敏感信息或未修漏洞按 CONTRIBUTING.md 中的私密安全报告通道处理，不在公开 Issue 粘贴实例值。
