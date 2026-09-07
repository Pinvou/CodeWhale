# 保管人本机解封

单独的联网程序；默认 `kms-recovery` 仍无网络。仅提供 `-action unseal`，没有 init、reset、migrate、任意 URL/API 或密钥打印功能。不会发送包中的 root token。

## 使用

在独立模块根目录构建 `go build -trimpath -o <本机受控目录>/kms-operator.exe ./cmd/kms-operator`。在真实本机终端运行，不通过 Agent 终端、日志转录或聊天输入口令：

```text
kms-operator -action unseal -config <配置绝对路径> -directory <custody绝对路径> -bundle <initialization.age绝对路径>
```

程序在本机无回显提示恢复口令，不从参数或环境变量读取口令。私钥、解封材料只在内存中使用，不写明文文件或打印；Go 和操作系统不提供完全可证明的内存擦除。禁止在不可信或会录制输入/转储内存的设备运行。

公开配置由部署时核对，字段如下：

```json
{
  "endpoint": "https://kms.example.com:8200",
  "ca_file": "<本机专用CA证书绝对路径>",
  "recipient_sha256": "<与初始化包一致的恢复公钥指纹>",
  "instance_id": "<与初始化包一致的实例编号>",
  "connect_address": "127.0.0.1:38200"
}
```

`endpoint` 必须与原始初始化包完全相同。可选 `connect_address` 仅接受 loopback IP 加有效端口，用于已经建立的 SSH 隧道。它只覆盖 TCP 地址，不修改 HTTP Host 或 TLS 校验名称；专用 CA/原目标主机名仍必须真实验证。省略此字段时直接连接原目标。工具不建立隧道、不执行外部命令，也不修改 DNS 或代理。

所有文件必须是规范绝对路径、普通文件且不经过符号链接。Linux 配置/CA 不得组或全局可写；custody 和加密包文件仅保管人访问。Windows 必须在交付前核对目录 ACL 仅当前用户与 SYSTEM，不依赖 POSIX mode。目录、配置和公钥来源须独立受控，不能把可被替换的配置当作可信证明。

## 安全状态

先完整解密并验证认证标签，再校验版本、成功状态、捕获时间、实例编号、原始 origin、CA 和 recipient 指纹；任何不匹配均在联网前拒绝。包内份额格式还需符合当前单人 Shamir 1/1 模式。过期根 token 不影响普通解封，因为它完全不参与请求。

读取状态确认已初始化且份额阈值匹配后，才发送一次 `sys/unseal`。已解封则不重复发送；成功后重新读取封存状态。没有透明写重试：超时、断线、跳转、响应异常或仍封存报告 `OPERATOR_UNSEAL_RESULT_UNCERTAIN_CHECK_STATE_BEFORE_RETRY`，操作者应先只读核对当前状态再决定是否重试。程序不自动重置解封进度或删除服务。

`KMS_UNSEALED` 仅表示解封后状态已验证，不代表策略/服务身份初始化、root 撤销、快照恢复或生产部署完成。TLS 证书更新到同一可信 CA 下的新叶证书不需要更换恢复包；CA 或 Shamir 主密钥轮换必须走独立受控的恢复材料更新流程，不能放宽绑定校验。
