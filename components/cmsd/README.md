# Cube Manage Software（cmsd）

MegaCube 的本机模型管理守护进程，源码在 Pinvou 仓库的 `components/cmsd/`，以此目录作为唯一开发来源。Pinvou 和模型商店提供管理界面；关闭桌面后，已接受的模型任务及推理服务继续运行。

本组件由原 Cube Manage Software 仓库提交 `80a7ebcaf21aef929a53ceea56ce9d0d81a2f0b4` 导入，保留独立 Rust crate 和组件版本。日后随 Pinvou 从同一提交构建和分发，无须先向独立 CMS 仓库发布 Release。

本批 Pinvou 商店闭环使用同一 Linux 账号的用户级服务。原有 system 模式保留独立管理用途；root 后台与桌面用户商店的跨账号接入不在本批支持范围。

## 本批实现

- Unix socket 的 `/v1` 设备、模型、引擎、资源预算、日志与持久任务接口；只读本机网络观测。
- 原生 systemd 与 Docker/Compose 模型的启停、默认选择、固定推理网关、独立开机策略及预算恢复。
- 标准模型安装委托 Pinvou 后台商店执行；关联原任务、进度、日志、取消及显式重试，恢复不重复下载。
- 组件维护和模型卸载事务的排他、持久回执与故障恢复；发现或实验登记不授予模型启停权。
- 商店逻辑卡只解析到最新成功部署。历史权重保留归档，退出普通启动、默认切换和自动恢复。
- 参数和进程身份共同核对原生实例，保留模型卡声明的上下文、槽位与预算。

本批只提供本机 `/v1`，不包含 TLS 配对、Plan/Grant/Operation、组事务、平台治理或出厂初始化器。已有这些模块的开发状态不能交给单机组件降级读取；启动及 Pinvou 更新预检保留并拒绝不支持的授权目录。无须删除任何状态来启用本服务。

## 开发验证

以下命令从 Pinvou 仓库根目录进入组件后执行：

```sh
cd components/cmsd
CARGO_BUILD_JOBS=1 cargo test --locked -- --test-threads=1
CARGO_BUILD_JOBS=1 cargo clippy --locked --all-targets -- -D warnings
cargo fmt --all -- --check
python3 -m unittest discover -s tests -p 'test_*.py'
```

真实服务测试必须显式提供独立二进制和新的证据目录，默认测试不连接用户服务或下载模型。例如：

```sh
cargo build --locked
python3 tests/real_startup_boundaries.py --binary target/debug/cmsd --evidence /absolute/new-qa/startup
python3 tests/real_gateway_listener.py --binary target/debug/cmsd --evidence /absolute/new-qa/gateway --port 28922
```

真实推理和商店纳管另外使用 `tests/real_store_adoption.py --help`。需要实际权重、CPU 模型服务及 Pinvou 后台商店，不能把模拟 API 或上述无模型测试称为推理验收。

## 配置与组件包

用户级配置为 `~/.config/megacube/cmsd.toml`，状态跟随 `$XDG_STATE_HOME/megacube`，socket 优先使用 `$XDG_RUNTIME_DIR/megacube/cmsd.sock`。原有 system 模式使用 `/etc/megacube/cmsd.toml`、`/var/lib/megacube` 和 `/run/megacube/cmsd.sock`。显式坏配置拒绝启动，不回落默认路径。

```sh
CMSD_CONFIG=/absolute/dev/cmsd.toml cargo run -- --check-config
CMSD_CONFIG=/absolute/dev/cmsd.toml cargo run -- --diagnose
python3 scripts/build-linux-component.py --sysroot /absolute/ubuntu22.04-sysroot --output /absolute/new-component
```

源码构建仅支持原生 Linux `x86_64` / `aarch64`，构建后的 ELF 必须满足 GLIBC ≤ 2.35。Ubuntu 22.04 可直接构建；较新宿主应显式提供匹配架构且包含 libc 开发文件的 `--sysroot`。未指定 sysroot 时保留调用方的 Cargo linker 配置。依赖按 `Cargo.lock` 下载；`--offline` 或 `CARGO_NET_OFFLINE=true` 可要求仅使用本地缓存。

正式构建要求组件内源码全部已被 Pinvou 当前提交跟踪且没有修改，二进制 `--component-info` 的 `build_revision` 绑定 Pinvou 完整提交 SHA。脚本在编译前后核对源码指纹，并验证二进制身份，拒绝把旧二进制归给新提交。其他 Pinvou 目录的本地修改不影响组件自身的干净检查。

开发时可显式添加 `--allow-dirty`；此时未提交源码使用 `development:<Pinvou提交>:<源码摘要>` 身份，不能作为正式发布来源。`--target-dir` 应使用默认的组件 `target/` 或组件目录外的构建缓存，`--output` 应指定新的目录。输出包括 `cmsd`、原有九字段 `manifest.json` 和构建回执 `source.json`；Pinvou 安装资源只分发前两项，回执留作构建证据。

组件清单的摘要用于校验完整性，发布者信任来自 Pinvou 自身的发布链。Pinvou 正式 `build` / `bundle` 从同仓本目录源码编译组件，`PINVOU3_CMSD_SYSROOT` 仅选择 ABI sysroot，Linux 制品仍须通过其 ELF 兼容门禁。手动离线组件包可由运行时组件管理界面验证后导入；历史独立仓库保留记录，不再双份开发。源码、配置示例和制包工具进入 Git，运行状态、凭据、权重、组件包与日志留在仓库外。

## 协议与恢复说明

[单机领域决策](docs/single-node-decisions.md) 说明状态所有权、方案取舍、故障证据与验收边界；下面的协议文档继续提供具体合同。

- [单机部署与恢复](docs/single-node.md)、[配置检查](docs/configuration-v1.md)、[启动和 socket](docs/startup-and-socket-v1.md)
- [组件维护](docs/component-maintenance-v1.md)、[模型维护事务](docs/model-maintenance-v1.md)、[预算恢复](docs/recovery-and-robustness-v1.md)
- [当前部署选择](docs/latest-installed-model-v1.md)、[原生实例隔离](docs/native-unit-isolation-v1.md)、[原生预算](docs/native-profile-budget-v1.md)
- [后台安装重试](docs/install-retry-v1.md)、[脚本监督](docs/script-runner-v1.md)、[网关可用性](docs/gateway-availability-v1.md)
- [本机网络事实](docs/network-observation-v1.md)、[只读诊断](docs/diagnostics-v1.md)、[生命周期版本](docs/lifecycle-revision-v1.md)

物理多机、整机断电恢复、其他操作系统、长上下文满负载及多日稳定性不在本批验收声明内。
