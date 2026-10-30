# PR #1995：执行回执与复跑入口

本目录保存作者的实际运行输出及完整命令，供审阅者下载、校验和独立复跑。它不表示外部审阅者已经运行，也不代替人工批准或云端门禁。

本包的历史固定源码为 `e768c60efdf52bbae38032bcb0565809aa8183ee`，tree 为 `e3700ee8906eeb2df226d5c14aff8b7e8358de62`，CodeWhale 为 `f1eb10efcfe2468fde42d1724d7f5f19d54b904c`。该签署提交包含当轮五个代码文件。运行期间只有本文目录及 `docs/model-store-updates.md` 在整理；产品不嵌入这两处文档。历史回执提交 `a52872c2ddb27aa5df819ab6d954013bece077fb` 只改这两处文档，当时的产品、测试、锁文件及工作流输入与固定源码保持一致。此结论只锚定这两个历史提交，不延伸到此后主线整合或新的 PR head。

## 后续主线整合边界

2026-10-30 为处理真实冲突，将主线 `e41ec811905f8fef72ea412bc7924ede6a8261b9`（#2225）整合到 PR；该主线变更包含 CI、测试入口和 runner 镜像配置。因此，新工作树的全部工作流输入与本包固定源码不再相同。保留本包日志、计数、源码摘要及 manifest 的历史身份，不将重基后的执行结果回填到这些原始输出。

本次整合保留原 PR 更严格的 Connector 矩阵断言，并继续直接检查发布/交叉构建的真实运行器表达式和磁盘分支。产品 Rust、React、资源及 CodeWhale 输入保持原字节；这项源码等价检查不代替新组合的 CI 与测试验证。新组合的实际复跑结果另在 [PR 更新说明](https://github.com/Pinvou/pinvou3/pull/1995) 中记录，不能从下表的历史计数推算。

## 已执行结果

| 入口 | 实际版本及结果 | 输出 |
|---|---|---|
| Linux APP lib | 固定源码：**5401 passed / 0 failed / 21 ignored**，含 333 条 model_store 通过行 | [完整输出](current/linux-app-rust.stdout.txt) |
| 真实 Wry / EnginePool | 固定源码：显式执行 **1 passed / 0 failed / 0 ignored** | [完整输出](current/live-wry-pool.stdout.txt) |
| 生产失效标记负控 | 同一最终测试源码；临时移除共享生产处理器中的唯一 `mark_model_updated` 调用，确实在第二次同配置启用的真实 mailbox 重建断言失败；原字节恢复后通过 | [失败输出](current/production-marker-negative.stdout.txt)、[恢复输出](current/restored-hostile-proxy.stdout.txt) |
| 完整 Node | 固定源码：**2123 passed / 0 failed / 16 skipped**；全文以 gzip 无损压缩 | [完整压缩输出](current/node.stdout.txt.gz) |
| Python fast-gate | 固定源码：**846 tests / OK** | [完整输出](current/python-fastgate.stdout.txt) |
| 控制器人工入口 | 固定源码：**8 tests / OK**，其中一项含 **200 次真实子进程启停** | [完整输出](current/controller.stdout.txt) |
| Chrome fixture | 固定源码：实际 Chrome，**12 个 PASS 分组**，使用 JS IPC 桩 | [完整输出](current/chrome.stdout.txt) |
| 生产 / 测试 Clippy | 固定源码：生产 `-D warnings` exit 0；测试目标按原 CI warn 策略 exit 0，lib test 有 **19018** 条 warning | [生产全文](current/clippy-production.stdout.txt)、[测试首尾节选](current/clippy-tests.excerpt.txt) |
| 测试登记 | 固定源码：5422 个唯一用例，12 个必需用例齐全 | [完整输出](current/registry.stdout.txt) |
| Connector 矩阵负控 | 实际完整 Node 单文件入口；60 个错误变体全部拒绝，两次正控通过 | [输出](current/connector-controls.stdout.txt)、[可复跑脚本](replay-connector-matrix-controls.py) |
| CodeWhale Engine loopback | **历史回执**：父仓 `948f9b60f`，同一 gitlink `f1eb10efc`，6 passed / 0 failed | [完整输出](historical/codewhale-f1-engine.stdout.txt) |

完整 Linux lib 的 21 项忽略包含新加的显示环境用例；该用例另外显式运行通过，不能把默认忽略记为通过或重复相加。Linux 全文包含实际 OFD 存活后代、SIGKILL、第二进程/跨卡准入、配额、部分删除及恢复用例的准确名称和结果。桌面/Web Vite 构建、格式、架构、fork fast 与提交规范/DCO 也在固定源码通过，具体 argv、exit、时间、原始输出摘要均列在 manifest。

## 校验输出与源码

[manifest.json](manifest.json) 分别记录原始字节与公开字节的 SHA-256、长度、UTC 起始时间、argv、cwd、特定环境变量和边界。全文替换本地工作区、target、生成的临时目录及运行用户目录前缀，并去除行尾空格及末尾多余空行以满足仓库空白检查；测试名和结果保持，公开字节不冒充原始字节；Clippy 测试日志的首尾节选明确标出省略范围。Node gzip 内容解压后仍是全文，另外记录解压后的摘要。作者日志与外部独立执行是不同证据。

第一条 diff 只验证两个固定历史提交之间除回执文档外没有差异；第二条检查当前 checkout 的产品、测试及工作流目录没有未提交改动。两条都应为空，但不证明当前 HEAD 与历史源码等价。后续主线整合必须单独核对并验证：

```sh
git diff --exit-code e768c60efdf52bbae38032bcb0565809aa8183ee a52872c2ddb27aa5df819ab6d954013bece077fb -- . \
  ':(exclude)docs/model-store-updates.md' \
  ':(exclude)docs/model-store-review-evidence/2026-10-29'
git diff --exit-code HEAD -- pinvou3-app CodeWhale scripts .github
python3 - <<'PY_HASH'
import gzip, hashlib, json
from pathlib import Path
root = Path('docs/model-store-review-evidence/2026-10-29')
manifest = json.loads((root / 'manifest.json').read_text())
for item in manifest['files']:
    data = (root / item['path']).read_bytes()
    assert len(data) == item['published_bytes'], item['path']
    assert hashlib.sha256(data).hexdigest() == item['published_sha256'], item['path']
    if item.get('encoding') == 'gzip':
        decoded = gzip.decompress(data)
        assert len(decoded) == item['decoded_bytes'], item['path']
        assert hashlib.sha256(decoded).hexdigest() == item['decoded_sha256'], item['path']
print('published evidence hashes: OK')
PY_HASH
```

可用 `python3 -m gzip -d` 解压 Node 输出副本；避免覆盖证据原件。严格复跑源码也可在单独 checkout 中固定到上述完整源码 SHA；证据与负控脚本位于随后仅文档的提交，测试本体在源码提交中已完整保存。

## 复跑命令

按 [CONTRIBUTING.md](../../../CONTRIBUTING.md) 准备系统依赖，初始化公共 CodeWhale submodule，并安装 npm 锁定依赖。这些准备会下载公开源码/依赖。以下入口从仓库根执行，使用临时夹具、受控本机进程及回环服务，不需要 GPU、真实权重或外部模型账号。保存输出时须保留真实 exit code，例如 Bash `set -o pipefail` 后使用 `tee`，筛选必须实际匹配非零用例。

复现上表历史结果时，应在单独 checkout 固定到上述历史源码 SHA，并从历史回执提交取得本目录。更新后的工作树也可运行相同入口，但所得结果属于其实际 HEAD，需另记源码版本和输出。

```sh
git submodule update --init --recursive
npm --prefix pinvou3-app ci
CHROME=/usr/bin/google-chrome npm --prefix pinvou3-app run test:model-store-versions-ui
npm --prefix pinvou3-app run test:node
python3 -m unittest discover -s scripts/tests -p 'test_*.py'
python3 docs/model-store-review-evidence/2026-10-29/replay-connector-matrix-controls.py
python3 scripts/test-registry-guard.py
npm --prefix pinvou3-app run build:ui
npm --prefix pinvou3-app run build:web
python3 scripts/architecture-guard.py
./scripts/fork-guard.sh --fast
```

Linux Rust（cwd 为 `pinvou3-app/src-tauri`）：

```sh
cargo test --locked --no-default-features --lib --features benchmark-hooks -- --test-threads=1
cargo test --locked --no-default-features --lib --features benchmark-hooks \
  app::model_store_events::platform_tests::linux_live_pool_tests::identical_enable_rebuilds_real_pool_and_next_turn_preserves_history \
  -- --ignored --exact --nocapture --test-threads=1
cargo clippy --locked --lib --bins --no-deps --features dev-tools -- -D warnings
cargo clippy --locked --lib --bins --tests --no-deps --features dev-tools
cargo fmt -- --check
```

真实 Wry 用例需要可用 Linux `DISPLAY`、GTK/WebKit；没有显示环境会失败。测试创建隐藏窗口，隔离 HOME、CodeWhale、凭据文件和 WebKit 数据目录，清除子进程的代理并明确绕过代理。它通过真实 Wry Webview 的生成命令分发执行 lifecycle/service/SQLite/settings，使用同一 app 管理的真实 EnginePool 和 `get_or_spawn`，核对真实 mailbox 复用/重建、下一轮 SSE 及持久化历史。HTTP/SSE 提供方是回环夹具，未覆盖 JavaScript 页面传输、GPU 模型或其它 OS。第二次成功启用要求全量设置不变，避免配置变化掩盖失效标记遗漏；无关配置使用独立端点，完整配置与真实引擎均保持。

本机是 Ubuntu 24.04.4 aarch64 / glibc 2.39，不是 CI 22.04 runner。额外设置 `PINVOU_REQUIRE_REMOTE_E2E=1`、`CHROME=/usr/bin/google-chrome`，使用独立 Cargo target；bindgen 的主机参数为 `BINDGEN_EXTRA_CLANG_ARGS='-isystem /usr/lib/gcc/aarch64-linux-gnu/13/include'`，其它机器应采用自身实际头文件位置。root 记录的 rustc/cargo 为 1.98.0，Node 22.23.1，Python 3.12.3，Chrome 153.0.8010.36。所有命令的实际参数以 manifest 为准。

控制器完整发布源未随本包分发。可从发布方取得，或下载已验证的不可变 by-hash 输入，先验证摘要再运行：

```sh
PINVOU_CONTROLLER_TEST_DIR=$(mktemp -d)
curl --fail --location --proto '=https' \
  'https://account.pinvou.com/model-store/assets/by-hash/5cb4539ce50c34948ac22bc720d2fce2f08e840e615a963860453f9e1c321bc7' \
  --output "$PINVOU_CONTROLLER_TEST_DIR/native_engine_process.py"
printf '%s  %s\n' \
  '5cb4539ce50c34948ac22bc720d2fce2f08e840e615a963860453f9e1c321bc7' \
  "$PINVOU_CONTROLLER_TEST_DIR/native_engine_process.py" | sha256sum --check
python3 scripts/test-native-engine-launch-receipt.py \
  "$PINVOU_CONTROLLER_TEST_DIR/native_engine_process.py"
```

输入为 14453 字节，链为 `5cb453… → 6b9968… → 868605…`，完整三段摘要和 URL 见 manifest。测试在临时副本修复，不改输入；部分身份时序用例有模拟，200 次循环保留实际 Popen、主机进程发现及 /proc 身份核对。它不覆盖未知最新控制器、真实模型或 GPU。历史底座的六项入口为 `cargo test --manifest-path CodeWhale/Cargo.toml -p codewhale-tui --locked --test integration partial_input_engine_ -- --test-threads=1`；它不替代本轮真实应用池接缝。

## 保留的验收边界

Chrome 的 JS IPC 桩、MockRuntime service 夹具与本轮真实 Wry/EnginePool 是分别运行的边界，不能拼成同一条浏览器到原生推理端到端会话。默认忽略、测试 Clippy warning、历史底座版本与未执行平台均按原范围记录。

本包未提供真实 runner 镜像构建、Docker Compose 部署、GPU/权重、Windows/macOS 原生或完整安装包证明。最终云端适用 required gates、维护工作流安排、人工评审及四条已回复讨论仍以 PR 实况为准；标题检查、作者计数和子 agent 通过均不替代这些门禁。
