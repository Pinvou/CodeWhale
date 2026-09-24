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
                if attempt.previous().len() >= 10 || attempt.url().scheme() != "https" {
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
                    return resp
                        .json::<T>()
                        .map_err(|e| StoreError::new(format!("解析商店返回数据失败：{e}")));
                }
                Err(e) => {
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
}
