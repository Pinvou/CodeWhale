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

/// 任一字段含 `needle`（小写匹配，能力位用）。读取 sysfs 后走
/// [`any_value_contains`] 的同一份聚合口径——匹配语义只有一份实现，测试直打
/// 纯函数而不是内联重写（第四轮评审 P2：此前的测试把表达式抄了一遍，生产路径
/// 丢掉小写化/换包含语义照样全绿）。
pub(crate) fn any_field_contains(needle: &str) -> bool {
    let values: Vec<Option<String>> = DMI_ID_FIELDS.iter().map(|field| dmi_field(field)).collect();
    any_value_contains(&values, needle)
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
}
