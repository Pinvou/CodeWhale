//! 测试模型商店的数据源：卡片信息和六类脚本直接打包在应用本地，不发任何
//! 网络请求，接口跟真实 `StoreClient` 保持一致，所以商店视图和业务逻辑
//! （ModelManager）可以原样复用，只是换一个数据来源。
//!
//! 跟 Python 版的区别：脚本不再是 .rs 源码里的巨型字符串字面量，而是放在
//! `src/data/test-store/<card-id>/*.sh` 下的真实 shell 文件，用 `include_str!`
//! 编译期嵌入——脚本本来就是 bash，用 .sh 文件存能享受语法高亮/shellcheck，
//! 改起来也不用跟 Rust 的转义规则打架。

use std::collections::BTreeMap;

use super::store_client::{
    DeployProfile, DeployTopology, StoreCardDetail, StoreCardSummary, WeightVerification,
};

macro_rules! card_scripts {
    ($dir:literal) => {{
        let mut m: BTreeMap<&'static str, &'static str> = BTreeMap::new();
        m.insert(
            "install_engine",
            include_str!(concat!(
                "../../data/test-store/",
                $dir,
                "/install_engine.sh"
            )),
        );
        m.insert(
            "download_model",
            include_str!(concat!(
                "../../data/test-store/",
                $dir,
                "/download_model.sh"
            )),
        );
        m.insert(
            "deploy",
            include_str!(concat!("../../data/test-store/", $dir, "/deploy.sh")),
        );
        m.insert(
            "enable",
            include_str!(concat!("../../data/test-store/", $dir, "/enable.sh")),
        );
        m.insert(
            "stop",
            include_str!(concat!("../../data/test-store/", $dir, "/stop.sh")),
        );
        m.insert(
            "uninstall",
            include_str!(concat!("../../data/test-store/", $dir, "/uninstall.sh")),
        );
        m
    }};
}

pub struct TestCard {
    pub summary: StoreCardSummary,
    pub source_repo_id: Option<&'static str>,
    pub profiles: Vec<DeployProfile>,
    pub scripts: BTreeMap<&'static str, &'static str>,
    /// 卡片附属文件：相对路径 -> 正文。整包跟六个入口脚本一起落到
    /// `data_dir()/cards/<card_id>/`，脚本以那里为工作目录执行。
    /// 大多数卡片是空的（六个自包含脚本就够了）。
    pub assets: BTreeMap<&'static str, &'static str>,
}

/// Qwen3.8-Flash-Next 单机卡片的交付包。
///
/// 这张卡片跟其它测试卡片形态不同：研究团队交付的是一整个目录（controller.py
/// + model_assets.py + runtime/patch 九个补丁 + SHA256SUMS 自校验），而不是六段
/// 可以随便改写的 shell。包在每次操作前都会用 SHA256SUMS 校验自己的完整性，
/// 所以必须原样整包下发，一个字节都不能改。
///
/// 这里逐个文件列出来、而不是写个 build.rs 扫目录：少一个文件包的
/// bundle_check 就会失败，让它在编译期炸掉，比等真机上跑出"缺少交付文件"好。
fn qwen38fn_single_assets() -> BTreeMap<&'static str, &'static str> {
    let mut m: BTreeMap<&'static str, &'static str> = BTreeMap::new();
    macro_rules! pkg {
        ($rel:literal) => {
            m.insert(
                $rel,
                include_str!(concat!(
                    "../../data/test-store/qwen38fn-nvfp4-single/package/",
                    $rel
                )),
            );
        };
    }
    pkg!("IMAGE-PROVENANCE.json");
    pkg!("README.md");
    pkg!("SHA256SUMS");
    pkg!("SOURCE.md");
    pkg!("UPGRADE.md");
    pkg!("VALIDATION.md");
    pkg!("build_release.py");
    pkg!("controller.py");
    pkg!("megacube.sh");
    pkg!("model_assets.py");
    pkg!("test_package.py");
    pkg!("runtime/LICENSE.upstream");
    pkg!("runtime/NOTICE.upstream");
    pkg!("runtime/patch/model_state.py");
    pkg!("runtime/patch/mtp_draft_vocab.py");
    pkg!("runtime/patch/ple_layer.py");
    pkg!("runtime/patch/ple_mmap.py");
    pkg!("runtime/patch/upstream-overlays/PROVENANCE.md");
    pkg!("runtime/patch/upstream-overlays/modelopt.py");
    pkg!("runtime/patch/upstream-overlays/ops_ple.py");
    pkg!("runtime/patch/upstream-overlays/ops_qsa.py");
    pkg!("runtime/patch/upstream-overlays/platforms_interface.py");
    pkg!("runtime/patch/upstream-overlays/qsa.py");
    // 六个入口脚本共用的一段壳，不属于交付包（所以不在 SHA256SUMS 里）
    m.insert(
        "common.sh",
        include_str!("../../data/test-store/qwen38fn-nvfp4-single/common.sh"),
    );
    m
}

fn profile(id: &str, name: &str, description: &str) -> DeployProfile {
    DeployProfile {
        id: id.to_string(),
        name: name.to_string(),
        description: Some(description.to_string()),
    }
}

/// DeepSeek-V4-Flash-Vision 双机卡片的交付包（40 个文件）。
///
/// 跟单机那张卡同一形态：整包自校验，必须原样下发，一个字节都不能改。
fn deepseek_dual_assets() -> BTreeMap<&'static str, &'static str> {
    let mut m: BTreeMap<&'static str, &'static str> = BTreeMap::new();
    macro_rules! pkg {
        ($rel:literal) => {
            m.insert(
                $rel,
                include_str!(concat!(
                    "../../data/test-store/deepseek-v4-flash-vision-dual/package/",
                    $rel
                )),
            );
        };
    }
    pkg!("README.md");
    pkg!("SHA256SUMS");
    pkg!("VALIDATION.md");
    pkg!("build_release.py");
    pkg!("controller.py");
    pkg!("megacube.sh");
    pkg!("runtime/LICENSE.upstream");
    pkg!("runtime/docker-compose.dspark.yml");
    pkg!("runtime/patches/fix-nvfp4-ds-mla-long-context.patch");
    pkg!("runtime/patches/hotfix-dsv4-assistant-final-continuation.py");
    pkg!("runtime/patches/hotfix-dsv4-dense-prefill-indexer-48407.sh");
    pkg!("runtime/patches/hotfix-dsv4-flashmla-workspace-50298.sh");
    pkg!("runtime/patches/hotfix-dsv4-grammar-advance.sh");
    pkg!("runtime/patches/hotfix-dsv4-issue133-triton-specialization.py");
    pkg!("runtime/patches/hotfix-dsv4-issue141-sparse-mla-decode-chunk.py");
    pkg!("runtime/patches/hotfix-dsv4-issue26-hybrid-swa-min.py");
    pkg!("runtime/patches/hotfix-dsv4-issue27-partial-prefill-concurrency.py");
    pkg!("runtime/patches/hotfix-dsv4-issue31-v2-thinking-budget-gpu.py");
    pkg!("runtime/patches/hotfix-dsv4-issue43-decode-fairness-and-diag.py");
    pkg!("runtime/patches/hotfix-dsv4-issue55-tool-truncation.py");
    pkg!("runtime/patches/hotfix-dsv4-mtp-buffer-50312.sh");
    pkg!("runtime/patches/hotfix-dsv4-skip-empty-c128-48957.sh");
    pkg!("runtime/patches/hotfix-dsv4-skip-topk-49486.sh");
    pkg!("runtime/patches/hotfix-dsv4-suppress-stops-in-reasoning.py");
    pkg!("runtime/patches/hotfix-dsv4-vision-exp.py");
    pkg!("runtime/patches/hotfix-encoding-dsv4-issue21.py");
    pkg!("runtime/patches/hotfix-gb10-spin-wait.sh");
    pkg!("runtime/patches/hotfix-nvfp4-ds-mla-issue22.sh");
    pkg!("runtime/patches/hotfix-vllm-empty-encoder-output.py");
    pkg!("runtime/patches/hotfix-vllm-issue136-xgrammar-termination.py");
    pkg!("runtime/patches/hotfix-vllm-issue138-responses-history.py");
    pkg!("runtime/patches/hotfix-vllm-redact-api-key-log.sh");
    pkg!("runtime/patches/keys-concurrency.patch");
    pkg!("runtime/patches/official-main-b12x-nvfp4-python.patch");
    pkg!("runtime/patches/vision_exp/__init__.py");
    pkg!("runtime/patches/vision_exp/apply.py");
    pkg!("runtime/patches/vision_exp/image_processor.py");
    pkg!("runtime/patches/vision_exp/processor.py");
    pkg!("runtime/patches/vision_exp/vision.py");
    pkg!("test_controller.py");
    m.insert(
        "common.sh",
        include_str!("../../data/test-store/deepseek-v4-flash-vision-dual/common.sh"),
    );
    m
}

/// Qwen3.8-27B × SGLang v0.5.19 卡片的厂商交付物。
///
/// 跟前两张整包卡不同，厂商这次给的是 4 个 shell + 一份说明，没有自校验清单。
/// 原件一字不改放在 `vendor/` 下：**不能落在卡片目录根上**——厂商的
/// `deploy.sh` / `stop.sh` / `uninstall.sh` 跟 LMM 的六个入口脚本重名，落在
/// 同一层会互相覆盖，六个入口脚本是最后写的，等于把厂商的三个原件抹掉。
fn qwen38_sglang0519_assets() -> BTreeMap<&'static str, &'static str> {
    let mut m: BTreeMap<&'static str, &'static str> = BTreeMap::new();
    macro_rules! pkg {
        ($rel:literal) => {
            m.insert(
                concat!("vendor/", $rel),
                include_str!(concat!(
                    "../../data/test-store/qwen38-27b-sglang0519/package/",
                    $rel
                )),
            );
        };
    }
    pkg!("DEPLOYMENT.md");
    pkg!("deploy.sh");
    pkg!("start.sh");
    pkg!("stop.sh");
    pkg!("uninstall.sh");
    // 下面两份是我们自己加的，不属于厂商交付物：
    // common.sh    六个入口脚本共用的一段壳
    // weights.sha256  权重摘要清单（规范 3.3），取自 HuggingFace 的官方 LFS 摘要
    m.insert(
        "common.sh",
        include_str!("../../data/test-store/qwen38-27b-sglang0519/common.sh"),
    );
    m.insert(
        "weights.sha256",
        include_str!("../../data/test-store/qwen38-27b-sglang0519/weights.sha256"),
    );
    m
}

/// MiniMax-H3 INT8 单机（ComfyUI 视频生成）的厂商交付物。
///
/// 这张卡的厂商交的**就是**六个 LMM 入口脚本——整个包是照《模型卡片脚本规范》
/// 1.0 做的，四个交付包里唯一一个。所以原件放 `package/` 下一字不改，顶层的六个
/// 入口脚本只做翻译（补 🟡 多源变量、补两个二进制源码归档、加心跳），详见
/// `common.sh` 开头。
///
/// 交付包里有三样没带过来，理由写在 `common.sh` 的文件头里：两个 `sources/*.tar.gz`
/// （二进制，`assets` 是 `BTreeMap<&str, &str>`、按 UTF-8 文本下发，过不来）、
/// `card.json`（元数据以本文件为准）、`SHA256SUMS`（它列着那两个 tar.gz，搬过来
/// 就是一份永远校验不过的清单）。那两个归档的 sha256 锁在 `sources/SOURCE_LOCKS.json`
/// 里，一个都没丢。
fn minimax_h3_single_assets() -> BTreeMap<&'static str, &'static str> {
    let mut m: BTreeMap<&'static str, &'static str> = BTreeMap::new();
    macro_rules! pkg {
        ($rel:literal) => {
            m.insert(
                concat!("package/", $rel),
                include_str!(concat!(
                    "../../data/test-store/minimax-h3-int8-single/package/",
                    $rel
                )),
            );
        };
    }
    // 厂商原件（六个入口脚本跟 LMM 的六个同名，所以整包必须待在子目录里，
    // 见 assets_never_shadow_the_six_entry_scripts）
    pkg!("README.md");
    pkg!("install_engine.sh");
    pkg!("download_model.sh");
    pkg!("deploy.sh");
    pkg!("enable.sh");
    pkg!("stop.sh");
    pkg!("uninstall.sh");
    pkg!("weights.sha256");
    pkg!("sources/SOURCE_LOCKS.json");
    pkg!("workflows/MiniMaxH3_QUALITY_CURRENT_1080P.json");
    pkg!("workflows/MiniMaxH3_BALANCED_FAST_1080P.json");
    // 六个入口脚本共用的一段壳，不属于厂商交付物
    m.insert(
        "common.sh",
        include_str!("../../data/test-store/minimax-h3-int8-single/common.sh"),
    );
    m
}

pub fn test_cards() -> Vec<TestCard> {
    vec![
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Single,
                id: "qwen38-27b-nvfp4-mtp".into(),
                name: "Qwen3.8-27B (NVFP4 + Native MTP)".into(),
                org: Some("Qwen".into()),
                description: Some(
                    "SGLang + NVFP4 + Native MTP，面向 H3C Cube/GB10（ARM64）。同一个模型/\
                     同一个引擎镜像下有研究团队验证过的多套部署参数方案：默认「基准测试\
                     配置」，另可切换到面向生产/Agent 场景的「生产采样配置」。"
                        .into(),
                ),
                size_bytes: 21_945_295_265,
                quantization: Some("NVFP4".into()),
                engine_name: "sglang".into(),
                engine_version: Some("v0.5.16-cu130".into()),
                // 权重校验：**没有**。download_model.sh 的 _qwen_model_complete 只查
                // index.json 里列的分片文件存不存在（download_model.sh:40-56），一个字节
                // 都不核对；install_engine.sh 写进 .env 的是 HF_REVISION=main（:375、:456），
                // 版本还是浮动的；下载走设置里的 HF_ENDPOINT（默认 hf-mirror）。
                // 三件事叠起来 = 镜像站上的副本被换过，这条链路上没有任何一环会发现。
                // 补 weights.sha256 并锁死 revision 之后，把这里改成 Sha256sums。
                weight_verification: WeightVerification::None,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            source_repo_id: Some("RadixArk/Qwen3.8-27B-NVFP4"),
            profiles: vec![
                profile(
                    "benchmark",
                    "基准测试配置（默认）",
                    "研究团队 Benchmark 验证的默认参数：8192 上下文（部署时覆盖为 65536）、\
                     BF16 KV Cache，默认只监听 127.0.0.1。",
                ),
                profile(
                    "production-sampling",
                    "生产采样配置",
                    "面向生产环境 + Agent/工具调用调过的参数：262144 上下文、FP8 E4M3 KV \
                     Cache，默认监听 0.0.0.0，带工具调用/推理内容解析和生产默认采样参数。",
                ),
            ],
            scripts: card_scripts!("qwen38-27b-nvfp4-mtp"),
            assets: Default::default(),
        },
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Single,
                id: "qwen38-27b-sglang0519".into(),
                name: "Qwen3.8-27B (NVFP4 · SGLang v0.5.19)".into(),
                org: Some("unsloth".into()),
                description: Some(
                    "同一个 Qwen3.8-27B 的另一套配方：unsloth 的 NVFP4 权重（单文件 + 独立 \
                     MTP 头，约 23.4GB）配**官方** SGLang v0.5.19 镜像，不需要任何自编译。\
                     带 EAGLE MTP 投机解码、工具调用与推理内容解析，可直接给 Agent 用。\
                     对外端口 30001——测试商店里的「Qwen3.8-27B (NVFP4 + Native MTP)」占的是 \
                     30000，两张卡可以装在同一台设备上互不打架（同时只能启用一个，128GB \
                     统一内存装不下两份）。"
                        .into(),
                ),
                // hf-mirror 上仓库元数据的精确合计：13 个文件，其中
                // model.safetensors 22,568,192,096 + model_mtp.safetensors 849,400,392
                size_bytes: 23_444_511_852,
                quantization: Some("NVFP4".into()),
                engine_name: "sglang".into(),
                engine_version: Some("v0.5.19".into()),
                // 权重校验：卡片自带 weights.sha256（随卡片下发，跟权重不同源），
                // 下完逐个文件 sha256sum -c（download_model.sh:206-214，清单路径见
                // common.sh:79）。清单只列了三个走 LFS 的大文件，但那是 23,437,581,813
                // 字节 / 99.97% 的体量；剩下十个几 KB 的配置文件官方只给 git blob sha1，
                // 由 lmm_model_complete 查存在（common.sh:85-99）。
                weight_verification: WeightVerification::Sha256sums,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            source_repo_id: Some("unsloth/Qwen3.8-27B-NVFP4"),
            profiles: vec![],
            scripts: card_scripts!("qwen38-27b-sglang0519"),
            assets: qwen38_sglang0519_assets(),
        },
        // Z-Image-Turbo 没有搬进内置测试商店。
        //
        // 它的 install_engine 需要一个 5.9MB 的离线 diffusers wheel，LMM 的做法是把这个
        // 二进制 base64 内联进脚本（落地时再 `base64 -d` 解回来）。那要求源码树里放一个
        // 5.9MB 的二进制 blob，跟本仓库「源码树不存放预编译产物」的规矩相冲。
        //
        // 不搬的代价很小：这张卡在**正式商店**里有（线上 4 张卡之一），照样能完整验证；
        // 而且它是文生图卡，不在「下载→部署→启用→对话」这条主链路上。
        //
        // 真要内置，正确解法不是把 blob 塞进仓库，而是走「从本地目录 / U 盘导入」那条
        // 路——它跟几十 GB 的引擎镜像是同一类问题，该用同一套机制。
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Single,
                id: "ornith15-bf16".into(),
                name: "Ornith-1.5-35B-A3B".into(),
                org: Some("ornith-ai".into()),
                description: Some(
                    "纯文本对话模型，vLLM 原生 serve 部署，面向 NVIDIA DGX Spark/GB10\
                     （ARM64）：直接用官方 vllm-openai 镜像自带的 vllm serve 入口，\
                     无需额外部署包。"
                        .into(),
                ),
                size_bytes: 71_928_654_040,
                quantization: Some("BF16".into()),
                engine_name: "vllm".into(),
                engine_version: Some("v0.27.1-aarch64".into()),
                // 权重校验：**没有**。跟 qwen38-27b-nvfp4-mtp 是同一份写法——
                // _ornith15_model_complete 只查分片文件存在（download_model.sh:34-50），
                // install_engine.sh:116 是 HF_REVISION=main，下载经设置里的镜像站。
                weight_verification: WeightVerification::None,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            source_repo_id: Some("ornith-ai/Ornith-1.5-35B-A3B"),
            profiles: vec![],
            scripts: card_scripts!("ornith15-bf16"),
            assets: Default::default(),
        },
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Single,
                id: "qwen38fn-nvfp4-single".into(),
                name: "Qwen3.8-Flash-Next".into(),
                org: Some("nvidia".into()),
                // 描述必须跟包里的实际运行参数对得上——用户就是照着这段判断这张卡
                // 适不适合自己的，脱节比没有描述更糟。rc3 相对 rc1 改了这几项：
                // KV 精度 FP8 E4M3 -> auto、调度并发 6 -> 1、批处理 4096 -> 2048、
                // prefix cache 关 -> 开、CUDA Graph 自定义 -> vLLM 默认。
                // 原文案点名的「FP8 E4M3 KV Cache」已经不用了，换成现在真正的卖点。
                //
                // 「候选包」三个字不能省：rc3 自己的 VALIDATION.md 写的就是
                // 「未通过新镜像 GB10 推理验收」，而我们也还没端到端验过。
                description: Some(
                    "面向单台 GB10 MegaCube（128GB 统一内存、Linux aarch64）的归档部署配方：\
                     vLLM + NVFP4 + 原生 MTP 投机解码，262144 上下文，开启前缀缓存\
                     （Agent 多轮会话实测 TTFT 降约 87%），调度并发上限 1（优先保交互延迟，\
                     不适合并发压测）。由研究团队以完整交付包形式提供，包内含 SHA256SUMS \
                     自校验、镜像摘要锁定和权重逐文件官方摘要校验，权重约 123.6GiB。\
                     当前为 1.0.0-rc3 候选包，尚未通过新镜像的真机推理验收。"
                        .into(),
                ),
                size_bytes: 132_700_000_000,
                quantization: Some("NVFP4".into()),
                engine_name: "vllm".into(),
                engine_version: Some("nightly-8a728663".into()),
                // 权重校验：逐文件比对官方摘要，但**参照来自上游自己**。
                // model_assets.py 首次下载时向 HF API 要一份文件清单（LFS 文件用
                // sha256、其余用 git blob sha1），连同解析出来的 commit SHA 一起锁进
                // model-source.json，之后每次都按它校验、对不上就重下
                // （model_assets.py:106-133；锁文件格式校验在 :35-42）。
                // revision 锁死、且固定走官方 Hub 不经镜像（download_model.sh:20-26），
                // 但清单和权重同源，上游那一份本身是坏的时候发现不了。
                weight_verification: WeightVerification::Manifest,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            source_repo_id: Some("nvidia/Qwen3.8-Flash-Next-NVFP4"),
            profiles: vec![],
            scripts: card_scripts!("qwen38fn-nvfp4-single"),
            assets: qwen38fn_single_assets(),
        },
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Dual,
                id: "deepseek-v4-flash-vision-dual".into(),
                name: "DeepSeek-V4-Flash-Vision-Exp".into(),
                org: Some("deepseek-ai".into()),
                description: Some(
                    "需要两台 GB10 MegaCube 通过 QSFP 直连线组成集群才能跑起来的多模态                     （文本 + 视觉）配方：权重约 168GB，两台各存一份，由主机通过直连线                     分发给从机。下载和部署发生在哪台设备，哪台就是主机。                     下载只用主机一台，不需要接直连线、也不需要从机在场（权重也可以                     从 U 盘拷进来）；直连线和从机是点「部署」时才需要的。部署时填一次                     从机的管理 IP、用户名和密码，之后配网、推包、配对、分发权重、启动                     全部自动完成。"
                        .into(),
                ),
                size_bytes: 168_000_000_000,
                quantization: Some("NVFP4".into()),
                engine_name: "vllm".into(),
                engine_version: Some("dspark-0.1.1".into()),
                // 权重校验：联网下载这条路是 Manifest——REVISION 是写死的 commit
                // （package/controller.py:26），snapshot_download 过程中按官方摘要逐文件
                // 校验过。**但从 U 盘拷权重那条路更弱**，而且卡片脚本自己把话说明白了
                // （download_model.sh:122-136）：没有随附 model-manifest.json 时，只能拿
                // 现有文件算出一份清单，「它记录的是当前磁盘上是什么，不构成校验」。
                // 一张卡只能声明一个档位。**别把这里读成「声明弱路的档位」**——
                // 弱路（无清单的 U 盘）严格说落的是 None。这个档位真正的来由是：
                // 弱路的存在**排除了** Sha256sums（做不到逐文件比对官方摘要），
                // 而联网那条路按定义本就是 Manifest，于是两条路的上界就是 Manifest。
                weight_verification: WeightVerification::Manifest,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            source_repo_id: Some("deepseek-ai/DeepSeek-V4-Flash-Vision-Exp"),
            profiles: vec![],
            scripts: card_scripts!("deepseek-v4-flash-vision-dual"),
            assets: deepseek_dual_assets(),
        },
        TestCard {
            summary: StoreCardSummary {
                deploy_topology: DeployTopology::Single,
                id: "minimax-h3-int8-single".into(),
                name: "MiniMax-H3 INT8 单机视频生成（MegaCube GB10）".into(),
                // card.json 里这个字段叫 "organization"，不叫规范和 StoreCardSummary
                // 里的 "org"——交付包的字段名对不上，我们按自己的结构体取名。
                //
                // 它的值也不能直接用：card.json 写的是「H3C MegaCube 本地验证」，
                // 那是**验证方**，不是模型提供方。org 在界面上显示在模型名下方，
                // 用户会把它读成"这模型是谁做的"。真正的来源在 source_repo_id 里：
                // 主模型/编码器/VAE 来自 Comfy-Org/MiniMax-H3，Turbo LoRA 来自
                // larryvrh/MiniMax-H3-Turbo-Lora。
                org: Some("Comfy-Org / larryvrh".into()),
                description: Some(
                    "ComfyUI 视频生成工作流（**不是对话模型**）：INT8 ConvRot 主模型 + NVFP4 \
                     AWQ 文本编码器 + Turbo LoRA + RealESRGAN 2× 逐帧超分，出 1080p 带同步\
                     音频的视频，权重约 40.34GiB，面向单台 MegaCube GB10（aarch64）。\
                     对外是 ComfyUI 的 HTTP 服务（提交任务走 POST /prompt + 工作流 JSON），\
                     没有 /v1/chat/completions，接不进品悟等 Agent 应用的对话模型列表。\
                     ⚠️ 交付包 README 里的「验证通过」是厂商在自己的设备上的自证，\
                     我们还没有在真机上端到端验收过，所以只在测试商店。另外装引擎时需要\
                     设备上有那份交付包（两个源码归档是二进制，商店只能下发文本）。"
                        .into(),
                ),
                // card.json 的 size_bytes，跟 README 里逐个文件列出来的合计一致。
                // 厂商的 download_model.sh 里写死了一句
                // `[ "$TOTAL" = "43317497012" ] || die`，改这个数字它会直接拒绝下载。
                size_bytes: 43_317_497_012,
                quantization: Some("INT8 ConvRot（主模型）+ NVFP4 AWQ（文本编码器）".into()),
                engine_name: "comfyui".into(),
                engine_version: Some("0.35.0+torch2.13.0-cu130-aarch64".into()),
                // 权重校验：厂商包自带 package/weights.sha256，六个文件一个不漏
                // （主模型 / 文本编码器 / 两个 VAE / Turbo LoRA / 超分模型），下完
                // sha256sum -c，对不上的**直接删掉**再非 0 退出
                // （package/download_model.sh:25、:66-75）；deploy 之前还会再校一遍
                // （package/deploy.sh:15）。三个来源仓库里两个锁了 commit SHA，
                // 第三个（RealESRGAN）是 main，但它的内容同样被清单钉死了。
                weight_verification: WeightVerification::Sha256sums,
                // 测试商店的哈希在 get_card 时按脚本正文现算，列表阶段留空
                script_sha256: Default::default(),
            },
            // card.json 里就是这么写的一行两个仓库。脚本不读它（厂商的
            // download_model.sh 把两个 repo id 和各自的 revision 都写死了，
            // 这是它锁版本的方式），所以这里保持跟 card.json 一致即可。
            source_repo_id: Some("Comfy-Org/MiniMax-H3 + larryvrh/MiniMax-H3-Turbo-Lora"),
            profiles: vec![
                profile(
                    "quality-current",
                    "当前质量配置（默认）",
                    "1376×768、243 帧、24 fps、10 步、Turbo LoRA 1.3，并逐帧 2× AI 超分后\
                     缩放为 1920×1080。",
                ),
                profile(
                    "balanced-fast",
                    "快速稳定配置",
                    "1376×768、124 帧、24 fps、6 步、Turbo LoRA 1.0，并输出 768p 与 \
                     1920×1080 两份视频。",
                ),
            ],
            scripts: card_scripts!("minimax-h3-int8-single"),
            assets: minimax_h3_single_assets(),
        },
    ]
}

/// 取某张卡某个脚本的最终内容。
///
/// LMM 里这里还有一步「把 `@@LMM_DIFFUSERS_WHEEL_B64@@` 占位符换成真实 base64」，
/// 是 Z-Image-Turbo 那张卡专用的。那张卡没有搬进来（理由见上面卡片列表处的注释），
/// 所以这一步一并去掉——留着一个永远不会命中的分支，只会让下一个人去找那个占位符
/// 是谁塞进来的。
pub fn script_content(card: &TestCard, script_type: &str) -> Option<String> {
    card.scripts.get(script_type).map(|raw| raw.to_string())
}

pub fn to_detail(
    card: &TestCard,
    scripts: BTreeMap<String, String>,
    sha256: BTreeMap<String, String>,
    assets: BTreeMap<String, String>,
    asset_sha256: BTreeMap<String, String>,
) -> StoreCardDetail {
    StoreCardDetail {
        asset_sha256: if asset_sha256.is_empty() {
            None
        } else {
            Some(asset_sha256)
        },
        assets,
        summary: card.summary.clone(),
        source_repo_id: card.source_repo_id.map(|s| s.to_string()),
        source_filename: None,
        scripts,
        script_sha256: Some(sha256),
        profiles: card.profiles.clone(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_cards_have_six_scripts() {
        for c in test_cards() {
            assert_eq!(c.scripts.len(), 6, "卡片 {} 脚本数不对", c.summary.id);
            for t in [
                "install_engine",
                "download_model",
                "deploy",
                "enable",
                "stop",
                "uninstall",
            ] {
                let body = script_content(&c, t).expect(t);
                assert!(body.starts_with("#!"), "{} 的 {t} 不像脚本", c.summary.id);
            }
        }
    }

    /// 附属文件不许跟六个入口脚本重名。
    ///
    /// 两者都落到同一个卡片目录里，入口脚本是**后**写的（见 ModelManager::prepare），
    /// 重名的话厂商原件会被静默覆盖掉——脚本还在、内容变了，报出来的错会指向一个
    /// 完全无关的地方。Qwen3.8-27B × SGLang 那张卡差点踩上：厂商包里就有
    /// deploy.sh / stop.sh / uninstall.sh，所以整包挪到了 vendor/ 下。
    #[test]
    fn assets_never_shadow_the_six_entry_scripts() {
        let entries = [
            "install_engine",
            "download_model",
            "deploy",
            "enable",
            "stop",
            "uninstall",
        ];
        for c in test_cards() {
            for name in entries {
                let clash = format!("{name}.sh");
                assert!(
                    !c.assets.contains_key(clash.as_str()),
                    "卡片 {} 的附属文件 {clash} 跟入口脚本重名，落盘时会被入口脚本覆盖掉。\
                     厂商包里同名的文件请整包放到子目录（如 vendor/）下。",
                    c.summary.id
                );
            }
        }
    }

    /// 整包交付的卡片必须能通过它自己的 SHA256SUMS 自校验。
    ///
    /// 包在设备上每次操作前都会跑 bundle_check() / verify_bundle()，任何一个
    /// 字节对不上就整个拒绝执行。而字节最容易在两个地方被悄悄改掉：git 的
    /// CRLF 转换，和有人顺手"修"了包里的 controller.py。这两种都不会有编译
    /// 错误，只会等到真机上跑出一句"交付文件已变化"。所以在这里就把校验做掉。
    #[test]
    fn packaged_cards_match_their_own_checksums() {
        use sha2::{Digest, Sha256};
        // (卡片名, 包内容, 除清单外还额外多出几个文件)
        let packs: [(&str, BTreeMap<&str, &str>, usize); 2] = [
            ("qwen38fn-nvfp4-single", qwen38fn_single_assets(), 2),
            ("deepseek-v4-flash-vision-dual", deepseek_dual_assets(), 2),
        ];
        for (card, assets, extra) in packs {
            let sums = assets
                .get("SHA256SUMS")
                .unwrap_or_else(|| panic!("{card} 的包里没有 SHA256SUMS"));
            let mut listed = 0;
            for line in sums.lines().filter(|l| !l.trim().is_empty()) {
                let (sha, name) = line
                    .split_once("  ")
                    .unwrap_or_else(|| panic!("{card} 的 SHA256SUMS 行格式不对：{line}"));
                let content = assets.get(name).unwrap_or_else(|| {
                    panic!("{card} 缺少交付文件：{name}（没在 assets 里列出来）")
                });
                assert_eq!(
                    crate::platform::encoding::hex_lower(&Sha256::digest(content.as_bytes())),
                    sha,
                    "{card} 的交付文件 {name} 跟 SHA256SUMS 对不上（多半是 CRLF 转换）"
                );
                listed += 1;
            }
            assert!(
                listed >= 20,
                "{card} 的 SHA256SUMS 只列了 {listed} 个文件，包像是残缺的"
            );
            // 清单之外只允许多出 SHA256SUMS 自己和我们加的 common.sh
            assert_eq!(
                assets.len(),
                listed + extra,
                "{card} 的 assets 里有清单外的多余文件"
            );
        }
    }

    /// 双机卡的交付包里带着一个**我们自己打的临时补丁**，这条测试是为了让它
    /// 永远藏不住。
    ///
    /// 包原本的 gid_index() 遍历 255 个 GID 槽位而不防空槽，较新的 mlx5 驱动读
    /// 空槽返回 EINVAL，于是 preflight 整段崩掉，check / deploy / start 全过不去
    /// （真机实测 255 个槽里 251 个报错，而正确答案就在剩下几个里）。为了先把
    /// 双机流程跑通，我们在本地给它加了 try/except 并重算了 SHA256SUMS。
    ///
    /// **研究团队修好之后**：换回官方包，这条测试会失败——那时把补丁和这条测试
    /// 一起删掉即可。测试失败正是我们要的提醒，别把它改成"有没有都行"。
    #[test]
    fn dual_card_still_carries_our_local_gid_patch() {
        let assets = deepseek_dual_assets();
        let controller = assets.get("controller.py").expect("包里没有 controller.py");
        assert!(
            controller.contains("[LMM 本地补丁"),
            "controller.py 里的本地补丁不见了。
             如果是换成了研究团队修好的官方包：确认新包里 gid_index 已经会跳过空 GID              槽位（读到 EINVAL 时 continue），然后把这条测试一并删掉。
             如果是误删：这个补丁不打的话，双机卡在 ConnectX + 新 mlx5 驱动上必然             在 preflight 崩溃。"
        );
    }

    /// MiniMax-H3 的权重总量在三个地方各写了一遍，必须永远相等。
    ///
    /// 厂商的 download_model.sh 开头有一句硬检查：
    /// `[ "$TOTAL" = "43317497012" ] || die`——卡片登记的 size_bytes 只要跟它差
    /// 一个字节，用户点下载得到的就是一句「LMM_MODEL_SIZE_BYTES 应为 …」，而且
    /// 是在真机上、下载那一步才发现。这条测试把它挪到编译期。
    ///
    /// 第三处是我们自己的 download_model.sh：它在调厂商脚本之前先拦一道，好让
    /// 错误信息指向"去改 test_store_data.rs"而不是一串环境变量名。
    #[test]
    fn minimax_size_bytes_stays_in_lockstep_with_the_vendor_pin() {
        const PIN: &str = "43317497012";
        let cards = test_cards();
        let card = cards
            .iter()
            .find(|c| c.summary.id == "minimax-h3-int8-single")
            .expect("测试商店里没有 minimax-h3-int8-single");
        assert_eq!(
            card.summary.size_bytes.to_string(),
            PIN,
            "卡片登记的 size_bytes 跟交付包锁定的 {PIN} 对不上，厂商的 download_model.sh 会拒绝下载"
        );
        let vendor = card
            .assets
            .get("package/download_model.sh")
            .expect("厂商的 download_model.sh 没在 assets 里");
        assert!(
            vendor.contains(PIN),
            "厂商的 download_model.sh 里找不到 {PIN} 了——交付包换过版本？\
             请核对新包锁的总量，并同步 size_bytes 和我们自己的 download_model.sh"
        );
        let ours = script_content(card, "download_model").expect("download_model");
        assert!(
            ours.contains(PIN),
            "我们自己的 download_model.sh 里那道前置拦截不见了"
        );
    }

    /// MiniMax-H3 的 package/ 里不许出现 card.json 和 SHA256SUMS。
    ///
    /// 两个都是好心办坏事的典型，所以钉在这里：
    /// - `card.json`：check_card.py 的 detect_layout 看到 `*/card.json` 就会把这张
    ///   卡判成"研究团队交付包"，转而去查 package/ 里厂商那六个脚本——我们自己写的
    ///   六个入口脚本反倒一项都不查了，而且元数据会变成两份各改各的。
    /// - `SHA256SUMS`：厂商那份列着两个二进制 tar.gz，而它们按规范 1.4 根本进不了
    ///   卡片（附属文件是按 UTF-8 文本下发的）。搬过来就是一份永远校验不过的清单。
    ///   那两个归档的 sha256 锁在 sources/SOURCE_LOCKS.json 里，没有丢。
    #[test]
    fn minimax_package_carries_no_unusable_manifest() {
        let assets = minimax_h3_single_assets();
        for forbidden in ["package/card.json", "package/SHA256SUMS"] {
            assert!(
                !assets.contains_key(forbidden),
                "{forbidden} 不该被带进内置卡，理由见本测试的注释和 common.sh 开头"
            );
        }
        assert!(
            assets.contains_key("package/sources/SOURCE_LOCKS.json"),
            "SOURCE_LOCKS.json 没了——两个源码归档就再没有可核对的 sha256 了"
        );
    }

    /// 防的是「哪张卡的脚本里还留着只有 LMM 才会替换的占位符」。
    ///
    /// LMM 用 `@@LMM_..._B64@@` 这类占位符在运行时把二进制内联进脚本。那套机制
    /// 没有搬过来，所以任何一张卡的脚本里都不该再出现这种东西——留着的话脚本会
    /// 带着一串字面量的 `@@...@@` 落到设备上，然后以一句莫名其妙的话失败。
    #[test]
    fn no_card_still_carries_an_unsubstituted_placeholder() {
        for card in test_cards() {
            for script_type in card.scripts.keys() {
                let body = script_content(&card, script_type).unwrap();
                assert!(
                    !body.contains("@@LMM_"),
                    "{} 的 {} 里还留着占位符，但替换机制已经没有了",
                    card.summary.id,
                    script_type
                );
            }
        }
    }

    /// 声明的权重校验档位必须跟卡片里**真实存在的东西**对得上。
    ///
    /// 这条不是把上面那张表再抄一遍（那种测试只会在改表时一起被改掉）。它查的是
    /// 两件可证伪的事：
    ///   - 标了 `Sha256sums` 的卡，卡片自己得真的带着一份摘要清单，而且真的有一步
    ///     去 `sha256sum -c` 它。光有清单没人校验，跟没有清单是一回事。
    ///   - 标了 `None` 的卡，卡片里**不该**躺着一份没人用的清单——那种情况要么是
    ///     档位标低了，要么是校验步骤掉了，两种都得有人看一眼。
    #[test]
    fn declared_weight_verification_matches_what_the_card_actually_ships() {
        for card in test_cards() {
            let id = &card.summary.id;
            let manifest = card
                .assets
                .keys()
                .find(|k| k.ends_with("weights.sha256"))
                .copied();
            // 校验这个动作可能写在我们的入口脚本里（sglang0519），也可能写在厂商
            // 原件里由入口脚本转调（minimax），所以两边一起找。
            let verifies = card
                .scripts
                .values()
                .chain(card.assets.values())
                .any(|body| body.contains("sha256sum -c"));
            match card.summary.weight_verification {
                WeightVerification::Sha256sums => {
                    let path = manifest.unwrap_or_else(|| {
                        panic!(
                            "{id} 标了 sha256sums，但卡片里没有 weights.sha256。\
                             清单必须随卡片下发才算这一档——从上游现要的摘要跟权重同源，\
                             那是 manifest 档。"
                        )
                    });
                    assert!(
                        verifies,
                        "{id} 带着 {path}，却没有任何一处 `sha256sum -c` 去用它。\
                         放着不校验的清单等于没有清单，档位该降到 manifest 或 none。"
                    );
                }
                WeightVerification::Manifest | WeightVerification::None => {
                    assert!(
                        manifest.is_none(),
                        "{id} 里躺着一份 {} 却没声明 sha256sums。要么是档位标低了，\
                         要么是校验那一步掉了——两种都得有人去看一眼，不能默认放过。",
                        manifest.unwrap_or("weights.sha256")
                    );
                }
            }
        }
    }

    /// 这两张卡的「下载完成」判据只是文件存在性，必须一直是最不安全那一档。
    ///
    /// 单独钉出来是因为它们是评审点名的那两张，而**把档位从 `None` 改上去是一行
    /// 的事**——改完这条测试不会红（上面那条只管"有清单就得校验"，管不了
    /// "没清单却吹自己校验了"）。所以留一道专门的闸：真补上了 weights.sha256 或
    /// 锁死了 revision，来这里连同注释一起改，别只改卡片那一行。
    #[test]
    fn existence_only_cards_stay_declared_as_unverified() {
        for id in ["ornith15-bf16", "qwen38-27b-nvfp4-mtp"] {
            let cards = test_cards();
            let card = cards
                .iter()
                .find(|c| c.summary.id == id)
                .unwrap_or_else(|| panic!("测试商店里没有 {id}"));
            assert_eq!(
                card.summary.weight_verification,
                WeightVerification::None,
                "{id} 的 download_model.sh 只查分片文件存不存在，revision 还是浮动的 \
                 main，下载默认经镜像站——在这三件事里任何一件被修掉之前，它就是 none"
            );
            let body = script_content(card, "download_model").expect("download_model");
            assert!(
                !body.contains("sha256sum"),
                "{id} 的 download_model.sh 里出现了 sha256sum：如果是真的补上了逐文件\
                 校验，请把档位一并改掉，并修正这条测试"
            );
        }
    }

    /// 许可未清权的卡。**这不是备忘，是闸门的一半**——另一半在
    /// `THIRD_PARTY_NOTICES.md` 的 open item 段落里，两边必须对得上。
    ///
    /// 为什么不写进发版清单：清单靠人记得去读。这两个包的情况是「作者没说过按什么
    /// 条款给我们用」，而它随 `.deb` 分发——这种事一旦漏过一次发版，就没有第二个
    /// 环节会拦住它。写成测试之后，它只会在被解决时消失，不会在被遗忘时消失。
    const CARDS_WITH_UNRESOLVED_LICENCE: &[&str] =
        &["minimax-h3-int8-single", "qwen38-27b-sglang0519"];

    /// 厂商交付包要么自带许可声明，要么挂在 open item 上。
    ///
    /// 判据是 `assets` 空不空：空的那两张（`ornith15-bf16`、`qwen38-27b-nvfp4-mtp`）
    /// 只下发我们自己写的入口脚本，没有 vendor 任何第三方内容，不存在清权问题；
    /// 非空的才是把别人的文件逐字节放进了本仓库。
    ///
    /// 四个方向都要红，少一个这道闸门就会漏：
    ///   - 新卡带了交付包、没有许可、也没登记 → 漏登记
    ///   - 登记在案的卡补上了许可文件，但没人回来删这条 → 名单越挂越长，最后没人信
    ///   - 名单里的 id 对不上任何一张真卡 → 改名/删卡之后名单变成一句空话
    ///   - `THIRD_PARTY_NOTICES.md` 里不再提这个 id → 对外那份说明和代码里这份脱节
    #[test]
    fn vendored_packages_either_carry_a_licence_or_stay_declared() {
        const NOTICES: &str = include_str!("../../../../../../../THIRD_PARTY_NOTICES.md");

        // 只认包内自带的声明文件。上游 tarball 里那份不算——它是运行时下到设备上的，
        // 不随我们分发，清权要清的是"我们仓库里这些字节"。
        fn carries_licence(card: &TestCard) -> bool {
            card.assets.keys().any(|k| {
                let name = k.rsplit('/').next().unwrap_or(k).to_ascii_uppercase();
                name.starts_with("LICENSE")
                    || name.starts_with("LICENCE")
                    || name.starts_with("COPYING")
            })
        }

        let cards = test_cards();
        for card in &cards {
            let id = card.summary.id.as_str();
            let declared = CARDS_WITH_UNRESOLVED_LICENCE.contains(&id);
            if card.assets.is_empty() {
                assert!(
                    !declared,
                    "{id} 没有 vendor 任何第三方内容（assets 为空），不该挂在未清权名单上"
                );
                continue;
            }
            match (carries_licence(card), declared) {
                (true, false) | (false, true) => {}
                (false, false) => panic!(
                    "{id} 带着交付包却没有许可声明，也没登记在 \
                     CARDS_WITH_UNRESOLVED_LICENCE 里。要么把作者给的许可文件放进包内，\
                     要么把它加进名单并同步写进 THIRD_PARTY_NOTICES.md 的 open item。"
                ),
                (true, true) => panic!(
                    "{id} 已经带上许可声明了，但还挂在 CARDS_WITH_UNRESOLVED_LICENCE \
                     名单上。把它从名单和 THIRD_PARTY_NOTICES.md 的 open item 里一起删掉——\
                     名单留着过期条目，下一个人就不会再认真看它。"
                ),
            }
        }

        for id in CARDS_WITH_UNRESOLVED_LICENCE {
            assert!(
                cards.iter().any(|c| c.summary.id == *id),
                "未清权名单里的 {id} 对不上任何一张内置卡。卡改名或下架时，\
                 这条名单和 THIRD_PARTY_NOTICES.md 都要跟着改。"
            );
            assert!(
                NOTICES.contains(id),
                "THIRD_PARTY_NOTICES.md 里找不到 {id}。对外那份说明是给读者看的，\
                 代码里这份是给 CI 看的，两份说的必须是同一件事。"
            );
        }
    }
}
