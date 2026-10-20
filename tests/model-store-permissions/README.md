# 模型商店 fresh-only 权限回归

在 Linux、Rust/Cargo 及依赖缓存就绪时，从仓库根目录运行：

```sh
bash tests/model-store-permissions/run.sh
```

runner 使用 locked/offline 构建，在独立子进程中分别继承 umask0002、0022、0077；每种掩码运行5组断言，共15组。不会执行卡片脚本、下载模型、访问设备或修改真实用户数据。

2026-10-20整合验证：WSL Linux下15组全部通过、exit0；应用与本harness的cargo fmt检查及architecture-guard均通过。未执行完整应用编译或ARM打包。

## 覆盖

直接编译生产 config.rs、platform filesystem.rs/paths.rs；build.rs 提取并编译生产 ScriptsSource::prepare 和 materialize_assets 的原始函数体。资产来源、HOME、keyring外围是隔离夹具，不等同于完整应用/ARM或真实引擎验证。

- 不预置模型商店目录，验证实际配置路径与prepare新建目录0700。
- 实际assets含native_engine_process.py时，prepare在脚本执行/健康日志前预建CARD_DIR/.runtime0700；普通多文件卡不创建runtime。
- 既有755/775配置、staging、models、cards父目录和runtime保持原权限；receipt字节保留。
- 明确验证原shared asset writer仍会把既有卡片目录775改为700，runtime不受此写入器影响。
- 无全局umask修改；无关mkdir仍遵循继承掩码。

## 安全与兼容边界

本修复仅将原create_dir_all换成DirBuilderExt::mode(0700).recursive(true).create，并为当前native多文件契约预建runtime。既有目录仍按原递归创建语义处理；initializer不chmod、不判断历史trust、不认领进程或receipt、不新增符号链接/父链防护。

路径定位API仍可能忽略创建错误并返回路径，不是全局阻断边界。既有runtime775不会被initializer洗白，后续原脚本的mode/identity检查仍须拒绝；本测试不执行该脚本，也不声称已验证其拒绝行为。

shared writer及其旧chmod行为未改；不能宣称整个prepare不chmod旧目录。旧775历史仍需独立安全迁移，手工改权限不证明metadata可信。下载脚本自行创建的模型leaf仍可能受umask影响；这里只初始化配置模型根，不覆盖任意下载子目录。单文件卡入口及原runtime身份检查不变。
