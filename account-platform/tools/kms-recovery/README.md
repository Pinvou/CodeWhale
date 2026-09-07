# KMS 离线恢复托管工具

独立 Go 模块，使用 age v1.3.2 标准格式，不引入业务服务依赖。默认构建的 `kms-recovery` 命令无网络功能，不读取环境或命令行中的口令，不输出解密明文。另有单独构建、显式联网的 Linux [kms-initializer](cmd/kms-initializer/README.md)，不能将两个命令的边界混淆。

## 构建与验证

在本目录执行 `go test -count=1 ./...`、`go vet ./...`、`go build -trimpath -o <受控目录>/kms-recovery.exe .`。

## 保管人操作

在真实的本机终端运行（不要通过录屏、转录日志、聊天或 Agent 终端输入口令）：

```powershell
.\kms-recovery.exe -action prepare -directory C:\Recovery\new-custody
.\kms-recovery.exe -action verify -directory C:\Recovery\new-custody
```

父目录必须存在，托管子目录必须不存在；已有身份绝不覆盖。使用至少16个字符的独立强口令，建议密码管理器生成或多个随机词组合。工具不记录口令，忘记口令无法恢复。

- `identity.age`：口令加密的恢复私钥，必须离线备份。
- `recipient.txt`：恢复公钥，供材料加密；必须防止被替换。
- `CUSTODY_READY`：已成功读取刚写入身份并完成内存加解密校验；**不是 KMS 已初始化/恢复包已生成的证明**。

Windows 上必须先收紧父目录 ACL，仅允许当前保管人和 SYSTEM，不能依赖 Go 的 POSIX mode 表示 Windows 文件权限。Linux/macOS 父目录仅保管人可访问。口令与加密文件分开保管，至少一份离线备份。不要把真实文件提交到仓库或业务服务器。

## 加密材料与验证

`-action seal -directory <托管目录> -bundle <新包绝对路径>` 从标准输入读取最多1 MiB材料，向标准输出仅打印固定成功状态。输入应来自受控操作进程，禁止先写明文文件或用命令行参数/环境变量传入实际秘密；只读公钥，无需保管人口令。输出已存在时拒绝，不覆盖旧包。

`-action verify -directory <托管目录> -bundle <包绝对路径>` 在本机终端提示口令，将包完全解密至内存并检查认证标签，不显示明文、不写解密文件。此命令只验证密码学可恢复，不证明材料的业务正确性或 KMS 解封已成功；真实恢复演练仍需后续受控流程。

恢复身份及包都兼容标准 age。私钥解密后的字节缓冲会尽力清零，但 Go/库内部字符串和操作系统内存不能保证彻底擦除；不要在不受信任的本机运行。程序不产生 crash dump，也不配置系统级转储或交换；这些本机管理策略需由保管人保证。

2026-09-07：三项测试 PASS（6.991 秒），`go vet` 与架构守卫 PASS。正式密钥尚未生成，本工具不是自动 KMS 初始化/部署脚本。
