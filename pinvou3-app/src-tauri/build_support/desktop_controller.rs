//! Immutable release eligibility, independent of the process runtime environment.

pub fn build_flag(profile: &str, requested: Option<&str>) -> Result<&'static str, &'static str> {
    match requested {
        Some("0") => Ok("0"),
        Some("1") => Ok("1"),
        None if profile == "debug" => Ok("1"),
        None => Ok("0"),
        Some(_) => Err("desktop controller build flag must be exactly 0 or 1"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn release_and_unknown_profiles_default_closed() {
        for profile in ["release", "custom", ""] {
            assert_eq!(build_flag(profile, None), Ok("0"));
        }
        assert_eq!(build_flag("debug", None), Ok("1"));
    }

    #[test]
    fn explicit_flags_are_exact_and_override_development_default() {
        for profile in ["debug", "release"] {
            assert_eq!(build_flag(profile, Some("0")), Ok("0"));
            assert_eq!(build_flag(profile, Some("1")), Ok("1"));
            for flag in ["", "true", "01", " 1", "1 "] {
                assert!(build_flag(profile, Some(flag)).is_err());
            }
        }
    }
}
