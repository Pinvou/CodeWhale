//! 内存预算与准入（T5）。
//!
//! 设计文档 §3.3：统一内存平台上 OOM 表现为整机僵死而非报错，预防式准入是
//! 平台刚需。estimate 必须保守：安全边际 ≥ 8GiB 或总量 8%，取两者较大。
//! cgroup 对 GPU 分配无效（已核实），预算只看系统级 MemAvailable。

#[derive(Debug, Clone, Default)]
pub struct MemInfo {
    pub total_kb: u64,
    pub free_kb: u64,
    pub available_kb: u64,
    pub buffers_kb: u64,
    pub cached_kb: u64,
    pub swap_total_kb: u64,
    pub swap_free_kb: u64,
}

impl MemInfo {
    pub fn total_gib(&self) -> f64 {
        self.total_kb as f64 / 1048576.0
    }
    pub fn available_gib(&self) -> f64 {
        self.available_kb as f64 / 1048576.0
    }
    pub fn used_gib(&self) -> f64 {
        (self.total_kb.saturating_sub(self.available_kb)) as f64 / 1048576.0
    }
    pub fn swap_total_gib(&self) -> f64 {
        self.swap_total_kb as f64 / 1048576.0
    }
    pub fn swap_free_gib(&self) -> f64 {
        self.swap_free_kb as f64 / 1048576.0
    }
}

pub fn read_meminfo(path: &std::path::Path) -> std::io::Result<MemInfo> {
    let text = std::fs::read_to_string(path)?;
    let mut mi = MemInfo::default();
    for line in text.lines() {
        let mut parts = line.split_whitespace();
        let Some(key) = parts.next() else { continue };
        let Some(val) = parts.next().and_then(|v| v.parse::<u64>().ok()) else {
            continue;
        };
        match key {
            "MemTotal:" => mi.total_kb = val,
            "MemFree:" => mi.free_kb = val,
            "MemAvailable:" => mi.available_kb = val,
            "Buffers:" => mi.buffers_kb = val,
            "Cached:" => mi.cached_kb = val,
            "SwapTotal:" => mi.swap_total_kb = val,
            "SwapFree:" => mi.swap_free_kb = val,
            _ => {}
        }
    }
    Ok(mi)
}

/// 安全边际：max(8 GiB, 总量 8%)（设计 §3.3）。
pub fn safety_margin_gib(total_kb: u64) -> f64 {
    (total_kb as f64 / 1048576.0 * 0.08).max(8.0)
}

/// 引擎运行时开销默认估计（GiB）。CUDA 上下文、运行时库、scratch。
/// 第一个标定数据点来自 V2 真机实测（红线 2），此后用实测值。
pub const DEFAULT_OVERHEAD_GIB: f64 = 3.5;

#[derive(Debug, Clone)]
pub struct Budget {
    pub weights_gib: f64,
    pub overhead_gib: f64,
    pub required_gib: f64,
    pub margin_gib: f64,
    pub available_gib: f64,
    pub ok: bool,
    pub shortfall_gib: f64,
    /// 预算依据：estimate（权重+默认开销）或 measured（V2 实测标定，更可信）。
    pub basis: String,
    /// 给人看的一句话结论。
    pub verdict: String,
}

pub fn evaluate(
    mem: &MemInfo,
    weights_bytes: u64,
    overhead_gib: f64,
    measured_footprint_gib: Option<f64>,
    available_override_mib: Option<u64>,
) -> Budget {
    let weights_gib = weights_bytes as f64 / 1073741824.0;
    let (required, basis, basis_note) =
        required(weights_bytes, overhead_gib, measured_footprint_gib);
    let mut budget =
        evaluate_with_required(mem, required, basis, &basis_note, available_override_mib);
    budget.weights_gib = weights_gib;
    budget.overhead_gib = overhead_gib;
    budget
}

/// System-wide before/after deltas are observations, not isolated measurements.
/// Concurrent frees must never make a large model look cheaper than its floor.
pub fn required(
    weights_bytes: u64,
    overhead_gib: f64,
    measured: Option<f64>,
) -> (f64, &'static str, String) {
    let weights_gib = weights_bytes as f64 / 1073741824.0;
    let estimate = weights_gib + overhead_gib;
    match measured {
        Some(m) if m.is_finite() && m >= estimate => (
            m,
            "measured",
            format!("实测常驻 {m:.1}（不低于权重与运行时保守下限）"),
        ),
        _ => (
            estimate,
            "estimate",
            format!("权重 {weights_gib:.1} + 运行时 {overhead_gib:.1}（估计）"),
        ),
    }
}

/// Explicit native profile reservations are floors, including on the first
/// startup. Lower/noisy measurements and smaller weight estimates cannot shrink them.
pub fn required_native(
    weights_bytes: u64,
    measured: Option<f64>,
    reserved: Option<f64>,
) -> (f64, &'static str, String) {
    let base = required(weights_bytes, DEFAULT_OVERHEAD_GIB, measured);
    match reserved {
        Some(value) if !value.is_finite() || value <= 0.0 => {
            (f64::INFINITY, "invalid", "原生档位内存预留无效".into())
        }
        Some(value) => (
            base.0.max(value),
            "static",
            format!("原生档位预留 {value:.1} GiB，且不低于{}", base.2),
        ),
        None => base,
    }
}

/// 容器卡预算（W6）：需求量由调用方算好（--mem-fraction-static × 总量 /
/// 实测标定 / 权重+开销 兜底，见 ops 的口径链），这里只做判定与中文判词。
pub fn evaluate_with_required(
    mem: &MemInfo,
    required_gib: f64,
    basis: &str,
    basis_note: &str,
    available_override_mib: Option<u64>,
) -> Budget {
    let required = required_gib;
    let margin = safety_margin_gib(mem.total_kb);
    let available = match available_override_mib {
        Some(mib) => mib as f64 / 1024.0,
        None => mem.available_gib(),
    };
    let valid = required.is_finite() && required >= 0.0;
    let ok = valid && available >= required + margin;
    let shortfall = if ok {
        0.0
    } else {
        required + margin - available
    };
    let verdict = if !valid {
        "资源需求参数不合法，拒绝启动；请核查运行时配置后重新部署".into()
    } else if ok {
        format!(
            "内存够：需要 {basis_note} + 安全边际 {margin:.1} = {:.1} GiB，当前可用 {available:.1} GiB",
            required + margin
        )
    } else {
        format!(
            "内存不够，还差 {shortfall:.1} GiB：需要 {basis_note} + 安全边际 {margin:.1} = {:.1} GiB，当前可用 {available:.1} GiB。请先释放内存（关闭大型应用）再试",
            required + margin
        )
    };
    Budget {
        weights_gib: 0.0,
        overhead_gib: 0.0,
        required_gib: required,
        margin_gib: margin,
        available_gib: available,
        ok,
        shortfall_gib: shortfall,
        basis: basis.to_string(),
        verdict,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn native_profile_reserves_memory_before_first_start_and_never_shrinks() {
        let weights = 22_892_664_832;
        for measured in [None, Some(0.0), Some(32.6), Some(f64::NAN)] {
            let (required, basis, note) = required_native(weights, measured, Some(48.0));
            assert_eq!(required, 48.0);
            assert_eq!(basis, "static");
            assert!(!evaluate_with_required(&mem(128.0, 50.0), required, basis, &note, None).ok);
            assert!(evaluate_with_required(&mem(128.0, 70.0), required, basis, &note, None).ok);
        }
        assert_eq!(required_native(weights, Some(64.0), Some(48.0)).0, 64.0);
        assert_eq!(
            required_native(weights, None, Some(1.0)).0,
            required(weights, DEFAULT_OVERHEAD_GIB, None).0
        );
        assert_eq!(
            required_native(weights, None, None),
            required(weights, DEFAULT_OVERHEAD_GIB, None)
        );
        for invalid in [0.0, -1.0, f64::NAN, f64::INFINITY] {
            let (required, basis, note) = required_native(weights, None, Some(invalid));
            assert!(!evaluate_with_required(&mem(128.0, 120.0), required, basis, &note, None).ok);
        }
    }

    fn mem(total_gib: f64, available_gib: f64) -> MemInfo {
        MemInfo {
            total_kb: (total_gib * 1048576.0) as u64,
            available_kb: (available_gib * 1048576.0) as u64,
            ..MemInfo::default()
        }
    }

    const ORNITH_BYTES: u64 = 22892664832; // 21.32 GiB

    #[test]
    fn unrelated_memory_frees_cannot_reduce_startup_admission() {
        for measured in [0.0, 0.1, -3.0, f64::NAN, f64::INFINITY] {
            let budget = evaluate(
                &mem(121.0, 12.0),
                ORNITH_BYTES,
                DEFAULT_OVERHEAD_GIB,
                Some(measured),
                None,
            );
            assert!(!budget.ok);
            assert!(budget.required_gib > 24.0);
        }
        for invalid in [f64::NAN, f64::INFINITY, -1.0] {
            assert!(
                !evaluate_with_required(&mem(121.0, 110.0), invalid, "invalid", "invalid", None).ok
            );
        }
    }

    #[test]
    fn margin_is_max_8_or_8pct() {
        assert_eq!(safety_margin_gib(64 * 1048576 * 1024 / 1024), 8.0); // 64GiB → 8
        assert!((safety_margin_gib(121 * 1048576) - 9.68).abs() < 0.01); // 121GiB → 9.68
    }

    #[test]
    fn ornith_fits_on_idle_machine() {
        // 真机勘察：121GiB 总量、108GiB 可用
        let b = evaluate(
            &mem(121.0, 108.0),
            ORNITH_BYTES,
            DEFAULT_OVERHEAD_GIB,
            None,
            None,
        );
        assert!(b.ok, "{}", b.verdict);
        assert!(b.required_gib > 24.0 && b.required_gib < 25.5);
        assert_eq!(b.basis, "estimate");
    }

    #[test]
    fn measured_footprint_overrides_estimate() {
        // V2 标定后：实测 33.0GiB 取代估计 24.8GiB 参与预算（设计 §3.3 实测优先）
        let b = evaluate(
            &mem(121.0, 40.0),
            ORNITH_BYTES,
            DEFAULT_OVERHEAD_GIB,
            Some(33.0),
            None,
        );
        assert_eq!(b.basis, "measured");
        assert!(!b.ok, "{}", b.verdict); // 33+9.7=42.7 > 40
        assert!(b.verdict.contains("实测常驻 33.0"), "{}", b.verdict);
        let b2 = evaluate(
            &mem(121.0, 40.0),
            ORNITH_BYTES,
            DEFAULT_OVERHEAD_GIB,
            None,
            None,
        );
        assert!(b2.ok); // 估计口径 24.8+9.7=34.5 < 40 —— 证明口径确实不同
    }

    #[test]
    fn ornith_refused_when_tight() {
        // 桌面常驻 20GiB + 另一个模型 25GiB 后：可用约 60GiB 时仍够；
        // 可用只剩 30GiB 时必须拒（21.3+3.5+9.7=34.5）
        let b = evaluate(
            &mem(121.0, 30.0),
            ORNITH_BYTES,
            DEFAULT_OVERHEAD_GIB,
            None,
            None,
        );
        assert!(!b.ok);
        assert!(b.shortfall_gib > 4.0 && b.shortfall_gib < 5.5);
        assert!(b.verdict.contains("还差"), "{}", b.verdict);
        assert!(b.verdict.contains("GiB"));
    }

    #[test]
    fn debug_override_wins() {
        let b = evaluate(
            &mem(121.0, 108.0),
            ORNITH_BYTES,
            DEFAULT_OVERHEAD_GIB,
            None,
            Some(1024),
        );
        assert!(!b.ok); // 伪装只有 1GiB 可用
    }

    #[test]
    fn meminfo_parse() {
        let dir = std::env::temp_dir().join(format!("cmsd-meminfo-test-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("meminfo");
        std::fs::write(
            &path,
            "MemTotal:       126812344 kB\nMemFree:         75000000 kB\nMemAvailable:   113000000 kB\nSwapTotal:        83000000 kB\nSwapFree:         68000000 kB\n",
        )
        .unwrap();
        let mi = read_meminfo(&path).unwrap();
        assert_eq!(mi.total_kb, 126812344);
        assert_eq!(mi.available_kb, 113000000);
        assert_eq!(mi.swap_free_kb, 68000000);
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn container_static_reserve_budget() {
        // W6 容器口径：--mem-fraction-static 0.60 × 121 GiB ≈ 72.6 GiB 静态预留
        let m = mem(121.0, 85.0);
        let required = 0.60 * m.total_gib();
        let b = evaluate_with_required(
            &m,
            required,
            "static",
            "静态预留 72.6（--mem-fraction-static 0.60 × 总量）",
            None,
        );
        assert!(b.ok, "{}", b.verdict);
        assert!((b.required_gib - 72.6).abs() < 0.1);
        assert_eq!(b.basis, "static");
        assert!(b.verdict.contains("静态预留"), "{}", b.verdict);
        // 可用只剩 25.5 GiB 时（10-14 事故场景）必须拒并说清缺口
        let b2 = evaluate_with_required(
            &mem(121.0, 25.5),
            required,
            "static",
            "静态预留 72.6（--mem-fraction-static 0.60 × 总量）",
            None,
        );
        assert!(!b2.ok);
        assert!(b2.verdict.contains("还差"), "{}", b2.verdict);
    }
}

/// 换胎预算（串行切换专用）：旧模型先停，其常驻占用会释放。
/// available_effective = 当前可用 + 将释放量。需求量由调用方算好（原生/容器
/// 口径链见 ops::required_gib_for），判定口径与 evaluate_with_required 一致。
pub fn evaluate_switch(
    mem: &MemInfo,
    required_gib: f64,
    basis_note: &str,
    freed_gib: Option<f64>,
    available_override_mib: Option<u64>,
) -> Budget {
    let required = required_gib;
    let margin = safety_margin_gib(mem.total_kb);
    let available_now = match available_override_mib {
        Some(mib) => mib as f64 / 1024.0,
        None => mem.available_gib(),
    };
    let freed = freed_gib
        .filter(|v| v.is_finite() && *v >= 0.0)
        .unwrap_or(0.0);
    let available = available_now + freed;
    let ok = available >= required + margin;
    let shortfall = if ok {
        0.0
    } else {
        required + margin - available
    };
    let freed_note = if freed > 0.0 {
        format!("（切换会先停旧模型释放 {freed:.1} GiB）")
    } else {
        String::new()
    };
    let verdict = if ok {
        format!(
            "内存够：需要 {basis_note} + 安全边际 {margin:.1} = {:.1} GiB，当前可用 {available_now:.1} + 释放 {freed:.1} = {available:.1} GiB",
            required + margin
        )
    } else {
        format!(
            "内存不够，还差 {shortfall:.1} GiB：需要 {basis_note} + 安全边际 {margin:.1} = {:.1} GiB，当前可用 {available_now:.1} + 释放 {freed:.1} = {available:.1} GiB{freed_note}。请先释放内存再试",
            required + margin
        )
    };
    Budget {
        weights_gib: 0.0,
        overhead_gib: 0.0,
        required_gib: required,
        margin_gib: margin,
        available_gib: available,
        ok,
        shortfall_gib: shortfall,
        basis: "switch".to_string(),
        verdict,
    }
}

#[cfg(test)]
mod switch_tests {
    use super::*;

    fn mem(total_gib: f64, available_gib: f64) -> MemInfo {
        MemInfo {
            total_kb: (total_gib * 1048576.0) as u64,
            available_kb: (available_gib * 1048576.0) as u64,
            ..MemInfo::default()
        }
    }

    #[test]
    fn switch_budget_counts_freed() {
        // 可用 20 GiB 装不下 Ornith（需 24.8+9.7=34.5），但停旧释放 33 GiB 后够
        let note = "权重 21.3 + 运行时 3.5（估计）";
        let b = evaluate_switch(&mem(121.0, 20.0), 24.8, note, Some(33.0), None);
        assert!(b.ok, "{}", b.verdict);
        assert!(b.verdict.contains("释放 33.0"));
        // 不释放就不够
        let b2 = evaluate_switch(&mem(121.0, 20.0), 24.8, note, None, None);
        assert!(!b2.ok);
        assert!(b2.verdict.contains("还差"));
    }
}
