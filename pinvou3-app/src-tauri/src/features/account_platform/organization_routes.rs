use reqwest::Method;

fn id(value: &str) -> bool {
    uuid::Uuid::parse_str(value).is_ok_and(|v| !v.is_nil() && v.to_string() == value)
}

pub(super) fn allowed(area: &str, method: &Method, path: &str, query: Option<&str>) -> bool {
    let p: Vec<_> = path.split('/').collect();
    if area == "account" {
        if query.is_some() {
            return false;
        }
        if *method == Method::GET
            && matches!(
                path,
                "/v1/me/organizations" | "/v1/me/invitations" | "/v1/me/role-bindings"
            )
        {
            return true;
        }
        if *method == Method::POST
            && matches!(
                path,
                "/v1/sessions/current/active-context" | "/v1/invitations/accept"
            )
        {
            return true;
        }
        return match p.as_slice() {
            ["", "v1", "organizations", org, collection] if id(org) => {
                *method == Method::GET
                    && matches!(
                        *collection,
                        "units" | "members" | "quota-members" | "role-bindings"
                    )
                    || *method == Method::POST
                        && matches!(
                            *collection,
                            "units" | "invitations" | "role-bindings" | "ownership"
                        )
            }
            ["", "v1", "organizations", org, collection, target] if id(org) && id(target) => {
                *method == Method::DELETE
                    && matches!(*collection, "invitations" | "role-bindings" | "members")
            }
            ["", "v1", "organizations", org, "members", user, operation]
                if id(org) && id(user) && matches!(*operation, "roles" | "units") =>
            {
                *method == Method::PUT
            }
            ["", "v1", "organizations", org, "units", unit, "parent"] if id(org) && id(unit) => {
                *method == Method::PATCH
            }
            _ => false,
        };
    }
    if area != "token" {
        return false;
    }
    match p.as_slice() {
        ["", "v1", "organizations", org, "quota-grants"] if id(org) && *method == Method::GET => {
            query.is_none_or(|q| q.strip_prefix("member_user_id=").is_some_and(id))
        }
        ["", "v1", "organizations", org, name] if id(org) && query.is_none() => {
            *method == Method::GET
                && matches!(
                    *name,
                    "balance" | "entitlement-packages" | "payment-capabilities" | "model-policy"
                )
                || *method == Method::POST && *name == "quota-grants:batch"
                || *method == Method::PUT && *name == "model-policy"
        }
        ["", "v1", "organizations", org, "recharge", "products"] if id(org) && query.is_none() => {
            *method == Method::GET
        }
        ["", "v1", "organizations", org, "recharge", "orders"] if id(org) && query.is_none() => {
            *method == Method::POST
        }
        ["", "v1", "organizations", org, "recharge", "orders", order]
            if id(org) && id(order) && query.is_none() =>
        {
            *method == Method::GET
        }
        [
            "",
            "v1",
            "organizations",
            org,
            "quota-grants",
            grant,
            "revoke",
        ] if id(org) && id(grant) && query.is_none() => *method == Method::POST,
        _ => false,
    }
}

pub(super) fn keyed(area: &str, method: &Method, path: &str) -> bool {
    allowed(area, method, path, None)
        && *method != Method::GET
        && path != "/v1/sessions/current/active-context"
        && path != "/v1/invitations/accept"
}

pub(super) fn valid_key(area: &str, key: &str) -> bool {
    let bounds = if area == "token" { 16..=128 } else { 8..=120 };
    bounds.contains(&key.len()) && key.bytes().all(|c| c.is_ascii_graphic())
}

// The WebView submits financial digits as strings. Restore only this declared
// int64 field after crossing the bridge; no JavaScript Number conversion occurs.
pub(super) fn exact_body(
    area: &str,
    path: &str,
    body: &mut Option<serde_json::Value>,
) -> Result<(), String> {
    if area != "token" || !path.ends_with("/quota-grants:batch") {
        return Ok(());
    }
    let items = body
        .as_mut()
        .and_then(|b| b.get_mut("items"))
        .and_then(|i| i.as_array_mut())
        .ok_or("account_platform_request_invalid: quota items are required")?;
    for item in items {
        let digits = item
            .get("amount_microcredits")
            .and_then(|v| v.as_str())
            .ok_or("account_platform_request_invalid: exact quota amount is required")?;
        let amount = digits
            .parse::<i64>()
            .map_err(|_| "account_platform_request_invalid: quota amount is invalid")?;
        if amount <= 0 || amount.to_string() != digits {
            return Err("account_platform_request_invalid: quota amount is invalid".into());
        }
        item["amount_microcredits"] = serde_json::Value::Number(amount.into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn actual_account_and_billing_key_bounds_stay_distinct() {
        for (area, size, want) in [
            ("account", 7, false),
            ("account", 8, true),
            ("account", 120, true),
            ("account", 121, false),
            ("token", 15, false),
            ("token", 16, true),
            ("token", 128, true),
            ("token", 129, false),
        ] {
            assert_eq!(valid_key(area, &"k".repeat(size)), want);
        }
        assert!(!valid_key("token", "unsafe whitespace key"));
    }
    #[test]
    fn current_org_routes_and_future_boundaries() {
        let org = "11111111-1111-4111-8111-111111111111";
        let other = "22222222-2222-4222-8222-222222222222";
        for (area, method, path, query, want) in [
            (
                "account",
                Method::GET,
                "/v1/me/organizations".to_string(),
                None,
                true,
            ),
            (
                "account",
                Method::POST,
                "/v1/sessions/current/active-context".to_string(),
                None,
                true,
            ),
            (
                "account",
                Method::PATCH,
                format!("/v1/organizations/{org}/units/{other}/parent"),
                None,
                true,
            ),
            (
                "account",
                Method::PUT,
                format!("/v1/organizations/{org}/members/{other}/units"),
                None,
                true,
            ),
            (
                "account",
                Method::POST,
                format!("/v1/organizations/{org}/members/{other}/units"),
                None,
                false,
            ),
            (
                "account",
                Method::GET,
                format!("/v1/organizations/{org}/members/{other}/units"),
                None,
                false,
            ),
            (
                "account",
                Method::PUT,
                format!("/v1/organizations/{org}/members/{other}/units"),
                Some("all=1".to_string()),
                false,
            ),
            (
                "account",
                Method::PUT,
                format!(
                    "/v1/organizations/{org}/members/00000000-0000-0000-0000-000000000000/units"
                ),
                None,
                false,
            ),
            (
                "token",
                Method::GET,
                format!("/v1/organizations/{org}/balance"),
                None,
                true,
            ),
            (
                "token",
                Method::POST,
                format!("/v1/organizations/{org}/quota-grants:batch"),
                None,
                true,
            ),
            (
                "token",
                Method::GET,
                format!("/v1/organizations/{org}/model-policy"),
                None,
                true,
            ),
            (
                "token",
                Method::PUT,
                format!("/v1/organizations/{org}/model-policy"),
                None,
                true,
            ),
            (
                "token",
                Method::POST,
                format!("/v1/organizations/{org}/model-policy"),
                None,
                false,
            ),
            (
                "token",
                Method::GET,
                format!("/v1/organizations/{org}/quota-grants"),
                Some(format!("member_user_id={other}")),
                true,
            ),
            (
                "token",
                Method::GET,
                format!("/v1/organizations/{org}/quota-grants"),
                Some(format!("member_user_id={other}&all=1")),
                false,
            ),
            (
                "token",
                Method::POST,
                format!("/v1/organizations/{org}/grants/{other}/account-closed-reclaim"),
                None,
                false,
            ),
            (
                "account",
                Method::DELETE,
                "/v1/me/closure-requests".to_string(),
                None,
                false,
            ),
            (
                "token",
                Method::POST,
                "/v1/organizations/00000000-0000-0000-0000-000000000000/quota-grants:batch"
                    .to_string(),
                None,
                false,
            ),
        ] {
            assert_eq!(
                allowed(area, &method, &path, query.as_deref()),
                want,
                "{path}"
            );
        }
    }
    #[test]
    fn exact_quota_body_preserves_int64_and_rejects_rounded_webview_number() {
        let mut body =
            Some(serde_json::json!({"items":[{"amount_microcredits":"9007199254740993"}]}));
        exact_body(
            "token",
            "/v1/organizations/11111111-1111-4111-8111-111111111111/quota-grants:batch",
            &mut body,
        )
        .unwrap();
        assert_eq!(
            body.unwrap()["items"][0]["amount_microcredits"].as_i64(),
            Some(9007199254740993)
        );
        for invalid in [
            serde_json::json!(9007199254740992u64),
            serde_json::json!("01"),
            serde_json::json!("9223372036854775808"),
            serde_json::json!("0"),
        ] {
            let mut b = Some(serde_json::json!({"items":[{"amount_microcredits":invalid}]}));
            assert!(
                exact_body(
                    "token",
                    "/v1/organizations/11111111-1111-4111-8111-111111111111/quota-grants:batch",
                    &mut b
                )
                .is_err()
            );
        }
    }
}
