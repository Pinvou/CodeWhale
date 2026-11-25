# cmsd 本机组件维护（v1）

用于 Pinvou 管理 cmsd 软件本身；模型生命周期和商店安装仍使用各自既有接口。此能力不包含平台授权、远程配对或自动分发。

## 组件制品

`cmsd --version` 和 `cmsd --component-info` 在加载配置、创建状态和绑定 socket 之前返回。
后者包含 component、version、os、architecture、api、state_format、update_contract 和
`authority_scope=local-v1` 和 build_revision；这是兼容性声明，不是发布者签名，也不构成客户发行认证。

在可信构建环境制作离线目录：

```bash
python3 scripts/build-linux-component.py --output /absolute/new/package \
  --sysroot /absolute/ubuntu22.04-sysroot
```

构建器要求干净的已提交源码，使用离线依赖并核查 GLIBC 2.35 上限；在兼容基线主机可省略 sysroot。只打包已有二进制时使用 `build-component-package.py`。

输出 `cmsd` 和 `manifest.json`，包含平台、语义版本、v1 API、状态格式、大小与 SHA256。
Pinvou 先静态校验清单、ELF 架构和摘要，操作接受时固定预览摘要；后台再次核验，复制到私有
摘要目录后才执行兼容探针。包来自操作者选择的可信来源，不能把自附 SHA256 当成签名。
本实现没有在线更新地址、签名发行链或跨发行版 ABI 认证。

## 持久维护门

仅在既有本机 Unix socket 提供：

```
POST /v1/component-maintenance
{"action":"begin","owner":"component-unique-id"}
{"action":"end","owner":"component-unique-id"}
```

`owner` 仅允许 1—128 个 ASCII 字母、数字、`-`、`_`。不是远程授权令牌。
`begin` 在同一任务状态锁内拒绝任何未终结任务，然后原子持久化
`state_dir/component-maintenance.json`。模型任务准入持相同锁并检查该文件，避免
“查完无任务、准备停服务时又接单”的窗口。重复同 owner 的 begin/end 幂等；其他 owner
拒绝；损坏或不可读时拒绝新模型变更，不能视为没有维护。普通服务维护不自动取消模型任务。
维护回执按单次文件描述符有界读取（16KiB），拒绝链接、FIFO、非普通文件及未知目录；
异常不会解除门或在持任务状态锁时等待文件写者。只有确实缺失的路径才视为无维护。

`GET /v1/status` 返回 `component_maintenance`；`/v1/device` 声明能力
`component.maintenance`。关闭或重启 daemon 后门仍保留，恢复入口在验证组件服务和
socket 对应后解除。服务停止期间，Pinvou 持同一 daemon.lock 检查持久任务和维护门。
旧版本无维护能力时，在线普通停止/更新会明确失败；不能转成不受保护的停止。

## Pinvou 的本机管理入口

参见 Pinvou 文档 `docs/megacube-single-node.md`。它管理当前用户的已验证 user unit，
安装独立版本目录，记录更新/回退阶段，关闭桌面后由独立 helper 继续执行；不把模型进程
作为 cmsd 的子服务杀掉。卸载服务保留模型、配置、设备身份、任务与回退缓存。

隔离用户服务的验证应包含 systemd 注册、socket 查询、任务拦截、更新失败回退、卸载保留和恢复。系统级安装、跨账号权限、机器重启/linger、签名发行及跨机分发仍需单独验收。

组件启动会拒绝含远程或计划授权格式的既有状态目录。Pinvou 必须在停止旧服务前核查 `authority_scope` 与状态兼容；更新不能以删除状态完成降级。
