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
        let url = format!("{}{}", self.base_url, path);
        let client = reqwest::blocking::Client::builder()
            .timeout(API_TIMEOUT)
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
        self.get_json(&format!("/api/v1/cards/{card_id}"))
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
                dir.join(format!("{script_type}.sh")).to_string_lossy().to_string(),
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
            asset_hashes.insert(rel.to_string(), crate::platform::encoding::hex_lower(&Sha256::digest(content.as_bytes())));
        }
        Ok(super::test_store_data::to_detail(&card, scripts, hashes, assets, asset_hashes))
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
        for (s, t) in [("single", DeployTopology::Single), ("dual", DeployTopology::Dual)] {
            let parsed: DeployTopology = serde_json::from_str(&format!("\"{s}\"")).unwrap();
            assert_eq!(parsed, t);
            assert_eq!(serde_json::to_string(&t).unwrap(), format!("\"{s}\""));
        }
        assert_eq!(DeployTopology::Dual.node_count(), 2);
    }
}
