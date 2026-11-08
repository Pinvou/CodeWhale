# 保管人本机运维

独立运维程序；默认 `kms-recovery` 仍无网络。提供有界联网 `unseal`、`bootstrap`、`finalize-bootstrap`、`rotate-transit` 和离线 `stage-agents`、`verify-snapshot`，没有 init、reset、migrate、任意 URL/API 或密钥打印功能。普通解封和 Transit 轮换不使用 root token；只有明确的身份引导/退场操作使用内存中的 root。

## 固定 Transit key 版本轮换

仅在全新数据库 prepare 与现场接线就绪后，由保管人在同一串行运维窗口执行。不能与其他操作者或自动轮换同时进行，Transit API 不提供版本 CAS。

```text
kms-operator -action rotate-transit -config <原公开配置> -directory <custody绝对路径> -bundle <既有roles.age绝对路径> -output-directory <私有父目录中的新操作目录> -expected-version <prepare得到的旧版本整数>
```

仍只通过真实终端无回显读取已有口令。不读取 initialization.age，不恢复 root，不输出/导出第四个运维身份。先核对角色包、`BOOTSTRAP_FINALIZED` 摘要及实例/endpoint/CA/recipient 绑定，再登录固定 `pinvou-kms-operations`。要求独立最小 token 和无额外 entity/group 策略；只操作 `pinvou-token-data`，保持 AES-GCM、不可导出/明文备份/删除、不使用 derived key、最低解密版本为 1，以保全历史备份。

工具先创建 0700 操作目录、持有操作系统进程锁，并持久化 0600 `ROTATION_CONTEXT`。每次可能签发 token 前，都独占建立 `CURRENT_INVOCATION`；预检通过后才写 `ROTATION_ATTEMPT`，同步文件及父目录并读回，然后只发送一次 rotate。实际版本必须恰好等于预期旧版本加一。确认临时 token 已撤销并归档该次调用后才保存 `ROTATION_VERIFIED`。记录不含口令、token 或 SecretID。轮换命令支持 Windows 和 Linux；其他系统显式拒绝。

响应丢失、错误、并发变化或不完整记录都必须保留原目录。使用相同配置、版本和目录再运行时，只登录、读取状态并撤销新产生的本次 token，**不再发送 rotate**；版本仍未变化也不会重试。目录存在但尝试记录不完整时同样拒绝，不得删除目录或改用新目录绕过不确定状态。记录最多接受 24 小时；超过窗口需独立核查，不能重新发起轮换来“修复”证据。

`KMS_ROTATION_TOKEN_RETIREMENT_UNCONFIRMED_PRESERVE_ATTEMPT` 表示临时 token 撤销未能确认，不能认为交接完成。首次和每次只读重入都分别记录、撤销并归档为 `invocation-<随机编号>`；已有完成标记不能绕过尚未关闭的调用。下次登录前必须先确认其退场标记，或由 KMS 服务端时间证明已经超过经验证的 token 创建时间加 900 秒硬寿命及 5 秒裕量；本机时钟不能代替此证据。若登录响应丢失、尚未验证租约即崩溃或记录损坏，则不能假定 token 已到期，必须保留目录并独立核查，不自动解锁或再次签发。收到但属性不合格的 token 同样使用独立于操作取消的上下文撤销。token 不会明文保存供重用。`KMS_ROTATION_VERIFIED` 仅证明版本变化及受控身份退场条件，之后仍需既有加密快照、离线保存和完整业务安全门禁。原有加密恢复包不能删除；新快照必须包含轮换后的密钥版本。

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

## 固定身份引导与初始化 root 退场

下面两个操作同样只在本机真实终端输入已有口令，每次均重新校验恢复包绑定：

```text
kms-operator -action bootstrap -config <公开配置> -directory <custody> -bundle <initialization.age> -output-directory <新的受控输出目录>
kms-operator -action finalize-bootstrap -config <相同公开配置> -directory <custody> -bundle <相同initialization.age> -output-directory <上一步输出目录>
```

`bootstrap` 只接受已解封且没有 transit/approle 挂载或四个同名策略的新实例；固定策略来自编译时计划，不接受用户传入策略/角色/密钥名。ACL 创建使用 create-only CAS。创建 AES-256-GCM 不可导出业务 key，以及 `pinvou-token-gateway`、`pinvou-credential-portal`、`pinvou-kms-backup`、`pinvou-kms-operations` 四个角色。每角色独立 SecretID，300/900 秒 service token，无 default policy。运维角色可为四个固定角色轮换 SecretID，因而属于特权离线身份，不得投递给业务容器；它不直接加解密、不修改策略/挂载，也不是长期 root。

新目录先写 `BOOTSTRAP_ATTEMPTED`。每个一次性响应立即加密为 `*-response.age`，完整角色材料写 `roles.age`，随后核对固定配置、实际登录、权限正负例、AAD 和真实快照读取，才写 `BOOTSTRAP_PREPARED`。所有文件 0600、同步及读回；Windows 仍需人工核对受控父目录 ACL。没有 root/SecretID 明文文件，runtime 和 backup 的 Agent 文件投递尚由后续受控部署步骤负责。

在退场前将新目录加入既定离线备份。`finalize-bootstrap` 完整验证 `roles.age` 的实例/origin/CA/recipient 绑定及四个唯一身份，然后再次实际验证，才撤销初始化 root；必须看到旧 root 403 且四角色重新登录成功才写 `BOOTSTRAP_FINALIZED`（包含加密包摘要）。探测 token 逐一主动撤销，不留长期测试 token。快照读取仅是有界权限探测（最多 16 MiB），**不是已保存备份**，正式备份调度/加密/异地保留仍须单独验收。

任何创建阶段断网/异常均停止，不自动重试、覆盖或删除，保留已收到的加密响应和初始 root。若完整 `roles.age` 已写入但探测临时失败，可直接用同一包执行 `finalize-bootstrap` 重新验证；没有完整包时必须受控核对部分状态，不能重新运行 bootstrap 或清库。撤销响应不确定时，也只重跑同一包的 finalize：已撤销 root 不再发送撤销请求，验证仍通过才完成标记。已存在的错误标记/权限/包损坏不覆盖。

常规 SecretID 轮换至少每 30 天：先投递新值并验证 Agent 重新认证，再销毁旧 accessor。旧 SecretID 被撤销不立即撤销其既存 token，最长 15 分钟仍有效；紧急泄露还需隔离受影响业务与 Agent 网络。此受限运维角色不授予全局 token accessor 撤销或策略管理；扩大权限及 break-glass 必须走独立受控流程，不能重新启用初始化 root。

## 离线暂存 Agent 引导文件

```text
kms-operator -action stage-agents -config <相同公开配置> -directory <custody绝对路径> -bundle <roles.age绝对路径> -output-directory <独立staging父目录中的新目录>
```

本操作只接受 `roles.age` 与同目录 `BOOTSTRAP_FINALIZED` 的匹配摘要，验证完整角色包绑定、身份唯一性及本机口令，不读取初始化包，也不建立网络连接。专用 CA 仍用于核对包的指纹，不等于重新向服务器认证或重新执行 bootstrap。

输出仅包括固定三个目录 `pinvou-token-gateway`、`pinvou-credential-portal`、`pinvou-kms-backup`，每个目录只有 `role-id` 与 `secret-id`，另有 `AGENT_FILES_STAGED` 摘要清单。第四个 `pinvou-kms-operations` 绝不写出；root、解封材料和 custody 私钥不在输入包的读取路径中。所有文件独占创建为 0600 并同步、读回，不覆盖已有输出。

**这些是给 Agent 的长期机器引导秘密，不是给业务的短期 token，也不是离线恢复包。** 必须使用独立受控 staging 父目录（Windows 仅当前用户与 SYSTEM），不能放在 custody 或角色恢复包的上一级目录中。通过验证的 SSH 传输到受控 Agent 目录并逐文件核对摘要/UID/权限；Agent 只读 bootstrap，业务只读各自的短期 sink，不能挂载本目录。只有确认传输及 Agent 登录成功后才精确删除本机暂存副本，恢复能力仍来自原有加密包。失败保留部分文件，不因目录存在认定投递完成；必须有匹配清单及后续部署验收。
