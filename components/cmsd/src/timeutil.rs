//! 时间工具：epoch 秒与 ISO 8601 UTC 互转（std-only，无 chrono）。

use std::time::{SystemTime, UNIX_EPOCH};

pub fn now_epoch() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

/// 民用日期结构（UTC）。
pub struct Civil {
    pub year: i64,
    pub month: u32,
    pub day: u32,
    pub hour: u32,
    pub minute: u32,
    pub second: u32,
}

/// epoch 秒 → UTC 民用时间（Howard Hinnant 算法，公历）。
pub fn civil_from_epoch(epoch: u64) -> Civil {
    let secs = epoch % 86400;
    let days = (epoch / 86400) as i64;
    // days since 1970-01-01 → y/m/d
    let z = days + 719468;
    let era = if z >= 0 { z } else { z - 146096 } / 146097;
    let doe = (z - era * 146097) as u64; // [0, 146096]
    let yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365; // [0, 399]
    let y = yoe as i64 + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100); // [0, 365]
    let mp = (5 * doy + 2) / 153; // [0, 11]
    let d = doy - (153 * mp + 2) / 5 + 1; // [1, 31]
    let m = if mp < 10 { mp + 3 } else { mp - 9 }; // [1, 12]
    Civil {
        year: if m <= 2 { y + 1 } else { y },
        month: m as u32,
        day: d as u32,
        hour: (secs / 3600) as u32,
        minute: ((secs % 3600) / 60) as u32,
        second: (secs % 60) as u32,
    }
}

/// epoch 秒 → "2026-10-12T08:30:00Z"。
pub fn iso8601(epoch: u64) -> String {
    let c = civil_from_epoch(epoch);
    format!(
        "{:04}-{:02}-{:02}T{:02}:{:02}:{:02}Z",
        c.year, c.month, c.day, c.hour, c.minute, c.second
    )
}

pub fn now_iso8601() -> String {
    iso8601(now_epoch())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn epoch_zero_is_1970() {
        let c = civil_from_epoch(0);
        assert_eq!(
            (c.year, c.month, c.day, c.hour, c.minute, c.second),
            (1970, 1, 1, 0, 0, 0)
        );
    }

    #[test]
    fn known_date() {
        // 2026-10-12T00:00:00Z = 1791763200
        assert_eq!(iso8601(1791763200), "2026-10-12T00:00:00Z");
        // 2000-02-29 闰日
        assert_eq!(iso8601(951782400), "2000-02-29T00:00:00Z");
        // 2038-01-19 边界附近
        assert_eq!(iso8601(2147483647), "2038-01-19T03:14:07Z");
    }

    #[test]
    fn formats_padding() {
        assert_eq!(iso8601(1), "1970-01-01T00:00:01Z");
    }
}
