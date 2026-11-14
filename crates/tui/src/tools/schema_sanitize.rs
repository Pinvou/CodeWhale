//! Schema sanitizer for tool `input_schema` before sending to provider APIs.
//!
//! DeepSeek's `/beta/chat/completions` strict tool mode is harsh. MCP tool
//! schemas frequently arrive with Pydantic-style `anyOf:[{type:"string"},
//! {type:"null"}]` unions, bare `{type:"object"}` with no `properties`, or
//! `required` entries that don't appear in `properties`. These dirty schemas
//! cause silent 400s that users can't diagnose.
//!
//! The default sanitizer runs in-place on every schema returned by
//! `ToolRegistry::tools_for_api()` before the registry hands them off.
//! Provider-specific helpers below add stricter DeepSeek and OpenAI Responses
//! compatibility passes where their request shapes need it.
//!
//! # Moonshot / Kimi tool-schema policy (ruling of 2026-11-05)
//!
//! The wire shape sent to Moonshot function-calling endpoints is governed
//! strictly by the published MFJS specification
//! (<https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md>,
//! fetched 2026-11-05; the English original lives at `docs/mfjs-spec.md` in
//! the same repository). Three rules follow from that ruling:
//!
//! 1. The wire carries only the specification's supported keyword set. Its
//!    closing clause is the authority for everything else: "若官方规范中的
//!    某些概念、关键字或特性在本文档中未被提及，则表示当前 MFJS 尚未提供官方
//!    支持……我们不保证它们能在最终的结构化输出中产生稳定、符合预期的结果".
//! 2. Live probes are observations only and can never widen the wire. A
//!    2026-11-04 probe of `https://api.kimi.com/coding/v1` observed the
//!    endpoint accepting `pattern`, `format`, `$schema`, `prefixItems`,
//!    exclusive bounds, nested `allOf`, and a root `anyOf` union with 200 —
//!    none of that ships, because the specification does not list it. A
//!    probe may motivate tightening (it did: the route-tier whitelists this
//!    module carried for one day were removed on exactly that evidence's
//!    insufficiency), never loosening.
//! 3. When the documents and the server source disagree, the documents
//!    govern. Source-comparison note: the walle implementation
//!    (`model.go`, `SupportedKeywords`) additionally accepts `title`, `$id`,
//!    `minLength`, `maxLength`, `minimum`, `maximum`, `minItems`,
//!    `maxItems`, and `ValidateLevelDefault == "ultra"` validates with that
//!    wider set — the published specification does not list any of them, so
//!    this module takes the narrower face: those keywords are stripped
//!    upstream of validation (`strip_mfjs_advisory_keywords`), never
//!    shipped.
//!
//! One pipeline serves every Moonshot route (platform `api.moonshot.cn`/`ai`
//! and coding `api.kimi.com` / `api.kimi.ai`): all of those routes'
//! documentation points at the same specification, so a per-route whitelist
//! tier would be precision without a mandate.

use std::collections::HashSet;

use serde_json::{Map, Value};

use crate::models::Tool;

/// Sanitize a JSON Schema in-place for DeepSeek strict-tool compatibility.
///
/// Applies a sequence of normalisations chosen to be semantics-preserving:
/// - Collapse `{"anyOf":[X, {"type":"null"}]}` → `X ∪ {"nullable": true}`
/// - Inject `"properties": {}` on bare-object schemas
/// - Prune dangling `required` entries
/// - Collapse single-element `oneOf` / `allOf`
/// - Walk recursively through all subschemas
pub fn sanitize(schema: &mut Value) {
    collapse_nullable_unions(schema);
    inject_properties_on_bare_objects(schema);
    prune_dangling_required(schema);
    collapse_single_element_unions(schema);
    // Recurse into all sub-schemas
    if let Some(obj) = schema.as_object_mut() {
        for (_, v) in obj.iter_mut() {
            sanitize(v);
        }
    } else if let Some(arr) = schema.as_array_mut() {
        for v in arr.iter_mut() {
            sanitize(v);
        }
    }
}

/// Prepare a complete active tool set for DeepSeek strict function-calling.
///
/// Each tool is evaluated independently: compatible schemas are sanitized and
/// marked strict, while incompatible schemas remain unchanged and non-strict.
/// Returns `true` only when every tool in the set can use strict mode.
pub fn prepare_tools_for_strict_mode(tools: &mut [Tool]) -> bool {
    let mut all_strict = true;
    for tool in tools {
        if strict_schema_supported(&tool.input_schema) {
            sanitize_for_strict(&mut tool.input_schema);
            tool.strict = Some(true);
        } else {
            tool.strict = None;
            all_strict = false;
        }
    }
    all_strict
}

/// Sanitize a schema for DeepSeek strict function-calling.
///
/// This extends the general sanitizer with the official strict-mode object
/// rules: every object must set `additionalProperties: false`, and every
/// property must be listed in `required`.
pub fn sanitize_for_strict(schema: &mut Value) {
    sanitize(schema);
    enforce_strict_subset(schema);
}

/// Sanitize a tool `parameters` schema for xAI chat completions.
///
/// xAI validates that the parameters root is an object schema and rejects a
/// root-level `anyOf`/`oneOf` union with any non-object branch
/// ("tool parameter root must be an object type"). The built-in `apply_patch`
/// schema's `oneOf: [{required:["patch"]}, {required:["changes"]}]` trips this
/// with a 400 on the first tool-bearing request. The Responses-API pass
/// performs exactly the required normalization — merge root composition
/// properties, force `type: object`, drop root-only composition keywords —
/// so reuse it and surface the dropped constraint as a description note.
pub fn sanitize_for_xai_parameters(parameters: &mut Value) -> Option<String> {
    sanitize_for_responses(parameters)
}

/// Sanitize a schema for OpenAI Responses function tools.
///
/// The Responses API requires the top-level `parameters` schema to be an object
/// and rejects top-level `oneOf` / `anyOf` / `allOf` / `enum` / `not`. Keep the
/// schema permissive rather than changing tool semantics: merge any root
/// alternative properties we can see, then remove the root-only composition
/// keywords while preserving nested schemas.
///
/// Returns a short description note when root composition constraints with
/// meaningful `required` groups are dropped.
pub fn sanitize_for_responses(schema: &mut Value) -> Option<String> {
    let constraint_note = schema
        .as_object()
        .and_then(root_composition_constraint_note);
    let dependent_note = drop_dependent_keywords(schema);

    sanitize(schema);

    if !schema.is_object() {
        *schema = Value::Object(Map::new());
    }

    let Some(obj) = schema.as_object_mut() else {
        return combine_constraint_notes(constraint_note, dependent_note);
    };

    merge_root_composition_properties(obj);
    obj.insert("type".into(), Value::String("object".to_string()));
    obj.remove("oneOf");
    obj.remove("anyOf");
    obj.remove("allOf");
    obj.remove("enum");
    obj.remove("not");
    ensure_properties_object(obj);
    prune_dangling_required(schema);
    combine_constraint_notes(constraint_note, dependent_note)
}

fn strict_schema_supported(schema: &Value) -> bool {
    let mut normalized = schema.clone();
    sanitize(&mut normalized);
    !has_strict_incompatible_composition(&normalized, true)
}

fn has_strict_incompatible_composition(schema: &Value, is_root: bool) -> bool {
    if let Some(obj) = schema.as_object() {
        if obj.contains_key("oneOf")
            || obj.contains_key("allOf")
            || obj.contains_key("dependentSchemas")
        {
            return true;
        }
        if is_root && obj.contains_key("anyOf") {
            return true;
        }
        return obj
            .values()
            .any(|value| has_strict_incompatible_composition(value, false));
    }
    schema.as_array().is_some_and(|arr| {
        arr.iter()
            .any(|value| has_strict_incompatible_composition(value, false))
    })
}

/// Collapse `{"anyOf":[X, {"type":"null"}]}` → `X ∪ {"nullable": true}`.
///
/// Same treatment for `oneOf`. Only collapses when exactly one non-null
/// member and exactly one null-type member are present.
fn collapse_nullable_unions(schema: &mut Value) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    for key in ["anyOf", "oneOf"] {
        let members: Vec<Value> = match obj.get(key).and_then(|v| v.as_array()) {
            Some(arr) => arr.clone(),
            None => continue,
        };
        let (nulls, nons): (Vec<_>, Vec<_>) = members.into_iter().partition(is_null_type);
        if nulls.len() == 1 && nons.len() == 1 {
            // `nullable` is only meaningful when it annotates a concrete
            // schema type. Collapsing `$ref | null` or another untyped branch
            // would manufacture an annotation-only schema and lose the
            // terminating union needed by MFJS reference validation.
            let has_concrete_non_null_type = nons[0]
                .as_object()
                .and_then(|branch| branch.get("type"))
                .and_then(Value::as_str)
                .is_some_and(|schema_type| schema_type != "null");
            if !has_concrete_non_null_type {
                continue;
            }
            obj.remove(key);
            if let Value::Object(non_obj) = nons.into_iter().next().unwrap() {
                for (k, v) in non_obj {
                    if k != "type" || v != "null" {
                        obj.insert(k, v);
                    }
                }
            }
            obj.insert("nullable".into(), Value::Bool(true));
        }
    }
}

fn is_null_type(v: &Value) -> bool {
    v.as_object()
        .and_then(|o| o.get("type"))
        .and_then(|t| t.as_str())
        == Some("null")
}

/// Bare `{"type": "object"}` (no `properties`, no `additionalProperties`)
/// → inject `"properties": {}` so DeepSeek's strict validator doesn't 400.
fn inject_properties_on_bare_objects(schema: &mut Value) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    if obj.get("type").and_then(|t| t.as_str()) != Some("object") {
        return;
    }
    if obj.contains_key("properties") || obj.contains_key("additionalProperties") {
        return;
    }
    obj.insert("properties".into(), Value::Object(Map::new()));
}

/// Remove entries from `required` that aren't keys in `properties`.
fn prune_dangling_required(schema: &mut Value) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    // Collect known property names first (immutable borrow), then prune.
    let known_keys: Vec<String> = obj
        .get("properties")
        .and_then(|v| v.as_object())
        .map(|props| props.keys().cloned().collect())
        .unwrap_or_default();
    let Some(required) = obj.get_mut("required").and_then(|v| v.as_array_mut()) else {
        return;
    };
    required.retain(|entry| {
        entry
            .as_str()
            .is_some_and(|k| known_keys.iter().any(|known| known == k))
    });
    if required.is_empty() {
        obj.remove("required");
    }
}

/// Collapse `{"oneOf": [X]}` → X, same for `allOf`.
///
/// Single-element unions are semantically equivalent to the element itself;
/// DeepSeek's strict validator doesn't always flatten them.
fn collapse_single_element_unions(schema: &mut Value) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    for key in ["oneOf", "allOf", "anyOf"] {
        let single = match obj.get(key).and_then(|v| v.as_array()) {
            Some(arr) if arr.len() == 1 => arr[0].clone(),
            _ => continue,
        };
        obj.remove(key);
        if let Value::Object(inner) = single {
            for (k, v) in inner {
                if !obj.contains_key(&k) {
                    obj.insert(k, v);
                }
            }
        }
    }
}

fn enforce_strict_subset(schema: &mut Value) {
    if let Some(obj) = schema.as_object_mut() {
        strip_unsupported_strict_keywords(obj);
        if is_object_schema(obj) {
            let originally_required = required_names(obj);
            let properties = ensure_properties_object(obj);
            let mut property_names: Vec<String> = properties.keys().cloned().collect();
            property_names.sort();
            for property_name in &property_names {
                if !originally_required
                    .iter()
                    .any(|required| required == property_name)
                    && let Some(property_schema) = properties.get_mut(property_name)
                {
                    mark_nullable(property_schema);
                }
            }
            obj.insert(
                "required".into(),
                Value::Array(property_names.into_iter().map(Value::String).collect()),
            );
            obj.insert("additionalProperties".into(), Value::Bool(false));
        }

        for value in obj.values_mut() {
            enforce_strict_subset(value);
        }
    } else if let Some(arr) = schema.as_array_mut() {
        for value in arr {
            enforce_strict_subset(value);
        }
    }
}

fn strip_unsupported_strict_keywords(obj: &mut Map<String, Value>) {
    obj.remove("patternProperties");
    match obj.get("type").and_then(Value::as_str) {
        Some("string") => {
            obj.remove("minLength");
            obj.remove("maxLength");
        }
        Some("array") => {
            obj.remove("minItems");
            obj.remove("maxItems");
        }
        _ => {}
    }
}

fn is_object_schema(obj: &Map<String, Value>) -> bool {
    obj.get("type").and_then(Value::as_str) == Some("object") || obj.contains_key("properties")
}

fn ensure_properties_object(obj: &mut Map<String, Value>) -> &mut Map<String, Value> {
    let needs_replacement = !matches!(obj.get("properties"), Some(Value::Object(_)));
    if needs_replacement {
        obj.insert("properties".into(), Value::Object(Map::new()));
    }
    obj.get_mut("properties")
        .and_then(Value::as_object_mut)
        .expect("properties was just ensured as object")
}

fn required_names(obj: &Map<String, Value>) -> Vec<String> {
    obj.get("required")
        .and_then(Value::as_array)
        .map(|required| {
            required
                .iter()
                .filter_map(Value::as_str)
                .map(ToOwned::to_owned)
                .collect()
        })
        .unwrap_or_default()
}

fn mark_nullable(schema: &mut Value) {
    if let Some(obj) = schema.as_object_mut() {
        obj.insert("nullable".into(), Value::Bool(true));
    }
}

fn merge_root_composition_properties(obj: &mut Map<String, Value>) {
    let mut merged = Map::new();
    for key in ["oneOf", "anyOf", "allOf"] {
        let Some(items) = obj.get(key).and_then(Value::as_array) else {
            continue;
        };
        for item in items {
            let Some(properties) = item.get("properties").and_then(Value::as_object) else {
                continue;
            };
            for (name, schema) in properties {
                merged.entry(name.clone()).or_insert_with(|| schema.clone());
            }
        }
    }

    if merged.is_empty() {
        return;
    }

    let properties = ensure_properties_object(obj);
    for (name, schema) in merged {
        properties.entry(name).or_insert(schema);
    }
}

fn root_composition_constraint_note(obj: &Map<String, Value>) -> Option<String> {
    for (key, prefix) in [
        ("oneOf", "Exactly one"),
        ("anyOf", "At least one"),
        ("allOf", "All"),
    ] {
        let Some(items) = obj.get(key).and_then(Value::as_array) else {
            continue;
        };
        let mut groups: Vec<String> = items.iter().filter_map(required_group_label).collect();
        groups.sort();
        groups.dedup();
        // Every dropped group must be disclosed, including a single one:
        // with one distinct label the flatten still relaxed the original
        // constraint (e.g. only one of two branches required it), and
        // suppressing the note there used to hide the loosening entirely.
        if !groups.is_empty() {
            return Some(format!(
                "{prefix} of these parameter groups must be provided: {}.",
                groups.join(" | ")
            ));
        }
    }
    None
}

/// Strip `dependentSchemas` / `dependentRequired` from every node.
///
/// MFJS validates each node against a closed keyword allow-list, so a schema
/// carrying either keyword is refused outright — and that refusal reaches
/// `sanitize_moonshot_chat_tools`, which fails the whole request build, so one
/// composed schema would break *every* tool-bearing Moonshot turn rather than
/// degrade its own tool.
///
/// Status note (2026-11-04): that whole-request failure mode no longer
/// exists — the request builder degrades per tool (drop + projection
/// warning). These two conditional keywords remain outside
/// the MFJS supported keyword set (the walle MFJS specification has no
/// conditional keywords), so the strip stays the semantics-preserving way to keep such a
/// tool shippable.
fn drop_dependent_keywords(schema: &mut Value) -> Option<String> {
    strip_dependent_keywords(schema).then(|| {
        "This provider cannot express conditional requirements from the original schema. \
         Honor any such requirements documented by the tool when calling it."
            .to_string()
    })
}

fn combine_constraint_notes(first: Option<String>, second: Option<String>) -> Option<String> {
    match (first, second) {
        (Some(first), Some(second)) => Some(format!("{first} {second}")),
        (Some(note), None) | (None, Some(note)) => Some(note),
        (None, None) => None,
    }
}

fn strip_dependent_keywords(schema: &mut Value) -> bool {
    match schema {
        Value::Object(obj) => {
            let mut dropped = obj.remove("dependentRequired").is_some();
            dropped |= obj.remove("dependentSchemas").is_some();
            for value in obj.values_mut() {
                dropped |= strip_dependent_keywords(value);
            }
            dropped
        }
        Value::Array(items) => {
            let mut dropped = false;
            for item in items {
                dropped |= strip_dependent_keywords(item);
            }
            dropped
        }
        _ => false,
    }
}

fn required_group_label(item: &Value) -> Option<String> {
    let mut names: Vec<String> = item
        .get("required")?
        .as_array()?
        .iter()
        .filter_map(Value::as_str)
        .map(|name| format!("`{name}`"))
        .collect();
    if names.is_empty() {
        None
    } else {
        names.sort();
        names.dedup();
        Some(names.join(" + "))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn test_tool(name: &str, input_schema: Value) -> Tool {
        Tool {
            tool_type: None,
            name: name.to_string(),
            description: name.to_string(),
            input_schema,
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }
    }

    #[test]
    fn collapses_nullable_anyof() {
        let mut schema = json!({
            "anyOf": [
                {"type": "string"},
                {"type": "null"}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(schema["type"], "string");
        assert_eq!(schema["nullable"], true);
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn collapses_nullable_oneof() {
        let mut schema = json!({
            "oneOf": [
                {"type": "null"},
                {"type": "integer", "minimum": 0}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(schema["type"], "integer");
        assert_eq!(schema["minimum"], 0);
        assert_eq!(schema["nullable"], true);
    }

    #[test]
    fn preserves_non_null_anyof() {
        let original = json!({
            "anyOf": [
                {"type": "string"},
                {"type": "integer"}
            ]
        });
        let mut schema = original.clone();
        sanitize(&mut schema);
        // Multi-typed anyOf should collapse to single element after
        // recursive walk — but here neither is null so the collapse
        // doesn't trigger. The anyOf array itself remains.
        assert!(schema.get("anyOf").is_some());
    }

    #[test]
    fn injects_properties_on_bare_object() {
        let mut schema = json!({"type": "object"});
        sanitize(&mut schema);
        assert!(schema.get("properties").is_some());
        assert_eq!(schema["properties"], json!({}));
    }

    #[test]
    fn does_not_inject_properties_when_present() {
        let mut schema = json!({
            "type": "object",
            "properties": {"name": {"type": "string"}}
        });
        let expected = schema.clone();
        sanitize(&mut schema);
        assert_eq!(schema, expected);
    }

    #[test]
    fn prunes_dangling_required() {
        let mut schema = json!({
            "type": "object",
            "properties": {"name": {"type": "string"}},
            "required": ["name", "email"]
        });
        sanitize(&mut schema);
        let required = schema["required"].as_array().unwrap();
        assert_eq!(required.len(), 1);
        assert_eq!(required[0], "name");
    }

    #[test]
    fn removes_required_when_all_pruned() {
        let mut schema = json!({
            "type": "object",
            "properties": {},
            "required": ["ghost"]
        });
        sanitize(&mut schema);
        assert!(schema.get("required").is_none());
    }

    #[test]
    fn collapses_single_element_oneof() {
        let mut schema = json!({
            "oneOf": [{"type": "string", "minLength": 1}]
        });
        sanitize(&mut schema);
        assert!(schema.get("oneOf").is_none());
        assert_eq!(schema["type"], "string");
        assert_eq!(schema["minLength"], 1);
    }

    #[test]
    fn collapses_single_element_anyof() {
        let mut schema = json!({
            "anyOf": [{"type": "boolean"}]
        });
        sanitize(&mut schema);
        assert!(schema.get("anyOf").is_none());
        assert_eq!(schema["type"], "boolean");
    }

    #[test]
    fn recursive_walk_into_properties() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "opt_name": {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "null"}
                    ]
                }
            }
        });
        sanitize(&mut schema);
        let prop = &schema["properties"]["opt_name"];
        assert_eq!(prop["type"], "string");
        assert_eq!(prop["nullable"], true);
    }

    #[test]
    fn recursive_walk_into_items() {
        let mut schema = json!({
            "type": "array",
            "items": {
                "anyOf": [
                    {"type": "integer"},
                    {"type": "null"}
                ]
            }
        });
        sanitize(&mut schema);
        let items = &schema["items"];
        assert_eq!(items["type"], "integer");
        assert_eq!(items["nullable"], true);
    }

    #[test]
    fn nested_anyof_in_nullable_union_stays_structural() {
        // Pydantic can nest unions: Optional[Union[str, int]].
        let mut schema = json!({
            "anyOf": [
                {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "integer"}
                    ]
                },
                {"type": "null"}
            ]
        });
        sanitize(&mut schema);
        // The non-null branch has no concrete type of its own. Collapsing the
        // outer union would manufacture an annotation-only `nullable` schema,
        // so retain both levels and their explicit null termination.
        assert!(schema.get("nullable").is_none());
        assert_eq!(schema["anyOf"][1], json!({"type": "null"}));
        assert_eq!(
            schema["anyOf"][0]["anyOf"],
            json!([{"type": "string"}, {"type": "integer"}])
        );
    }

    #[test]
    fn idempotent() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "name": {"type": "string"},
                "maybe": {
                    "anyOf": [{"type": "integer"}, {"type": "null"}]
                }
            },
            "required": ["name", "missing_field"]
        });
        sanitize(&mut schema);
        let after_first = schema.clone();
        sanitize(&mut schema);
        assert_eq!(schema, after_first, "sanitize must be idempotent");
    }

    #[test]
    fn strict_sanitize_requires_all_object_properties_and_closes_extra_keys() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "name": {"type": "string"},
                "count": {"type": "integer"}
            },
            "required": ["name"],
            "additionalProperties": {"type": "string"}
        });

        sanitize_for_strict(&mut schema);

        assert_eq!(schema["additionalProperties"], false);
        assert_eq!(schema["required"], json!(["count", "name"]));
        assert_eq!(schema["properties"]["count"]["nullable"], true);
        assert!(schema["properties"]["name"].get("nullable").is_none());
    }

    #[test]
    fn strict_sanitize_preserves_optional_properties_as_nullable() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "start_line": {"type": "integer"},
                "max_lines": {"type": "integer"},
                "options": {
                    "type": "object",
                    "properties": {
                        "encoding": {"type": "string"},
                        "trim": {"type": "boolean"}
                    },
                    "required": ["encoding"]
                }
            },
            "required": ["path", "options"]
        });

        sanitize_for_strict(&mut schema);

        assert_eq!(
            schema["required"],
            json!(["max_lines", "options", "path", "start_line"])
        );
        assert!(schema["properties"]["path"].get("nullable").is_none());
        assert!(schema["properties"]["options"].get("nullable").is_none());
        assert_eq!(schema["properties"]["start_line"]["nullable"], true);
        assert_eq!(schema["properties"]["max_lines"]["nullable"], true);
        assert_eq!(
            schema["properties"]["options"]["required"],
            json!(["encoding", "trim"])
        );
        assert!(
            schema["properties"]["options"]["properties"]["encoding"]
                .get("nullable")
                .is_none()
        );
        assert_eq!(
            schema["properties"]["options"]["properties"]["trim"]["nullable"],
            true
        );
    }

    #[test]
    fn strict_sanitize_applies_object_rules_recursively() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "outer": {
                    "type": "object",
                    "properties": {
                        "inner": {"type": "string"}
                    },
                    "required": []
                }
            },
            "required": []
        });

        sanitize_for_strict(&mut schema);

        assert_eq!(schema["required"], json!(["outer"]));
        assert_eq!(schema["additionalProperties"], false);
        assert_eq!(schema["properties"]["outer"]["required"], json!(["inner"]));
        assert_eq!(schema["properties"]["outer"]["additionalProperties"], false);
    }

    #[test]
    fn strict_sanitize_removes_unsupported_string_and_array_bounds() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "name": {
                    "type": "string",
                    "minLength": 1,
                    "maxLength": 64,
                    "pattern": "^[a-z]+$"
                },
                "items": {
                    "type": "array",
                    "minItems": 1,
                    "maxItems": 5,
                    "items": {"type": "string"}
                },
                "score": {
                    "type": "integer",
                    "minimum": 1,
                    "maximum": 5
                }
            }
        });

        sanitize_for_strict(&mut schema);

        let name = &schema["properties"]["name"];
        assert!(name.get("minLength").is_none());
        assert!(name.get("maxLength").is_none());
        assert_eq!(name["pattern"], "^[a-z]+$");

        let items = &schema["properties"]["items"];
        assert!(items.get("minItems").is_none());
        assert!(items.get("maxItems").is_none());

        let score = &schema["properties"]["score"];
        assert_eq!(score["minimum"], 1);
        assert_eq!(score["maximum"], 5);
    }

    #[test]
    fn strict_mode_applies_per_tool_in_mixed_catalog() {
        let mut tools = vec![
            test_tool(
                "lookup",
                json!({
                    "type": "object",
                    "properties": {
                        "query": {"type": "string"}
                    },
                    "required": []
                }),
            ),
            test_tool(
                "either",
                json!({
                    "type": "object",
                    "properties": {
                        "a": {"type": "string"},
                        "b": {"type": "string"}
                    },
                    "anyOf": [
                        {"required": ["a"]},
                        {"required": ["b"]}
                    ]
                }),
            ),
            test_tool(
                "nested",
                json!({
                    "type": "object",
                    "properties": {
                        "value": {
                            "oneOf": [
                                {"type": "string"},
                                {"type": "integer"}
                            ]
                        }
                    }
                }),
            ),
        ];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(tools[0].input_schema["required"], json!(["query"]));
        assert_eq!(tools[0].input_schema["additionalProperties"], false);
        assert_eq!(tools[1].strict, None);
        assert!(tools[1].input_schema.get("anyOf").is_some());
        assert_eq!(tools[2].strict, None);
        assert!(
            tools[2].input_schema["properties"]["value"]
                .get("oneOf")
                .is_some()
        );
    }

    #[test]
    fn strict_mode_rejects_nested_unsupported_composition() {
        let mut tools = vec![Tool {
            tool_type: None,
            name: "nested".to_string(),
            description: "Nested oneOf".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "value": {
                        "oneOf": [
                            {"type": "string"},
                            {"type": "integer"}
                        ]
                    }
                }
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
    }

    #[test]
    fn strict_mode_leaves_dependent_schema_tools_non_strict() {
        let schema = json!({
            "type": "object",
            "properties": {
                "action": {"type": "string"},
                "message": {"type": "string"}
            },
            "dependentSchemas": {
                "action": {
                    "properties": {"message": {}},
                    "required": ["message"]
                }
            }
        });
        let mut tools = vec![test_tool("agent", schema.clone())];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(tools[0].input_schema, schema);
    }

    #[test]
    fn strict_mode_marks_compatible_tools_strict() {
        let mut tools = vec![Tool {
            tool_type: None,
            name: "lookup".to_string(),
            description: "Lookup".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string"}
                },
                "required": []
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(tools[0].input_schema["required"], json!(["query"]));
        assert_eq!(tools[0].input_schema["additionalProperties"], false);
    }

    #[test]
    fn responses_sanitize_removes_root_composition_from_apply_patch_shape() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "path": {"type": "string"},
                "patch": {"type": "string"},
                "replace": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "path": {"type": "string"},
                            "content": {"type": "string"}
                        },
                        "required": ["path", "content"]
                    }
                },
                "changes": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "path": {"type": "string"},
                            "content": {"type": "string"}
                        },
                        "required": ["path", "content"]
                    }
                }
            },
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["replace"]},
                {"required": ["changes"]}
            ]
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert!(schema.get("anyOf").is_none());
        assert!(schema.get("allOf").is_none());
        assert!(schema.get("enum").is_none());
        assert!(schema.get("not").is_none());
        assert!(schema["properties"].get("patch").is_some());
        assert!(schema["properties"].get("replace").is_some());
        assert!(schema["properties"].get("changes").is_some());
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `changes` | `patch` | `replace`."
            )
        );
    }

    #[test]
    fn responses_sanitize_merges_root_alternative_properties() {
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {
                        "path": {"type": "string"}
                    },
                    "required": ["path"]
                },
                {
                    "type": "object",
                    "properties": {
                        "url": {"type": "string"}
                    },
                    "required": ["url"]
                }
            ]
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert!(schema["properties"].get("path").is_some());
        assert!(schema["properties"].get("url").is_some());
        assert!(schema.get("required").is_none());
        assert_eq!(
            note.as_deref(),
            Some("At least one of these parameter groups must be provided: `path` | `url`.")
        );
    }

    #[test]
    fn responses_sanitize_preserves_nested_alternatives() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "value": {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "integer"}
                    ]
                }
            }
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert!(schema["properties"]["value"].get("anyOf").is_some());
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_flattens_apply_patch_root_one_of() {
        // The exact shape that produced the live 400:
        // "apply_patch: tool parameter root must be an object type (root
        // schema is an anyOf/oneOf union with a non-object branch)".
        use crate::tools::spec::ToolSpec as _;
        let mut schema = crate::tools::apply_patch::ApplyPatchTool.input_schema();
        assert!(schema.get("oneOf").is_some(), "fixture must match the tool");

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert!(schema.get("anyOf").is_none());
        assert!(schema["properties"].get("patch").is_some());
        assert!(schema["properties"].get("changes").is_some());
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `changes` | `patch` | `replace`."
            )
        );
    }

    #[test]
    fn responses_sanitize_plain_object_has_no_constraint_note() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "query": {"type": "string"}
            }
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(schema["type"], "object");
        assert_eq!(note, None);
    }

    #[test]
    fn responses_constraint_note_is_sorted_and_deduped() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"},
                "c": {"type": "string"}
            },
            "oneOf": [
                {"required": ["b", "a", "a"]},
                {"required": ["c"]},
                {"required": ["a", "b"]}
            ]
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `a` + `b` | `c`.")
        );
    }

    #[test]
    fn responses_constraint_note_covers_a_single_dropped_group() {
        // One distinct dropped group is still a dropped constraint: when
        // only one of the branches carries a required group, the flatten
        // relaxes that requirement and the note must fire — it used to be
        // suppressed below the two-group gate, hiding the loosening
        // entirely.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                },
                {"type": "object"}
            ]
        });

        let note = sanitize_for_responses(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert_eq!(
            note.as_deref(),
            Some("At least one of these parameter groups must be provided: `path`.")
        );
    }
}

/// Normalize a tool's function schema for Kimi / Moonshot API compatibility.
///
/// Kimi's API enforces stricter JSON Schema validation: when a schema uses
/// `anyOf` / `oneOf`, the `type` field must be placed inside each item rather
/// than on the parent object.  This function walks the schema root and any
/// nested objects, pushing `"type": "object"` down into `anyOf` / `oneOf`
/// items when present.
///
/// Invariant: only mutates objects that carry a top-level `type` + an
/// `anyOf` or `oneOf` array — pure schemas without conditional alternatives
/// are left untouched.
pub fn sanitize_for_kimi(schema: &mut serde_json::Value) {
    if let Some(obj) = schema.as_object_mut() {
        // Recurse first so a type injected into this object's alternatives is
        // not immediately removed again by processing that freshly-mutated item.
        for map_key in ["properties", "$defs"] {
            if let Some(children) = obj.get_mut(map_key).and_then(Value::as_object_mut) {
                for child in children.values_mut() {
                    sanitize_for_kimi(child);
                }
            }
        }
        if let Some(items) = obj.get_mut("items") {
            sanitize_for_kimi(items);
        }
        if let Some(additional) = obj.get_mut("additionalProperties")
            && additional.is_object()
        {
            sanitize_for_kimi(additional);
        }
        for union_key in ["anyOf", "oneOf"] {
            if let Some(branches) = obj.get_mut(union_key).and_then(Value::as_array_mut) {
                for branch in branches {
                    sanitize_for_kimi(branch);
                }
            }
        }

        // If this object has `type` + `anyOf`/`oneOf`, push `type` into
        // each item and remove it from the parent. Otherwise leave it alone.
        let should_push =
            obj.contains_key("type") && (obj.contains_key("anyOf") || obj.contains_key("oneOf"));
        if should_push && let Some(type_val) = obj.remove("type") {
            for key in ["anyOf", "oneOf"] {
                if let Some(items) = obj.get_mut(key).and_then(|v| v.as_array_mut()) {
                    for item in items {
                        if let Some(item_obj) = item.as_object_mut()
                            && !item_obj.contains_key("type")
                        {
                            item_obj.insert("type".to_string(), type_val.clone());
                        }
                    }
                }
            }
            // The provider-neutral sanitizer injects an empty `properties`
            // map on every bare object before this provider pass. MFJS permits
            // only annotations beside `anyOf`, so remove that semantic no-op
            // after moving the object type into each branch.
            if obj
                .get("properties")
                .and_then(Value::as_object)
                .is_some_and(Map::is_empty)
            {
                obj.remove("properties");
            }
        }
    }
}

/// A safe, provider-facing reason that Kimi parameters could not be emitted.
///
/// These diagnostics deliberately never include the schema or `$ref` value:
/// tool schemas can be supplied by MCP servers and may contain private data.
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
pub enum KimiParameterSchemaError {
    #[error("Moonshot function parameters root must be a JSON object schema")]
    RootMustBeObject,
    #[error("Moonshot function parameters contain an unsupported root reference")]
    UnsupportedRootReference,
    #[error("Moonshot function parameters contain an unresolved internal root reference")]
    UnresolvedRootReference,
    #[error("Moonshot function parameters contain a cyclic internal root reference")]
    CyclicRootReference,
    #[error("Moonshot function parameters root reference must resolve to an object schema")]
    ReferencedRootMustBeObject,
    #[error("Moonshot function parameters contain unsupported nested allOf composition")]
    UnsupportedNestedAllOf,
    #[error("Moonshot function parameters contain conflicting nested union composition")]
    ConflictingNestedUnion,
    #[error("Moonshot function parameters contain an unsupported const literal")]
    UnsupportedConstLiteral,
    #[error("Moonshot function parameters contain conflicting literal constraints")]
    ConflictingLiteralConstraint,
    #[error("Moonshot function parameters contain an invalid nullable marker")]
    InvalidNullable,
    #[error("Moonshot function parameters contain an unsupported MFJS keyword")]
    UnsupportedKeyword,
    #[error("Moonshot function parameters contain an invalid MFJS schema node")]
    InvalidSchemaNode,
    #[error("Moonshot function parameters contain an invalid MFJS keyword value")]
    InvalidKeywordValue,
    #[error("Moonshot function parameters contain an invalid MFJS reference")]
    InvalidReference,
    #[error("Moonshot function parameters contain an MFJS schema without a concrete type")]
    MissingType,
    #[error("Moonshot function parameters exceed an MFJS resource limit")]
    ResourceLimitExceeded,
    #[error("Moonshot function parameters contain a non-terminating MFJS reference")]
    NonTerminatingReference,
}

/// Normalize a complete Kimi / Moonshot `function.parameters` object.
///
/// Function parameters have an additional MFJS constraint: the root must end
/// as a plain `type: "object"` schema. Root composition is flattened using the
/// same compatibility pass as Responses and xAI, while supported nested
/// `anyOf` branches remain nested. Internal root `$ref` values are resolved
/// and inlined before normalization so we never manufacture the invalid
/// `type + allOf($ref)` shape rejected by MFJS.
///
/// One pipeline serves every Moonshot route (api.moonshot.cn / ai platform,
/// api.kimi.com and api.kimi.ai coding endpoints): the documents behind those
/// routes all point at the same MFJS specification, so the wire contract is
/// uniform. Keywords the specification does not list are either stripped
/// (annotation and unlisted bound keywords, whose removal is a
/// semantics-lossless or soft-constraint-losing simplification) or drop the
/// whole tool with a projection warning (composition and pattern keywords
/// with no simplification equivalent).
///
/// Unsupported, unresolved, cyclic, and non-object root references fail
/// closed with a non-secret diagnostic instead of being sent to Moonshot.
///
/// MFJS differences from JSON Schema:
/// <https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-walle-vs-draft-2020-12.md>
pub fn sanitize_for_kimi_parameters(
    parameters: &mut Value,
) -> Result<Option<String>, KimiParameterSchemaError> {
    // Work on a clone so a rejected schema remains byte-for-byte unchanged for
    // callers that retain the catalog and retry against another provider.
    let mut candidate = parameters.clone();
    let constraint_note = sanitize_kimi_parameters_candidate(&mut candidate)?;
    validate_mfjs_parameters(&candidate)?;
    *parameters = candidate;
    Ok(constraint_note)
}

fn sanitize_kimi_parameters_candidate(
    parameters: &mut Value,
) -> Result<Option<String>, KimiParameterSchemaError> {
    let Some(root) = parameters.as_object() else {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    };
    if root
        .get("type")
        .is_some_and(|schema_type| schema_type != "object")
    {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    }

    inline_internal_kimi_root_ref(parameters)?;

    // A function schema's root cannot carry MFJS composition because the
    // Tool Use documentation requires an object root ("The root of
    // `parameters` must be an `object`",
    // https://platform.kimi.ai/docs/api/tool-use, fetched 2026-11-05). The
    // MFJS specification lists `anyOf` as an applicator without authorizing
    // it at a parameters root, so on the conflicting documents the strict
    // face wins: every root union is flattened, and the flatten retains the
    // dropped required-group contract as a prompt note for the model. (The
    // 2026-11-04 live probe of the coding endpoint accepted a root union with
    // 200 — an observation, and per policy never a shipping basis against the
    // documents.)
    // MFJS has no conditional keywords. The shared compatibility pass drops
    // them with a bounded description note instead of sending a shape the
    // validator would reject (which would drop the whole tool).
    let constraint_note = sanitize_for_responses(parameters);

    // Restore nullable unions collapsed by the registry's provider-neutral
    // sanitizer, translate MFJS-safe scalar const values, and normalize nested
    // composition. This changes model-facing schema guidance only; each tool
    // keeps responsibility for validating its own arguments. Nested allOf
    // fails closed.
    normalize_kimi_compatibility(parameters, true)?;

    // MFJS requires `type` to live inside each anyOf branch, never alongside
    // the union keyword. The root is composition-free at this point, so this
    // only adjusts valid nested unions.
    sanitize_for_kimi(parameters);

    // Keywords the MFJS specification does not list are removed before
    // validation when removal is a semantics-lossless or
    // soft-constraint-losing simplification, so the tool still ships and the
    // wire carries only the document's supported set. Anything without such
    // an equivalent (pattern, format, $schema, prefixItems, nested allOf,
    // exclusive bounds) is left for the validator to fail closed.
    strip_mfjs_advisory_keywords(parameters);
    strip_mfjs_unsupported_enum_values(parameters);

    // JSON Schema reads an empty `items` schema as "items are unconstrained",
    // which is exactly the MFJS-legal shape with the keyword absent. MFJS
    // itself has no empty schema node and fails the whole tool with
    // MissingType, so repair the equivalent shape instead of dropping the
    // tool from the request (the Pinvou browser `evaluate_script` catalog
    // historically shipped `args: {"type": "array", "items": {}}`).
    remove_empty_mfjs_items_schemas(parameters);

    let Some(root) = parameters.as_object() else {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    };
    if root.get("type").and_then(Value::as_str) != Some("object")
        || root.contains_key("anyOf")
        || root.contains_key("oneOf")
        || root.contains_key("allOf")
        || root.contains_key("$ref")
    {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    }

    Ok(constraint_note)
}

/// Remove the advisory keywords the MFJS specification does not support from
/// every schema node of a Kimi parameters document.
///
/// The strip/fail-closed boundary this function embodies is the module's
/// degradation policy: keywords whose removal is lossless (annotations) or
/// loses only a soft constraint (numeric bounds) are stripped so the wire
/// still carries the document's supported set; form keywords with no
/// document support that materially steer generation (`pattern`, `format`,
/// `prefixItems`, …) keep the baseline whole-tool removal — better a missing
/// tool than an unguaranteed shape. Source-comparison note (not a shipping
/// basis): the walle implementation carries `pattern` and friends in its
/// `FutureKeywords` set — future, i.e. not currently validated — which is
/// consistent with, and only corroborates, keeping them off the wire.
///
/// The specification's supported keyword set is closed ("若官方规范中的某些
/// 概念、关键字或特性在本文档中未被提及，则表示当前 MFJS 尚未提供官方支持",
/// <https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md>,
/// fetched 2026-11-05), and its unsupported-features list names the
/// annotation group (`title`, `$comment`) explicitly. The bound keywords
/// (`minLength`, `maxLength`, `minimum`, `maximum`, `minItems`, `maxItems`)
/// and the document identity keyword `$id` are likewise unlisted. Stripping
/// them is either semantics-lossless (annotations) or loses only soft
/// constraints (bounds the model is free to violate without breaking the
/// call), so removal keeps the tool shippable instead of degrading it —
/// strictly more conservative on the wire than shipping an unlisted keyword.
/// Every other unlisted keyword (pattern, format, $schema, prefixItems,
/// exclusive bounds, nested allOf) has no such simplification equivalent and
/// is left for `validate_mfjs_parameters` to fail closed.
///
/// The traversal matches `remove_empty_mfjs_items_schemas`: property values,
/// `$defs` entries, `anyOf` branches, object-form `additionalProperties`, and
/// `items` are the schema-value positions. `oneOf`/`allOf` need no visit:
/// this runs after `normalize_kimi_compatibility`, which has already
/// converted or rejected every such node.
fn strip_mfjs_advisory_keywords(value: &mut Value) {
    const ADVISORY_KEYWORDS: [&str; 9] = [
        "title",
        "$comment",
        "$id",
        "minLength",
        "maxLength",
        "minimum",
        "maximum",
        "minItems",
        "maxItems",
    ];
    let Some(obj) = value.as_object_mut() else {
        return;
    };
    if let Some(properties) = obj.get_mut("properties").and_then(Value::as_object_mut) {
        for child in properties.values_mut() {
            strip_mfjs_advisory_keywords(child);
        }
    }
    if let Some(definitions) = obj.get_mut("$defs").and_then(Value::as_object_mut) {
        for child in definitions.values_mut() {
            strip_mfjs_advisory_keywords(child);
        }
    }
    if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
        for branch in branches.iter_mut() {
            strip_mfjs_advisory_keywords(branch);
        }
    }
    if let Some(additional) = obj.get_mut("additionalProperties")
        && additional.is_object()
    {
        strip_mfjs_advisory_keywords(additional);
    }
    if let Some(items) = obj.get_mut("items") {
        strip_mfjs_advisory_keywords(items);
    }
    for keyword in ADVISORY_KEYWORDS {
        obj.shift_remove(keyword);
    }
}

/// Remove every `enum` whose values the MFJS specification does not support.
///
/// "枚举值我们只支持基础类型中的 float, int 和 str，其它基础类型以及 object
/// 和 array 暂不支持，并且不支持混合类型 (List 中的每个枚举值的类型必须相同，
/// 如不可以 str + int)" (<https://github.com/MoonshotAI/walle/blob/main/docs/
/// mfjs-spec.zh.md>, fetched 2026-11-05). An enum carrying boolean, null,
/// object, or array values, or mixing value kinds, is replaced by no
/// constraint at all — a node without `enum` accepts any value of its type,
/// so removal only loses the enumeration guidance and the tool stays
/// shippable instead of being degraded wholesale. Homogeneous
/// float/int/str enums pass through untouched, and an empty or non-array
/// `enum` is left for the validator to fail closed (a malformed keyword is a
/// schema bug, not a simplification opportunity).
fn strip_mfjs_unsupported_enum_values(value: &mut Value) {
    let Some(obj) = value.as_object_mut() else {
        return;
    };
    if let Some(properties) = obj.get_mut("properties").and_then(Value::as_object_mut) {
        for child in properties.values_mut() {
            strip_mfjs_unsupported_enum_values(child);
        }
    }
    if let Some(definitions) = obj.get_mut("$defs").and_then(Value::as_object_mut) {
        for child in definitions.values_mut() {
            strip_mfjs_unsupported_enum_values(child);
        }
    }
    if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
        for branch in branches.iter_mut() {
            strip_mfjs_unsupported_enum_values(branch);
        }
    }
    if let Some(additional) = obj.get_mut("additionalProperties")
        && additional.is_object()
    {
        strip_mfjs_unsupported_enum_values(additional);
    }
    if let Some(items) = obj.get_mut("items") {
        strip_mfjs_unsupported_enum_values(items);
    }
    if let Some(values) = obj.get("enum").and_then(Value::as_array) {
        // `None` kinds (non-scalars) or differing kinds make the enum
        // unsupported; an empty array stays for the validator to fail
        // closed, per the doc comment above. The all() below is vacuously
        // true for an empty array — `kinds[0]` is never evaluated — so an
        // empty enum is kept and the validator's non-empty check rejects
        // the whole tool instead of this strip silently widening an
        // unsatisfiable constraint into "anything goes".
        let kinds: Option<Vec<MfjsLiteralKind>> = values.iter().map(mfjs_literal_kind).collect();
        let homogeneous = kinds.is_some_and(|kinds| kinds.iter().all(|kind| kind == &kinds[0]));
        if !homogeneous {
            obj.shift_remove("enum");
        }
    }
}

/// Drop every empty `items` schema node (`"items": {}`) from a Kimi
/// parameters document.
///
/// Empty-schema nodes sit exactly where schema values live: property values,
/// `items`, object-form `additionalProperties`, `anyOf` branches, and `$defs`
/// entries. `properties` itself is a name map, so its own emptiness is legal
/// and only its values are visited. `oneOf` needs no visit: this runs after
/// `normalize_kimi_compatibility` has converted every non-root `oneOf` to
/// `anyOf`, and root composition was already flattened before that.
/// Schema values under keywords MFJS rejects (`contains`, `prefixItems`,
/// `patternProperties`, `not`, `if`/`then`/`else`, `propertyNames`,
/// `unevaluatedProperties`, `unevaluatedItems`, nested `allOf`, tuple-form
/// `items`) need no visit: the keyword itself fails closed —
/// `UnsupportedKeyword` at validation for the unlisted keywords,
/// `UnsupportedNestedAllOf` for a nested `allOf` and `InvalidSchemaNode`
/// for tuple-form `items` already in `normalize_kimi_compatibility` —
/// before a stranded empty `items` node inside one could matter.
/// `dependentSchemas` is the one
/// exception in shape: `strip_dependent_keywords` removes it (with its
/// subtree) rather than failing closed, so it cannot strand one either.
///
/// Children are visited before the node's own `items` check so a nested
/// repair can empty the parent (`"items": {"items": {}}` is vacuously true
/// once the inner keyword is gone, so dropping the outer keyword is still
/// semantics-preserving). Every removal keeps the document equivalent: an
/// absent `items` keyword already means unconstrained items, so a legal
/// document passes through unchanged.
fn remove_empty_mfjs_items_schemas(value: &mut Value) {
    let Some(obj) = value.as_object_mut() else {
        return;
    };
    if let Some(properties) = obj.get_mut("properties").and_then(Value::as_object_mut) {
        for child in properties.values_mut() {
            remove_empty_mfjs_items_schemas(child);
        }
    }
    if let Some(definitions) = obj.get_mut("$defs").and_then(Value::as_object_mut) {
        for child in definitions.values_mut() {
            remove_empty_mfjs_items_schemas(child);
        }
    }
    if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
        for branch in branches.iter_mut() {
            remove_empty_mfjs_items_schemas(branch);
        }
    }
    if let Some(additional) = obj.get_mut("additionalProperties")
        && additional.is_object()
    {
        remove_empty_mfjs_items_schemas(additional);
    }
    if let Some(items) = obj.get_mut("items") {
        remove_empty_mfjs_items_schemas(items);
    }
    if obj
        .get("items")
        .is_some_and(|items| items.as_object().is_some_and(Map::is_empty))
    {
        obj.shift_remove("items");
    }
}

fn inline_internal_kimi_root_ref(parameters: &mut Value) -> Result<(), KimiParameterSchemaError> {
    let document = parameters.clone();
    let Some(document_root) = document.as_object() else {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    };
    let Some(root_ref) = document_root.get("$ref") else {
        return Ok(());
    };
    let Some(mut reference) = root_ref.as_str() else {
        return Err(KimiParameterSchemaError::UnsupportedRootReference);
    };

    let mut visited = HashSet::new();
    let resolved = loop {
        if !reference.starts_with("#/") {
            return Err(KimiParameterSchemaError::UnsupportedRootReference);
        }
        if !visited.insert(reference.to_string()) {
            return Err(KimiParameterSchemaError::CyclicRootReference);
        }
        let target = document
            .pointer(&reference[1..])
            .ok_or(KimiParameterSchemaError::UnresolvedRootReference)?;
        let target = target
            .as_object()
            .ok_or(KimiParameterSchemaError::ReferencedRootMustBeObject)?;
        if let Some(next_ref) = target.get("$ref") {
            reference = next_ref
                .as_str()
                .ok_or(KimiParameterSchemaError::UnsupportedRootReference)?;
            continue;
        }
        if target.get("type").and_then(Value::as_str) != Some("object") {
            return Err(KimiParameterSchemaError::ReferencedRootMustBeObject);
        }
        break target.clone();
    };

    let mut inlined = resolved;
    for (key, value) in document_root {
        if key != "$ref" {
            inlined.insert(key.clone(), value.clone());
        }
    }
    *parameters = Value::Object(inlined);
    Ok(())
}

fn normalize_kimi_compatibility(
    schema: &mut Value,
    is_root: bool,
) -> Result<(), KimiParameterSchemaError> {
    let Some(obj) = schema.as_object_mut() else {
        return Err(KimiParameterSchemaError::InvalidSchemaNode);
    };

    if !is_root {
        if obj.contains_key("allOf") {
            // The walle MFJS specification does not list the keyword anywhere,
            // and unlike annotation or bound keywords its removal has no
            // semantics-preserving equivalent, so the whole tool fails
            // closed with a projection warning.
            return Err(KimiParameterSchemaError::UnsupportedNestedAllOf);
        }
        if let Some(one_of) = obj.remove("oneOf") {
            if obj.contains_key("anyOf") {
                return Err(KimiParameterSchemaError::ConflictingNestedUnion);
            }
            obj.insert("anyOf".to_string(), one_of);
        }
    }

    if let Some(constant) = obj.remove("const") {
        // A string/number const translates onto the MFJS-supported enum
        // landing verbatim. A bool/null const has no supported equivalent:
        // translating it to `enum: [v]` only hands the node to the
        // unsupported-enum-value strip, which would leave the bare declared
        // type behind — silently widening the author's pin (`const: true`
        // becoming "any boolean"), and the engine never re-enforces the
        // original schema before execution (review 4539829125). Like the
        // unsupported `allOf` and the non-scalar consts, that fails closed
        // and drops the tool instead of broadening its contract.
        if !is_mfjs_scalar_const(&constant) {
            return Err(KimiParameterSchemaError::UnsupportedConstLiteral);
        }
        if !obj.contains_key("type") {
            // Type inference stays limited to the MFJS-supported enum value
            // kinds: the strip never invents a type the schema author did
            // not declare, so an untyped bool/null const ends up with no
            // type once its translated enum is stripped and fails
            // MissingType in the validator.
            if let Some(inferred) = mfjs_literal_kind(&constant) {
                let schema_type = match inferred {
                    MfjsLiteralKind::Integer => "integer",
                    MfjsLiteralKind::Number => "number",
                    MfjsLiteralKind::String => "string",
                };
                obj.insert("type".to_string(), Value::String(schema_type.to_string()));
            }
        }
        if let Some(existing) = obj.get("enum") {
            let agrees = existing
                .as_array()
                .is_some_and(|values| values.as_slice() == [constant.clone()]);
            if !agrees {
                return Err(KimiParameterSchemaError::ConflictingLiteralConstraint);
            }
        } else {
            obj.insert("enum".to_string(), Value::Array(vec![constant]));
        }
    }

    let nullable = obj.remove("nullable");
    if nullable.is_some()
        && !obj
            .get("type")
            .is_some_and(|schema_type| schema_type.as_str().is_some_and(is_mfjs_concrete_type))
    {
        return Err(KimiParameterSchemaError::InvalidNullable);
    }
    match nullable.as_ref().map(Value::as_bool) {
        None => {}
        Some(Some(false)) => {}
        Some(Some(true)) if is_root => {
            // Function parameters are required to be an object at the root;
            // null was never a valid wire instance there.
        }
        Some(Some(true)) => {
            let non_null = Value::Object(std::mem::take(obj));
            *schema = serde_json::json!({
                "anyOf": [non_null, {"type": "null"}]
            });
        }
        Some(None) => return Err(KimiParameterSchemaError::InvalidNullable),
    }

    normalize_kimi_child_schemas(schema)?;
    Ok(())
}

fn normalize_kimi_child_schemas(schema: &mut Value) -> Result<(), KimiParameterSchemaError> {
    let Some(obj) = schema.as_object_mut() else {
        return Err(KimiParameterSchemaError::InvalidSchemaNode);
    };

    for map_key in ["properties", "$defs"] {
        if let Some(children) = obj.get_mut(map_key).and_then(Value::as_object_mut) {
            for child in children.values_mut() {
                normalize_kimi_compatibility(child, false)?;
            }
        }
    }

    if let Some(items) = obj.get_mut("items") {
        normalize_kimi_compatibility(items, false)?;
    }
    if let Some(additional) = obj.get_mut("additionalProperties")
        && additional.is_object()
    {
        normalize_kimi_compatibility(additional, false)?;
    }
    if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
        for branch in branches {
            normalize_kimi_compatibility(branch, false)?;
        }
    }
    Ok(())
}

/// Whether a `const` value maps onto an MFJS-supported enum literal:
/// strings and numbers do (their const→enum landing is verbatim), while
/// bool/null consts have no supported equivalent — a translated
/// `enum: [true]` is stripped downstream and would silently widen the pin
/// to its bare declared type (review 4539829125), so they fail closed
/// like object/array consts.
fn is_mfjs_scalar_const(value: &Value) -> bool {
    value.is_string() || value.is_number()
}

/// Validate one fully normalized MFJS function-parameters schema.
///
/// Every error is a fixed enum variant: schemas can originate in MCP or
/// runtime tools and may contain private names or values, so diagnostics must
/// never echo a keyword, property, reference, or literal from the document.
///
/// The keyword whitelist is the MFJS specification's supported set, exactly:
/// `$ref` (internal, already inlined at the root), root-only `$defs`, `anyOf`,
/// `properties`, `additionalProperties`, `items`, `type`, `enum`, `required`,
/// `default`, and `description`. The specification's own source
/// (walle `SupportedKeywords`) additionally accepts `title`, `$id`, and the
/// min/max bound keywords, but its published specification does not — on the
/// conflicting documents the specification text governs, so the narrower set
/// wins and the advisory keywords are stripped upstream in the sanitizer.
pub fn validate_mfjs_parameters(parameters: &Value) -> Result<(), KimiParameterSchemaError> {
    let root = parameters
        .as_object()
        .ok_or(KimiParameterSchemaError::RootMustBeObject)?;
    if root.get("type").and_then(Value::as_str) != Some("object")
        || root.contains_key("anyOf")
        || root.contains_key("oneOf")
        || root.contains_key("allOf")
        || root.contains_key("$ref")
    {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    }
    if serde_json::to_vec(parameters)
        .map(|encoded| encoded.len() > MFJS_MAX_SCHEMA_BYTES)
        .unwrap_or(true)
    {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }

    let mut state = MfjsValidationState::new(parameters);
    state.validate_schema(parameters, true, false, 0, 0)?;

    let mut visiting_refs = HashSet::new();
    let mut prepass_work = 0usize;
    if !mfjs_schema_can_terminate(
        parameters,
        parameters,
        &mut visiting_refs,
        0,
        &mut prepass_work,
    )? {
        return Err(KimiParameterSchemaError::NonTerminatingReference);
    }

    // One shared walk state across the root and every named definition: the
    // memo is keyed by (node identity, depths), so definitions that share
    // reference subgraphs are not re-walked, and the work budget covers the
    // whole document instead of restarting per definition.
    let mut state = MfjsExpandedWalkState::default();
    validate_mfjs_expanded_depth(parameters, parameters, 0, 0, &mut state)?;
    if let Some(definitions) = root.get("$defs").and_then(Value::as_object) {
        for definition in definitions.values() {
            validate_mfjs_expanded_depth(definition, parameters, 0, 0, &mut state)?;
        }
    }
    Ok(())
}

// Resource budgets mirror the walle validator's own defaults (source
// comparison against `conf.go`, `DefaultValidatorConfig`, fetched from
// <https://github.com/MoonshotAI/walle> on 2026-11-05 — a source-comparison
// note, not a document clause: the MFJS specification sets no numeric
// limits): MaxAnyOfItems=500, MaxTotalPropertiesKeysNum=3000,
// MaxSchemaDepth=30, MaxEnumItems=1000, MaxEnumStringLength=75000.
// MaxSchemaSize=120000 below already matches. The alignment exists so a
// third-party schema the server's own validator would accept is not dropped
// by an engine-local tightening. The enum-length check threshold stays
// engine-local: applying the same MaxEnumStringLength budget from 250 values
// up is a stricter checking cadence than walle's 2500-value threshold, never
// a wider budget. Scope caveat, same spirit: walle's MaxEnumItems is
// per-node (`len(enum) > MaxEnumItems` in keyword_validators.go) while
// MFJS_MAX_TOTAL_ENUM_VALUES accumulates across the whole document, so a
// multi-node document can be dropped here at a total the server would
// accept node-by-node. MFJS_MAX_STRUCTURAL_DEPTH is engine-only — a
// recursion guard for this walker, not a walle budget.
const MFJS_MAX_ANY_OF_ITEMS: usize = 500;
const MFJS_MAX_OBJECT_DEPTH: usize = 30;
const MFJS_MAX_TOTAL_PROPERTIES: usize = 3_000;
const MFJS_MAX_TOTAL_ENUM_VALUES: usize = 1_000;
const MFJS_ENUM_LENGTH_CHECK_THRESHOLD: usize = 250;
const MFJS_MAX_ENUM_STRING_LENGTH: usize = 75_000;
const MFJS_MAX_SCHEMA_BYTES: usize = 120_000;
const MFJS_MAX_STRUCTURAL_DEPTH: usize = 64;
// Expanded-reference walk budget (review 5886233560): the memoized walk
// below evaluates each (node, property depth, structural depth) state once,
// so a shared acyclic reference DAG costs linear work instead of one walk
// per path. The cap bounds the residual worst case — a byte-budget document
// whose nodes sit at many distinct depth combinations — and fails closed
// into the same per-tool drop any other resource-limit error takes.
const MFJS_MAX_EXPANDED_STATES: usize = 100_000;
// 2^53-1: the IEEE-754 double precision bound JSON numbers interoperate
// through — an interop ceiling, not a document clause.
const MFJS_MAX_SAFE_INTEGER: f64 = 9_007_199_254_740_991.0;

struct MfjsValidationState<'a> {
    document: &'a Value,
    total_properties: usize,
    total_enum_values: usize,
}

impl<'a> MfjsValidationState<'a> {
    fn new(document: &'a Value) -> Self {
        Self {
            document,
            total_properties: 0,
            total_enum_values: 0,
        }
    }

    fn validate_schema(
        &mut self,
        schema: &Value,
        is_root: bool,
        allow_empty: bool,
        property_depth: usize,
        structural_depth: usize,
    ) -> Result<(), KimiParameterSchemaError> {
        if structural_depth > MFJS_MAX_STRUCTURAL_DEPTH {
            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
        }
        let obj = schema
            .as_object()
            .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;
        if obj.is_empty() {
            return if allow_empty {
                Ok(())
            } else {
                Err(KimiParameterSchemaError::MissingType)
            };
        }

        // The specification's supported keyword set, exactly. The walle
        // source's `SupportedKeywords` additionally contains `title`, `$id`,
        // and the min/max bound keywords, but the published specification
        // does not list them; the document governs, so they sit outside this
        // set (the sanitizer strips them upstream, so only a direct
        // unsanitized call can reach this rejection).
        const ALLOWED_KEYWORDS: &[&str] = &[
            "$ref",
            "$defs",
            "anyOf",
            "properties",
            "additionalProperties",
            "items",
            "type",
            "enum",
            "required",
            "default",
            "description",
        ];
        if obj
            .keys()
            .any(|keyword| !ALLOWED_KEYWORDS.contains(&keyword.as_str()))
        {
            return Err(KimiParameterSchemaError::UnsupportedKeyword);
        }

        if obj
            .get("description")
            .is_some_and(|value| !value.is_string())
        {
            return Err(KimiParameterSchemaError::InvalidKeywordValue);
        }

        if let Some(definitions) = obj.get("$defs") {
            if !is_root {
                return Err(KimiParameterSchemaError::InvalidKeywordValue);
            }
            let definitions = definitions
                .as_object()
                .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
            for (name, definition) in definitions {
                if name.is_empty() || name.contains('/') {
                    return Err(KimiParameterSchemaError::InvalidKeywordValue);
                }
                self.validate_schema(definition, false, false, 0, structural_depth + 1)?;
            }
        }

        if let Some(reference) = obj.get("$ref") {
            let reference = reference
                .as_str()
                .ok_or(KimiParameterSchemaError::InvalidReference)?;
            if resolve_mfjs_reference(self.document, reference).is_none() {
                return Err(KimiParameterSchemaError::InvalidReference);
            }
            let allowed_ref_sibling = |key: &str| {
                matches!(key, "$ref" | "description") || (is_root && matches!(key, "$defs"))
            };
            if obj.keys().any(|key| !allowed_ref_sibling(key)) {
                return Err(KimiParameterSchemaError::InvalidReference);
            }
            return Ok(());
        }

        if let Some(any_of) = obj.get("anyOf") {
            let branches = any_of
                .as_array()
                .filter(|branches| !branches.is_empty() && branches.len() <= MFJS_MAX_ANY_OF_ITEMS)
                .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
            let allowed_union_sibling = |key: &str| {
                matches!(key, "anyOf" | "description") || (is_root && matches!(key, "$defs"))
            };
            if obj.keys().any(|key| !allowed_union_sibling(key)) {
                return Err(KimiParameterSchemaError::InvalidKeywordValue);
            }
            for branch in branches {
                self.validate_schema(branch, false, false, property_depth, structural_depth + 1)?;
            }
            return Ok(());
        }

        let schema_type = obj
            .get("type")
            .and_then(Value::as_str)
            .filter(|schema_type| is_mfjs_concrete_type(schema_type))
            .ok_or(KimiParameterSchemaError::MissingType)?;
        if obj
            .keys()
            .any(|keyword| !mfjs_keyword_allowed_for_type(keyword, schema_type, is_root))
        {
            return Err(KimiParameterSchemaError::InvalidKeywordValue);
        }

        if let Some(values) = obj.get("enum") {
            validate_mfjs_enum(values, schema_type, self)?;
        }

        if let Some(properties) = obj.get("properties") {
            let properties = properties
                .as_object()
                .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
            self.total_properties = self
                .total_properties
                .checked_add(properties.len())
                .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
            if self.total_properties > MFJS_MAX_TOTAL_PROPERTIES {
                return Err(KimiParameterSchemaError::ResourceLimitExceeded);
            }
            for (name, property) in properties {
                if name.is_empty()
                    || matches!(
                        name.as_str(),
                        "$defs" | "$ref" | "anyOf" | "required" | "additionalProperties"
                    )
                {
                    return Err(KimiParameterSchemaError::InvalidKeywordValue);
                }
                let child_depth = property_depth
                    .checked_add(1)
                    .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
                if child_depth > MFJS_MAX_OBJECT_DEPTH {
                    return Err(KimiParameterSchemaError::ResourceLimitExceeded);
                }
                self.validate_schema(property, false, false, child_depth, structural_depth + 1)?;
            }
        }

        if let Some(required) = obj.get("required") {
            let required = required
                .as_array()
                .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
            let properties = obj
                .get("properties")
                .and_then(Value::as_object)
                .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
            let mut seen = HashSet::new();
            for name in required {
                let name = name
                    .as_str()
                    .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
                if name.is_empty() || !properties.contains_key(name) || !seen.insert(name) {
                    return Err(KimiParameterSchemaError::InvalidKeywordValue);
                }
            }
        }

        if let Some(additional) = obj.get("additionalProperties") {
            match additional {
                Value::Bool(_) => {}
                Value::Object(_) => self.validate_schema(
                    additional,
                    false,
                    true,
                    property_depth,
                    structural_depth + 1,
                )?,
                _ => return Err(KimiParameterSchemaError::InvalidKeywordValue),
            }
        }

        if let Some(items) = obj.get("items") {
            self.validate_schema(items, false, false, property_depth, structural_depth + 1)?;
        }

        Ok(())
    }
}

fn is_mfjs_concrete_type(schema_type: &str) -> bool {
    matches!(
        schema_type,
        "null" | "boolean" | "object" | "array" | "number" | "integer" | "string"
    )
}

fn mfjs_keyword_allowed_for_type(keyword: &str, schema_type: &str, is_root: bool) -> bool {
    // The specification's per-type keyword placement, exactly. `title`, the
    // min/max bound keywords, and `$id` are not listed in the specification
    // and no longer appear anywhere on the wire (the sanitizer strips the
    // advisory ones); only a direct unsanitized validation call can see them
    // here, and they fail closed like any other unlisted keyword.
    //
    // `default` is the one keyword the specification deliberately keeps
    // type-free: "default 字段为目标输出提供一个默认值……这儿不做类型检查，
    // 需要调用者自行保证其值类型内容和所在位置期望的内容相匹配" (any value,
    // any node; <https://github.com/MoonshotAI/walle/blob/main/docs/
    // mfjs-spec.zh.md>, fetched 2026-11-05). The engine therefore neither
    // restricts its placement by node type nor type-checks its value — the
    // caller owns the value's correctness, exactly as the document says.
    if is_root && keyword == "$defs" {
        return true;
    }
    if matches!(keyword, "type" | "description" | "default") {
        return true;
    }
    match schema_type {
        "object" => matches!(keyword, "properties" | "required" | "additionalProperties"),
        "array" => keyword == "items",
        "string" | "number" | "integer" => keyword == "enum",
        "boolean" | "null" => false,
        _ => false,
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum MfjsLiteralKind {
    Integer,
    Number,
    String,
}

fn mfjs_literal_kind(value: &Value) -> Option<MfjsLiteralKind> {
    if value.is_string() {
        return Some(MfjsLiteralKind::String);
    }
    let number = value.as_number()?;
    if number.is_i64() || number.is_u64() {
        Some(MfjsLiteralKind::Integer)
    } else {
        Some(MfjsLiteralKind::Number)
    }
}

fn validate_mfjs_enum(
    value: &Value,
    schema_type: &str,
    state: &mut MfjsValidationState<'_>,
) -> Result<(), KimiParameterSchemaError> {
    let values = value
        .as_array()
        .filter(|values| !values.is_empty())
        .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
    state.total_enum_values = state
        .total_enum_values
        .checked_add(values.len())
        .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
    if state.total_enum_values > MFJS_MAX_TOTAL_ENUM_VALUES {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }

    let values_match_type = match schema_type {
        "string" => values.iter().all(Value::is_string),
        "integer" => values
            .iter()
            .all(|value| mfjs_integer_value(value).is_some()),
        "number" => values.iter().all(Value::is_number),
        _ => false,
    };
    if !values_match_type {
        return Err(KimiParameterSchemaError::InvalidKeywordValue);
    }

    if values.len() > MFJS_ENUM_LENGTH_CHECK_THRESHOLD {
        let encoded_length = values.iter().try_fold(0usize, |total, value| {
            let literal_length = value
                .as_str()
                .map(str::len)
                .unwrap_or_else(|| value.to_string().len());
            total.checked_add(literal_length)
        });
        if encoded_length.is_none_or(|length| length > MFJS_MAX_ENUM_STRING_LENGTH) {
            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
        }
    }
    Ok(())
}

fn mfjs_integer_value(value: &Value) -> Option<f64> {
    let value = value.as_number()?.as_f64()?;
    (value.is_finite() && value.fract() == 0.0 && value.abs() <= MFJS_MAX_SAFE_INTEGER)
        .then_some(value)
}

fn resolve_mfjs_reference<'a>(document: &'a Value, reference: &str) -> Option<&'a Value> {
    if reference == "#" {
        return Some(document);
    }
    let name = reference.strip_prefix("#/$defs/")?;
    if name.is_empty() || name.contains('/') {
        return None;
    }
    document
        .pointer(&reference[1..])
        .filter(|target| target.is_object())
}

/// The termination prepass runs before the expanded-depth walk, and its
/// per-branch/per-property `visiting_refs` clones can multiply shared-subgraph
/// work the same way the walk did: a compact DAG of duplicate refs with a
/// required non-terminating leaf explores every path before it can answer.
/// The `work` counter bounds TOTAL visits under
/// [`MFJS_MAX_EXPANDED_STATES`] and fails closed into the same per-tool drop
/// (review thread on 5896144003) — the memoized walk downstream does not run
/// when this prepass answers.
fn mfjs_schema_can_terminate(
    schema: &Value,
    document: &Value,
    visiting_refs: &mut HashSet<String>,
    structural_depth: usize,
    work: &mut usize,
) -> Result<bool, KimiParameterSchemaError> {
    *work += 1;
    if *work > MFJS_MAX_EXPANDED_STATES {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    if structural_depth > MFJS_MAX_STRUCTURAL_DEPTH {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    let obj = schema
        .as_object()
        .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;

    if let Some(reference) = obj.get("$ref").and_then(Value::as_str) {
        if !visiting_refs.insert(reference.to_string()) {
            return Ok(false);
        }
        let target = resolve_mfjs_reference(document, reference)
            .ok_or(KimiParameterSchemaError::InvalidReference)?;
        let terminates = mfjs_schema_can_terminate(
            target,
            document,
            visiting_refs,
            structural_depth + 1,
            work,
        )?;
        visiting_refs.remove(reference);
        return Ok(terminates);
    }

    if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
        for branch in branches {
            let mut branch_refs = visiting_refs.clone();
            if mfjs_schema_can_terminate(
                branch,
                document,
                &mut branch_refs,
                structural_depth + 1,
                work,
            )?
            {
                return Ok(true);
            }
        }
        return Ok(false);
    }

    match obj.get("type").and_then(Value::as_str) {
        Some("object") => {
            let Some(required) = obj.get("required").and_then(Value::as_array) else {
                return Ok(true);
            };
            if required.is_empty() {
                return Ok(true);
            }
            let properties = obj
                .get("properties")
                .and_then(Value::as_object)
                .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
            for name in required {
                let property = name
                    .as_str()
                    .and_then(|name| properties.get(name))
                    .ok_or(KimiParameterSchemaError::InvalidKeywordValue)?;
                let mut property_refs = visiting_refs.clone();
                if !mfjs_schema_can_terminate(
                    property,
                    document,
                    &mut property_refs,
                    structural_depth + 1,
                    work,
                )? {
                    return Ok(false);
                }
            }
            Ok(true)
        }
        Some("array") => {
            // `minItems` is outside the specification's supported set, so the
            // sanitizer has already stripped it and an absent keyword reads
            // as 0 — an empty array always satisfies, so the node terminates.
            // A direct unsanitized validation call can still surface a
            // carried `minItems` here; reading it as the lower bound stays
            // the conservative reading.
            let min_items = obj.get("minItems").and_then(Value::as_u64).unwrap_or(0);
            if min_items == 0 {
                return Ok(true);
            }
            // Ported from codewhale-hq e3e563204 (the #7015 review
            // follow-up): MFJS allows `items` to be absent, and an
            // unconstrained array can always be satisfied, so absent `items`
            // counts as terminating instead of failing with
            // InvalidSchemaNode.
            let Some(items) = obj.get("items") else {
                return Ok(true);
            };
            mfjs_schema_can_terminate(items, document, visiting_refs, structural_depth + 1, work)
        }
        Some(schema_type) if is_mfjs_concrete_type(schema_type) => Ok(true),
        _ => Err(KimiParameterSchemaError::MissingType),
    }
}

/// Shared state for one [`validate_mfjs_expanded_depth`] traversal.
///
/// `completed` memoizes fully-walked (node identity, property depth,
/// structural depth) states: the walk's verdict for a state depends only on
/// the state, and an error propagates before anything is memoized, so a
/// shared acyclic reference subgraph reached again at the same depths is
/// skipped instead of re-walked — a compact DAG of shared `$ref`s would
/// otherwise recompute every path and turn request preparation into
/// exponential synchronous work on a bounded-size document (review
/// 5886233560). `visiting_refs` stays the per-path cycle guard; its early
/// return is path-contextual, so it is never memoized. `work` counts
/// computed states against [`MFJS_MAX_EXPANDED_STATES`].
#[derive(Default)]
struct MfjsExpandedWalkState {
    visiting_refs: HashSet<String>,
    completed: HashSet<(usize, usize, usize)>,
    work: usize,
}

fn validate_mfjs_expanded_depth(
    schema: &Value,
    document: &Value,
    property_depth: usize,
    structural_depth: usize,
    state: &mut MfjsExpandedWalkState,
) -> Result<(), KimiParameterSchemaError> {
    if property_depth > MFJS_MAX_OBJECT_DEPTH || structural_depth > MFJS_MAX_STRUCTURAL_DEPTH {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    let obj = schema
        .as_object()
        .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;

    if let Some(reference) = obj.get("$ref").and_then(Value::as_str) {
        if !state.visiting_refs.insert(reference.to_string()) {
            return Ok(());
        }
        let target = resolve_mfjs_reference(document, reference)
            .ok_or(KimiParameterSchemaError::InvalidReference)?;
        let result = validate_mfjs_expanded_depth(
            target,
            document,
            property_depth,
            structural_depth + 1,
            state,
        );
        state.visiting_refs.remove(reference);
        return result;
    }

    // The document is never mutated during validation, so the node address
    // is a stable identity for the memo.
    let state_key = (
        schema as *const Value as usize,
        property_depth,
        structural_depth,
    );
    if !state.completed.insert(state_key) {
        return Ok(());
    }
    state.work += 1;
    if state.work > MFJS_MAX_EXPANDED_STATES {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }

    if let Some(properties) = obj.get("properties").and_then(Value::as_object) {
        for property in properties.values() {
            validate_mfjs_expanded_depth(
                property,
                document,
                property_depth + 1,
                structural_depth + 1,
                state,
            )?;
        }
    }
    if let Some(items) = obj.get("items") {
        validate_mfjs_expanded_depth(
            items,
            document,
            property_depth,
            structural_depth + 1,
            state,
        )?;
    }
    if let Some(additional) = obj
        .get("additionalProperties")
        .filter(|additional| additional.is_object())
    {
        validate_mfjs_expanded_depth(
            additional,
            document,
            property_depth,
            structural_depth + 1,
            state,
        )?;
    }
    if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
        for branch in branches {
            validate_mfjs_expanded_depth(
                branch,
                document,
                property_depth,
                structural_depth + 1,
                state,
            )?;
        }
    }
    Ok(())
}

#[cfg(test)]
mod kimi_tests {
    use super::*;
    use crate::tools::apply_patch::ApplyPatchTool;
    use crate::tools::spec::ToolSpec;
    use serde_json::json;

    #[test]
    fn kimi_sanitize_pushes_type_into_anyof_items() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "handle": {
                    "type": "object",
                    "anyOf": [
                        {"type": "string"},
                        {"type": "null"}
                    ]
                }
            }
        });
        sanitize_for_kimi(&mut schema);
        let handle = &schema["properties"]["handle"];
        assert!(
            !handle.as_object().unwrap().contains_key("type"),
            "root type should be removed"
        );
        let any_of = handle["anyOf"].as_array().unwrap();
        assert_eq!(any_of[0]["type"], "string");
        assert_eq!(any_of[1]["type"], "null");
    }

    #[test]
    fn kimi_sanitize_injects_missing_anyof_item_types() {
        let mut schema = json!({
            "type": "object",
            "anyOf": [
                {"properties": {"path": {"type": "string"}}},
                {"required": ["url"], "properties": {"url": {"type": "string"}}}
            ]
        });

        sanitize_for_kimi(&mut schema);

        assert!(
            !schema.as_object().unwrap().contains_key("type"),
            "parent type should be removed"
        );
        let any_of = schema["anyOf"].as_array().unwrap();
        assert_eq!(any_of[0]["type"], "object");
        assert_eq!(any_of[1]["type"], "object");
    }

    #[test]
    fn mfjs_expanded_walk_memoizes_shared_reference_subgraphs() {
        // A compact acyclic DAG of shared `$ref`s (review 5886233560): every
        // Di branches to D(i+1) twice, so the expanded-depth walk crosses
        // this document along 2^22 paths. The memo evaluates each (node,
        // depth) state once — linear work on a ~2 KB document — instead of
        // recomputing every shared subgraph per path inside synchronous
        // request preparation. Removing the memo makes this test order of
        // magnitude slower while still passing; the state-count budget
        // bounds the residual worst case.
        let mut definitions = serde_json::Map::new();
        definitions.insert(
            "D22".to_string(),
            json!({"type": "object", "properties": {"end": {"type": "string"}}}),
        );
        for level in (0..22).rev() {
            definitions.insert(
                format!("D{level}"),
                json!({
                    "type": "object",
                    "properties": {
                        "p": {"$ref": format!("#/$defs/D{}", level + 1)},
                        "q": {"$ref": format!("#/$defs/D{}", level + 1)}
                    }
                }),
            );
        }
        let schema = json!({
            "type": "object",
            "properties": {"a": {"$ref": "#/$defs/D0"}},
            "$defs": Value::Object(definitions)
        });

        validate_mfjs_parameters(&schema).expect("a shared-reference DAG must validate");
    }

    #[test]
    fn mfjs_termination_prepass_is_bounded_on_shared_reference_dags() {
        // The termination prepass clones its ref set per required property
        // and memoizes nothing, so a compact shared DAG costs 2^depth walks
        // even when every leaf terminates — synchronous request-preparation
        // work the downstream memoized walk never reaches (review thread on
        // 5896144003). The prepass now shares the total-work budget and
        // fails closed into the same per-tool drop.
        let mut definitions = serde_json::Map::new();
        definitions.insert(
            "D21".to_string(),
            json!({"type": "object", "properties": {"end": {"type": "string"}}}),
        );
        for level in (0..21).rev() {
            definitions.insert(
                format!("D{level}"),
                json!({
                    "type": "object",
                    "required": ["p", "q"],
                    "properties": {
                        "p": {"$ref": format!("#/$defs/D{}", level + 1)},
                        "q": {"$ref": format!("#/$defs/D{}", level + 1)}
                    }
                }),
            );
        }
        let schema = json!({
            "type": "object",
            "required": ["a"],
            "properties": {"a": {"$ref": "#/$defs/D0"}},
            "$defs": Value::Object(definitions)
        });

        assert!(
            matches!(
                validate_mfjs_parameters(&schema),
                Err(KimiParameterSchemaError::ResourceLimitExceeded)
            ),
            "the termination prepass must trip the shared work budget on a \
             shared-reference DAG: {schema}"
        );
    }

    #[test]
    fn mfjs_expanded_walk_keeps_the_property_depth_budget_through_shared_refs() {
        // The memo is keyed by depth too (review 5886233560): the same
        // shared definition must be walked at every distinct property depth
        // it is reached at, so the depth budget still fires through a ref.
        // The deep wrapper chain reaches the shared five-level definition at
        // property depth 28; its innermost leaf lands past
        // MFJS_MAX_OBJECT_DEPTH and must be rejected — a node-only memo
        // would inherit the shallow walk's verdict and wave it through.
        let deep_definition = json!({
            "type": "object",
            "properties": {
                "l1": {"type": "object", "properties": {
                    "l2": {"type": "object", "properties": {
                        "l3": {"type": "object", "properties": {
                            "l4": {"type": "string"}
                        }}
                    }}
                }}
            }
        });
        let mut deep_property = json!({"$ref": "#/$defs/leaf5"});
        for _ in 0..27 {
            deep_property = json!({"type": "object", "properties": {"w": deep_property}});
        }
        let schema = json!({
            "type": "object",
            "properties": {
                "shallow": {"$ref": "#/$defs/leaf5"},
                "deep": deep_property
            },
            "$defs": {"leaf5": deep_definition}
        });

        assert!(
            matches!(
                validate_mfjs_parameters(&schema),
                Err(KimiParameterSchemaError::ResourceLimitExceeded)
            ),
            "the depth budget must fire through a shared ref: {schema}"
        );
    }

    #[test]
    fn kimi_sanitize_preserves_type_injected_into_nested_anyof_item() {
        let mut schema = json!({
            "type": "object",
            "anyOf": [
                {
                    "anyOf": [
                        {"properties": {"path": {"type": "string"}}}
                    ]
                }
            ]
        });

        sanitize_for_kimi(&mut schema);

        let outer_item = &schema["anyOf"][0];
        assert_eq!(outer_item["type"], "object");
        assert!(
            !schema.as_object().unwrap().contains_key("type"),
            "outer parent type should be removed"
        );
    }

    #[test]
    fn kimi_sanitize_leaves_pure_object_untouched() {
        let original = json!({
            "type": "object",
            "properties": {"x": {"type": "string"}},
            "required": ["x"]
        });
        let mut schema = original.clone();
        sanitize_for_kimi(&mut schema);
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_parameters_add_type_to_empty_root() {
        let mut schema = json!({});
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema, json!({"type": "object", "properties": {}}));
    }

    #[test]
    fn kimi_parameters_add_type_to_properties_root_without_corrupting_properties_map() {
        let mut schema = json!({
            "properties": {
                "path": {"type": "string"}
            },
            "required": ["path"]
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["type"], "object");
        assert_eq!(schema["properties"]["path"]["type"], "string");
        assert!(schema["properties"].get("type").is_none());
    }

    // Function parameters must end as a plain object root. Composition stays
    // available only in valid nested anyOf positions: the Moonshot Tool Use
    // documentation requires "The root of `parameters` must be an `object`"
    // (https://platform.kimi.ai/docs/api/tool-use, fetched 2026-11-05), and
    // the MFJS specification lists `anyOf` as an applicator without
    // authorizing it at a parameters root — on the conflicting documents the
    // strict face wins, so every root union is flattened. The tests below pin
    // those protective shapes.

    #[test]
    fn kimi_parameters_add_type_to_anyof_root() {
        let mut schema = json!({
            "anyOf": [
                {"type": "object", "properties": {"path": {"type": "string"}}},
                {"type": "null"}
            ]
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert_eq!(schema["properties"]["path"]["type"], "string");
    }

    #[test]
    fn kimi_parameters_add_type_to_allof_root() {
        let mut schema = json!({
            "allOf": [
                {"type": "object", "properties": {"name": {"type": "string"}}}
            ]
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["type"], "object");
        assert!(schema.get("allOf").is_none());
        assert_eq!(schema["properties"]["name"]["type"], "string");
    }

    #[test]
    fn kimi_parameters_add_type_to_oneof_root() {
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"id": {"type": "integer"}},
                    "required": ["id"]
                },
                {
                    "type": "object",
                    "properties": {"name": {"type": "string"}},
                    "required": ["name"]
                }
            ]
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert_eq!(schema["properties"]["id"]["type"], "integer");
        assert_eq!(schema["properties"]["name"]["type"], "string");
        validate_mfjs_parameters(&schema).unwrap();
    }

    #[test]
    fn kimi_parameters_still_flattens_root_union_with_non_object_branch() {
        // A branch without an explicit object type keeps the protective
        // flatten: the Tool Use documentation requires an object root, and a
        // union with e.g. a null branch has no documented wire shape.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                },
                {"type": "null"}
            ]
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert_eq!(schema["properties"]["path"]["type"], "string");
    }

    #[test]
    fn kimi_parameters_still_flattens_root_union_with_extra_root_keywords() {
        // Keywords beside the union that would be lost by a union passthrough
        // (here: root `properties`) keep the flatten — a parameters root must
        // be an object schema, so every root union merges into one.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                },
                {
                    "type": "object",
                    "properties": {"url": {"type": "string"}},
                    "required": ["url"]
                }
            ],
            "properties": {"legacy": {"type": "string"}}
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn kimi_parameters_flattens_actual_apply_patch_root_and_returns_constraint_note() {
        let mut schema = ApplyPatchTool.input_schema();

        let note = sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert!(schema.get("anyOf").is_none());
        assert!(schema.get("allOf").is_none());
        assert_eq!(schema["properties"]["patch"]["type"], "string");
        assert_eq!(schema["properties"]["replace"]["type"], "array");
        assert_eq!(schema["properties"]["changes"]["type"], "array");
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `changes` | `patch` | `replace`."
            )
        );
    }

    #[test]
    fn kimi_parameters_preserves_nested_anyof_branches() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "selector": {
                    "type": "object",
                    "anyOf": [
                        {"properties": {"path": {"type": "string"}}},
                        {"properties": {"id": {"type": "integer"}}}
                    ]
                }
            }
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["type"], "object");
        let selector = &schema["properties"]["selector"];
        assert!(selector.get("type").is_none());
        let branches = selector["anyOf"].as_array().unwrap();
        assert_eq!(branches.len(), 2);
        assert!(branches.iter().all(|branch| branch["type"] == "object"));
    }

    #[test]
    fn kimi_parameters_converts_nested_oneof_to_supported_anyof() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "selector": {
                    "type": "object",
                    "oneOf": [
                        {"properties": {"path": {"type": "string"}}},
                        {"properties": {"id": {"type": "integer"}}}
                    ]
                }
            }
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        let selector = &schema["properties"]["selector"];
        assert!(selector.get("oneOf").is_none());
        assert!(selector["anyOf"].is_array());
        assert!(selector.get("type").is_none());
    }

    #[test]
    fn kimi_parameters_restores_registry_collapsed_nullable_anyof() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "query": {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "null"}
                    ]
                }
            }
        });

        // Exercise the exact two-stage production path: ToolRegistry applies
        // the provider-neutral pass before the Moonshot request adapter sees
        // the schema.
        sanitize(&mut schema);
        assert_eq!(schema["properties"]["query"]["nullable"], true);
        assert!(schema["properties"]["query"].get("anyOf").is_none());

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        let query = &schema["properties"]["query"];
        assert!(query.get("nullable").is_none(), "{query}");
        assert_eq!(
            query["anyOf"],
            json!([{"type": "string"}, {"type": "null"}])
        );
        validate_mfjs_parameters(&schema).unwrap();
    }

    #[test]
    fn kimi_parameters_recursively_translates_safe_const_to_enum() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "envelope": {
                    "type": "object",
                    "properties": {
                        "items": {
                            "type": "array",
                            "items": {
                                "type": "object",
                                "properties": {
                                    "kind": {"type": "string", "const": "var_handle"}
                                },
                                "required": ["kind"]
                            }
                        }
                    }
                }
            }
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        let kind = schema
            .pointer("/properties/envelope/properties/items/items/properties/kind")
            .expect("nested kind schema");
        assert!(kind.get("const").is_none(), "{kind}");
        assert_eq!(kind["enum"], json!(["var_handle"]));
    }

    #[test]
    fn kimi_parameters_rejects_unsafe_const_without_mutating_or_leaking() {
        // Non-scalar consts have no scalar-enum translation, so they keep
        // failing closed exactly as before.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "private-shape-8172": {"type": "object", "const": {"nested": true}}
            }
        });
        let original = schema.clone();

        let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();

        assert_eq!(error, KimiParameterSchemaError::UnsupportedConstLiteral);
        assert!(!error.to_string().contains("private-shape-8172"));
        assert_eq!(schema, original, "a rejected schema must remain reusable");
    }

    #[test]
    fn kimi_parameters_fails_closed_on_bool_and_null_const() {
        // MFJS supports enum literals of string/number only, and a bool/null
        // const has no supported equivalent. Translating it to `enum: [v]`
        // would hand the node to the unsupported-enum-value strip and ship
        // the bare declared type — silently widening the author's pin
        // (`const: true` becoming "any boolean" the engine never
        // re-validates against, review 4539829125). Fail closed instead:
        // the tool drops with UnsupportedConstLiteral, exactly like a
        // non-scalar const.
        let mut typed = json!({
            "type": "object",
            "properties": {
                "toggle": {"type": "boolean", "const": true},
                "absent": {"type": "null", "const": null}
            }
        });
        let error = sanitize_for_kimi_parameters(&mut typed).unwrap_err();
        assert!(
            matches!(error, KimiParameterSchemaError::UnsupportedConstLiteral),
            "a typed bool/null const must fail closed instead of widening: {error:?}"
        );

        let mut untyped = json!({
            "type": "object",
            "properties": {"flag": {"const": false}}
        });
        let error = sanitize_for_kimi_parameters(&mut untyped).unwrap_err();
        assert!(
            matches!(error, KimiParameterSchemaError::UnsupportedConstLiteral),
            "an untyped bool const must fail closed too: {error:?}"
        );

        // Scalar string/number consts keep their documented enum landing.
        let mut scalars = json!({
            "type": "object",
            "properties": {
                "kind": {"type": "string", "const": "var_handle"},
                "level": {"type": "integer", "const": 2}
            }
        });
        sanitize_for_kimi_parameters(&mut scalars).unwrap();
        assert_eq!(scalars["properties"]["kind"]["enum"], json!(["var_handle"]));
        assert_eq!(scalars["properties"]["level"]["enum"], json!([2]));
    }

    #[test]
    fn kimi_parameters_validator_fails_closed_without_echoing_schema_values() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "private-field-4921": {
                    "type": "string",
                    "pattern": "private-pattern-value-7395"
                }
            }
        });
        let original = schema.clone();

        let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();
        let diagnostic = error.to_string();

        assert_eq!(error, KimiParameterSchemaError::UnsupportedKeyword);
        assert!(!diagnostic.contains("private-field-4921"));
        assert!(!diagnostic.contains("private-pattern-value-7395"));
        assert_eq!(schema, original, "failed validation must be transactional");
    }

    #[test]
    fn kimi_parameters_rejects_untyped_schema_and_nullable_transactionally() {
        for (mut schema, expected, sentinels) in [
            (
                json!({
                    "type": "object",
                    "properties": {
                        "private-missing-type-1207": {
                            "description": "private-description-1208"
                        }
                    }
                }),
                KimiParameterSchemaError::MissingType,
                ["private-missing-type-1207", "private-description-1208"],
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "private-nullable-1209": {
                            "nullable": true,
                            "description": "private-nullable-description-1210"
                        }
                    }
                }),
                KimiParameterSchemaError::InvalidNullable,
                ["private-nullable-1209", "private-nullable-description-1210"],
            ),
        ] {
            let original = schema.clone();
            let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();
            assert_eq!(error, expected);
            for sentinel in sentinels {
                assert!(!error.to_string().contains(sentinel));
            }
            assert_eq!(schema, original, "rejection must be transactional");
        }
    }

    #[test]
    fn kimi_parameters_infers_safe_types_for_untyped_const() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "kind": {"const": "var_handle"},
                "count": {"const": 7},
                "ratio": {"const": 1.25}
            }
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["properties"]["kind"]["type"], "string");
        assert_eq!(schema["properties"]["kind"]["enum"], json!(["var_handle"]));
        assert_eq!(schema["properties"]["count"]["type"], "integer");
        assert_eq!(schema["properties"]["count"]["enum"], json!([7]));
        assert_eq!(schema["properties"]["ratio"]["type"], "number");
        assert_eq!(schema["properties"]["ratio"]["enum"], json!([1.25]));
    }

    #[test]
    fn kimi_parameters_allows_only_the_documented_empty_schema_exception() {
        let mut schema = json!({
            "type": "object",
            "additionalProperties": {}
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["additionalProperties"], json!({}));

        let mut invalid = json!({
            "type": "object",
            "properties": {"value": {}}
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut invalid).unwrap_err(),
            KimiParameterSchemaError::MissingType
        );
    }

    #[test]
    fn kimi_parameters_repairs_empty_items_as_unconstrained() {
        // The Pinvou browser `evaluate_script` catalog shipped
        // `args: {"type": "array", "items": {}}`. JSON Schema reads the empty
        // items schema as unconstrained, but MFJS fails it with MissingType
        // and the request builder silently dropped the whole tool — the model
        // kept seeing a tool_search reference it could never call. The
        // MFJS-legal equivalent shape drops the keyword instead.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "function": {"type": "string"},
                "args": {"type": "array", "items": {}}
            },
            "required": ["function"],
            "additionalProperties": true
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("empty items must repair to the keyword being absent");
        assert_eq!(schema["properties"]["args"], json!({"type": "array"}));

        // Nested occurrences repair too; other untyped schema nodes still
        // fail closed because no semantics-preserving MFJS shape exists.
        let mut nested = json!({
            "type": "object",
            "properties": {
                "outer": {
                    "type": "object",
                    "properties": {
                        "inner": {"type": "array", "items": {}}
                    },
                    "required": ["inner"]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut nested).expect("nested empty items repair");
        assert_eq!(
            nested["properties"]["outer"]["properties"]["inner"],
            json!({"type": "array"})
        );

        let mut described_only = json!({
            "type": "object",
            "properties": {
                "args": {"type": "array", "items": {"description": "anything"}}
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut described_only).unwrap_err(),
            KimiParameterSchemaError::MissingType
        );
    }

    #[test]
    fn kimi_parameters_repairs_nested_and_sibling_empty_items() {
        // items-in-items: the inner keyword repairs to nothing, which empties
        // the outer node; `{"items": X}` is vacuously true for non-arrays and
        // `{"items": {}}` accepts everything, so dropping the outer keyword
        // preserves the accepted set. The emptiness check must run after the
        // recursion or the parent is stranded as `{}` and fails MissingType.
        let mut grid = json!({
            "type": "object",
            "properties": {
                "grid": {"type": "array", "items": {"items": {}}}
            }
        });
        sanitize_for_kimi_parameters(&mut grid).expect("items-in-items repair");
        assert_eq!(grid["properties"]["grid"], json!({"type": "array"}));

        // A root `items` keyword beside `$defs`: the repair must visit every
        // schema-value sibling of the root node, not just the removed key's
        // own subtree, or `$defs` stays unrepaired and fails the whole tool.
        let mut with_defs = json!({
            "type": "object",
            "items": {},
            "$defs": {"rows": {"type": "array", "items": {}}},
            "properties": {"rows": {"$ref": "#/$defs/rows"}}
        });
        sanitize_for_kimi_parameters(&mut with_defs).expect("root items beside $defs repair");
        assert!(with_defs.get("items").is_none());
        assert_eq!(with_defs["$defs"]["rows"], json!({"type": "array"}));

        // Every schema-value position the validator recurses into must be
        // visited: an empty `items` inside an object-form
        // `additionalProperties` or an `anyOf` branch repairs too, and a
        // walker that skips either position would strand the node and fail
        // the whole tool with MissingType.
        let mut nested_positions = json!({
            "type": "object",
            "properties": {
                "map": {
                    "type": "object",
                    "additionalProperties": {"type": "array", "items": {}}
                },
                "choice": {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "array", "items": {}}
                    ]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut nested_positions)
            .expect("additionalProperties and anyOf empty items repair");
        assert_eq!(
            nested_positions["properties"]["map"]["additionalProperties"],
            json!({"type": "array"})
        );
        assert_eq!(
            nested_positions["properties"]["choice"]["anyOf"][1],
            json!({"type": "array"})
        );

        // The root's own object-form `additionalProperties` is a schema-value
        // position too: an empty `items` there repairs to the keyword being
        // absent and the tool ships.
        let mut root_additional = json!({
            "type": "object",
            "properties": {"name": {"type": "string"}},
            "additionalProperties": {"type": "array", "items": {}}
        });
        sanitize_for_kimi_parameters(&mut root_additional)
            .expect("root additionalProperties empty items repair");
        assert_eq!(
            root_additional["additionalProperties"],
            json!({"type": "array"})
        );

        // An empty `items` beside a `$ref` drops the same way, leaving the
        // clean `$ref` node — MFJS admits no sibling beyond
        // title/description next to a reference, so without the repair the
        // whole tool would fail closed with InvalidReference.
        let mut ref_with_items = json!({
            "type": "object",
            "properties": {"a": {"$ref": "#/$defs/a", "items": {}}},
            "$defs": {"a": {"type": "string"}}
        });
        sanitize_for_kimi_parameters(&mut ref_with_items)
            .expect("$ref with an empty items sibling repairs");
        assert_eq!(
            ref_with_items["properties"]["a"],
            json!({"$ref": "#/$defs/a"})
        );
    }

    #[test]
    fn kimi_parameters_min_items_without_absent_items_terminates_upstream() {
        // Ported from codewhale-hq e3e563204 (the #7015 review follow-up):
        // once an empty `items` repairs to the keyword being absent, a
        // required array no longer fails mfjs_schema_can_terminate with
        // InvalidSchemaNode — MFJS allows `items` to be absent, and an
        // unconstrained array can always be satisfied, so the repaired tool
        // ships at every terminator-checked position. `minItems` itself sits
        // outside the specification's supported set, so the advisory strip
        // removes it first and the wire carries the document-exact
        // unconstrained shape.
        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "items": {}, "minItems": 1}},
            "required": ["a"]
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("a required array without items terminates after the repairs");
        assert_eq!(
            schema["properties"]["a"],
            json!({"type": "array"}),
            "empty items and unlisted minItems both repair to the absent form"
        );

        // The same repairs apply at the terminator's other checked positions:
        // an `anyOf` branch and a `$ref` target reached from a required
        // property.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {
                    "anyOf": [
                        {"type": "array", "items": {}, "minItems": 1},
                        {"type": "string"}
                    ]
                }
            },
            "required": ["a"]
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("an anyOf branch array without items terminates");
        assert_eq!(
            schema["properties"]["a"]["anyOf"][0],
            json!({"type": "array"})
        );

        let mut schema = json!({
            "type": "object",
            "$defs": {"a": {"type": "array", "items": {}, "minItems": 1}},
            "properties": {"x": {"$ref": "#/$defs/a"}},
            "required": ["x"]
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("a $ref target array without items terminates");
        assert_eq!(schema["$defs"]["a"], json!({"type": "array"}));

        // A carried `minItems >= 1` beside present `items` keeps the items
        // constraint out of the terminator's way only because the strip
        // already removed the keyword; a direct unsanitized validation call
        // still reads the conservative lower bound through the port.
        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "minItems": 1, "items": {"type": "string"}}},
            "required": ["a"]
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("the carried items schema survives with minItems stripped");
        assert_eq!(
            schema["properties"]["a"],
            json!({"type": "array", "items": {"type": "string"}})
        );

        // Tuple semantics stay out of the document entirely: `prefixItems`
        // is unlisted, has no strip equivalent, and fails the tool closed.
        let schema = json!({
            "type": "object",
            "properties": {
                "a": {
                    "type": "array",
                    "minItems": 1,
                    "prefixItems": [{"type": "string"}]
                }
            },
            "required": ["a"]
        });
        validate_mfjs_parameters(&schema).expect_err("prefixItems is outside the document set");

        let schema = json!({
            "type": "object",
            "properties": {
                "a": {
                    "type": "array",
                    "prefixItems": [
                        {"type": "array", "items": {"$ref": "#"}}
                    ]
                }
            },
            "required": ["a"]
        });
        validate_mfjs_parameters(&schema)
            .expect_err("a non-terminating tuple prefix still fails closed");
    }

    fn probe_observed_keyword_schema() -> Value {
        // Keywords the 2026-11-04 live probe of https://api.kimi.com/coding/v1
        // merely observed the server accepting (200), plus nested allOf. The
        // MFJS specification does not list any of them, and the closing
        // clause ("未被提及……我们不保证它们能在最终的结构化输出中产生稳定、
        // 符合预期的结果") leaves them unsupported — so the probe is an
        // observation only and never a shipping basis: the wire stays on the
        // document's supported set and these shapes keep dropping the tool.
        json!({
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "type": "object",
            "properties": {
                "label": {
                    "type": "string",
                    "pattern": "^[a-z][a-z0-9-]*$",
                    "format": "uri"
                },
                "price": {
                    "type": "number",
                    "exclusiveMinimum": 0
                },
                "rank": {"type": "integer", "exclusiveMaximum": 10},
                "row": {
                    "type": "array",
                    "prefixItems": [{"type": "string"}, {"type": "integer"}]
                },
                "target": {
                    "type": "object",
                    "allOf": [
                        {
                            "type": "object",
                            "properties": {"path": {"type": "string"}},
                            "required": ["path"]
                        },
                        {
                            "type": "object",
                            "properties": {"mode": {"type": "string"}}
                        }
                    ]
                }
            },
            "required": ["label"]
        })
    }

    #[test]
    fn probe_observed_keywords_fail_closed_on_every_route() {
        // Probe facts are observations only. No route may admit keywords the
        // MFJS specification does not list: the whole tool degrades with a
        // projection warning instead of shipping an unguaranteed shape. One
        // pipeline serves every Moonshot route, so one run of the composed
        // probe schema covers all of them.
        let mut schema = probe_observed_keyword_schema();
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("probe-observed keywords stay outside every route's document whitelist");
        assert!(
            matches!(
                error,
                KimiParameterSchemaError::UnsupportedKeyword
                    | KimiParameterSchemaError::UnsupportedNestedAllOf
            ),
            "the first out-of-document shape fails closed: {error:?}"
        );

        // Individual document-absent keywords, each pinned so a whitelist
        // change cannot slip one back in silently.
        for (keyword, node) in [
            ("pattern", json!({"type": "string", "pattern": "^a$"})),
            ("format", json!({"type": "string", "format": "uri"})),
            (
                "$schema",
                json!({"type": "string", "$schema": "https://json-schema.org/draft/2020-12/schema"}),
            ),
            (
                "prefixItems",
                json!({"type": "array", "prefixItems": [{"type": "string"}]}),
            ),
            (
                "exclusiveMinimum",
                json!({"type": "number", "exclusiveMinimum": 0}),
            ),
            (
                "exclusiveMaximum",
                json!({"type": "integer", "exclusiveMaximum": 10}),
            ),
        ] {
            let mut schema = json!({
                "type": "object",
                "properties": {"leaf": node},
                "required": ["leaf"]
            });
            let error = sanitize_for_kimi_parameters(&mut schema)
                .err()
                .unwrap_or_else(|| panic!("{keyword} must fail closed, not sanitize"));
            assert!(
                matches!(error, KimiParameterSchemaError::UnsupportedKeyword),
                "{keyword} must drop the tool with UnsupportedKeyword: {error:?}"
            );
        }

        let mut all_of_only = json!({
            "type": "object",
            "properties": {
                "target": {
                    "type": "object",
                    "allOf": [
                        {
                            "type": "object",
                            "properties": {"path": {"type": "string"}},
                            "required": ["path"]
                        },
                        {
                            "type": "object",
                            "properties": {"mode": {"type": "string"}}
                        }
                    ]
                }
            }
        });
        // Two branches: a single-branch allOf is collapsed by the shared
        // semantics-preserving pass before normalization ever sees it.
        let error = sanitize_for_kimi_parameters(&mut all_of_only)
            .expect_err("nested allOf keeps failing closed");
        assert!(
            matches!(error, KimiParameterSchemaError::UnsupportedNestedAllOf),
            "nested allOf keeps the fail-closed drop: {error:?}"
        );
    }

    #[test]
    fn advisory_keywords_are_stripped_not_shipped() {
        // Keywords the MFJS specification does not list but whose removal is
        // a semantics-lossless or soft-constraint-losing simplification are
        // stripped in place instead of dropping the tool: the annotation
        // keywords (title, $comment, $id) and the unlisted bound keywords
        // (minLength, maxLength, minimum, maximum, minItems, maxItems). The
        // wire must carry only the document's supported set.
        let mut schema = json!({
            "title": "Outer annotation",
            "$comment": "why this tool exists",
            "$id": "https://example.com/outer",
            "type": "object",
            "properties": {
                "label": {
                    "title": "Label",
                    "type": "string",
                    "minLength": 1,
                    "maxLength": 40
                },
                "price": {
                    "type": "number",
                    "minimum": 0,
                    "maximum": 100
                },
                "row": {
                    "type": "array",
                    "minItems": 1,
                    "maxItems": 2,
                    "items": {"type": "string"}
                }
            },
            "required": ["label"]
        });

        sanitize_for_kimi_parameters(&mut schema)
            .expect("advisory keywords must not drop the tool");

        for keyword in [
            "title",
            "$comment",
            "$id",
            "minLength",
            "maxLength",
            "minimum",
            "maximum",
            "minItems",
            "maxItems",
        ] {
            assert!(
                !contains_key_anywhere(&schema, keyword),
                "{keyword} must not reach the wire: {schema}"
            );
        }
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["properties"]["label"]["type"], "string");
        assert_eq!(schema["properties"]["row"]["items"]["type"], "string");
        validate_mfjs_parameters(&schema).unwrap();

        // The same document sanitizes identically everywhere: one pipeline,
        // one wire contract for every Moonshot route.
        let mut reference = schema.clone();
        sanitize_for_kimi_parameters(&mut reference).unwrap();
        assert_eq!(schema, reference);
    }

    #[test]
    fn unsupported_enum_values_are_stripped_and_tool_survives() {
        // The MFJS specification supports enum values only as float/int/str
        // scalars of one single type ("枚举值我们只支持基础类型中的 float,
        // int 和 str……不支持混合类型 (List 中的每个枚举值的类型必须相同)").
        // An enum carrying anything else loses the keyword — an
        // unconstrained node stays shippable — instead of dropping the tool.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mixed_kinds": {"type": "string", "enum": ["fast", 1]},
                "boolean_values": {"type": "boolean", "enum": [true]},
                "object_values": {"type": "object", "enum": [{"nested": true}]},
                "array_values": {"type": "array", "enum": [["a"]]}
            }
        });

        sanitize_for_kimi_parameters(&mut schema)
            .expect("unsupported enum values must strip, not drop the tool");

        for name in [
            "mixed_kinds",
            "boolean_values",
            "object_values",
            "array_values",
        ] {
            assert!(
                schema["properties"][name].get("enum").is_none(),
                "{name} must lose its unsupported enum: {schema}"
            );
        }
        assert_eq!(schema["properties"]["mixed_kinds"]["type"], "string");
        assert_eq!(schema["properties"]["boolean_values"]["type"], "boolean");
        assert_eq!(schema["properties"]["object_values"]["type"], "object");
        assert_eq!(schema["properties"]["array_values"]["type"], "array");
        validate_mfjs_parameters(&schema).unwrap();

        // A node whose only keyword is an unsupported `enum` has no type to
        // fall back to: the strip leaves an empty schema node, which fails
        // closed with MissingType exactly as the baseline did — the strip
        // never invents a type the schema author did not declare.
        let mut untyped = json!({
            "type": "object",
            "properties": {"leaf": {"enum": ["a", 1]}}
        });
        let error = sanitize_for_kimi_parameters(&mut untyped)
            .expect_err("an enum-only node has no type to survive on");
        assert!(
            matches!(error, KimiParameterSchemaError::MissingType),
            "{error:?}"
        );

        // Homogeneous scalar enums stay: str-only, int-only, and float-only
        // are exactly the document's supported shapes.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "role": {"type": "string", "enum": ["fast", "full"]},
                "level": {"type": "integer", "enum": [1, 2]},
                "ratio": {"type": "number", "enum": [0.5, 1.5]}
            }
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(
            schema["properties"]["role"]["enum"],
            json!(["fast", "full"])
        );
        assert_eq!(schema["properties"]["level"]["enum"], json!([1, 2]));
        assert_eq!(schema["properties"]["ratio"]["enum"], json!([0.5, 1.5]));
    }

    #[test]
    fn kimi_parameters_empty_enum_fails_closed_in_the_validator() {
        // An empty enum accepts nothing, so stripping it would silently
        // widen an unsatisfiable constraint into "anything goes". The strip
        // keeps the keyword (the homogeneous check is vacuously true for an
        // empty array) and the validator's non-empty rule fails the whole
        // tool — the documented "malformed keyword is a schema bug" policy.
        let mut schema = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": []}},
            "required": ["choice"]
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("an empty enum is malformed and must fail closed");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
            "empty enum drops via the validator: {error:?}"
        );

        // A non-array `enum` takes the same road: kept by the strip, then
        // failed closed by the validator.
        let mut malformed = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": "fast"}}
        });
        let error = sanitize_for_kimi_parameters(&mut malformed)
            .expect_err("a non-array enum is malformed and must fail closed");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
            "non-array enum drops via the validator: {error:?}"
        );
    }

    #[test]
    fn kimi_parameters_fail_closed_shapes_stay_dropped() {
        // The body's fail-closed enumeration, each pinned so a validator
        // change cannot silence the drop silently: empty `anyOf` branches
        // (including one the nested repair itself empties), empty `$defs`
        // entries, boolean and tuple `items`.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "choice": {"anyOf": [{"type": "string"}, {}]}
            }
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("an empty anyOf branch fails closed");
        assert!(
            matches!(error, KimiParameterSchemaError::MissingType),
            "empty branch drops with MissingType: {error:?}"
        );

        let mut schema = json!({
            "type": "object",
            "properties": {
                "choice": {"anyOf": [{"type": "string"}, {"items": {"items": {}}}]}
            }
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("a branch the nested repair empties to {} fails closed");
        assert!(
            matches!(error, KimiParameterSchemaError::MissingType),
            "nested-emptied branch drops with MissingType: {error:?}"
        );

        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "string"}},
            "$defs": {"x": {}}
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("an empty $defs entry fails closed");
        assert!(
            matches!(error, KimiParameterSchemaError::MissingType),
            "empty $defs entry drops with MissingType: {error:?}"
        );

        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "items": true}},
            "required": ["a"]
        });
        let error =
            sanitize_for_kimi_parameters(&mut schema).expect_err("boolean items fail closed");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidSchemaNode),
            "boolean items drops with InvalidSchemaNode: {error:?}"
        );

        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "items": [{"type": "string"}]}},
            "required": ["a"]
        });
        let error =
            sanitize_for_kimi_parameters(&mut schema).expect_err("tuple-form items fail closed");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidSchemaNode),
            "tuple items drops with InvalidSchemaNode: {error:?}"
        );
    }

    #[test]
    fn kimi_parameters_rejects_required_direct_and_mutual_recursion() {
        let fixtures = [
            json!({
                "type": "object",
                "properties": {
                    "private-root-node-2201": {"$ref": "#/$defs/private-node-2202"}
                },
                "required": ["private-root-node-2201"],
                "$defs": {
                    "private-node-2202": {
                        "type": "object",
                        "properties": {
                            "private-next-2203": {"$ref": "#/$defs/private-node-2202"}
                        },
                        "required": ["private-next-2203"]
                    }
                }
            }),
            json!({
                "type": "object",
                "properties": {
                    "private-root-a-2204": {"$ref": "#/$defs/private-a-2205"}
                },
                "required": ["private-root-a-2204"],
                "$defs": {
                    "private-a-2205": {
                        "type": "object",
                        "properties": {
                            "private-to-b-2206": {"$ref": "#/$defs/private-b-2207"}
                        },
                        "required": ["private-to-b-2206"]
                    },
                    "private-b-2207": {
                        "type": "object",
                        "properties": {
                            "private-to-a-2208": {"$ref": "#/$defs/private-a-2205"}
                        },
                        "required": ["private-to-a-2208"]
                    }
                }
            }),
        ];

        for mut schema in fixtures {
            let original = schema.clone();
            let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();
            assert_eq!(error, KimiParameterSchemaError::NonTerminatingReference);
            for sentinel in ["private-root", "private-node", "private-to"] {
                assert!(!error.to_string().contains(sentinel));
            }
            assert_eq!(schema, original, "recursive rejection must be atomic");
        }
    }

    #[test]
    fn kimi_parameters_preserves_optional_and_nullable_recursive_termination() {
        let mut optional = json!({
            "type": "object",
            "properties": {
                "node": {"$ref": "#/$defs/Node"}
            },
            "$defs": {
                "Node": {
                    "type": "object",
                    "properties": {
                        "next": {"$ref": "#/$defs/Node"}
                    },
                    "required": ["next"]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut optional).unwrap();

        let mut nullable = json!({
            "type": "object",
            "properties": {
                "node": {
                    "anyOf": [
                        {"$ref": "#/$defs/Node"},
                        {"type": "null"}
                    ]
                }
            },
            "required": ["node"],
            "$defs": {
                "Node": {
                    "type": "object",
                    "properties": {
                        "next": {
                            "anyOf": [
                                {"$ref": "#/$defs/Node"},
                                {"type": "null"}
                            ]
                        }
                    },
                    "required": ["next"]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut nullable).unwrap();
    }

    #[test]
    fn kimi_parameters_enforces_anyof_and_aggregate_resource_limits() {
        // Each boundary mirrors the walle validator's own default (the
        // constant block cites `conf.go`, `DefaultValidatorConfig`, fetched
        // 2026-11-05). The aligned value itself must ship — the limit exists
        // to drop only what the server's own validator would also refuse, so
        // an engine-local tightening cannot silently re-emerge here.
        let branches_schema = |count: usize| {
            json!({
                "type": "object",
                "properties": {
                    "choice": {
                        "anyOf": (0..count)
                            .map(|_| json!({"type": "string"}))
                            .collect::<Vec<_>>()
                    }
                }
            })
        };
        let mut at_any_of_budget = branches_schema(500);
        sanitize_for_kimi_parameters(&mut at_any_of_budget)
            .expect("500 anyOf branches is the aligned budget and must ship");
        let mut too_many_branches = branches_schema(501);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_branches).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let string_property = || json!({"type": "string"});
        let properties_schema = |root_count: usize, definition_count: usize| {
            let mut root_properties = Map::new();
            for index in 0..root_count {
                root_properties.insert(format!("root_{index}"), string_property());
            }
            let mut definition_properties = Map::new();
            for index in 0..definition_count {
                definition_properties.insert(format!("definition_{index}"), string_property());
            }
            json!({
                "type": "object",
                "properties": Value::Object(root_properties),
                "$defs": {
                    "Holder": {
                        "type": "object",
                        "properties": Value::Object(definition_properties)
                    }
                }
            })
        };
        let mut at_property_budget = properties_schema(3000, 0);
        sanitize_for_kimi_parameters(&mut at_property_budget)
            .expect("3000 total properties is the aligned budget and must ship");
        let mut too_many_properties = properties_schema(1501, 1500);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_properties).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let enum_values = |start: usize, count: usize| {
            Value::Array((start..start + count).map(|value| json!(value)).collect())
        };
        let enum_schema = |count: usize| {
            json!({
                "type": "object",
                "properties": {
                    "choice": {"type": "integer", "enum": enum_values(0, count)}
                }
            })
        };
        let mut at_enum_budget = enum_schema(1000);
        sanitize_for_kimi_parameters(&mut at_enum_budget)
            .expect("1000 enum values is the aligned budget and must ship");
        let mut too_many_enum_values = enum_schema(1001);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_enum_values).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_enforces_depth_and_enum_text_limits() {
        let nest_properties = |depth: usize| {
            let mut nested = json!({"type": "string"});
            for index in (0..depth).rev() {
                let mut properties = Map::new();
                properties.insert(format!("level_{index}"), nested);
                nested = json!({
                    "type": "object",
                    "properties": Value::Object(properties)
                });
            }
            nested
        };
        let mut at_depth_budget = nest_properties(30);
        sanitize_for_kimi_parameters(&mut at_depth_budget)
            .expect("30 property levels is the aligned budget and must ship");
        let mut too_deep = nest_properties(31);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_deep).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        // The encoded-length budget only arms past the check threshold, so
        // both probes below carry enough values to reach it: 1000 values of
        // 74 encoded chars total just under the aligned 75000 budget and
        // must ship; the same count at 88 chars oversteps it and must not.
        let long_enum = |padding: usize| {
            Value::Array(
                (0..1000)
                    .map(|index| {
                        Value::String(format!("private-enum-{index:04}-{}", "x".repeat(padding)))
                    })
                    .collect(),
            )
        };
        let mut at_enum_text_budget = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": long_enum(56)}}
        });
        sanitize_for_kimi_parameters(&mut at_enum_text_budget)
            .expect("74000 encoded enum chars sit inside the aligned budget");
        let mut too_much_enum_text = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": long_enum(70)}}
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_much_enum_text).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_enum_text_budget_boundary_is_exact() {
        // The probes above straddle the encoded-length budget (74000 under,
        // 88000 over); this pins the exact boundary — 1000 values of exactly
        // 75 encoded chars total 75000 and ship, one char more each totals
        // 76000 and must not. A value edit that moves the budget silently
        // fails here.
        let long_enum = |padding: usize| {
            Value::Array(
                (0..1000)
                    .map(|index| {
                        Value::String(format!("private-enum-{index:04}-{}", "x".repeat(padding)))
                    })
                    .collect(),
            )
        };
        let mut at_limit = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": long_enum(57)}}
        });
        sanitize_for_kimi_parameters(&mut at_limit)
            .expect("exactly 75000 encoded enum chars sit at the budget and ship");
        let mut over = json!({
            "type": "object",
            "properties": {"choice": {"type": "string", "enum": long_enum(58)}}
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut over).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_total_enum_budget_accumulates_across_nodes() {
        // The enum budget is document-wide (state.total_enum_values), while
        // walle's MaxEnumItems counts per node — the constant block discloses
        // the scope caveat. This pins the accumulation itself: two 500-value
        // nodes ship at the 1000 total, and one extra value anywhere fails
        // the whole tool.
        let enum_values =
            |count: usize| Value::Array((0..count).map(|value| json!(value)).collect());
        let schema = |left: usize, right: usize| {
            json!({
                "type": "object",
                "properties": {
                    "left": {"type": "integer", "enum": enum_values(left)},
                    "right": {"type": "integer", "enum": enum_values(right)}
                }
            })
        };
        let mut at_total_budget = schema(500, 500);
        sanitize_for_kimi_parameters(&mut at_total_budget)
            .expect("1000 total enum values across two nodes ship");
        let mut over_total_budget = schema(500, 501);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut over_total_budget).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_whole_document_byte_budget_rejects_oversized_schemas() {
        // MFJS_MAX_SCHEMA_BYTES bounds the whole serialized document after
        // the strip; it has no probe anywhere else, so pin both faces here.
        let blob_schema = |description_len: usize| {
            json!({
                "type": "object",
                "properties": {
                    "blob": {
                        "type": "string",
                        "description": "y".repeat(description_len)
                    }
                },
                "required": ["blob"]
            })
        };
        let mut over = blob_schema(130_000);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut over).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        let mut roomy = blob_schema(1_000);
        sanitize_for_kimi_parameters(&mut roomy)
            .expect("a small document sits far inside the byte budget");
    }

    #[test]
    fn kimi_parameters_structural_depth_boundary_is_engine_local() {
        // MFJS_MAX_STRUCTURAL_DEPTH=64 counts every node descent (items
        // chains included) and is an engine-only recursion guard — walle has
        // no counterpart budget. 63 wrapped item levels reach exactly 64
        // structural entries from the root and ship; one more level exceeds
        // the guard.
        let nested_items = |levels: usize| {
            let mut node = json!({"type": "string"});
            for _ in 0..levels {
                node = json!({"type": "array", "items": node});
            }
            json!({
                "type": "object",
                "properties": {"tree": node},
                "required": ["tree"]
            })
        };
        let mut at_guard = nested_items(63);
        sanitize_for_kimi_parameters(&mut at_guard)
            .expect("64 structural levels reach the engine guard exactly and ship");
        let mut over = nested_items(64);
        assert_eq!(
            sanitize_for_kimi_parameters(&mut over).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_ships_default_on_any_node_without_type_check() {
        // The specification's `default` clause, verbatim: "default 字段为
        // 目标输出提供一个默认值……这儿不做类型检查，需要调用者自行保证其值
        // 类型内容和所在位置期望的内容相匹配" (any value, any node;
        // https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-spec.zh.md,
        // fetched 2026-11-05). The wire therefore carries `default` verbatim:
        // no per-type placement rejection and no value/type mismatch
        // fail-closed — the caller owns the value's correctness.
        let mut valid = json!({
            "type": "object",
            "properties": {
                "enabled": {"type": "boolean", "default": true},
                "count": {"type": "integer", "default": 3},
                "ratio": {"type": "number", "default": 1.5},
                "label": {"type": "string", "default": "default-label"},
                "empty": {"type": "null", "default": null},
                "mismatched": {
                    "type": "integer",
                    "default": "private-default-value-3302"
                },
                "object_valued": {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "default": {"a": "b"}
                },
                "array_valued": {
                    "type": "array",
                    "items": {"type": "string"},
                    "default": ["x"]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut valid).unwrap();
        assert_eq!(
            valid["properties"]["mismatched"]["default"],
            json!("private-default-value-3302")
        );
        assert_eq!(
            valid["properties"]["object_valued"]["default"],
            json!({"a": "b"})
        );
        assert_eq!(valid["properties"]["array_valued"]["default"], json!(["x"]));

        // A node that only carries `default` still fails the type-absence
        // rule (MissingType) — that rejection is about the node, not about
        // the default keyword or its value.
        let mut untyped = json!({
            "type": "object",
            "properties": {"private-untyped-default-3303": {"default": 1}}
        });
        let original = untyped.clone();
        let error = sanitize_for_kimi_parameters(&mut untyped).unwrap_err();
        assert_eq!(error, KimiParameterSchemaError::MissingType);
        assert!(!error.to_string().contains("private-untyped-default"));
        assert_eq!(untyped, original);
    }

    #[test]
    fn kimi_parameters_strips_bounds_instead_of_rejecting_them() {
        // The bound keywords sit outside the specification's supported set,
        // so even malformed or inverted bounds never degrade the tool: the
        // advisory strip removes them (losing only the soft constraint) and
        // the wire carries the unconstrained node. No bound value is ever
        // validated, because none ever ships.
        for (mut schema, keywords) in [
            (
                json!({
                    "type": "object",
                    "properties": {
                        "value": {"type": "string", "minLength": 5, "maxLength": 4}
                    }
                }),
                vec!["minLength", "maxLength"],
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "value": {"type": "array", "minItems": 3, "maxItems": 2}
                    }
                }),
                vec!["minItems", "maxItems"],
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "value": {"type": "number", "minimum": 10, "maximum": 9}
                    }
                }),
                vec!["minimum", "maximum"],
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "value": {"type": "integer", "minimum": 1.5}
                    }
                }),
                vec!["minimum"],
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "value": {"type": "integer", "maximum": 2.5}
                    }
                }),
                vec!["maximum"],
            ),
        ] {
            sanitize_for_kimi_parameters(&mut schema)
                .expect("bounds must strip, never degrade the tool");
            for keyword in keywords {
                assert!(
                    !contains_key_anywhere(&schema, keyword),
                    "{keyword} must not reach the wire: {schema}"
                );
            }
            validate_mfjs_parameters(&schema).expect("the stripped schema must validate");
        }
    }

    #[test]
    fn kimi_parameters_inlines_valid_internal_object_root_ref() {
        let mut schema = json!({
            "$ref": "#/$defs/FileArgs",
            "$defs": {
                "FileArgs": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                }
            },
            "description": "File arguments"
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["type"], "object");
        assert_eq!(schema["properties"]["path"]["type"], "string");
        assert_eq!(schema["required"], json!(["path"]));
        assert_eq!(schema["description"], "File arguments");
        assert!(schema["$defs"].is_object());
        assert!(schema.get("$ref").is_none());
        assert!(schema.get("allOf").is_none());
    }

    #[test]
    fn kimi_parameters_rejects_unresolved_root_ref_without_leaking_it() {
        let mut schema = json!({
            "$ref": "#/$defs/private-schema-name-9217",
            "$defs": {}
        });
        let original = schema.clone();

        let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();

        assert_eq!(error, KimiParameterSchemaError::UnresolvedRootReference);
        assert!(!error.to_string().contains("private-schema-name-9217"));
        assert_eq!(schema, original, "a rejected schema must never be emitted");
    }

    fn contains_key_anywhere(value: &Value, key: &str) -> bool {
        match value {
            Value::Object(map) => {
                map.contains_key(key) || map.values().any(|v| contains_key_anywhere(v, key))
            }
            Value::Array(items) => items.iter().any(|v| contains_key_anywhere(v, key)),
            _ => false,
        }
    }

    fn action_discriminated_schema() -> Value {
        json!({
            "type": "object",
            "properties": {
                "action": {"type": "string", "enum": ["start", "status"]},
                "prompt": {"type": "string"},
                "agent_id": {"type": "string"}
            },
            "required": ["action"],
            "dependentRequired": {
                "prompt": ["action"]
            },
            "dependentSchemas": {
                "action": {"required": ["prompt"]}
            }
        })
    }

    #[test]
    fn kimi_degrades_dependent_keywords_to_the_flat_schema() {
        let mut schema = action_discriminated_schema();

        let note = sanitize_for_kimi_parameters(&mut schema).expect("must not refuse the schema");

        assert!(!contains_key_anywhere(&schema, "dependentSchemas"));
        assert!(!contains_key_anywhere(&schema, "dependentRequired"));
        let properties = schema["properties"].as_object().expect("properties");
        for name in ["action", "prompt", "agent_id"] {
            assert!(properties.contains_key(name), "{name} left the flat schema");
        }
        assert_eq!(schema["required"], json!(["action"]));
        validate_mfjs_parameters(&schema).expect("degraded schema must validate");

        let note = note.expect("dropping a requirement must be reported to the model");
        assert_eq!(
            note,
            "This provider cannot express conditional requirements from the original schema. \
             Honor any such requirements documented by the tool when calling it."
        );
    }

    #[test]
    fn kimi_leaves_schemas_without_dependent_keywords_unannotated() {
        let mut schema = json!({
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"]
        });

        let note = sanitize_for_kimi_parameters(&mut schema).expect("plain schema");

        assert_eq!(
            note, None,
            "a schema that lost nothing must not gain a note"
        );
        assert_eq!(schema["required"], json!(["path"]));
    }

    #[test]
    fn kimi_degrades_dependent_keywords_nested_under_properties() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "target": {
                    "type": "object",
                    "properties": {
                        "kind": {"type": "string"},
                        "path": {"type": "string"}
                    },
                    "dependentRequired": {"kind": ["path"]}
                }
            }
        });

        let note = sanitize_for_kimi_parameters(&mut schema).expect("nested composition");

        assert!(!contains_key_anywhere(&schema, "dependentRequired"));
        assert!(
            schema["properties"]["target"]["properties"]
                .as_object()
                .expect("nested properties")
                .contains_key("path")
        );
        validate_mfjs_parameters(&schema).expect("degraded schema must validate");
        assert!(note.is_some(), "nested drop must still be reported");
    }

    #[test]
    fn kimi_reports_malformed_dependent_keywords_that_it_drops() {
        let mut schema = json!({
            "type": "object",
            "properties": {},
            "dependentRequired": false,
            "dependentSchemas": ["invalid"]
        });

        let note = sanitize_for_kimi_parameters(&mut schema).expect("degraded schema");

        assert!(note.is_some(), "every dropped dependency must be reported");
        assert!(!contains_key_anywhere(&schema, "dependentRequired"));
        assert!(!contains_key_anywhere(&schema, "dependentSchemas"));
    }

    #[test]
    fn kimi_parameters_rejects_non_object_root_ref_without_leaking_it() {
        let mut schema = json!({
            "$ref": "#/$defs/private-scalar-name-4831",
            "$defs": {
                "private-scalar-name-4831": {"type": "string"}
            }
        });
        let original = schema.clone();

        let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();

        assert_eq!(error, KimiParameterSchemaError::ReferencedRootMustBeObject);
        assert!(!error.to_string().contains("private-scalar-name-4831"));
        assert_eq!(schema, original, "a rejected schema must never be emitted");
    }

    // Cross-repo integration seam (parent-repo batch 2026-11-04): the fixture
    // is the shipped `make_pptx` tool definition exported verbatim from the
    // parent repository's pptx MCP server
    // (pinvou3-app/resources/mcp-servers/pptx/server.py, `TOOL_DEFS`). Its
    // slides.items is a per-layout anyOf — exactly one explicit `type: object`
    // branch per layout, the MFJS branch budget used to the last slot — with
    // per-branch required groups, a nested string-or-object item union
    // (agenda/kpi entries), and typed nested arrays (chart series). The shape
    // exists so constrained tool-call decoding (Kimi K3) fills the per-branch
    // required fields instead of collapsing to layout+title-only pages. This
    // test pins the seam between the host-side schema authoring and this
    // Moonshot compatibility layer: the shipped schema must normalize, pass
    // the MFJS validator, keep its nested union structure, and carry only
    // keywords inside the specification's supported set. A schema drift past
    // an MFJS budget turns the normalization red; a renewed flattening of
    // nested unions turns the branch assertions red; a keyword drifting
    // outside the document's supported set turns the wire-keyword matrix red —
    // each on exactly the side that regressed.
    const SHIPPED_PPTX_TOOL_DEFS: &str =
        include_str!("../../tests/fixtures/pptx_make_pptx_schema.json");

    #[test]
    fn shipped_pptx_slides_schema_passes_moonshot_validation_on_the_document_set() {
        let fixture: Value = serde_json::from_str(SHIPPED_PPTX_TOOL_DEFS)
            .expect("the exported pptx tool definitions must stay valid JSON");
        let shipped = Value::Object(
            fixture["tools"][0]["inputSchema"]
                .as_object()
                .expect("fixture must carry the make_pptx inputSchema")
                .clone(),
        );

        // Fixture sanity: the shipped shape is the per-layout union. Every
        // branch declares an explicit object type, selects its layout through
        // a single-value enum, and carries the required group its renderer
        // needs.
        let branch_contract = |branches: &[Value]| -> Vec<(String, Vec<String>)> {
            branches
                .iter()
                .map(|branch| {
                    assert_eq!(
                        branch["type"], "object",
                        "every shipped branch declares an explicit object type"
                    );
                    let mut layouts: Vec<String> = branch["properties"]["layout"]["enum"]
                        .as_array()
                        .expect("branch layout enum")
                        .iter()
                        .map(|value| value.as_str().expect("string layout").to_string())
                        .collect();
                    assert_eq!(
                        layouts.len(),
                        1,
                        "selecting a branch is selecting a layout: single-value enum"
                    );
                    let required: Vec<String> = branch["required"]
                        .as_array()
                        .expect("per-branch required group")
                        .iter()
                        .map(|value| value.as_str().expect("string name").to_string())
                        .collect();
                    (layouts.remove(0), required)
                })
                .collect()
        };
        let shipped_branches = shipped["properties"]["slides"]["items"]["anyOf"]
            .as_array()
            .expect("slides.items must carry the per-layout anyOf");
        assert_eq!(
            shipped_branches.len(),
            10,
            "the shipped union has exactly one branch per layout"
        );
        // The branch count is not the anyOf budget: the engine mirrors the
        // walle validator's aligned default (500, see the constant block), so
        // the sentinel only has to show the shipped union sits far below it
        // and the whole pipeline still passes.
        assert!(
            shipped_branches.len() * 10 <= MFJS_MAX_ANY_OF_ITEMS,
            "10 shipped branches must sit far below the anyOf budget {}",
            MFJS_MAX_ANY_OF_ITEMS
        );
        let expected = branch_contract(shipped_branches);

        // Per-branch required groups pinned to the parent-side server
        // contract (2026-11-05 required audit, single standard: only an
        // empty body group is a business exception). agenda renders the
        // 「目录」 fallback and end the 「谢谢」 fallback, so their titles are
        // optional; cover/section/image stay layout+title; the six body
        // layouts keep layout+title plus their body group.
        let required_of = |layout: &str| -> Vec<String> {
            expected
                .iter()
                .find(|(name, _)| name == layout)
                .map(|(_, required)| required.clone())
                .unwrap_or_else(|| panic!("{layout} branch missing from the shipped union"))
        };
        assert_eq!(required_of("agenda"), vec!["layout", "items"]);
        assert_eq!(required_of("end"), vec!["layout"]);
        assert_eq!(required_of("cover"), vec!["layout", "title"]);
        assert_eq!(required_of("section"), vec!["layout", "title"]);
        assert_eq!(required_of("image"), vec!["layout", "title"]);
        for layout in ["bullets", "two_col", "kpi", "chart", "table"] {
            let required = required_of(layout);
            assert!(
                required.contains(&"layout".to_string())
                    && required.contains(&"title".to_string())
                    && required.len() >= 3,
                "{layout} keeps layout+title plus its body group: {required:?}"
            );
        }

        let mut parameters = shipped.clone();
        let note = sanitize_for_kimi_parameters(&mut parameters)
            .expect("the shipped make_pptx schema must pass Moonshot normalization");
        assert_eq!(
            note, None,
            "the shipped schema drops no composition, so it must stay unannotated"
        );
        validate_mfjs_parameters(&parameters)
            .expect("the normalized schema must satisfy the MFJS validator");

        // The root stays the plain object schema the tool declared.
        assert_eq!(parameters["type"], "object");
        assert_eq!(parameters["required"], json!(["slides"]));

        // The union survives intact: no flatten into a merged object, same
        // branch count, same per-branch layout/required contract.
        let items = &parameters["properties"]["slides"]["items"];
        assert!(
            items.get("properties").is_none(),
            "slides.items must not be flattened into a merged object: {items}"
        );
        let branches = items["anyOf"]
            .as_array()
            .expect("the slides.items union must survive normalization");
        assert_eq!(
            branches.len(),
            shipped_branches.len(),
            "normalization must not change the branch count"
        );
        assert_eq!(branch_contract(branches), expected);

        // The nested item union (agenda/kpi entries) survives too.
        for layout in ["agenda", "kpi"] {
            let branch = branches
                .iter()
                .find(|branch| branch["properties"]["layout"]["enum"][0] == json!(layout))
                .expect("layout branch");
            let nested = &branch["properties"]["items"]["items"]["anyOf"];
            assert_eq!(
                nested.as_array().map(Vec::len),
                Some(2),
                "the {layout} item union must keep both branches: {nested}"
            );
        }

        // Wire-keyword matrix: every keyword anywhere on the normalized
        // document must sit inside the MFJS specification's supported set.
        // The `layout`/`title`-style names below the schema value positions
        // are property names, not keywords, so the walk only counts keys at
        // schema nodes: the root, property values, `items`,
        // object-form `additionalProperties`, `anyOf` branches, and `$defs`
        // entries. Anything the parent-side schema adds beyond the document
        // set must either be stripped here (advisory keywords) or drop the
        // tool — never ship.
        let supported: &[&str] = &[
            "$defs",
            "$ref",
            "anyOf",
            "properties",
            "additionalProperties",
            "items",
            "type",
            "enum",
            "required",
            "default",
            "description",
        ];
        let mut wire_keywords: Vec<String> = Vec::new();
        fn collect_schema_keywords(value: &Value, keywords: &mut Vec<String>) {
            let Some(obj) = value.as_object() else {
                return;
            };
            keywords.extend(obj.keys().cloned());
            if let Some(properties) = obj.get("properties").and_then(Value::as_object) {
                for child in properties.values() {
                    collect_schema_keywords(child, keywords);
                }
            }
            if let Some(definitions) = obj.get("$defs").and_then(Value::as_object) {
                for child in definitions.values() {
                    collect_schema_keywords(child, keywords);
                }
            }
            if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
                for branch in branches {
                    collect_schema_keywords(branch, keywords);
                }
            }
            if let Some(additional) = obj.get("additionalProperties").filter(|a| a.is_object()) {
                collect_schema_keywords(additional, keywords);
            }
            if let Some(items) = obj.get("items") {
                collect_schema_keywords(items, keywords);
            }
        }
        collect_schema_keywords(&parameters, &mut wire_keywords);
        wire_keywords.sort();
        wire_keywords.dedup();
        assert!(
            wire_keywords
                .iter()
                .all(|keyword| supported.contains(&keyword.as_str())),
            "only specification-supported keywords may reach the wire: {wire_keywords:?}"
        );
    }
}
