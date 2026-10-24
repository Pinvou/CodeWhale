//! DMI（桌面管理接口）产品信息读取的单一真相源。
//!
//! `product_name` / `product_family` / `board_name` 三个候选字段的清单原本在
//! `platform/capabilities.rs`（能力位：任一字段含 "megacube"）与
//! `features/device/platform/linux.rs`（设备页展示：第一个非空字段）各写一份，
//! 聚合口径还不同——两处单边修改候选表就会漂移（第三轮评审 P2）。这里收敛为
//! 一份字段清单 + 一个读取函数，两个消费方各自在此之上做聚合。
//!
//! 非 Linux 平台这些 sysfs 路径不存在，读取自然失败返回 None（进程级 OnceLock
//! 缓存后只付一次 3 个 stat 的成本）；`is_megacube_device` 的判定函数再以运行时
//! OS 判断兜底恒 false——两层都是 fail-closed，无需 cfg。

/// DMI id 字段候选（`/sys/class/dmi/id/<field>`），顺序即优先级。
const DMI_ID_FIELDS: &[&str] = &["product_name", "product_family", "board_name"];

/// 读单个 DMI 字段并 trim，限长 64（SMBIOS 规范单字符串上限 64 字节，但固件可
/// 违反，且内核支持 `dmi_product_name=` 启动参数覆写为任意自由文本——该值进每拍
/// 快照 JSON 与 doctor 云端上下文，与本仓对同类"环境不可信文本"的 64 字符上限
/// 纪律对齐，第二十轮评审 P3）；缺失/为空 → None。
pub(crate) fn dmi_field(field: &str) -> Option<String> {
    let text = std::fs::read_to_string(format!("/sys/class/dmi/id/{field}")).ok()?;
    let trimmed = text.trim();
    (!trimmed.is_empty() && !is_dmi_placeholder(trimmed)).then(|| cap_dmi_text(trimmed))
}

/// 固件占位值（纯函数，测试直打）：通用板卡常把 product_name 留成
/// "To Be Filled By O.E.M." / "Default string" / "System Product Name" 之类——
/// 它们不是型号，原样进设备页头部与 doctor 云端上下文只会误导（二十一轮评审
/// P3）。按小写精确匹配一张小表，再加 "to be filled" 子串臂兜固件各式变体。
fn is_dmi_placeholder(value: &str) -> bool {
    const PLACEHOLDERS: &[&str] = &[
        "default string",
        "system product name",
        "system version",
        "system manufacturer",
        "base board product name",
        "not specified",
        "not applicable",
        "n/a",
        "none",
        "unknown",
        "o.e.m.",
        "oem",
        "xxxxxx",
    ];
    let lower = value.trim().to_ascii_lowercase();
    lower.contains("to be filled") || PLACEHOLDERS.contains(&lower.as_str())
}

/// 限长口径（纯函数，测试直打）：生产路径 [`dmi_field`] 的唯一出口。
fn cap_dmi_text(trimmed: &str) -> String {
    trimmed.chars().take(64).collect()
}

/// 第一个非空字段值（设备页头部展示用）。
pub(crate) fn dmi_first_nonempty() -> Option<String> {
    let values: Vec<Option<String>> = DMI_ID_FIELDS.iter().map(|field| dmi_field(field)).collect();
    first_nonempty(&values)
}

/// 任一字段含 `needle`（小写匹配，能力位用）。读取 sysfs 后走
/// [`any_value_contains`] 的同一份聚合口径——匹配语义只有一份实现，测试直打
/// 纯函数而不是内联重写（第四轮评审 P2：此前的测试把表达式抄了一遍，生产路径
/// 丢掉小写化/换包含语义照样全绿）。
pub(crate) fn any_field_contains(needle: &str) -> bool {
    let values: Vec<Option<String>> = DMI_ID_FIELDS.iter().map(|field| dmi_field(field)).collect();
    any_value_contains(&values, needle)
}

/// 聚合口径（纯函数，测试直打）：第一个非空值。
fn first_nonempty(values: &[Option<String>]) -> Option<String> {
    values.iter().flatten().next().cloned()
}

/// 聚合口径（纯函数，测试直打）：任一非空字段值小写化后包含 needle 即命中。
fn any_value_contains(values: &[Option<String>], needle: &str) -> bool {
    values
        .iter()
        .flatten()
        .any(|value| value.to_ascii_lowercase().contains(needle))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn placeholder_values_are_dropped_but_real_models_kept() {
        for placeholder in [
            "To Be Filled By O.E.M.",
            "to be filled by o.e.m",
            "Default string",
            "System Product Name",
            "None",
            "N/A",
            "Not Specified",
        ] {
            assert!(
                is_dmi_placeholder(placeholder),
                "{placeholder} must be a placeholder"
            );
        }
        for real in [
            "MegaCube GB10",
            "ThinkPad X1",
            "NUC13ANHi7",
            "Standard PC (Q35 + ICH9, 2009)",
        ] {
            assert!(
                !is_dmi_placeholder(real),
                "{real} must not be treated as a placeholder"
            );
        }
    }

    /// 聚合口径直打（与生产同一条路径；any_value_contains 同款拆法——此前
    /// 本测试把迭代表达式抄了一遍，生产口径改坏照样全绿，十二轮评审 P3）。
    #[test]
    fn first_nonempty_prefers_earlier_fields() {
        let values = vec![
            None,
            Some("GB10 Family".to_string()),
            Some("MegaCube".to_string()),
        ];
        assert_eq!(first_nonempty(&values).as_deref(), Some("GB10 Family"));
        assert_eq!(first_nonempty(&[]), None);
        assert_eq!(first_nonempty(&[None, None]), None);
    }

    #[test]
    fn contains_match_is_case_insensitive_over_any_field() {
        // 与生产同一条聚合路径：values 形状 = 逐字段读取结果（None = 读不到）。
        let values = vec![
            Some("LinSeer MegaCube".to_string()),
            Some("PowerEdge".to_string()),
            None,
        ];
        assert!(any_value_contains(&values, "megacube"));
        // 大小写必须被归一：丢掉 to_ascii_lowercase 的回归在这里必须变红。
        assert!(any_value_contains(
            &[Some("MEGACUBE-GB10".to_string()), None],
            "megacube"
        ));
        assert!(!any_value_contains(
            &[Some("PowerEdge".to_string()), Some("R750".to_string())],
            "megacube"
        ));
        // 全部字段读不到 → 不命中（fail-closed）。
        assert!(!any_value_contains(&[None, None], "megacube"));
        // 空集合同样不命中。
        assert!(!any_value_contains(&[], "megacube"));
    }

    #[test]
    fn field_list_covers_the_documented_candidates() {
        // 设计 §1 与 PR 正文的候选清单；单边增删字段时这条会响。
        assert_eq!(
            DMI_ID_FIELDS,
            &["product_name", "product_family", "board_name"]
        );
    }

    #[test]
    fn dmi_text_is_capped_at_64_chars() {
        // 限长纪律（第二十轮评审 P3）：该值进每拍快照与 doctor 云端上下文，
        // 固件/内核覆写的超长自由文本不得原样外发。直打生产路径的唯一出口。
        let overlong: String = "A".repeat(200);
        let capped = cap_dmi_text(&overlong);
        assert_eq!(capped.chars().count(), 64);
        assert!(capped.starts_with("AAAA"));
        // 恰好 64 与更短的原样通过。
        assert_eq!(cap_dmi_text(&"B".repeat(64)).chars().count(), 64);
        assert_eq!(cap_dmi_text("GB10"), "GB10");
    }
}
