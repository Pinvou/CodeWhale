# Linux Rust CI 缓存与国内下载源

Linux Rust lint、测试、knowledge、CLI 和 LoongArch64 交叉编译检查使用同一个薄封装
`.github/actions/linux-rust-cache`，仍由固定版本的 Swatinem/rust-cache
负责 Rust 版本、编译环境、Cargo manifest 和锁文件的兼容性哈希及清理。
保存权限沿用调用者的 main-only 策略；PR 和合并队列只读。
LoongArch64 的三个 Cargo workspace 复用独立的归档键。交叉工具链和 sysroot
仍分别使用 `.pinvou-loong64-cache/` 下的现有缓存。
交叉编译器、链接器和 CMake 工具链的路径仍参与兼容键。工作目录路径不同时，
LoongArch64 可能需要重新预热。

自托管的一实例一容器环境中，缓存 action 将 Cargo/Rustup Home 通过容器私有
`/tmp/pinvou-rust-cache-v1/` 别名访问原实例目录。它们没有共享可写 Home，
后续编译使用同一组别名访问原实例文件，避免归档命中后因依赖路径改变而重编译。
归档 target 使用相对路径，避免实例专属绝对路径
影响缓存版本。checkout 深度及运行环境类别仍进入键，防止 tar 相对路径在不同
目录深度恢复时指向错误位置。action 内的 Rust 下载端点使用稳定值计算缓存键，实际工具链准备
仍使用 Job 自有下载监听器。原有工具链版本和编译参数继续参与键，不关闭环境哈希。
项目 compiler wrapper 通过稳定转发入口调用原脚本，原路径语义不变；脚本内容的
SHA-256 参与键，避免实例路径不同造成分裂，也避免修改 wrapper 后误用旧兼容键。
原 Cargo Home 的配置内容也参与键，不能因路径统一而混用不同编译配置。
非容器的自托管 listener 保留自己的 Home，避免固定别名被多 listener 共享。

不归档 Cargo `bin`，也不让缓存保存阶段清理安装工具：持久实例已经有自己的
工具二进制，恢复另一实例的 bin 或删除已有 rustup 会干扰工具链初始化。
新兼容键首次运行需要预热；只有后续 main 写入后，才能验证 GitHub 存档命中。
恢复编译归档不等于所有 Cargo 编译单元都可复用，Cargo 仍自行验证指纹。

Linux 自托管环境默认选择 rsproxy 的 sparse registry，配置由现有
`ci-network-environment.py` 入口管理。Cargo 保留锁文件中的来源身份及包校验值；
不要为镜像改写或重新生成锁文件。自定义 source 配置会明确拒绝自动覆盖。
换 registry 会产生一次镜像侧本地缓存预热，旧的官方缓存保留。

Rust 工具链的大文件优先从 rsproxy 获取，SHA-256 来自官方 HTTPS checksum，
并继续复用已有内容寻址下载缓存。镜像失败或校验失败后，仅在原总预算剩余时间内
尝试官方源；不绕过 TLS、校验或增加 CI 超时。官方校验元数据本身仍需要可达。
镜像 component 尝试最多 30 秒、一次，不叠加无界重试。
若慢镜像耗尽这 30 秒，官方回源能使用的时间会相应减少。接近总预算的组件可能需要显式使用官方源。

在 Job 环境显式设置 `PINVOU_CI_RUST_MIRROR=official` 可恢复官方下载策略；
`rsproxy` 可显式启用国内策略。Windows、macOS 和 GitHub 托管环境默认保持官方策略。
镜像可用性需用完整包下载和锁定校验值验证，不能只看首页 HTTP 200。

验证包括：随机本地下载端口不影响兼容缓存键；不同私有目录可以恢复相同归档；
原工具链、git 依赖和 registry 文件仍落在所属实例；镜像字节校验、失败回退、
自定义 source 拒绝覆盖。线上记录精确命中/回退命中、实际下载量和各阶段耗时，
不能将本地仿真缓存服务的成功当成 GitHub 服务已经预热。
