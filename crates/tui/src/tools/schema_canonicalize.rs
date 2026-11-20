//! Byte-level canonicalization of JSON Schema for prefix-cache stability.
//!
//! When MCP servers return tool schemas, the field order within each schema
//! object and the order of entries in `required` / `dependentRequired` arrays
//! can vary across reconnections. This module normalizes those orderings so
//! that two logically equivalent schemas always produce identical bytes after
//! serialization.
//!
//! The approach mirrors `reasonix/internal/provider/schema_canonicalize.go`:
//!
//! 1. Sort every `"required"` array alphabetically.
//! 2. Sort every `"dependentRequired"` sub-array alphabetically.
//! 3. Recurse into all nested objects and arrays.
//!
//! `serde_json::Value::Object` uses `IndexMap` when `preserve_order` is
//! enabled (which this crate does). We therefore rebuild the map with sorted
//! keys to guarantee deterministic key ordering.

use serde_json::Value;

/// Recursively canonicalize a JSON Schema value in-place.
///
/// After canonicalization, two schemas that are semantically equivalent
/// (same keys, same `required` set, same `dependentRequired` sets) will
/// serialize to byte-identical JSON regardless of the original field or
/// array order.
pub fn canonicalize_schema(value: &mut Value) {
    match value {
        Value::Object(map) => {
            // `enum`, `const`, and `default` carry INSTANCE data, not
            // schema: an object under them is a literal argument, and a
            // `required` key inside that literal is plugin data. Sorting
            // its array or rebuilding its keys rewrites the only permitted
            // value — `enum: [{"required": ["b","a"]}]` advertised as
            // `["a","b"]` excludes the argument the plugin declared — so
            // these subtrees are skipped entirely. Their bytes come from
            // the manifest verbatim on every reconnect, so prefix-cache
            // stability is preserved (review 5915330170, the registry
            // companion to the sanitizer's enum/const instance-data rule).
            const INSTANCE_DATA_KEYS: &[&str] = &["enum", "const", "default"];
            // Sort `required` arrays (they are sets per JSON Schema spec).
            if let Some(Value::Array(req)) = map.get_mut("required") {
                sort_string_array(req);
            }
            // Sort `dependentRequired` sub-arrays.
            if let Some(Value::Object(deps)) = map.get_mut("dependentRequired") {
                for dep_value in deps.values_mut() {
                    if let Value::Array(arr) = dep_value {
                        sort_string_array(arr);
                    }
                }
            }
            // Recurse into every child value, leaving instance data alone.
            for (k, v) in map.iter_mut() {
                if INSTANCE_DATA_KEYS.contains(&k.as_str()) {
                    continue;
                }
                // A name→schema map (`dependentSchemas` included: its keys
                // are dependency TRIGGER names, and a trigger literally
                // named `properties` dispatched as the keyword walked the
                // dependency schema's const literal as a schema, reordering
                // its `required` — review 5987169388): the keys are names,
                // not keywords, so a
                // property literally named `enum` is NOT instance data — its
                // schema must still canonicalize (two schemas differing only
                // in `properties.enum.required` order otherwise keep
                // different bytes, regressing the cache-stability contract,
                // review 5986025535). The traversal is EXCLUSIVE: walk each
                // value as a schema node once, sort the map's own keys
                // explicitly, and continue — falling through to the generic
                // recursion re-entered the map and redispatched its names as
                // keywords (a property named `properties` descending into
                // its enum literal and reordering `required`, reviews
                // 5986731697/5986738054), and nested name maps were visited
                // twice per level (work exponential in depth).
                if matches!(
                    k.as_str(),
                    "properties"
                        | "patternProperties"
                        | "$defs"
                        | "definitions"
                        | "dependentSchemas"
                ) {
                    if let Some(name_map) = v.as_object_mut() {
                        let old = std::mem::take(name_map);
                        let mut entries: Vec<(String, Value)> = old.into_iter().collect();
                        entries.sort_by(|a, b| a.0.cmp(&b.0));
                        for (_name, prop_schema) in entries.iter_mut() {
                            canonicalize_schema(prop_schema);
                        }
                        for (name, prop_schema) in entries {
                            name_map.insert(name, prop_schema);
                        }
                        continue;
                    }
                }
                canonicalize_schema(v);
            }
            // Rebuild the map with sorted keys so serialization is deterministic.
            // serde_json::Map backed by IndexMap (preserve_order) doesn't have
            // drain(), so we swap to a temporary and rebuild.
            let old = std::mem::take(map);
            let mut entries: Vec<(String, Value)> = old.into_iter().collect();
            entries.sort_by(|a, b| a.0.cmp(&b.0));
            for (k, v) in entries {
                map.insert(k, v);
            }
        }
        Value::Array(arr) => {
            for v in arr.iter_mut() {
                canonicalize_schema(v);
            }
        }
        _ => {}
    }
}

/// Sort a JSON array of string values alphabetically in-place.
///
/// Non-string entries are left at the end in their original relative order.
fn sort_string_array(arr: &mut [Value]) {
    arr.sort_by(|a, b| match (a.as_str(), b.as_str()) {
        (Some(x), Some(y)) => x.cmp(y),
        (Some(_), None) => std::cmp::Ordering::Less,
        (None, Some(_)) => std::cmp::Ordering::Greater,
        (None, None) => std::cmp::Ordering::Equal,
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn sorts_required_array() {
        let mut schema = json!({
            "type": "object",
            "required": ["z", "a", "m"],
            "properties": {}
        });
        canonicalize_schema(&mut schema);
        assert_eq!(schema["required"], json!(["a", "m", "z"]));
    }

    #[test]
    fn equivalent_ordering_matches() {
        // Two schemas that differ only in field order and required order
        // must serialize to identical bytes.
        let mut a = json!({
            "required": ["b", "a"],
            "properties": {"x": {}, "y": {}},
            "type": "object"
        });
        let mut b = json!({
            "type": "object",
            "properties": {"y": {}, "x": {}},
            "required": ["a", "b"]
        });
        canonicalize_schema(&mut a);
        canonicalize_schema(&mut b);
        assert_eq!(
            serde_json::to_string(&a).unwrap(),
            serde_json::to_string(&b).unwrap(),
            "logically equivalent schemas must produce identical bytes"
        );
    }

    #[test]
    fn property_named_enum_still_canonicalizes() {
        // Review 5986025535 (P3): `enum` inside the properties map is a
        // property NAME, not the instance-data keyword. The name-blind skip
        // left such property schemas uncanonicalized, so two schemas
        // differing only in `properties.enum.required` order kept different
        // bytes. The map's values are walked as schema nodes; the
        // literal-data exclusions stay at schema-node level.
        let mut a = json!({
            "type": "object",
            "properties": {"enum": {"required": ["b", "a"], "type": "array"}}
        });
        let mut b = json!({
            "type": "object",
            "properties": {"enum": {"type": "array", "required": ["a", "b"]}}
        });
        canonicalize_schema(&mut a);
        canonicalize_schema(&mut b);
        assert_eq!(
            serde_json::to_string(&a).unwrap(),
            serde_json::to_string(&b).unwrap(),
            "schemas differing only in properties.enum.required order must match byte-for-byte"
        );
        assert_eq!(
            a["properties"]["enum"]["required"],
            json!(["a", "b"]),
            "the property schema's required order is canonicalized"
        );
    }

    #[test]
    fn keyword_named_property_keeps_its_enum_literal_verbatim() {
        // Reviews 5986731697/5986738054: a property literally named
        // `properties` is a NAME — falling through to the generic recursion
        // after the value walk re-entered the map, redispatched the name as
        // the keyword, and reordered the enum literal's `required`. The
        // traversal is exclusive: the literal stays byte-for-byte and the
        // name keys still sort.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "properties": {"enum": [{"required": ["b", "a"]}]}
            }
        });
        canonicalize_schema(&mut schema);
        assert_eq!(
            schema["properties"]["properties"]["enum"],
            json!([{"required": ["b", "a"]}]),
            "the enum literal under a property named `properties` must stay verbatim: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the canonicalized schema must compile");
        assert!(
            validator.is_valid(&json!({"properties": {"required": ["b", "a"]}})),
            "the original enum-admitted argument must stay valid: {schema}"
        );
        assert!(
            !validator.is_valid(&json!({"properties": {"required": ["a"]}})),
            "the reordered-admission value must stay rejected: {schema}"
        );
    }

    #[test]
    fn sorts_name_map_keys_without_keyword_dispatch() {
        // The name map's own keys (property names) sort deterministically;
        // sorting must not interpret them as keywords.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "zeta": {"type": "string"},
                "alpha": {"type": "string"}
            }
        });
        canonicalize_schema(&mut schema);
        let keys: Vec<&String> = schema["properties"]
            .as_object()
            .expect("map")
            .keys()
            .collect();
        assert_eq!(
            keys,
            vec!["alpha", "zeta"],
            "name-map keys sort for stable bytes"
        );
    }

    #[test]
    fn dependency_trigger_named_properties_keeps_dep_schema_const_verbatim() {
        // Review 5987169388: `dependentSchemas` is itself a name→schema
        // map, and its keys are dependency TRIGGER names. A trigger
        // literally named `properties` dispatched as the keyword, and the
        // dependency schema's const literal was walked as a schema — its
        // `required` reordered, rejecting the originally allowed value.
        let mut schema = json!({
            "type": "object",
            "properties": {"properties": {}, "required": {}},
            "dependentSchemas": {
                "properties": {
                    "const": {"properties": true, "required": ["b", "a"]}
                }
            }
        });
        canonicalize_schema(&mut schema);
        assert_eq!(
            schema["dependentSchemas"]["properties"]["const"],
            json!({"properties": true, "required": ["b", "a"]}),
            "the dependency schema's const literal must stay verbatim: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the canonicalized schema must compile");
        // The dependency applies when the instance carries the trigger key
        // `properties`, and its const constrains the WHOLE instance — so
        // the const-admitted instance is exactly the const value itself.
        assert!(
            validator.is_valid(&json!({
                "properties": true,
                "required": ["b", "a"]
            })),
            "the original const-admitted argument must stay valid: {schema}"
        );
        assert!(
            !validator.is_valid(&json!({
                "properties": true,
                "required": ["a"]
            })),
            "the reordered-admission value must stay rejected: {schema}"
        );
    }

    #[test]
    fn sorts_dependent_required() {
        let mut schema = json!({
            "type": "object",
            "dependentRequired": {
                "x": ["z", "a"],
                "y": ["m", "b"]
            }
        });
        canonicalize_schema(&mut schema);
        assert_eq!(schema["dependentRequired"]["x"], json!(["a", "z"]));
        assert_eq!(schema["dependentRequired"]["y"], json!(["b", "m"]));
    }

    #[test]
    fn recursive_into_properties() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "nested": {
                    "type": "object",
                    "required": ["z", "a"],
                    "properties": {}
                }
            }
        });
        canonicalize_schema(&mut schema);
        assert_eq!(
            schema["properties"]["nested"]["required"],
            json!(["a", "z"])
        );
    }

    #[test]
    fn preserves_non_required_array_order() {
        // Arrays that are not `required` or `dependentRequired` should
        // keep their semantic order (e.g. enum values, oneOf items).
        let mut schema = json!({
            "type": "string",
            "enum": ["z", "a", "m"]
        });
        canonicalize_schema(&mut schema);
        assert_eq!(schema["enum"], json!(["z", "a", "m"]));
    }

    #[test]
    fn handles_empty_schema() {
        let mut schema = json!({});
        canonicalize_schema(&mut schema);
        assert_eq!(schema, json!({}));
    }

    #[test]
    fn handles_deeply_nested() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "level1": {
                    "type": "object",
                    "properties": {
                        "level2": {
                            "type": "object",
                            "required": ["z", "a"]
                        }
                    }
                }
            }
        });
        canonicalize_schema(&mut schema);
        assert_eq!(
            schema["properties"]["level1"]["properties"]["level2"]["required"],
            json!(["a", "z"])
        );
    }

    #[test]
    fn key_order_is_alphabetical_after_canonicalize() {
        let mut schema = json!({
            "z_field": 1,
            "a_field": 2,
            "m_field": 3
        });
        canonicalize_schema(&mut schema);
        let keys: Vec<&str> = schema
            .as_object()
            .unwrap()
            .keys()
            .map(|s| s.as_str())
            .collect();
        assert_eq!(keys, vec!["a_field", "m_field", "z_field"]);
    }

    #[test]
    fn enum_literal_data_is_never_sorted_or_key_rebuilt() {
        // `enum`/`const`/`default` carry instance data: an object literal
        // under them is a permitted ARGUMENT, and a `required` key inside
        // it is plugin data. Sorting the array rewrote the only permitted
        // value — `{"required": ["b","a"]}` advertised as
        // `{"required": ["a","b"]}` excludes the argument the plugin
        // declared (review 5915330170) — so the canonicalizer must leave
        // these subtrees byte-identical while still sorting the schema's
        // own contract arrays.
        let mut schema = json!({
            "type": "object",
            "required": ["zeta", "alpha"],
            "properties": {
                "required": {
                    "type": "array",
                    "items": {"type": "string"}
                }
            },
            "enum": [
                {"required": ["b", "a"], "mode": {"nested": ["y", "x"]}},
                {"other": ["q", "p"]}
            ],
            "const": {"required": ["d", "c"]},
            "default": {"sorted": ["k", "j"]}
        });
        canonicalize_schema(&mut schema);
        assert_eq!(
            schema["required"],
            json!(["alpha", "zeta"]),
            "the schema's own required set is still canonicalized"
        );
        assert_eq!(
            schema["enum"][0]["required"],
            json!(["b", "a"]),
            "the enum literal's data array must keep its declared order"
        );
        assert_eq!(
            schema["enum"][0]["mode"]["nested"],
            json!(["y", "x"]),
            "nested data arrays inside the literal stay untouched"
        );
        assert_eq!(schema["enum"][1]["other"], json!(["q", "p"]));
        assert_eq!(schema["const"]["required"], json!(["d", "c"]));
        assert_eq!(schema["default"]["sorted"], json!(["k", "j"]));
    }
}
