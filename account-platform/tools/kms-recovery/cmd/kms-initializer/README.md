# 单次加密初始化器

独立联网工具，仅支持 Linux。原有 `kms-recovery` 仍完全离线。此工具仅初始化并加密捕获响应，不自动解封、创建服务身份、撤销 root、恢复快照或重置数据。

## 前置与配置

在本模块根目录执行 `go test ./...`、`go vet ./...`，构建 `go build -trimpath -o <受控位置>/kms-initializer ./cmd/kms-initializer`。

正式运行前，保管人必须已经完成本机身份解密验证、离线备份及口令分开保管。程序的确认开关只是操作确认，不能证明离线备份确实存在，部署负责人必须先取得这一事实。

配置文件无秘密，示意字段如下（使用现场核对值，不直接运行示例）：

```json
{
  "endpoint": "https://kms.example.com:8200",
  "ca_file": "/secure/operator/ca.crt",
  "recipient_file": "/secure/operator/recipient.txt",
  "recipient_sha256": "<去除首尾空白后的recipient字符串之SHA-256小写十六进制>",
  "output_directory": "/secure/operator/new-init-attempt",
  "instance_id": "deployment-unique-instance-id"
}
```

输入文件必须是绝对规范路径、普通文件且不经过符号链接；Linux 不得组/全局可写。输出父目录必须预先存在、0700 且由受信任操作人控制，新输出目录必须不存在。所有上级目录及配置来源也应受运维管控，宿主 root 在信任边界内。CA 文件是这个 KMS 的专用可信证书链，不自动附加系统根或代理；不可把服务端私钥或保管人私钥上传给初始化器。

公钥指纹由保管端独立核对，不能把不可信来源提供的公钥和指纹同时当作证明。实例 ID 为 16–64 位小写字母/数字/连字符，必须唯一并登记；它仅用于关联，不能替代 TLS 或恢复时的实例核对。

```text
kms-initializer -config /secure/operator/init.json -custody-backup-confirmed -initialize-new-instance
```

仅以 `GET sys/init` 和 `GET sys/seal-status` 确认未初始化的 Shamir 实例；一次 `POST sys/init`，固定单人 1/1。无代理、重定向、自动重试或明文日志。响应最多 256 KiB，单请求最多 45 秒；异常只返回固定错误，不显示 URL、响应、token 或解封材料。内存清零是尽力处理，Go 字符串和宿主内存不能保证完全擦除，操作环境禁止未加密 swap 和转储。

## 结果判定与中断

- `INIT_ATTEMPTED`：请求可能已发送，不能直接重跑或删除目录。
- `initialization.age`：标准 age 密文；包含版本、实例、origin、CA/recipient 指纹、捕获时间、HTTP 状态、完整性标志和原始响应。成功或收到非空失败/截断响应均尝试加密保存；文件存在不表示成功。
- 仅**进程退出 0 且有 `INIT_CAPTURED`**表示收到符合结构的完整初始化响应并完成文件/目录同步与密文读回。不代表保管人已解密、解封或完成生产验收。恢复时还必须验证加密包内的 `complete`、HTTP 状态、绑定信息和实际材料，不能单独信任可被修改的标记。
- 超时、响应丢失/截断、状态异常或落盘失败返回 `INITIALIZER_RESULT_UNCERTAIN_DO_NOT_RETRY_OR_DELETE_OUTPUT`。保留所有文件，不自动重试/清库；已初始化状态不能再次初始化，即便换输出路径也会拒绝。

磁盘与远端初始化不存在事务原子性，尤其服务完成初始化后响应丢失，仍可能无法取得一次性材料。遇此情况停止并人工核对；仅在确认为未承载业务的目标且获得明确处置决定后另行置备，工具绝不自动重置。

捕获后把整个尝试目录中的加密包安全复制到保管端，通过原有离线工具 `-action verify -bundle <initialization.age>` 验证密码学完整性。后续专用受控解封流程还须核对包内绑定与实际服务，完成解封、root 撤销和快照恢复演练。不要通过通用解密命令把包打印至终端，也不要用聊天或 Agent 读取保管人口令。

## 验证记录

2026-09-07：Go 单元测试和 vet 通过。Linux 同源测试二进制通过 `scripts/account-platform-openbao-fixture.py --isolated-fixture --config-directory <公开配置目录> --initializer-test-binary <Linux测试二进制>` 实际执行整个初始化模块测试集，无跳过，输出 `OPENBAO_INITIALIZER_FIXTURE_PASS`。固定 OpenBao 2.6.2 隔离 TLS/Raft 实例的响应直接加密、从包内恢复材料并实际解封/认证/撤销，未使用正式保管人身份或主密钥。实际恢复演练仍需独立快照恢复及人工流程，不以此替代。
