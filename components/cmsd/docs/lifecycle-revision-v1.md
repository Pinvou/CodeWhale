# 本机生命周期准入版本

`GET /v1/status` 增补 `lifecycle_revision`、`lifecycle_version_ready` 和
`lifecycle_version_error`。它是本机模型变更准入计数，与组协调权的 `control_epoch`、
设备归属的 `binding_generation`、部署的 `deployment_generation` 分开；不是动态观测序号。
本批仅提供后续 Plan 的持久版本基础，不声明 Plan、Grant、Operation 或组执行已完成。

## 持久化与共同准入

模型 enable、recover、disable、purge、set-default、switch 和商店 install 的任务
在同一 Store 锁下先检查现有维护门和任务冲突，再递增本机版本。版本标记先 fsync，任务及
`state.json` 再原子落盘，之后才允许创建工作线程或向商店下发请求。拒绝冲突不消耗版本；
已进入准入但后续保存失败可以产生空隙，不保证连续。失败任务也消耗版本，不能用任务成功数
推算版本。现有 v1 操作经过同一机制，不会留下绕过新计划前置条件的旧入口。

私有 `state_dir/lifecycle-revision.json` 绑定稳定设备 ID，另在 `state.json` 保存
`lifecycle_revision`。整数上限为 2^53−1，以适配现有 v1 精确整数编码；耗尽时拒绝变更，
不能回绕。正常重启保留版本。旧二进制不认识新字段而重新保存状态时，独立标记仍在；
升级恢复后取已知较大值再递增，使旧预览失效。标记与状态因中断不一致时采用同样规则。

这是本机回退检测机制，不是硬件防回滚。将整套旧状态和标记一并恢复可能恢复旧数值，
因此跨设备/组的旧协调者失权仍须依赖真实绑定代际、提交证据与执行围栏。

## 损坏、救援与回退

标记丢失但状态已有版本、标记损坏、设备 ID 不符、权限不合格或无法持久化时，不重置为 0。
`lifecycle_version_ready=false`，新模型变更返回 503 `lifecycle_version_unavailable`；
自动恢复/策略迁移暂停。本机事实查询、已有推理服务和独立组件救援接口仍保留。
进行中的已接单任务可以收尾，商店旧任务只关联查询，不重新下发。

设备身份读取使用非阻塞私有锁。正常查询与准入竞争只使当前请求返回 503 `identity_busy`，
不消耗版本、不设置故障围栏；竞争结束后可重新请求，仍须完整核对设备身份和版本标记。
若竞争发生在启动初始化阶段，沿启动身份查询的失败合同退出，由调用方或服务的
`Restart=on-failure` 重试；初始化完成前不启动恢复或接单；已有 socket bind 不代表已开始处理请求。

保留现场，核对并恢复相同设备的完整匹配状态备份后重启服务。不要只删标记、状态、
维护门或身份密钥来“修好”服务。同一进程中手动改文件不会解除已触发的故障围栏。
组件软件回退应只更换二进制/注册，不把旧状态快照覆盖当前身份、吊销记录或版本标记。
完整状态库无法安全解析时仍沿用原有启动拒绝策略，不会依据进程发现重建托管权限。

## 复验

```bash
CARGO_BUILD_JOBS=2 cargo test --locked -- --test-threads=2
CARGO_BUILD_JOBS=2 cargo clippy --locked --all-targets -- -D warnings
CARGO_BUILD_JOBS=2 cargo build --locked
python3 tests/real_lifecycle_revision.py --binary target/debug/cmsd \
  --legacy-binary /absolute/frozen/older/cmsd --evidence /absolute/new/private/evidence
```

单元用例覆盖并发准入、持久重载、旧状态编码/中断、丢失/损坏/符号链接、整数耗尽和保存失败。
真实验收启动新旧两个实际二进制，验证旧程序确实丢字段而标记保留、升级递增、SIGKILL 后
同版本、设备 ID 不变、12 次重复变更拒绝及匹配备份恢复。商店路径刻意不存在，属于真实
负向准入测试，不称为模型安装或推理验收；模型推理与压力证据另列。
