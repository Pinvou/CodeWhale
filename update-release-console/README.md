# Pinvou Windows 更新发布台

第一阶段的 Windows 静态更新发布服务。它接收已经由独立签名服务器签署的 NSIS 安装器，
完成 SHA-256/大小/版本校验、草稿预览、不可变文件发布、`latest.json` 原子切换、回滚和
审计记录。服务不构建安装器、不调用签名工具，也不读取任何签名凭据。

完整部署与操作说明见
[`docs/windows-update-release-console.md`](../docs/windows-update-release-console.md)。

## 本地运行

```bash
npm ci
cp environment.example .env.local
# 按实际目录与随机令牌修改 .env.local，再将变量导入当前 shell
npm start
```

Node.js 最低版本为 20.9。运行测试：

```bash
npm test
```

`PINVOU_UPDATE_TRUSTED_PROXIES` 必须显式列出允许提供 `X-Real-IP` 的直接反向代理
地址或 CIDR。Docker 示例只信任私有 bridge 范围，禁止配置全网段通配。
