//! 模型商店后台 API 客户端。
//!
//! `StoreClient` 连真正的商店服务器（按设置里的 store_url）；测试模型商店
//! （`LocalStoreClient`，阶段 7 移植 test_store_data 时补）没有服务器、卡片和
//! 脚本直接打包在应用本地，接口保持一致，业务逻辑层（model_manager）不感知
//! 数据源差异。

use std::time::Duration;

use serde::{Deserialize, Serialize};

use super::errors::AppError;

pub type StoreError = AppError;

const API_TIMEOUT: Duration = Duration::from_secs(15);

/// 单独的连接阶段时限，取值与 `llmapi_hub/adapter.rs` 的 `HTTP_CONNECT_TIMEOUT_SECS`
/// 一致。见 `get_json` 里的说明：没有它，握不上手的链路会把整个 API_TIMEOUT 吃光。
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);

/// 同一个引擎+模型组合下，不同的部署参数方案（比如"基准测试配置" vs
/// "生产采样配置"）——研究团队会不定期针对同一个模型调出新的参数组合，用户
/// 可能已经用旧方案下载/部署/启用着，不该逼着他们重新下载模型才能用上新参数，
/// 所以方案是挂在同一张卡片下面的，不是另开一张新卡片。
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeployProfile {
    pub id: String,
    pub name: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
}

/// 卡片作者实测的运行内存需求。
///
/// 权重文件大小不是运行时内存需求：量化方式、KV cache、上下文长度和引擎都会让两者
/// 相差很大。因此模型商店只接受卡片随部署方案下发的实测峰值；字段缺失时宁可告诉
/// 用户“无法可靠预测”，绝不把磁盘体积伪装成内存预算。
#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct ResourceRequirements {
    /// 统一内存设备（如 GB10）在稳定推理负载下的峰值。单位 MiB。
    #[serde(default)]
    pub unified_memory_peak_mib: Option<u64>,
    /// 独立主机内存设备上的运行峰值。单位 MiB。
    #[serde(default)]
    pub host_ram_peak_mib: Option<u64>,
    /// 独立显存设备上的运行峰值。单位 MiB；当前仅展示，准入不和主机内存相加。
    #[serde(default)]
    pub gpu_vram_peak_mib: Option<u64>,
}

impl ResourceRequirements {
    /// 当前启用准入应扣减的内存预算。统一内存优先，避免把同一块内存算两遍。
    pub fn admission_peak_mib(&self) -> Option<u64> {
        self.unified_memory_peak_mib.or(self.host_ram_peak_mib)
    }
}

/// 这张卡把权重下下来之后，**凭什么说这份权重就是它声称的那份**。
///
/// 为什么要有这个字段：内置卡之间的完整性保障差着两个数量级，界面上却长得一模
/// 一样。`ornith15-bf16` / `qwen38-27b-nvfp4-mtp` 的「下载完成」判据只是
/// 「index.json 里列的分片文件都存在」——一个字节都没核对过，revision 还是浮动的
/// `main`，默认经 hf-mirror；而 `qwen38-27b-sglang0519` 会拿卡片自带的
/// `weights.sha256` 把 23GB 逐个文件算一遍。权重投毒的后果是输出被操纵，用户至少
/// 有权知道自己站在哪一档上。
///
/// 三档的分界是**参照物从哪来**，不是「有没有跑过哈希」：
///
/// - `Sha256sums`：摘要清单**随卡片下发**（卡片本身走 https + 逐脚本哈希，见
///   `StoreClient::get_json` 里那段理由），跟权重字节不同源。镜像站上的副本被换
///   过时能当场发现。
/// - `Manifest`：确实逐文件比对了摘要，但摘要是下载时从上游 Hub 自己要来的（或
///   首次下载时锁进本地清单）。挡得住传输损坏和事后篡改，挡不住上游那一份本身
///   就是坏的。deepseek 卡 `download_model.sh` 里那句自陈说的正是这件事：拿刚从
///   这些文件算出来的哈希再去比对这些文件，永远相等。
/// - `None`：只查文件存在 / 大小。
///
/// **缺省是 `None`**，理由见下面 `From<Option<String>>` 的注释。
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize, Default)]
#[serde(rename_all = "snake_case", from = "Option<String>")]
pub enum WeightVerification {
    #[default]
    None,
    Manifest,
    Sha256sums,
}

/// 只认这三个字面量，**其余一律落到 `None`**——字段缺失、`null`、以及未来某个后台
/// 下发的、这个版本还不认识的新档位名。
///
/// 为什么不用 serde 的派生解析：派生出来的枚举遇到不认识的取值会**报错**，一张卡
/// 的新字段能把整个商店列表打成「连不上」。而"解析失败就退回 `#[serde(default)]`"
/// 这条路在别的字段上无所谓，在这个字段上等于「不认识的档位 = 不出警告」，正好把
/// 警告关在最该出现的那一刻。
///
/// 所以这里是一条安全方向明确的单行道：不认识就是没校验，宁可多出一条黄条，
/// 不可少出一条。
impl From<Option<String>> for WeightVerification {
    fn from(raw: Option<String>) -> Self {
        match raw.as_deref() {
            Some("sha256sums") => Self::Sha256sums,
            Some("manifest") => Self::Manifest,
            _ => Self::None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StoreCardSummary {
    pub id: String,
    /// 这张卡需要几台设备。缺省 single —— 老后台不下发这个字段时，按单机处理，
    /// 行为跟改动前完全一样。
    #[serde(default)]
    pub deploy_topology: DeployTopology,
    pub name: String,
    #[serde(default)]
    pub org: Option<String>,
    #[serde(default)]
    pub description: Option<String>,
    #[serde(default)]
    pub size_bytes: u64,
    #[serde(default)]
    pub quantization: Option<String>,
    pub engine_name: String,
    #[serde(default)]
    pub engine_version: Option<String>,
    /// script_type -> sha256。列表接口就带上，让"打开应用/打开商店时同步一次"
    /// 只需要一个请求就能顺带比对出哪些已下载模型的脚本有更新。
    /// 老后台不下发时是空 map，比对逻辑会跳过。
    #[serde(default)]
    pub script_sha256: std::collections::BTreeMap<String, String>,
    /// 权重完整性档位，界面据此对不达标的卡出提示条。
    ///
    /// 缺省 `None`（= 没校验）而不是「当作已校验」：老后台和忘了填这个字段的新卡
    /// 都会被划进最不安全那一档，于是"漏填"的代价是多一条黄条，而不是**静默地**
    /// 把一张没校验的卡摆成跟达标卡一模一样。反过来定缺省值的话，这个字段从上线
    /// 第一天起就只对"记得填"的人生效，那等于没做。
    #[serde(default)]
    pub weight_verification: WeightVerification,
    /// 由模型卡片下发的实测运行内存档位。老卡片没有时保留空值并走保守提示。
    #[serde(default)]
    pub resource_requirements: ResourceRequirements,
}

/// 卡片的部署拓扑。目前只区分单机和双机直连；多机以后再说，不预留半成品。
#[derive(Debug, Clone, Copy, PartialEq, Eq, serde::Serialize, serde::Deserialize, Default)]
#[serde(rename_all = "lowercase")]
pub enum DeployTopology {
    #[default]
    Single,
    /// 两台设备通过 QSFP 直连；下载/部署所在的那台是主机
    Dual,
}

impl DeployTopology {
    /// 需要几台设备
    pub fn node_count(self) -> u8 {
        match self {
            Self::Single => 1,
            Self::Dual => 2,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct StoreCardDetail {
    #[serde(flatten)]
    pub summary: StoreCardSummary,
    #[serde(default)]
    pub source_repo_id: Option<String>,
    #[serde(default)]
    pub source_filename: Option<String>,
    /// script_type -> 下载 URL（或测试商店里的本地文件路径）
    #[serde(default)]
    pub scripts: std::collections::BTreeMap<String, String>,
    /// 后台下发的脚本内容哈希（script_type -> sha256 hex），客户端执行前校验；
    /// 老后台不下发这个字段时为 None，跳过校验保持兼容
    #[serde(default)]
    pub script_sha256: Option<std::collections::BTreeMap<String, String>>,
    /// 卡片的**附属文件**：相对路径 -> 下载 URL。
    ///
    /// 六个入口脚本之外的一切（compose.yaml、controller.py、patch、子目录……）
    /// 都放这里。像 MegaCube 那种自带控制器的发布包有二三十个文件，塞不进
    /// 「六个脚本」的模型；老卡片没有这个字段，是空 map，行为完全不变。
    #[serde(default)]
    pub assets: std::collections::BTreeMap<String, String>,
    /// 附属文件的内容哈希（相对路径 -> sha256 hex），下发了就强制校验
    #[serde(default)]
    pub asset_sha256: Option<std::collections::BTreeMap<String, String>>,
    /// 部署参数方案列表；只有一个（或没有）方案的卡片这里是空列表，界面上不用
    /// 露出"选方案"这一步，跟没有方案概念的卡片体验完全一样。
    #[serde(default)]
    pub profiles: Vec<DeployProfile>,
}

/// 商店数据源统一接口：真实 HTTP 商店和本地测试商店都实现它，
/// model_manager 只面向这个 trait 编程。
pub trait StoreSource: Send + Sync {
    fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError>;
    fn get_card(&self, card_id: &str) -> Result<StoreCardDetail, StoreError>;
}

/// 商店 API 最多试几次。见 get_json 里的说明。
const STORE_API_ATTEMPTS: u32 = 3;

/// 商店 JSON 响应的体积上限。
///
/// 商店地址是用户可配的，无界 `json()` 等于把「往内存里灌多少」交给对端决定。
/// 64 MiB 取自仓库既有先例 `marketplace/python_dependencies.rs`（`MAX_WHEEL_BYTES`），
/// 保持同一个量级，免得两处各定一个数。卡片列表实际是几十 KB 量级，离这条线很远。
const MAX_STORE_RESPONSE_BYTES: u64 = 64 * 1024 * 1024;

/// 按上限读响应体时的两种失败。分成两支不是为了好看：**「超限」绝不能重试**
/// （对端下一次还会给同样大的东西，重试只是把内存压力乘以次数），而「读到一半
/// 断了」恰恰是这条链路上最常见的瞬时故障（见 script_runner 的
/// `SCRIPT_FETCH_ATTEMPTS`：真机上就是被一句 `error decoding response body`
/// 打断过），必须能重试。只返回一个 `AppError` 的话调用方分不出来。
pub(super) enum CappedReadError {
    /// 超过上限，带上当时用的上限（调用方拼错误文案要用）。
    TooLarge(u64),
    /// 读取过程本身失败，带底层原文。
    Io(String),
}

/// 把一个 HTTP 响应体按上限读进内存。
///
/// 为什么要有上限：商店地址是**用户在设置里自己填的**，无界的 `.json()` /
/// `.text()` 等于把「往我的内存里灌多少」这个决定权交给对端——一个填错的地址或
/// 一台被改过的商店，回一条几个 G 的 body 就能把应用打到 OOM。而脚本正文更要紧：
/// 它是在 sha256 校验**之前**整体进内存的，校验拦不住 OOM。
///
/// 两道一起来：
/// 1. 先信 header 声明的长度，能在读任何字节之前就退出；
/// 2. header 可以不声明（chunked 就没有 Content-Length）、也可以撒谎，所以照样
///    用 `take(max+1)` 截断读。多读 1 字节是为了把「正好等于上限」和「超了」
///    区分开——少了这一字节就是个 off-by-one，正好卡在线上的响应会被误杀。
///
/// 抽成独立函数有两个理由：`store_client::get_json` 和
/// `script_runner::http_get_text` 要的是同一套判据，一份实现比抄两遍安全；而且
/// 这段逻辑原先整段没有测试（删掉它套件依旧全绿），抽出来之后可以拿任意
/// `Read` 直测边界，**包括 chunked 那种不声明长度、第一道预检完全盖不住的情况**。
///
/// `max_bytes` 做成参数只是为了测试能用小上限跑真实边界；生产调用方都传
/// `MAX_STORE_RESPONSE_BYTES`。
pub(super) fn read_capped_body(
    declared_length: Option<u64>,
    reader: impl std::io::Read,
    max_bytes: u64,
) -> Result<Vec<u8>, CappedReadError> {
    if declared_length.is_some_and(|len| len > max_bytes) {
        return Err(CappedReadError::TooLarge(max_bytes));
    }
    let mut capped = std::io::Read::take(reader, max_bytes + 1);
    let mut body = Vec::new();
    std::io::Read::read_to_end(&mut capped, &mut body)
        .map_err(|e| CappedReadError::Io(e.to_string()))?;
    if body.len() as u64 > max_bytes {
        return Err(CappedReadError::TooLarge(max_bytes));
    }
    Ok(body)
}

/// 这一跳重定向要不要拒绝。
///
/// 两条：目标必须仍然是 https（上面那道 https 门只管得住首跳，见 `get_json`），
/// 跳数不超过 10（reqwest 默认上限）。抽成具名纯函数只是为了能直测——照同域
/// `script_runner::redirect_is_refused` 的先例，判据本身一字未改。
fn redirect_is_refused(url: &reqwest::Url, previous_hops: usize) -> bool {
    previous_hops >= 10 || url.scheme() != "https"
}

pub struct StoreClient {
    base_url: String,
}

impl StoreClient {
    pub fn new(base_url: &str) -> Self {
        Self {
            base_url: base_url.trim_end_matches('/').to_string(),
        }
    }

    fn get_json<T: serde::de::DeserializeOwned>(&self, path: &str) -> Result<T, StoreError> {
        // 商店 API 必须是 https。这条不是为了"接口安全"这种泛泛的理由：卡片 JSON
        // **就是脚本哈希的载体**，script_runner 拿到的 `expected_sha256` 全部来自
        // 这个响应。这条信道一旦是明文，中间人可以把脚本和哈希一起替换掉，
        // 下游那套"取回后逐字节校验"就只剩防传输损坏的作用，防不了伪造——而
        // install_engine 是经 pkexec 以 root 执行的。
        //
        // 代价说清楚：内网自建的 http 商店会被这条拒掉，用户得给自己的商店配上
        // https。放行 http 等于让哈希校验变成摆设，不值得。
        if !self.base_url.starts_with("https://") {
            return Err(StoreError::new(
                "模型商店地址必须是 https——卡片里的脚本哈希要靠这条链路本身保真，\
                 明文传输时校验挡不住伪造。请在设置里把商店地址改成 https。",
            ));
        }
        // 上面那道 https 门只管得住**首跳**。reqwest 默认会跟随最多 10 次重定向，
        // 而默认策略不挑协议：商店回一个 `302 → http://…`，脚本哈希就照样从明文
        // 链路取回来了，上面那段理由等于白写。所以重定向策略要自己给：非 https
        // 的跳转一律拒掉，并且把"拒了哪个地址"写进错误里——否则调用方看到的是
        // 一个 3xx 响应体解析失败，指向完全错误的方向。
        // 写法照搬 marketplace/python_dependencies.rs 里下载 wheel 的那处。
        //
        // 顺带一提：那里还带了主机白名单，这里没有——商店地址是用户在设置里自己
        // 填的，本来就没有"可信主机"这个名单可言。
        let url = format!("{}{}", self.base_url, path);
        let client = reqwest::blocking::Client::builder()
            // 只设总超时的话，一条连不上的慢链路能把 15 秒预算全耗在 TCP/TLS
            // 握手上，界面要等满 15 秒才看到"连不上"。对照 llmapi_hub/adapter.rs
            // 的 blocking_client()，两个超时都设。
            .connect_timeout(CONNECT_TIMEOUT)
            .timeout(API_TIMEOUT)
            .redirect(reqwest::redirect::Policy::custom(|attempt| {
                if redirect_is_refused(attempt.url(), attempt.previous().len()) {
                    let redirect_url = attempt.url().clone();
                    attempt.error(format!("商店把请求重定向到了不该跟的地址：{redirect_url}"))
                } else {
                    attempt.follow()
                }
            }))
            .build()
            .map_err(|e| StoreError::new(format!("创建 HTTP 客户端失败：{e}")))?;

        // 商店现在走的是一条反向隧道（公网入口 -> ssh -R -> 源站），而且那个入口
        // 在服务器上标着"临时"。隧道重连、入口偶发断连时，请求会在 TLS 握手阶段
        // 被直接掐掉，或者拿到一个 502——都是瞬时的，隔一秒再试就好。之前这里
        // 一次失败就整页报「连不上」。
        //
        // 只重试**快速失败**：超时本身已经等满了 API_TIMEOUT，再重试只会让页面
        // 卡得更久；4xx 是商店明确的回答，重试多少次结果都一样。
        let mut last_err = String::new();
        for attempt in 1..=STORE_API_ATTEMPTS {
            match client.get(&url).send() {
                Ok(resp) => {
                    let status = resp.status();
                    if status.as_u16() == 404 {
                        return Err(StoreError::new("未找到对应的商店内容"));
                    }
                    if status.is_server_error() && attempt < STORE_API_ATTEMPTS {
                        last_err = format!("模型商店返回错误（{}）", status.as_u16());
                        std::thread::sleep(std::time::Duration::from_secs(attempt as u64));
                        continue;
                    }
                    if !status.is_success() {
                        return Err(StoreError::new(format!(
                            "模型商店返回错误（{}）",
                            status.as_u16()
                        )));
                    }
                    // 这份响应体不能无界读进内存，理由与两道防线的写法见
                    // `read_capped_body`。这里不重试：超限重试没意义，读失败也
                    // 已经消耗掉了一次完整的请求预算。
                    let declared = resp.content_length();
                    let body = match read_capped_body(declared, resp, MAX_STORE_RESPONSE_BYTES) {
                        Ok(body) => body,
                        Err(CappedReadError::TooLarge(max)) => {
                            return Err(StoreError::new(format!(
                                "模型商店返回的数据超过 {} MiB 上限，已中止",
                                max / 1024 / 1024
                            )));
                        }
                        Err(CappedReadError::Io(e)) => {
                            return Err(StoreError::new(format!("读取商店返回数据失败：{e}")));
                        }
                    };
                    return serde_json::from_slice::<T>(&body)
                        .map_err(|e| StoreError::new(format!("解析商店返回数据失败：{e}")));
                }
                Err(e) => {
                    // 重定向被上面那条策略拒了：这是**来源判定**的结果，不是网络
                    // 抖动。重试三次只会把同一条 302 再走三遍，而且拒绝的理由会
                    // 被 `humanize_reqwest_error` 整段吃掉——它没有 is_redirect
                    // 分支、也不挖 source 链，用户最后只看到「操作失败，请稍后
                    // 重试」，而策略明明把「拒了哪个地址」写进了错误里。
                    //
                    // reqwest 的 Display 只印一句 "error following redirect for
                    // url …"，真正那句话在它的 source 上。同 PR 的
                    // `script_runner` 对同一场景就是这么处理的，这里对齐。
                    if e.is_redirect() {
                        let cause = std::error::Error::source(&e)
                            .map(|src| src.to_string())
                            .unwrap_or_else(|| e.to_string());
                        return Err(StoreError::new(cause));
                    }
                    last_err = format!(
                        "{}（请检查设置里的模型商店地址是否正确、设备是否能访问外网）",
                        humanize_reqwest_error(&e)
                    );
                    if e.is_timeout() || attempt == STORE_API_ATTEMPTS {
                        break;
                    }
                    std::thread::sleep(std::time::Duration::from_secs(attempt as u64));
                }
            }
        }
        Err(StoreError::new(last_err))
    }
}

impl StoreSource for StoreClient {
    fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError> {
        self.get_json("/api/v1/cards")
    }

    fn get_card(&self, card_id: &str) -> Result<StoreCardDetail, StoreError> {
        // card_id 直接拼进 URL 路径，所以先过一遍 `safe_path_component`——它只放
        // 行 [A-Za-z0-9._-] 并挡掉 `.` / `..`，这个字符集在 URL 路径段里本来就
        // 不需要转义，于是"校验"同时也就是"编码"。
        //
        // 刻意复用落盘那条路径的同一个谓词，而不是在这儿单写一套百分号编码：
        // card_id 既要进 URL 又要进 `cards/<id>` 目录（lifecycle.rs 用的就是这个
        // 函数），两处判据一旦分家，就会出现"能下载但落不了盘"或者反过来的错位。
        let safe = super::config::safe_path_component(card_id)
            .map_err(|e| StoreError::new(format!("卡片 id 不合法：{e}")))?;
        self.get_json(&format!("/api/v1/cards/{safe}"))
    }
}

/// 测试模型商店：卡片和六类脚本都打包在应用里，不发任何网络请求。脚本内容
/// 落到本地一个固定目录下，供 script_runner 像对待"下载下来的脚本"一样读取
/// 执行；哈希也自己算，让"下载即验签"这条代码路径在调试期就被反复执行，而
/// 不是只在连真实后台时才第一次跑。
pub struct LocalStoreClient {
    scripts_dir: std::path::PathBuf,
}

impl LocalStoreClient {
    pub fn new() -> Self {
        let scripts_dir = super::config::data_dir().join("test_store_scripts");
        let client = Self { scripts_dir };
        client.materialize();
        client
    }

    /// 把内置脚本写到磁盘（每次构造都重写一遍：应用升级后脚本可能改过，
    /// 不能让上一版留下的旧文件继续被执行）。
    ///
    /// 附属文件（整包交付的卡片才有）落到同一张卡的 `assets/` 子目录下，跟
    /// 六个入口脚本分开放，免得包里恰好也有个叫 `deploy.sh` 的文件被覆盖。
    /// 真正执行时 ModelManager 会把它们再展开到卡片工作目录，这里只是把
    /// "商店提供的文件"这一层如实模拟出来。
    fn materialize(&self) {
        for card in super::test_store_data::test_cards() {
            let dir = self.scripts_dir.join(&card.summary.id);
            if std::fs::create_dir_all(&dir).is_err() {
                continue;
            }
            for script_type in card.scripts.keys() {
                if let Some(content) = super::test_store_data::script_content(&card, script_type) {
                    let _ = std::fs::write(dir.join(format!("{script_type}.sh")), content);
                }
            }
            if !card.assets.is_empty() {
                let files = card
                    .assets
                    .iter()
                    .map(|(k, v)| (k.to_string(), v.to_string()))
                    .collect();
                let _ = super::script_runner::materialize_assets(&dir.join("assets"), &files);
            }
        }
    }

    fn find_card(&self, card_id: &str) -> Result<super::test_store_data::TestCard, StoreError> {
        super::test_store_data::test_cards()
            .into_iter()
            .find(|c| c.summary.id == card_id)
            .ok_or_else(|| StoreError::new(format!("未找到测试商店卡片：{card_id}")))
    }
}

impl Default for LocalStoreClient {
    fn default() -> Self {
        Self::new()
    }
}

impl StoreSource for LocalStoreClient {
    fn list_cards(&self) -> Result<Vec<StoreCardSummary>, StoreError> {
        Ok(super::test_store_data::test_cards()
            .into_iter()
            .map(|c| c.summary)
            .collect())
    }

    fn get_card(&self, card_id: &str) -> Result<StoreCardDetail, StoreError> {
        use sha2::{Digest, Sha256};
        let card = self.find_card(card_id)?;
        let dir = self.scripts_dir.join(card_id);
        let mut scripts = std::collections::BTreeMap::new();
        let mut hashes = std::collections::BTreeMap::new();
        for script_type in card.scripts.keys() {
            let content = super::test_store_data::script_content(&card, script_type)
                .ok_or_else(|| StoreError::new(format!("测试卡片缺少 {script_type} 脚本")))?;
            scripts.insert(
                script_type.to_string(),
                dir.join(format!("{script_type}.sh"))
                    .to_string_lossy()
                    .to_string(),
            );
            hashes.insert(
                script_type.to_string(),
                crate::platform::encoding::hex_lower(&Sha256::digest(content.as_bytes())),
            );
        }
        let mut assets = std::collections::BTreeMap::new();
        let mut asset_hashes = std::collections::BTreeMap::new();
        for (rel, content) in &card.assets {
            assets.insert(
                rel.to_string(),
                dir.join("assets").join(rel).to_string_lossy().to_string(),
            );
            asset_hashes.insert(
                rel.to_string(),
                crate::platform::encoding::hex_lower(&Sha256::digest(content.as_bytes())),
            );
        }
        Ok(super::test_store_data::to_detail(
            &card,
            scripts,
            hashes,
            assets,
            asset_hashes,
        ))
    }
}

/// 把 reqwest 的英文错误转成用户能看懂的中文提示（对应 Python 版
/// errors.humanize_error 里针对 requests 异常类型的分支）。
pub fn humanize_reqwest_error(e: &reqwest::Error) -> String {
    if e.is_timeout() {
        "连接超时，请检查网络（或尝试在设置里切换镜像地址）".to_string()
    } else if e.is_connect() {
        "网络连接失败，请检查网络是否可用".to_string()
    } else if e.is_request() {
        "网络请求失败，请检查网络连接后重试".to_string()
    } else {
        "操作失败，请稍后重试".to_string()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 老后台不下发 deploy_topology 时必须按单机处理。历史上架的卡片全是单机，
    /// 这里要是判成双机，用户点下载会被前置检查拦下——等于所有老卡片一起失效。
    #[test]
    fn cards_without_topology_default_to_single() {
        let json = r#"{
            "id": "old-card", "name": "老卡片", "engine_name": "vllm"
        }"#;
        let c: StoreCardSummary = serde_json::from_str(json).unwrap();
        assert_eq!(c.deploy_topology, DeployTopology::Single);
        assert_eq!(c.deploy_topology.node_count(), 1);
    }

    #[test]
    fn resource_requirements_prefer_unified_memory_and_never_infer_from_weight_size() {
        let unified = ResourceRequirements {
            unified_memory_peak_mib: Some(48 * 1024),
            host_ram_peak_mib: Some(16 * 1024),
            gpu_vram_peak_mib: Some(32 * 1024),
        };
        assert_eq!(unified.admission_peak_mib(), Some(48 * 1024));

        let discrete = ResourceRequirements {
            unified_memory_peak_mib: None,
            host_ram_peak_mib: Some(12 * 1024),
            gpu_vram_peak_mib: Some(24 * 1024),
        };
        assert_eq!(discrete.admission_peak_mib(), Some(12 * 1024));

        // 旧卡没有运行内存档位时，必须显式返回未知；调用方据此请求用户确认，
        // 而不是把下载后的权重文件大小编造成内存需求。
        assert_eq!(ResourceRequirements::default().admission_peak_mib(), None);
    }

    #[test]
    fn topology_round_trips_as_lowercase() {
        // 数据库里存的就是这两个字符串，跟服务端下发的写法必须一致
        for (s, t) in [
            ("single", DeployTopology::Single),
            ("dual", DeployTopology::Dual),
        ] {
            let parsed: DeployTopology = serde_json::from_str(&format!("\"{s}\"")).unwrap();
            assert_eq!(parsed, t);
            assert_eq!(serde_json::to_string(&t).unwrap(), format!("\"{s}\""));
        }
        assert_eq!(DeployTopology::Dual.node_count(), 2);
    }

    /// 没声明 = 没校验。这条是整个功能的地基：如果字段缺失时按"已校验"处理，
    /// 那么所有存量卡和将来忘了填的卡都会静默地不出警告，界面上的黄条就只对
    /// "记得填的人"生效——那等于没做。
    #[test]
    fn cards_without_weight_verification_are_treated_as_unverified() {
        let json = r#"{
            "id": "old-card", "name": "老卡片", "engine_name": "vllm"
        }"#;
        let c: StoreCardSummary = serde_json::from_str(json).unwrap();
        assert_eq!(c.weight_verification, WeightVerification::None);
        // null 跟"没有这个字段"是同一回事，别让它从旁边溜过去
        let with_null = r#"{
            "id": "old-card", "name": "老卡片", "engine_name": "vllm",
            "weight_verification": null
        }"#;
        let c: StoreCardSummary = serde_json::from_str(with_null).unwrap();
        assert_eq!(c.weight_verification, WeightVerification::None);
    }

    /// 认识的三档原样round-trip；**不认识的取值退到最不安全那一档，而不是报错**。
    ///
    /// 报错的后果比看上去严重：反序列化失败的是整张卡片，一张卡带了新档位名就能
    /// 把整个商店列表打成"连不上"。
    #[test]
    fn weight_verification_round_trips_and_degrades_safely() {
        for (s, t) in [
            ("none", WeightVerification::None),
            ("manifest", WeightVerification::Manifest),
            ("sha256sums", WeightVerification::Sha256sums),
        ] {
            let parsed: WeightVerification = serde_json::from_str(&format!("\"{s}\"")).unwrap();
            assert_eq!(parsed, t);
            assert_eq!(serde_json::to_string(&t).unwrap(), format!("\"{s}\""));
        }
        for unknown in ["\"signed\"", "\"SHA256SUMS\"", "\"\""] {
            let parsed: WeightVerification = serde_json::from_str(unknown).unwrap();
            assert_eq!(
                parsed,
                WeightVerification::None,
                "不认识的档位名 {unknown} 必须当成没校验：认不出来就说明这个版本\
                 判断不了它的强度，不能替用户打包票"
            );
        }
    }

    // ------------------------------------------------ 重定向判据与响应体上限
    //
    // 这两段是这个文件里唯一的**行为**逻辑，而在补这些用例之前它们零测试覆盖：
    // 把上限那几行或 `is_redirect` 那一支整段删掉，套件依旧全绿。以下用例的
    // 存在意义就是让那种删除变红。

    /// 重定向判据。照同域 `script_runner::redirect_is_refused` 的直测先例来。
    ///
    /// 盯的是两件不能松的事：明文 http 一律拒（`get_json` 开头那道 https 门只管
    /// 得住首跳，卡片 JSON 是脚本哈希的载体，第二跳走明文等于校验白做），
    /// 以及跳数有上限（否则一个重定向环能把请求挂死）。
    #[test]
    fn redirect_policy_refuses_plaintext_and_long_chains() {
        let https = reqwest::Url::parse("https://store.example/api/v1/cards").unwrap();
        let http = reqwest::Url::parse("http://store.example/api/v1/cards").unwrap();
        let ftp = reqwest::Url::parse("ftp://store.example/api/v1/cards").unwrap();

        assert!(!redirect_is_refused(&https, 0), "正常的 https 跳转被拦了");
        assert!(redirect_is_refused(&http, 0), "302 → 明文 http 被放行了");
        assert!(redirect_is_refused(&ftp, 0), "302 → 非 http(s) 被放行了");

        // 跳数边界：第 10 跳（前面已经走过 10 次）必须拒，第 9 跳还得放行。
        // 差一位就是要么提前掐掉正常的多跳 CDN，要么根本没有上限。
        assert!(!redirect_is_refused(&https, 9));
        assert!(redirect_is_refused(&https, 10), "重定向环没有上限");
    }

    /// 上限的行为级验证：**不声明 Content-Length** 的响应（chunked 就是这样）
    /// 第一道预检完全盖不住，只能靠第二道的 `take(max+1)` 截断。
    ///
    /// 边界逐字节验：正好等于上限必须过（少读那 1 字节就会把它误杀），
    /// 超一字节必须拒。上限做成参数就是为了能在这里用小数值跑真实边界，
    /// 而不是真的造一份 64 MiB。
    #[test]
    fn capped_read_enforces_limit_without_content_length() {
        // 正好等于上限：放行，且内容一字不少。
        let exact = vec![b'a'; 8];
        let body = read_capped_body(None, exact.as_slice(), 8).expect("正好等于上限的响应被误杀了");
        assert_eq!(body, exact);

        // 超一字节：必须拒，而且带回当时用的上限（调用方要拿它拼文案）。
        let over = vec![b'a'; 9];
        match read_capped_body(None, over.as_slice(), 8) {
            Err(CappedReadError::TooLarge(max)) => assert_eq!(max, 8),
            Ok(_) => panic!("不声明 Content-Length 的超限响应被整条读进内存了"),
            Err(CappedReadError::Io(e)) => panic!("应当报超限，却报了读取失败：{e}"),
        }

        // 空响应体也得是正常的 Ok（后面 serde 会自己报解析失败，不该在这里变成
        // 「超限」这种误导性的错误）。
        assert!(
            read_capped_body(None, b"".as_slice(), 8)
                .unwrap()
                .is_empty()
        );
    }

    /// 第一道预检：header 声明的长度超限时，**一个字节都不该读**。
    ///
    /// 用一个读到就 panic 的 Read 来证明这件事——只断言返回 Err 的话，实现把
    /// 预检整段删掉、靠第二道兜住，测试照样绿，而那恰恰丢掉了「不用把几个 G
    /// 拉下来才知道太大」这个全部价值。
    #[test]
    fn capped_read_rejects_oversized_content_length_without_reading() {
        struct Exploding;
        impl std::io::Read for Exploding {
            fn read(&mut self, _buf: &mut [u8]) -> std::io::Result<usize> {
                panic!("声明了超限长度，却还是去读了响应体");
            }
        }
        match read_capped_body(Some(9), Exploding, 8) {
            Err(CappedReadError::TooLarge(max)) => assert_eq!(max, 8),
            _ => panic!("header 声明超限时没有拒绝"),
        }
        // 声明的长度正好等于上限：这是合法响应，预检不能拦（拦了就是 off-by-one）。
        assert_eq!(
            read_capped_body(Some(8), vec![b'a'; 8].as_slice(), 8)
                .unwrap()
                .len(),
            8
        );
    }

    /// 读到一半断了要报 `Io` 而不是 `TooLarge`。
    ///
    /// 这个区分是有后果的：`script_runner::http_get_text` 按它决定要不要重试，
    /// 判错了的话一次瞬时的传输中断会被当成「商店给的东西太大」直接放弃，
    /// 把一次跑到一半的部署打断。
    #[test]
    fn capped_read_reports_transport_failure_separately() {
        struct Truncated;
        impl std::io::Read for Truncated {
            fn read(&mut self, _buf: &mut [u8]) -> std::io::Result<usize> {
                Err(std::io::Error::new(
                    std::io::ErrorKind::UnexpectedEof,
                    "connection reset",
                ))
            }
        }
        match read_capped_body(None, Truncated, 8) {
            Err(CappedReadError::Io(e)) => assert!(e.contains("connection reset"), "丢了原文：{e}"),
            _ => panic!("传输中断被报成了别的东西，调用方的重试判断会跟着错"),
        }
    }

    /// 生产上限就是 64 MiB，跟 `marketplace/python_dependencies.rs` 同一个量级。
    /// 上面的边界用例都用小上限跑，这条负责把真实取值本身钉住。
    #[test]
    fn production_cap_is_64_mib() {
        assert_eq!(MAX_STORE_RESPONSE_BYTES, 64 * 1024 * 1024);
    }
}
