# 本机交互式 KMS 解封

在独立模块根目录构建 `go build -trimpath -o <本机受控目录>/kms-operator.exe ./cmd/kms-operator`。

仅在真实本机终端执行，口令通过不回显提示读取；不要通过聊天、Agent、录屏或转录终端输入：

```text
kms-operator -action unseal -config <公开配置绝对路径> -directory <本机custody绝对路径> -bundle <initialization.age绝对路径>
```

配置包含绑定实例 ID、原逻辑 HTTPS endpoint、CA 文件及可选本机回环 SSH tunnel 地址。即使使用隧道，Host 和 TLS 服务名仍须是原逻辑 endpoint。配置及身份、包的父目录须受限，不使用代理或跟随重定向。

完整认证解密并核对实例、endpoint、CA 和恢复公钥后才联网。先查询 seal 状态，已解封时不提交密钥；仅对已初始化的单份 Shamir 实例提交一次解封密钥，再查实际状态。未知应答须保留原包并先重查实际状态，不盲目重复提交。命令不发送 root token、不输出解密材料。

成功输出仅证明目标已解封，不代表业务部署、正式密钥轮换或恢复验收完成。
