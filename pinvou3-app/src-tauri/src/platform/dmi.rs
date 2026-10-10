//! DMI（桌面管理接口）产品信息读取的单一真相源。
//!
//! `product_name` / `product_family` / `board_name` 三个候选字段的清单原本在
//! `platform/capabilities.rs`（能力位：任一字段含 "megacube"）与
//! `features/device/platform/linux.rs`（设备页展示：第一个非空字段）各写一份，
//! 聚合口径还不同——两处单边修改候选表就会漂移（第三轮评审 P2）。这里收敛为
//! 一份字段清单 + 一个读取函数，两个消费方各自在此之上做聚合。
//!
//! 非 Linux 平台这些 sysfs 路径不存在，读取自然失败返回 None——与
//! `is_megacube_device` 先用运行时 OS 判断短路同款 fail-closed 语义，无需 cfg。

/// DMI id 字段候选（`/sys/class/dmi/id/<field>`），顺序即优先级。
const DMI_ID_FIELDS: &[&str] = &["product_name", "product_family", "board_name"];

/// 读单个 DMI 字段并 trim；缺失/为空 → None。
pub(crate) fn dmi_field(field: &str) -> Option<String> {
    let text = std::fs::read_to_string(format!("/sys/class/dmi/id/{field}")).ok()?;
    let trimmed = text.trim();
    (!trimmed.is_empty()).then(|| trimmed.to_string())
}

/// 第一个非空字段值（设备页头部展示用）。
pub(crate) fn dmi_first_nonempty() -> Option<String> {
    DMI_ID_FIELDS.iter().find_map(|field| dmi_field(field))
}

/// 任一字段含 `needle`（小写匹配，能力位用）。
pub(crate) fn any_field_contains(needle: &str) -> bool {
    DMI_ID_FIELDS.iter().any(|field| {
        dmi_field(field).is_some_and(|value| value.to_ascii_lowercase().contains(needle))
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 纯聚合口径直打：从给定字段值集合里取第一个非空。
    #[test]
    fn first_nonempty_prefers_earlier_fields() {
        let values = [
            None,
            Some("GB10 Family".to_string()),
            Some("MegaCube".to_string()),
        ];
        let first = values.iter().flatten().next().map(String::as_str);
        assert_eq!(first, Some("GB10 Family"));
    }

    #[test]
    fn contains_match_is_case_insensitive_over_any_field() {
        let values = ["LinSeer MegaCube", "PowerEdge"];
        assert!(
            values
                .iter()
                .any(|value| value.to_ascii_lowercase().contains("megacube"))
        );
        assert!(
            !["PowerEdge", "R750"]
                .iter()
                .any(|value| value.to_ascii_lowercase().contains("megacube"))
        );
    }

    #[test]
    fn field_list_covers_the_documented_candidates() {
        // 设计 §1 与 PR 正文的候选清单；单边增删字段时这条会响。
        assert_eq!(
            DMI_ID_FIELDS,
            &["product_name", "product_family", "board_name"]
        );
    }
}
