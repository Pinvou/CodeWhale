use base64::Engine;
use reqwest::Method;

fn id(value: &str) -> bool {
    uuid::Uuid::parse_str(value).is_ok_and(|v| !v.is_nil() && v.to_string() == value)
}

fn query_component(value: &str) -> Option<String> {
    let mut decoded = Vec::with_capacity(value.len());
    let mut bytes = value.bytes();
    while let Some(byte) = bytes.next() {
        decoded.push(match byte {
            b'+' => b' ',
            b'%' => {
                let high = (bytes.next()? as char).to_digit(16)?;
                let low = (bytes.next()? as char).to_digit(16)?;
                (high * 16 + low) as u8
            }
            other => other,
        });
    }
    String::from_utf8(decoded).ok()
}

fn member_page_query(query: &str) -> bool {
    let mut seen = [false; 3];
    for pair in query.split('&') {
        let Some((key, value)) = pair.split_once('=') else {
            return false;
        };
        let (Some(key), Some(value)) = (query_component(key), query_component(value)) else {
            return false;
        };
        let (index, valid) = match key.as_str() {
            "limit" => (
                0,
                !value.is_empty()
                    && value.bytes().all(|b| b.is_ascii_digit())
                    && value.parse::<u16>().is_ok_and(|v| (1..=200).contains(&v)),
            ),
            "unit_id" => (1, id(&value)),
            // The server owns cursor contents and scope checks. This bridge only
            // admits its existing bounded, unpadded URL-safe base64 wire format.
            "cursor" => (
                2,
                !value.is_empty()
                    && value.len() <= 8192
                    && base64::engine::general_purpose::URL_SAFE_NO_PAD
                        .decode(&value)
                        .is_ok(),
            ),
            _ => return false,
        };
        if !valid || seen[index] {
            return false;
        }
        seen[index] = true;
    }
    true
}

pub(super) fn allowed(area: &str, method: &Method, path: &str, query: Option<&str>) -> bool {
    let p: Vec<_> = path.split('/').collect();
    if area == "account" {
        if let ["", "v1", "organizations", org, "members" | "quota-members"] = p.as_slice() {
            if id(org) && *method == Method::GET {
                return query.is_none_or(member_page_query);
            }
        }
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
        [
            "",
            "v1",
            "organizations",
            org,
            "account-closed-grants",
            grant,
            "reclaim",
        ] if id(org) && id(grant) && query.is_none() => *method == Method::POST,
        [
            "",
            "v1",
            "organizations",
            org,
            "account-closed-grants:reclaim-batch",
        ] if id(org) && query.is_none() => *method == Method::POST,
        ["", "v1", "organizations", org, "quota-grants"] if id(org) && *method == Method::GET => {
            query.is_none_or(|q| q.strip_prefix("member_user_id=").is_some_and(id))
        }
        ["", "v1", "organizations", org, name] if id(org) && query.is_none() => {
            *method == Method::GET
                && matches!(
                    *name,
                    "balance" | "entitlement-packages" | "payment-capabilities" | "model-policy"
                )
                || *method == Method::POST && matches!(*name, "quota-grants:batch")
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
    fn member_pages_admit_only_bounded_existing_pagination_parameters() {
        let org = "11111111-1111-4111-8111-111111111111";
        let unit = "22222222-2222-4222-8222-222222222222";
        for collection in ["members", "quota-members"] {
            let path = format!("/v1/organizations/{org}/{collection}");
            // Both directories emit an unfiltered cursor; only members supports
            // a unit-filtered cursor. Match encodeMemberListCursor in Go.
            let cursor = |unit_id: Option<&str>| {
                base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(
                    serde_json::to_vec(&serde_json::json!({
                        "resource": collection, "organization_id": org, "unit_id": unit_id,
                        "name": "Member 测试", "user_id": unit
                    }))
                    .unwrap(),
                )
            };
            let mut queries = vec![
                "limit=50".to_string(),
                "limit=1".to_string(),
                "limit=200".to_string(),
                format!("limit=50&cursor={}", cursor(None)),
                format!("cursor={}&limit=2", cursor(None)),
            ];
            if collection == "members" {
                queries.push(format!(
                    "limit=50&unit_id={unit}&cursor={}",
                    cursor(Some(unit))
                ));
                queries.push(format!(
                    "cursor={}&unit_id={unit}&limit=2",
                    cursor(Some(unit))
                ));
            }
            // Wire-format boundary cases only: Go still validates cursor facts
            // and decides whether a unit filter applies to the collection.
            queries.extend([
                format!("unit_id={unit}"),
                format!("%6cimit=%35%30&unit_id=%32{}&cursor=%6530", &unit[1..]),
                format!("cursor={}", "A".repeat(8192)),
            ]);
            for query in queries {
                assert!(
                    allowed("account", &Method::GET, &path, Some(&query)),
                    "{query}"
                );
            }
            for query in [
                "".to_string(),
                "limit=".to_string(),
                "limit=0".to_string(),
                "limit=201".to_string(),
                "limit=-1".to_string(),
                "limit=1.5".to_string(),
                "limit=+50".to_string(),
                "limit=%2535%2530".to_string(),
                "limit=50&limit=1".to_string(),
                "limit=50&%6cimit=1".to_string(),
                "limit=50&unknown=1".to_string(),
                "limit=50&".to_string(),
                "limit=50;unit_id=x".to_string(),
                "limit=%".to_string(),
                "limit=%GG".to_string(),
                "%FF=50".to_string(),
                "limit=%FF".to_string(),
                "unit_id=".to_string(),
                "unit_id=00000000-0000-0000-0000-000000000000".to_string(),
                format!("unit_id={unit}&unit_id={unit}"),
                "unit_id=22222222222242228222222222222222".to_string(),
                "unit_id=ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF".to_string(),
                "cursor=".to_string(),
                "cursor=A".to_string(),
                "cursor=e30=".to_string(),
                "cursor=e30%3D".to_string(),
                "cursor=e3%2530".to_string(),
                "cursor=../path".to_string(),
                "cursor=a+b".to_string(),
                "cursor=e30&cursor=e30".to_string(),
                format!("cursor={}", "A".repeat(8193)),
            ] {
                assert!(
                    !allowed("account", &Method::GET, &path, Some(&query)),
                    "{query}"
                );
            }
            for method in [Method::POST, Method::PUT, Method::PATCH, Method::DELETE] {
                assert!(!allowed("account", &method, &path, Some("limit=50")));
            }
        }
        for path in [
            format!("/v1/organizations/{org}/units"),
            format!("/v1/organizations/{org}/role-bindings"),
            format!("/v1/organizations/{org}/members/{unit}/units"),
            "/v1/organizations/00000000-0000-0000-0000-000000000000/members".into(),
            "/v1/organizations/ABCDEFAB-CDEF-4ABC-8ABC-ABCDEFABCDEF/members".into(),
            "/v1/me/organizations".into(),
        ] {
            assert!(
                !allowed("account", &Method::GET, &path, Some("limit=50")),
                "{path}"
            );
        }
    }

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

    #[test]
    fn account_closed_grant_routes_are_exact_keyed_operations() {
        let org = "11111111-1111-4111-8111-111111111111";
        let grant = "22222222-2222-4222-8222-222222222222";
        let single = format!("/v1/organizations/{org}/account-closed-grants/{grant}/reclaim");
        let batch = format!("/v1/organizations/{org}/account-closed-grants:reclaim-batch");
        for path in [&single, &batch] {
            assert!(allowed("token", &Method::POST, path, None));
            assert!(keyed("token", &Method::POST, path));
            assert!(!allowed("token", &Method::GET, path, None));
            assert!(!allowed("token", &Method::POST, path, Some("force=1")));
            assert!(!allowed("account", &Method::POST, path, None));
            assert!(!allowed(
                "token",
                &Method::POST,
                &format!("{path}/extra"),
                None
            ));
        }
        assert!(valid_key("token", "closed-grant-original-key"));
        assert!(!valid_key("token", "short"));
        assert!(!allowed(
            "token",
            &Method::POST,
            &single.replace(grant, "00000000-0000-0000-0000-000000000000"),
            None
        ));
    }
}
