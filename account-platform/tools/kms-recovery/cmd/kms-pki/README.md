# 专用 KMS 私有证书

独立离线程序，使用 Go 标准 X.509/ECDSA 与既有 age 格式。新建专用 CA 与首张叶证书，不联网，不初始化 KMS，不接受或回显恢复口令。CA 私钥从未写入明文文件；服务器仅接收另一个独立私钥签发的叶证书。

在模块根目录构建 `go build -trimpath -o <受控位置>/kms-pki ./cmd/kms-pki`。配置为绝对路径的公开 JSON：

```json
{
  "recipient_file": "<本机recipient.txt绝对路径>",
  "recipient_sha256": "<核对后的公钥SHA-256>",
  "output_directory": "<不存在的新输出目录绝对路径>",
  "server_ips": ["<现场核对的KMS私有IPv4地址>"]
}
```

运行 `kms-pki -config <配置绝对路径>`。配置/公钥不得经过符号链接，Linux 不得组或全局可写；输出父目录仅保管人访问。Windows 先核对父目录 ACL 仅当前用户与 SYSTEM，再生成。不覆盖已有目录或文件，失败保留现场。

固定 DNS 为 `openbao`，仅接受 1–4 个不同的私有 IPv4 地址；拒绝公网、loopback、IPv6 和通配域名。ECDSA P-256 根 CA 有效 5 年、禁止中间 CA；叶证书有效 90 天，只用于 TLS serverAuth。根和叶使用不同私钥及随机序列号，输出前验证证书链和叶密钥对应。

- `ca-private.age`：包含版本/用途/CA 证书及 PKCS#8 CA 私钥的标准 age 加密包，必须离线保管，不能发送给业务容器。
- `ca.crt`：公开信任根。
- `server.crt` / `server.key`：KMS 叶证书及私钥；私钥只能进入受控服务器证书目录，不进入代码、聊天或日志。安全传输确认后，本机暂存叶私钥可由明确清理步骤移除；不得误删 CA 恢复包。
- `PKI_READY`：证书链和密钥检查、私有写入同步与读回完成，不代表保管人已解密 CA 包或服务已部署。

保管人使用原有离线 `kms-recovery -action verify -directory <custody> -bundle <ca-private.age>` 验证密码学完整性，不打印明文。Go/系统内存擦除无法完全保证，生成环境禁止转储/未加密 swap。必须在到期前完成叶证书续签、原子替换和服务复载验证，并配置到期告警；本程序只负责首次签发，不冒充定时续签器。CA/恢复身份轮换须另行受控更新绑定，不能忽略校验。
