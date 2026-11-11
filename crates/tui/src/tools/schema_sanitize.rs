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
    sanitize_inner(schema, false);
}

/// The shared transforms plus recursion. `inside_literal` marks subtrees that
/// are VALUES (enum members, `const`, `default`, `examples`) rather than
/// schemas: the transforms rewrite schema shapes, and applying them to a
/// literal value corrupts it — an enum member `{"type":"object"}` is a value
/// that happens to look like a schema, and injecting `properties:{}` into it
/// changes the set of values the model may send while `required` pruning
/// deletes keys of object-shaped defaults.
fn sanitize_inner(schema: &mut Value, inside_literal: bool) {
    if !inside_literal {
        collapse_nullable_unions(schema);
        inject_properties_on_bare_objects(schema);
        prune_dangling_required(schema);
        collapse_single_element_unions(schema);
    }
    // Recurse into all sub-schemas
    if let Some(obj) = schema.as_object_mut() {
        for (key, v) in obj.iter_mut() {
            let child_literal =
                inside_literal || matches!(key.as_str(), "enum" | "const" | "default" | "examples");
            sanitize_inner(v, child_literal);
        }
    } else if let Some(arr) = schema.as_array_mut() {
        for v in arr.iter_mut() {
            sanitize_inner(v, inside_literal);
        }
    }
}

/// Prepare a complete active tool set for DeepSeek strict function-calling.
///
/// Each tool is evaluated independently: compatible schemas are sanitized and
/// marked strict, while incompatible schemas remain unchanged and non-strict.
/// Optional properties removed to satisfy the strict contract are reported in
/// the tool description so the model knows not to send them. Returns `true`
/// only when every tool in the set can use strict mode.
///
/// Incompatible includes schemas the documented strict subset cannot
/// represent faithfully: root alternatives, required nullables (`nullable`
/// or a null-type union branch — `sanitize` collapses the common `X | null`
/// union into `nullable: true`, so the keyword is how a required nullable
/// survives to the gate), null-carrying `type` arrays / `const` / `enum`
/// members, structural or conditional keywords outside the documented subset
/// (the same family the Moonshot MFJS path fails closed on), a null `const`
/// or schema-form `dependencies` table (stripped before the gate could see
/// them), and a removal that would leave the tool callable only with an
/// empty object — an all-optional root, or a nested object whose every
/// property was optional. Such a tool keeps its original schema and ships
/// non-strict rather than carrying an undocumented keyword under a strict
/// flag or advertising parameters it can no longer accept. The gate runs on
/// the enforced shape, so a null-carrying shape under an *optional* property
/// is exempt: the removal already represents it faithfully.
///
/// Callers gate this pipeline on official DeepSeek-family routes
/// (`crate::config::route_is_official_deepseek_family`): the reshaping
/// follows DeepSeek's strict-mode documents and would strip keywords other
/// providers document (e.g. `minItems`/`maxItems` on OpenAI).
/// Tools the strict pipeline fails closed by name because no schema-level
/// analysis available to the reshaping can see the breakage: the schema
/// advertises one discriminating parameter as the only required property
/// while `execute` demands sibling parameters the schema leaves optional,
/// so after the optional-removal pass the tool ships `strict: true`
/// callable only with values its runtime rejects.
///
/// `update_goal` runtime-requires `evidence` and `verification` for
/// `status: "complete"`, `blocker` for `"blocked"`, `verification` for
/// `"not_achieved"`, and `advisory` for `"advisory"` (`tools/goal.rs`);
/// `send_later` runtime-requires `message` for `action: "schedule"` and
/// `trigger_id` for `"read"`/`"cancel"` (`tools/send_later.rs`);
/// `agents/message` and `agents/followup` runtime-require a target
/// (`agent_id`/`name`, `tools/subagent/coord.rs`) for every call. The
/// documented strict subset cannot express those per-value requirements —
/// `if`/`then` and root alternatives are outside it — and widening the
/// schema requirements would make non-strict callers send junk for every
/// other value, so the tools ship unchanged and non-strict instead.
/// Per-action schema honesty would retire this list. The full-catalog
/// census test (core/engine/tests.rs) pins that nothing outside this list
/// reduces to an unusable strict shape.
const STRICT_UNUSABLE_TOOLS: &[&str] = &[
    "update_goal",
    "send_later",
    "agents/message",
    "agents/followup",
];

/// Tools that stay strict but lose specific capabilities to the
/// optional-removal pass: some actions runtime-require parameters the
/// strict schema no longer accepts, so those calls are rejected by the
/// runtime no matter what the model sends. The generic removal note only
/// says the parameters "must not be sent" — for these tools that sentence
/// points away from values the model actually needs, so an honest
/// per-tool sentence is appended naming what stopped working. Keep this
/// in lockstep with the full-catalog census manifest.
const STRICT_REMOVAL_CONTEXT_NOTES: &[(&str, &str)] = &[
    (
        "Git",
        "In strict mode `show` and `blame` are unavailable: they require \
         `rev`/`path`, which the strict schema no longer accepts. Use \
         `status`, `diff`, or `log`, or the shell tool for exact revisions.",
    ),
    (
        "lsp",
        "In strict mode `definition` and `references` are unavailable: they \
         require `line`. `read_lints`, `symbols`, and `diagnostics` still work.",
    ),
    (
        "pandoc_convert",
        "In strict mode binary target formats are unavailable: they require \
         `output_path`. Text targets still convert and return inline.",
    ),
    (
        "remember",
        "In strict mode only `append` is available: `revise` and `retire` \
         require `replaces`/`evidence`, which the strict schema no longer accepts.",
    ),
    (
        "Bash",
        "In strict mode the sandbox-escalation retry is unavailable: \
         `sandbox_permissions` and `justification` are no longer accepted, \
         so a sandbox-denied command cannot be escalated.",
    ),
    (
        "bash",
        "In strict mode the sandbox-escalation retry is unavailable: \
         `sandbox_permissions` and `justification` are no longer accepted, \
         so a sandbox-denied command cannot be escalated.",
    ),
];

/// Cap for the removal note. Names are schema-controlled (hostile MCP
/// schemas reach the pipeline verbatim) and the note joins the
/// model-facing description sent on every request, so both the name count
/// and each name's length are bounded before assembly.
const STRICT_NOTE_MAX_NAMES: usize = 20;
const STRICT_NOTE_MAX_NAME_CHARS: usize = 120;

/// Structural and conditional keywords outside the documented strict
/// subset. Their constraints cannot be expressed by the reshaping — the
/// same family the Moonshot MFJS path fails closed on — so a schema
/// relying on one ships unchanged and non-strict. Shared between the
/// composition gate and the test loop that pins each keyword so the two
/// lists cannot drift.
const STRICT_UNREPRESENTABLE_KEYWORDS: &[&str] = &[
    "not",
    "if",
    "then",
    "else",
    "contains",
    "prefixItems",
    "propertyNames",
    "unevaluatedProperties",
    "unevaluatedItems",
];

pub fn prepare_tools_for_strict_mode(tools: &mut [Tool]) -> bool {
    let mut all_strict = true;
    for tool in tools {
        if STRICT_UNUSABLE_TOOLS.contains(&tool.name.as_str()) {
            // The runtime demands parameters the schema marks optional (see
            // the list above); the removal pass would leave the tool strict
            // with only its discriminator. Ship unchanged and non-strict.
            tool.strict = None;
            all_strict = false;
            continue;
        }
        if strict_schema_supported(&tool.input_schema) {
            // Count the hybrid-map narrowings on the authored shape before
            // the enforce pass overwrites them, so the note can say so.
            let narrowed_maps = count_strict_map_narrowings(&tool.input_schema);
            let original_schema = tool.input_schema.clone();
            let removed_optionals = sanitize_for_strict(&mut tool.input_schema);
            // The strict contract removes every property the schema left
            // optional. A tool whose whole root property set was optional is
            // left with no way to pass any parameter while its runtime still
            // demands them (action-discriminated tools mark everything
            // optional and dispatch on the values `execute` re-reads) — that
            // shape cannot serve the tool's contract under a strict flag, so
            // it ships unchanged and non-strict, the same fail-closed rule
            // as required nullables. No removal note either: nothing was
            // removed from the schema that ships.
            let emptied = tool
                .input_schema
                .get("properties")
                .and_then(Value::as_object)
                .is_some_and(Map::is_empty)
                && !removed_optionals.is_empty();
            if emptied {
                tool.input_schema = original_schema;
                tool.strict = None;
                all_strict = false;
                continue;
            }
            if !removed_optionals.is_empty() || narrowed_maps > 0 {
                let note = strict_removal_note(&removed_optionals, narrowed_maps);
                let context = STRICT_REMOVAL_CONTEXT_NOTES
                    .iter()
                    .find(|(name, _)| name == &tool.name)
                    .map(|(_, context)| context);
                let description = tool.description.trim();
                let note = match context {
                    Some(context) => format!("{note}\n\n{context}"),
                    None => note,
                };
                tool.description = if description.is_empty() {
                    note
                } else {
                    format!("{description}\n\n{note}")
                };
            }
            tool.strict = Some(true);
        } else {
            tool.strict = None;
            all_strict = false;
        }
    }
    all_strict
}

/// Assemble the model-facing removal note. Names are backticked (they are
/// schema-controlled and must read as data, not prose), individually
/// truncated, and capped in count — a hostile schema may carry arbitrarily
/// many long names, and this text ships on every request.
fn strict_removal_note(removed: &[String], narrowed_maps: usize) -> String {
    let mut note = String::from(
        "Strict mode: these optional parameters were removed from the schema and \
         must not be sent:",
    );
    let shown = removed.len().min(STRICT_NOTE_MAX_NAMES);
    let parts: Vec<String> = removed
        .iter()
        .take(shown)
        .map(|name| {
            let truncated: String = name.chars().take(STRICT_NOTE_MAX_NAME_CHARS).collect();
            format!("`{truncated}`")
        })
        .collect();
    note.push_str(&format!(" {}.", parts.join(", ")));
    if removed.len() > shown {
        note.push_str(&format!(
            " (and {} more removed parameters)",
            removed.len() - shown
        ));
    }
    if narrowed_maps > 0 {
        note.push_str(&format!(
            " Extra-key acceptance (`additionalProperties`) was also narrowed to `false` \
             on {narrowed_maps} object schema{} under the strict contract.",
            if narrowed_maps == 1 { "" } else { "s" }
        ));
    }
    note
}

/// Count object schemas that carry named properties beside a schema-form or
/// `true` `additionalProperties`: the enforce pass overwrites that keyword
/// with `false` (the documented strict contract mandates it), silently
/// removing extra-key acceptance — the note says so instead.
fn count_strict_map_narrowings(schema: &Value) -> usize {
    let Some(obj) = schema.as_object() else {
        return schema
            .as_array()
            .map(|values| {
                values
                    .iter()
                    .map(count_strict_map_narrowings)
                    .sum::<usize>()
            })
            .unwrap_or(0);
    };
    let mut count = 0;
    if is_object_schema(obj)
        && obj
            .get("properties")
            .and_then(Value::as_object)
            .is_some_and(|properties| !properties.is_empty())
        && obj.get("additionalProperties").is_some_and(|additional| {
            !additional.is_boolean() || additional.as_bool() != Some(false)
        })
    {
        count += 1;
    }
    for (key, value) in obj {
        match key.as_str() {
            "properties" | "$defs" | "definitions" | "$def" => {
                if let Some(named_schemas) = value.as_object() {
                    count += named_schemas
                        .values()
                        .map(count_strict_map_narrowings)
                        .sum::<usize>();
                }
            }
            "enum" | "const" | "default" | "examples" => {}
            _ => count += count_strict_map_narrowings(value),
        }
    }
    count
}

/// Sanitize a schema for DeepSeek strict function-calling.
///
/// This extends the general sanitizer with the official strict-mode object
/// rules: every object must set `additionalProperties: false`, and every
/// property must be listed in `required`. A property the schema does not
/// require cannot satisfy "every property listed in `required`" honestly —
/// marking it `nullable:true` would inject a keyword outside the documented
/// strict subset — so it is removed from `properties` instead, and its name
/// is returned here so the caller can surface the removal.
///
/// Returns the removed optional property names, sorted and deduplicated.
pub fn sanitize_for_strict(schema: &mut Value) -> Vec<String> {
    sanitize(schema);
    let mut removed_optionals = Vec::new();
    enforce_strict_subset(schema, &mut removed_optionals);
    removed_optionals.sort();
    removed_optionals.dedup();
    removed_optionals
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
    // A root map shape — no properties of its own beside a `true` or
    // schema-valued `additionalProperties` — collapses under the enforce
    // pass into `additionalProperties: false` plus an empty `properties`
    // map: `strict: true` callable only with `{}` while the tool's
    // runtime reads arbitrary keys. The authored no-argument roots that
    // stay strict are the ones whose `additionalProperties` is absent or
    // already `false` (tools that take no values at all); the same shape
    // fails closed one level down through the gate's nested rule, so the
    // root fails closed here, while the authored keyword shape is still
    // visible. A node the enforce pass would not normalize at all keeps
    // the keyword and is caught by the detector below.
    if normalized
        .get("properties")
        .and_then(Value::as_object)
        .is_none_or(Map::is_empty)
        && normalized
            .get("additionalProperties")
            .is_some_and(|additional| {
                !additional.is_boolean() || additional.as_bool() == Some(true)
            })
    {
        return false;
    }
    // The enforce pass strips some constraints before the compatibility
    // gate below can judge them: a null `const` (dropped outright) and a
    // schema-form draft-7 `dependencies` table (dropped with the name-table
    // form) carry requirements the shipped schema would no longer express.
    // They are detected here, on the sanitized shape, with the
    // optional-removal in view — an occurrence under an optional property
    // is moot because the removal takes the whole property.
    if has_unrepresentable_stripped_constraint(&normalized, true) {
        return false;
    }
    // Gate the shape that would actually ship: the enforce pass removes
    // optional properties, so a null-carrying shape under an optional
    // property is faithfully represented by that removal and no longer
    // forces the tool non-strict, while the same shape under a required
    // property still does.
    enforce_strict_subset(&mut normalized, &mut Vec::new());
    !has_strict_incompatible_composition(&normalized, true)
}

/// Detect the constraints the enforce pass strips before the compatibility
/// gate can judge them. `ships` reports whether the node survives the
/// optional-removal pass — only then does its constraint still reach the
/// wire. A null `const` is a required nullable wearing another keyword, and
/// a schema-form `dependencies` value is semantically `dependentSchemas`
/// (which the gate fails closed), so its draft-7 spelling fails closed too
/// rather than shipping strict with the cross-schema requirement silently
/// gone; the name-table form is `dependentRequired`, whose strip only
/// widens and stays a strip.
fn has_unrepresentable_stripped_constraint(schema: &Value, ships: bool) -> bool {
    let Some(obj) = schema.as_object() else {
        return schema.as_array().is_some_and(|values| {
            values
                .iter()
                .any(|value| has_unrepresentable_stripped_constraint(value, ships))
        });
    };
    if ships && obj.get("const").is_some_and(Value::is_null) {
        return true;
    }
    if ships
        && obj
            .get("dependencies")
            .and_then(Value::as_object)
            .is_some_and(|table| table.values().any(Value::is_object))
    {
        return true;
    }
    // The enforce pass normalizes `additionalProperties` to `false` only
    // on schemas it treats as objects (`type: "object"` or a `properties`
    // map). On any other node the authored keyword ships verbatim under a
    // strict flag — and a `true` or schema-valued `additionalProperties`
    // is map semantics the documented subset does not describe, so the
    // node fails closed instead.
    if ships
        && !is_object_schema(obj)
        && obj.get("additionalProperties").is_some_and(|additional| {
            !additional.is_boolean() || additional.as_bool() == Some(true)
        })
    {
        return true;
    }
    // A set, not a Vec: this walker runs per property name, and a Vec scan
    // would make each name cost the whole `required` list.
    let required: std::collections::HashSet<String> =
        required_names(obj).into_iter().collect();
    for (key, value) in obj {
        match key.as_str() {
            "properties" | "$defs" | "definitions" | "$def" => {
                if let Some(named_schemas) = value.as_object() {
                    for (name, named_schema) in named_schemas {
                        // Definitions ship whenever their `$ref` resolves,
                        // so they are judged without the optional-property
                        // exemption; only a property the schema leaves
                        // optional is removed wholesale.
                        let ships_child = if key == "properties" {
                            required.contains(name.as_str())
                        } else {
                            // A definitions map nested inside a subtree the
                            // optional-removal takes away ships no more than
                            // the subtree around it: inherit the parent's
                            // shipping status instead of judging every
                            // definition as live.
                            ships
                        };
                        if has_unrepresentable_stripped_constraint(named_schema, ships_child) {
                            return true;
                        }
                    }
                }
            }
            "enum" | "const" | "default" | "examples" => {}
            _ => {
                if has_unrepresentable_stripped_constraint(value, ships) {
                    return true;
                }
            }
        }
    }
    false
}

fn has_strict_incompatible_composition(schema: &Value, is_root: bool) -> bool {
    if let Some(obj) = schema.as_object() {
        if obj.contains_key("oneOf")
            || obj.contains_key("allOf")
            || obj.contains_key("dependentSchemas")
            // `nullable` is outside the documented strict subset — the
            // supported-type list has no null and the documents never
            // mention the keyword. `sanitize` collapses the common
            // `X | null` union into `nullable: true`, so by the time a
            // schema reaches here the keyword is the surviving shape of a
            // required nullable; such a tool falls back to non-strict
            // instead of shipping an undocumented keyword under a strict
            // flag.
            || obj.contains_key("nullable")
        {
            return true;
        }
        if is_root && obj.contains_key("anyOf") {
            return true;
        }
        // A union that still carries a null branch after the collapse pass
        // (three or more alternatives) has the same undocumented-null
        // problem.
        if obj
            .get("anyOf")
            .and_then(Value::as_array)
            .is_some_and(|branches| branches.iter().any(is_null_type))
        {
            return true;
        }
        // The documented subset's union mechanism is `anyOf`; a `type` array
        // is a draft-4-style union the documents never describe, and `null`
        // is not a supported type anywhere — neither bare nor as an array
        // member. Either shape cannot be represented faithfully under a
        // strict flag.
        match obj.get("type") {
            Some(Value::String(schema_type)) if schema_type == "null" => return true,
            Some(Value::Array(_)) => return true,
            _ => {}
        }
        // An enum or const that accepts null is a required nullable wearing
        // another keyword: the property must be sent and can be null.
        if obj
            .get("enum")
            .and_then(Value::as_array)
            .is_some_and(|members| members.iter().any(Value::is_null))
        {
            return true;
        }
        if obj.get("const").is_some_and(Value::is_null) {
            return true;
        }
        // Keywords in `STRICT_UNREPRESENTABLE_KEYWORDS` carry constraints
        // the reshaping cannot express — the same family the Moonshot MFJS
        // path fails closed on. A schema relying on one ships unchanged and
        // non-strict rather than reshaped-and-strict.
        if obj
            .keys()
            .any(|key| STRICT_UNREPRESENTABLE_KEYWORDS.contains(&key.as_str()))
        {
            return true;
        }
        if obj
            .get("items")
            .is_some_and(|items| matches!(items, Value::Array(_)))
        {
            return true;
        }
        // A nested object left with no properties by the optional-removal
        // pass ships callable only with `{}` while the tool's runtime may
        // still read the removed members — the same degenerate shape the
        // root-emptied check in `prepare_tools_for_strict_mode` fails
        // closed, one level down (the root keeps its own check there so
        // authored no-argument roots stay strict). A bare
        // `{"type":"object"}` node gets an empty `properties` injected by
        // `sanitize`, and under `additionalProperties: false` that shape
        // cannot carry a value either. `$defs` entries are judged here too:
        // the gate does not track `$ref` reachability, so a definition that
        // could not serve a resolved reference also fails the tool closed.
        if !is_root
            && is_object_schema(obj)
            && obj
                .get("properties")
                .and_then(Value::as_object)
                .is_some_and(Map::is_empty)
        {
            return true;
        }
        // Walk children like the sibling walkers do: `properties`,
        // `$defs`, `definitions`, and `$def` map names to schemas, and
        // `enum`/`const`/`default`/`examples` hold literal values — only
        // their values are schemas. Judging the name maps themselves as
        // schemas made the gate fire on names alone: a required property
        // named after a keyword (or a definition named `not`) failed the
        // tool closed on its name, and object-shaped `enum` members or
        // `default` values failed it on their shape.
        for (key, value) in obj {
            match key.as_str() {
                "properties" | "$defs" | "definitions" | "$def" => {
                    if let Some(named_schemas) = value.as_object() {
                        if named_schemas.values().any(|named_schema| {
                            has_strict_incompatible_composition(named_schema, false)
                        }) {
                            return true;
                        }
                    }
                }
                "enum" | "const" | "default" | "examples" => {}
                _ => {
                    if has_strict_incompatible_composition(value, false) {
                        return true;
                    }
                }
            }
        }
        return false;
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
    // A set, not a Vec: schemas reach this per request from every active
    // tool, and a Vec turns each `required` entry into a full scan.
    let known_keys: std::collections::HashSet<String> = obj
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
            .is_some_and(|k| known_keys.contains(k))
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

fn enforce_strict_subset(schema: &mut Value, removed_optionals: &mut Vec<String>) {
    if let Some(obj) = schema.as_object_mut() {
        strip_unsupported_strict_keywords(obj);
        if is_object_schema(obj) {
            // A set, not a Vec: the membership test runs once per property,
            // and a Vec scan would make each property cost the whole list.
            let originally_required: std::collections::HashSet<String> =
                required_names(obj).into_iter().collect();
            let properties = ensure_properties_object(obj);
            let mut property_names: Vec<String> = properties.keys().cloned().collect();
            property_names.sort();
            // The strict contract is "every property is required". A property
            // the tool left optional would have to carry `nullable: true` to
            // keep accepting omission — a keyword outside the documented
            // strict subset — so it is removed instead and reported through
            // `removed_optionals` (see `sanitize_for_strict`).
            let removed_here: Vec<String> = property_names
                .iter()
                .filter(|name| !originally_required.contains(name.as_str()))
                .cloned()
                .collect();
            for name in &removed_here {
                properties.shift_remove(name);
            }
            removed_optionals.extend(removed_here);
            let mut remaining: Vec<String> = properties.keys().cloned().collect();
            remaining.sort();
            obj.insert(
                "required".into(),
                Value::Array(remaining.into_iter().map(Value::String).collect()),
            );
            obj.insert("additionalProperties".into(), Value::Bool(false));
        }

        // Recurse into sub-schemas, but never into the map-shaped values
        // that hold named schemas or literal values. Walking those as if
        // they were schemas applied the keyword strips and the object rules
        // to property/definition NAMES: a tool with a property named
        // `const`, `properties`, or `patternProperties` came out corrupted
        // (renamed, erased, or dangling in `required`) while marked strict.
        // Property and definition schemas are visited through their name
        // maps, and the literal keywords are left alone — their members are
        // values, not schemas.
        for (key, value) in obj.iter_mut() {
            match key.as_str() {
                // `$def` is the singular spelling the DeepSeek documents
                // themselves use for the definitions map; walk it like the
                // standard `$defs`/`definitions` so a definition named after
                // a stripped keyword is not deleted or renamed when its map
                // is visited as if it were a schema.
                "properties" | "$defs" | "definitions" | "$def" => {
                    if let Some(named_schemas) = value.as_object_mut() {
                        for named_schema in named_schemas.values_mut() {
                            enforce_strict_subset(named_schema, removed_optionals);
                        }
                    }
                }
                "enum" | "const" | "default" | "examples" => {}
                _ => enforce_strict_subset(value, removed_optionals),
            }
        }
    } else if let Some(arr) = schema.as_array_mut() {
        for value in arr {
            enforce_strict_subset(value, removed_optionals);
        }
    }
}

fn strip_unsupported_strict_keywords(obj: &mut Map<String, Value>) {
    obj.remove("patternProperties");
    // Conditional dependency tables are likewise outside the documented
    // strict subset — draft-7 `dependencies` covers both forms (a
    // name table like `dependentRequired`, a schema map like
    // `dependentSchemas`); dropping one only widens what the endpoint
    // accepts.
    obj.remove("dependentRequired");
    obj.remove("dependencies");
    // Uniqueness and the property/containment count bounds are not in the
    // documented subset either. With every property required and
    // `additionalProperties: false` the property-count bounds are redundant
    // or contradictory anyway, `minContains`/`maxContains` are meaningless
    // without `contains` (which the compatibility gate rejects outright),
    // and dropping a bound only widens what the endpoint accepts.
    obj.remove("uniqueItems");
    obj.remove("minProperties");
    obj.remove("maxProperties");
    obj.remove("minContains");
    obj.remove("maxContains");
    // The strict documents support `const` for number/integer schemas only.
    // A non-numeric const moves onto the documented `enum` keyword with its
    // value as the single member; a numeric const stays put, and a non-numeric
    // const beside an existing enum yields to it. A `const: null` is dropped
    // outright — null is not a supported type, and landing it on `enum` would
    // ship a null member (the compatibility gate already fails a tool closed
    // for a null const; this is the defense for direct `sanitize_for_strict`
    // calls). `default` stays verbatim (the documents show it, and list no
    // ban).
    if let Some(const_value) = obj.remove("const") {
        if !const_value.is_null() {
            if const_value.is_number() {
                obj.insert("const".into(), const_value);
            } else if !obj.contains_key("enum") {
                obj.insert("enum".into(), Value::Array(vec![const_value]));
            }
        }
    }
    // `format` is documented with an enumerated set (email, hostname,
    // ipv4, ipv6, uuid); any other value ships a constraint outside that
    // list, so it is dropped — dropping a format only widens what the
    // endpoint accepts. A draft-4 boolean `exclusiveMinimum`/`Maximum`
    // is likewise outside the documented numeric form.
    if obj
        .get("format")
        .and_then(Value::as_str)
        .is_some_and(|format| !matches!(format, "email" | "hostname" | "ipv4" | "ipv6" | "uuid"))
    {
        obj.remove("format");
    }
    for key in ["exclusiveMinimum", "exclusiveMaximum"] {
        if obj.get(key).is_some_and(Value::is_boolean) {
            obj.remove(key);
        }
    }
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
        if groups.len() >= 2 {
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

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(schema["additionalProperties"], false);
        assert_eq!(schema["required"], json!(["name"]));
        assert!(
            schema["properties"].get("count").is_none(),
            "an optional property is removed, not nullable-marked: {schema}"
        );
        assert_eq!(removed, vec!["count".to_string()]);
        assert!(schema["properties"]["name"].get("nullable").is_none());
    }

    #[test]
    fn strict_sanitize_removes_optional_properties_and_reports_them() {
        // The strict contract is "every property is required". The strict
        // documents' supported keyword set has no `nullable`, so keeping an
        // optional property would need an undocumented keyword to keep
        // accepting omission; the property is removed instead and its name
        // reported so the caller can tell the model. Nested objects are
        // treated the same way, recursively.
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

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(removed, vec!["max_lines", "start_line", "trim"]);
        assert_eq!(schema["required"], json!(["options", "path"]));
        assert!(schema["properties"].get("start_line").is_none());
        assert!(schema["properties"].get("max_lines").is_none());
        assert!(schema["properties"]["path"].get("nullable").is_none());
        assert!(schema["properties"]["options"].get("nullable").is_none());
        assert_eq!(
            schema["properties"]["options"]["required"],
            json!(["encoding"])
        );
        assert!(
            schema["properties"]["options"]["properties"]
                .get("trim")
                .is_none()
        );
        assert!(
            schema["properties"]["options"]["properties"]["encoding"]
                .get("nullable")
                .is_none()
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

        let removed = sanitize_for_strict(&mut schema);

        // Nothing was required at the root, so `outer` is removed with its
        // whole subtree — a removed property's internals go with it, so only
        // the name at the level where removal happened is reported.
        assert_eq!(removed, vec!["outer"]);
        assert_eq!(schema["required"], json!([]));
        assert!(schema["properties"].get("outer").is_none());
        assert_eq!(schema["additionalProperties"], false);
    }

    #[test]
    fn strict_sanitize_lands_non_numeric_const_on_the_documented_enum() {
        // The strict documents support `const` for number/integer schemas
        // only. A string/bool const moves onto the documented `enum` keyword
        // with its value as the single member; a numeric const stays `const`.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "kind": {"type": "string", "const": "fast"},
                "level": {"type": "integer", "const": 3},
                "flag": {"type": "boolean", "const": true}
            },
            "required": ["kind", "level", "flag"]
        });

        sanitize_for_strict(&mut schema);

        assert!(schema["properties"]["kind"].get("const").is_none());
        assert_eq!(schema["properties"]["kind"]["enum"], json!(["fast"]));
        assert_eq!(schema["properties"]["level"]["const"], 3);
        assert!(schema["properties"]["flag"].get("const").is_none());
        assert_eq!(schema["properties"]["flag"]["enum"], json!([true]));
    }

    #[test]
    fn strict_sanitize_non_numeric_const_yields_to_an_existing_enum() {
        // A non-numeric const beside an existing enum yields to the enum: the
        // const is dropped and the enum passes through verbatim, whether the
        // const value agrees with the enum or contradicts it.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "matching": {"type": "string", "const": "fast", "enum": ["fast", "slow"]},
                "contradicting": {"type": "string", "const": "turbo", "enum": ["fast", "slow"]}
            },
            "required": ["matching", "contradicting"]
        });

        sanitize_for_strict(&mut schema);

        assert!(schema["properties"]["matching"].get("const").is_none());
        assert_eq!(
            schema["properties"]["matching"]["enum"],
            json!(["fast", "slow"])
        );
        assert!(schema["properties"]["contradicting"].get("const").is_none());
        assert_eq!(
            schema["properties"]["contradicting"]["enum"],
            json!(["fast", "slow"])
        );
    }

    #[test]
    fn strict_sanitize_removes_unsupported_string_and_array_bounds() {
        // Every property is required so the nodes survive the optional-removal
        // pass; this test pins the bound keyword strip specifically.
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
            },
            "required": ["items", "name", "score"]
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
                        "query": {"type": "string"},
                        "index": {"type": "string", "enum": ["global", "workspace"]}
                    },
                    "required": ["index"]
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
        assert_eq!(
            tools[0].input_schema["required"],
            json!(["index"]),
            "the optional query is removed, not nullable-marked"
        );
        assert!(tools[0].input_schema["properties"].get("query").is_none());
        assert!(
            tools[0]
                .description
                .contains("optional parameters were removed"),
            "the removal must be surfaced in the tool description: {}",
            tools[0].description
        );
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
        // A required property stays so the removal does not empty the tool
        // (an all-optional root fails closed instead — see
        // `strict_mode_fails_closed_when_removal_empties_the_tool`); the
        // optional one is removed with a note.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "lookup".to_string(),
            description: "Lookup".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "index": {"type": "string", "enum": ["global", "workspace"]}
                },
                "required": ["index"]
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(tools[0].input_schema["required"], json!(["index"]));
        assert!(tools[0].input_schema["properties"].get("query").is_none());
        assert_eq!(tools[0].input_schema["additionalProperties"], false);
        assert!(
            tools[0].description.contains("must not be sent: `query`."),
            "the note names the removed optional, quoted as data: {}",
            tools[0].description
        );
    }

    #[test]
    fn strict_mode_note_replaces_an_empty_tool_description() {
        // A tool with no description gets the removal note as its whole
        // description instead of an empty prefix.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "lookup".to_string(),
            description: String::new(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "index": {"type": "string", "enum": ["global", "workspace"]}
                },
                "required": ["index"]
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert!(
            tools[0].description.starts_with("Strict mode:"),
            "the note must replace an empty description: {:?}",
            tools[0].description
        );
        assert!(
            tools[0].description.contains("query"),
            "the note names the removed optional: {}",
            tools[0].description
        );
    }

    #[test]
    fn strict_mode_fails_closed_when_removal_empties_the_tool() {
        // A tool whose whole root property set is optional loses every
        // parameter to the strict contract while its runtime still demands
        // them (action-discriminated tools mark everything optional and
        // dispatch on the values `execute` re-reads). That shape cannot
        // serve the tool under a strict flag, so it ships unchanged and
        // non-strict — with no removal note, because nothing was removed
        // from the schema that ships.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "tasks".to_string(),
            description: "Durable tasks".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "action": {"type": "string", "enum": ["create", "read", "cancel"]},
                    "prompt": {"type": "string"}
                },
                "required": []
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["action"]["enum"],
            json!(["create", "read", "cancel"])
        );
        assert_eq!(tools[0].input_schema["required"], json!([]));
        assert_eq!(
            tools[0].input_schema["properties"].get("prompt"),
            Some(&json!({"type": "string"}))
        );
        assert!(tools[0].input_schema.get("additionalProperties").is_none());
        assert_eq!(tools[0].description, "Durable tasks");
    }

    #[test]
    fn strict_mode_fails_closed_on_type_array_unions() {
        // The documented subset's union mechanism is `anyOf`; a `type`
        // array is a draft-4-style union the documents never describe, and
        // `null` is not a supported type anywhere — neither bare nor as an
        // array member. Either shape ships unchanged and non-strict instead
        // of carrying an undocumented construct under a strict flag.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "search".to_string(),
            description: "Search".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "mode": {"type": ["string", "null"]},
                    "level": {"type": ["integer", "string"]}
                },
                "required": ["mode", "level"]
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["mode"],
            json!({"type": ["string", "null"]})
        );
        assert_eq!(
            tools[0].input_schema["properties"]["level"],
            json!({"type": ["integer", "string"]})
        );
        assert_eq!(tools[0].description, "Search");
    }

    #[test]
    fn strict_mode_fails_closed_on_a_bare_null_type() {
        // A bare `{"type":"null"}` is a required nullable wearing another
        // keyword: the property must be sent and can be null, and null is
        // not a supported type anywhere in the documented subset. The tool
        // stays non-strict with its original schema. (Pinned per arm: a
        // shared fixture let a single-arm regression pass.)
        let mut tools = vec![test_tool(
            "probe",
            json!({
                "type": "object",
                "properties": {
                    "empty": {"type": "null"}
                },
                "required": ["empty"]
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["empty"],
            json!({"type": "null"})
        );
        assert_eq!(tools[0].description, "probe");
    }

    #[test]
    fn strict_mode_fails_closed_on_a_null_const() {
        // A `const: null` is the same required nullable in another keyword:
        // the strip drops it outright for direct callers, so the gate must
        // catch it before the strip or the tool would ship strict without
        // the constraint it advertises.
        let mut tools = vec![test_tool(
            "probe",
            json!({
                "type": "object",
                "properties": {
                    "fixed": {"const": null}
                },
                "required": ["fixed"]
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["fixed"],
            json!({"const": null})
        );
        assert_eq!(tools[0].description, "probe");
    }

    #[test]
    fn strict_mode_fails_closed_on_a_null_enum_member() {
        // An enum with a null member is a required nullable wearing the
        // documented `enum` keyword: landing on it would ship a null member
        // for a type the documents never support.
        let mut tools = vec![test_tool(
            "probe",
            json!({
                "type": "object",
                "properties": {
                    "choice": {"enum": [null, "a"]}
                },
                "required": ["choice"]
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["choice"],
            json!({"enum": [null, "a"]})
        );
        assert_eq!(tools[0].description, "probe");
    }

    #[test]
    fn strict_mode_keeps_strict_when_null_shapes_sit_under_optional_properties() {
        // The gate judges the enforced shape: a null-carrying shape under
        // an OPTIONAL property is faithfully represented by that removal,
        // so the tool stays strict and the property is removed with the
        // standard note instead of failing the whole tool closed. The same
        // shapes under a required property keep failing the tool closed
        // (see the per-arm tests above).
        let mut tools = vec![test_tool(
            "probe",
            json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "opt_union": {"anyOf": [{"type": "string"}, {"type": "null"}]},
                    "opt_nullable": {"type": "string", "nullable": true},
                    "opt_null_type": {"type": "null"},
                    "opt_null_const": {"const": null},
                    "opt_null_enum": {"enum": [null, "a"]},
                    "opt_type_array": {"type": ["string", "null"]}
                },
                "required": ["name"]
            }),
        )];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(tools[0].input_schema["required"], json!(["name"]));
        let properties = tools[0].input_schema["properties"].as_object().unwrap();
        assert_eq!(
            properties.len(),
            1,
            "only the required property ships: {properties:?}"
        );
        assert!(properties.contains_key("name"));
        assert!(
            tools[0].description.contains("opt_null_const"),
            "the note names the removed optional: {}",
            tools[0].description
        );
    }

    #[test]
    fn strict_mode_fails_closed_on_structural_and_conditional_keywords() {
        // Structural and conditional keywords outside the documented subset
        // carry constraints the reshaping cannot express — the same family
        // the Moonshot MFJS path fails closed on. Tuple-form `items` is
        // likewise a draft construct the documents never describe.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "check".to_string(),
            description: "Check".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "pattern": {"type": "string"},
                    "tags": {
                        "type": "array",
                        "prefixItems": [{"type": "string"}, {"type": "string"}]
                    }
                },
                "required": ["pattern", "tags"]
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert!(
            tools[0].input_schema["properties"]["tags"]
                .get("prefixItems")
                .is_some(),
            "the schema ships unchanged: {}",
            tools[0].input_schema
        );
    }

    #[test]
    fn strict_mode_fails_closed_on_each_structural_keyword() {
        // Each member of `STRICT_UNREPRESENTABLE_KEYWORDS` is pinned
        // individually — a shared fixture let a single-arm regression (a
        // typo in one keyword arm) pass silently. Iterating the same
        // constant the gate checks keeps the two lists from drifting.
        for &keyword in STRICT_UNREPRESENTABLE_KEYWORDS {
            let mut target = serde_json::Map::new();
            target.insert("type".into(), json!("string"));
            target.insert(keyword.into(), json!(true));
            let mut tools = vec![test_tool(
                "check",
                json!({
                    "type": "object",
                    "properties": {"target": Value::Object(target)},
                    "required": ["target"]
                }),
            )];

            assert!(
                !prepare_tools_for_strict_mode(&mut tools),
                "the {keyword} keyword must fail the tool closed"
            );
            assert_eq!(tools[0].strict, None, "keyword: {keyword}");
            assert!(
                tools[0].input_schema["properties"]["target"]
                    .get(keyword)
                    .is_some(),
                "the {keyword} keyword ships unchanged"
            );
        }
    }

    #[test]
    fn strict_mode_keeps_strict_when_a_required_property_is_named_after_a_keyword() {
        // The gate walks name maps as name maps: a required property named
        // after a structural keyword (or `nullable`) is a name, not a
        // keyword occurrence, and must not fail the tool closed on it.
        let mut tools = vec![test_tool(
            "search",
            json!({
                "type": "object",
                "properties": {
                    "needle": {"type": "string"},
                    "contains": {
                        "type": "string",
                        "description": "substring the result must contain"
                    }
                },
                "required": ["needle", "contains"]
            }),
        )];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(
            tools[0].input_schema["required"],
            json!(["contains", "needle"])
        );
    }

    #[test]
    fn strict_mode_keeps_strict_when_a_definition_is_named_after_a_keyword() {
        // `$defs` is a name map too: a definition named `not` ships when its
        // `$ref` resolves, and its name is not a keyword occurrence. A
        // definition whose SCHEMA relies on a structural keyword still fails
        // the tool closed.
        let mut named = vec![test_tool(
            "cite",
            json!({
                "type": "object",
                "properties": {"span": {"$ref": "#/$defs/not"}},
                "required": ["span"],
                "$defs": {"not": {"type": "string"}}
            }),
        )];
        assert!(prepare_tools_for_strict_mode(&mut named));
        assert_eq!(named[0].strict, Some(true));
        assert_eq!(
            named[0].input_schema["$defs"]["not"],
            json!({"type": "string"})
        );

        let mut structural = vec![test_tool(
            "cite",
            json!({
                "type": "object",
                "properties": {"span": {"$ref": "#/$defs/span"}},
                "required": ["span"],
                "$defs": {"span": {"type": "string", "not": {"type": "string"}}}
            }),
        )];
        assert!(!prepare_tools_for_strict_mode(&mut structural));
        assert_eq!(structural[0].strict, None);
    }

    #[test]
    fn strict_mode_keeps_strict_when_literals_hold_schema_shaped_values() {
        // `enum` members and `default` values are values, not schemas: an
        // object member that happens to carry a keyword-shaped key, or a
        // `default` shaped like `{"nullable": true}`, is not a keyword
        // occurrence.
        let mut tools = vec![test_tool(
            "filter",
            json!({
                "type": "object",
                "properties": {
                    "mode": {"enum": [{"contains": "abc"}, {"exact": "abc"}]},
                    "fallback": {
                        "type": "object",
                        "properties": {"limit": {"type": "integer"}},
                        "required": ["limit"],
                        "default": {"nullable": true}
                    }
                },
                "required": ["mode", "fallback"]
            }),
        )];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(
            tools[0].input_schema["properties"]["mode"]["enum"],
            json!([{"contains": "abc"}, {"exact": "abc"}])
        );
    }

    #[test]
    fn strict_mode_fails_closed_on_a_root_map_shape() {
        // A root that declares no properties but takes arbitrary keys
        // (`additionalProperties` in schema form or `true`) collapses under
        // the enforce pass into `properties: {}` +
        // `additionalProperties: false` — `strict: true` callable only with
        // `{}` while the runtime reads the keys. It ships unchanged and
        // non-strict instead. The authored no-argument roots that stay
        // strict are the ones whose `additionalProperties` is absent or
        // already `false`.
        let mut map_schema = vec![test_tool(
            "annotate",
            json!({
                "type": "object",
                "additionalProperties": {"type": "string"}
            }),
        )];
        assert!(!prepare_tools_for_strict_mode(&mut map_schema));
        assert_eq!(map_schema[0].strict, None);
        assert_eq!(
            map_schema[0].input_schema,
            json!({
                "type": "object",
                "additionalProperties": {"type": "string"}
            }),
            "the schema ships unchanged"
        );

        let mut open_map = vec![test_tool(
            "annotate",
            json!({
                "type": "object",
                "additionalProperties": true
            }),
        )];
        assert!(!prepare_tools_for_strict_mode(&mut open_map));
        assert_eq!(open_map[0].strict, None);

        let mut no_arg = vec![test_tool(
            "ping",
            json!({
                "type": "object",
                "additionalProperties": false
            }),
        )];
        assert!(prepare_tools_for_strict_mode(&mut no_arg));
        assert_eq!(no_arg[0].strict, Some(true));
    }

    #[test]
    fn strict_mode_fails_closed_when_a_typeless_map_keyword_would_ship_verbatim() {
        // The enforce pass normalizes `additionalProperties` only on object
        // schemas; a node without `type`/`properties` keeps the authored
        // keyword, and a non-`false` value is map semantics the documented
        // subset does not describe — it fails closed instead of shipping the
        // keyword under a strict flag.
        let mut tools = vec![test_tool(
            "tag",
            json!({
                "type": "object",
                "properties": {
                    "labels": {"additionalProperties": {"type": "string"}}
                },
                "required": ["labels"]
            }),
        )];
        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
    }

    #[test]
    fn strict_mode_fails_closed_on_a_required_nested_map() {
        // One level down the same shape already fails closed through the
        // gate's nested rule once the enforce pass injects the empty
        // `properties` map — pinned here beside the root variants.
        let mut tools = vec![test_tool(
            "annotate",
            json!({
                "type": "object",
                "properties": {
                    "meta": {
                        "type": "object",
                        "additionalProperties": {"type": "string"}
                    }
                },
                "required": ["meta"]
            }),
        )];
        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
    }

    #[test]
    fn strict_mode_fails_closed_when_removal_empties_a_nested_object() {
        // The root-emptied rule has a nested analog: a REQUIRED object whose
        // every property is optional loses them all and ships callable only
        // with `{}` while the runtime still reads the removed members. The
        // tool ships unchanged and non-strict, like the root case.
        let mut tools = vec![test_tool(
            "deploy",
            json!({
                "type": "object",
                "properties": {
                    "id": {"type": "string"},
                    "opts": {
                        "type": "object",
                        "properties": {
                            "region": {"type": "string"},
                            "replicas": {"type": "integer"}
                        },
                        "required": []
                    }
                },
                "required": ["id", "opts"]
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["opts"]["required"],
            json!([])
        );
        assert!(
            tools[0].input_schema["properties"]["opts"]["properties"]
                .get("region")
                .is_some(),
            "the nested schema ships unchanged: {}",
            tools[0].input_schema
        );
        assert_eq!(tools[0].description, "deploy");
    }

    #[test]
    fn strict_mode_fails_closed_on_a_required_bare_object_parameter() {
        // `sanitize` injects an empty `properties` on a bare
        // `{"type":"object"}` node; under `additionalProperties: false` that
        // shape cannot carry a value, so a required bare-object parameter
        // fails the tool closed instead of shipping strict accepting only
        // `{}`.
        let mut tools = vec![test_tool(
            "configure",
            json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "config": {"type": "object"}
                },
                "required": ["name", "config"]
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["properties"]["config"],
            json!({"type": "object"}),
            "the schema ships unchanged"
        );
        assert_eq!(tools[0].description, "configure");
    }

    #[test]
    fn strict_mode_fails_closed_on_a_schema_form_dependencies_table() {
        // A draft-7 `dependencies` entry whose value is a schema is
        // semantically `dependentSchemas`, which the gate fails closed on;
        // the strip drops both spellings, so the schema form must be
        // detected before the strip or the tool would ship strict with the
        // cross-schema requirement silently gone. The name-table form
        // (`dependentRequired`) keeps its widening-only strip.
        let mut tools = vec![test_tool(
            "agent",
            json!({
                "type": "object",
                "properties": {
                    "a": {"type": "string"},
                    "b": {"type": "string"}
                },
                "required": ["a"],
                "dependencies": {"a": {"required": ["b"]}}
            }),
        )];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(
            tools[0].input_schema["dependencies"]["a"],
            json!({"required": ["b"]}),
            "the table ships unchanged"
        );
        assert_eq!(tools[0].description, "agent");
    }

    #[test]
    fn strict_mode_keeps_strict_when_a_schema_form_dependencies_table_is_optional() {
        // The same table under an OPTIONAL property is removed wholesale —
        // no fail-closed, no widening concern: the constraint ships nowhere.
        let mut tools = vec![test_tool(
            "agent",
            json!({
                "type": "object",
                "properties": {
                    "name": {"type": "string"},
                    "opts": {
                        "type": "object",
                        "properties": {
                            "a": {"type": "string"}
                        },
                        "required": ["a"],
                        "dependencies": {"a": {"required": ["b"]}, "b": {"type": "string"}}
                    }
                },
                "required": ["name"]
            }),
        )];

        assert!(prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, Some(true));
        assert_eq!(tools[0].input_schema["required"], json!(["name"]));
        assert!(tools[0].input_schema["properties"].get("opts").is_none());
    }

    #[test]
    fn strict_mode_fails_closed_on_required_nullable_unions() {
        // `nullable` is outside the documented strict subset (the supported
        // type list has no null and the documents never mention the keyword),
        // and `sanitize` collapses the common `X | null` union into
        // `nullable: true`. A schema that still carries the keyword after
        // that collapse — i.e. a required nullable — cannot be represented
        // faithfully under a strict flag, so the tool ships unchanged and
        // non-strict instead of inviting the server 400 the pipeline exists
        // to prevent.
        let mut tools = vec![Tool {
            tool_type: None,
            name: "search".to_string(),
            description: "Search".to_string(),
            input_schema: json!({
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "mode": {"anyOf": [{"type": "string"}, {"type": "null"}]},
                    "limit": {
                        "anyOf": [
                            {"type": "integer"},
                            {"type": "string"},
                            {"type": "null"}
                        ]
                    }
                },
                "required": ["query", "mode", "limit"]
            }),
            allowed_callers: None,
            defer_loading: None,
            input_examples: None,
            strict: None,
            cache_control: None,
        }];

        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(tools[0].description, "Search");
        assert_eq!(
            tools[0].input_schema["properties"]["mode"]["anyOf"],
            json!([{"type": "string"}, {"type": "null"}])
        );
        assert_eq!(
            tools[0].input_schema["properties"]["limit"]["anyOf"],
            json!([
                {"type": "integer"},
                {"type": "string"},
                {"type": "null"}
            ])
        );
        assert_eq!(
            tools[0].input_schema["required"],
            json!(["query", "mode", "limit"])
        );
    }

    #[test]
    fn strict_sanitize_keeps_properties_named_after_schema_keywords_intact() {
        // The recursion must treat the `properties` value as a name map, not
        // as a schema: walking it as a schema applied the keyword strips and
        // the object rules to property NAMES, renaming a required `const`
        // property to `enum`, erasing a `properties` property's schema, and
        // dangling `patternProperties` in `required` — all while marking the
        // tool strict. Required keyword-named properties survive intact; the
        // strict object rules land on the tool schema and on each property's
        // own sub-schema, never on the name map.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "const": {"type": "string"},
                "properties": {"type": "object"},
                "patternProperties": {"type": "object"},
                "type": {"type": "string"}
            },
            "required": ["const", "patternProperties", "properties", "type"]
        });

        let removed = sanitize_for_strict(&mut schema);

        assert!(removed.is_empty());
        assert_eq!(schema["properties"]["const"], json!({"type": "string"}));
        assert_eq!(schema["properties"]["type"], json!({"type": "string"}));
        assert_eq!(
            schema["properties"]["properties"]["type"],
            json!("object"),
            "the property's own schema keeps its type keyword"
        );
        assert_eq!(
            schema["properties"]["properties"]["additionalProperties"], false,
            "the strict object rules apply to the property schema itself"
        );
        assert_eq!(
            schema["properties"]["patternProperties"]["type"],
            json!("object")
        );
        assert_eq!(
            schema["required"],
            json!(["const", "patternProperties", "properties", "type"])
        );
        assert_eq!(schema["additionalProperties"], false);
        assert!(
            schema["properties"].get("required").is_none()
                && schema["properties"].get("additionalProperties").is_none(),
            "the name map gains no spurious schema keys: {schema}"
        );
    }

    #[test]
    fn strict_sanitize_reports_an_optional_property_named_properties() {
        // An optional keyword-named property is removed and reported like any
        // other optional, and the name map stays clean.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "query": {"type": "string"},
                "properties": {"type": "object"}
            },
            "required": ["query"]
        });

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(removed, vec!["properties".to_string()]);
        assert!(schema["properties"].get("properties").is_none());
        assert_eq!(schema["properties"]["query"], json!({"type": "string"}));
        assert_eq!(schema["required"], json!(["query"]));
        assert!(schema["properties"].get("required").is_none());
    }

    #[test]
    fn strict_sanitize_walks_schemas_but_never_name_maps_or_literals() {
        // `$defs` entries are schemas and keep getting the strict rules, but
        // a definition named after a keyword must not corrupt the map, and
        // `enum`/`default` members are values, not schemas — they ship
        // verbatim.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {
                    "type": "string",
                    "enum": [
                        {"type": "object", "properties": {"a": {"type": "string"}}}
                    ],
                    "default": {"const": 1}
                }
            },
            "required": ["mode"],
            "$defs": {
                "properties": {"type": "string"},
                "span": {
                    "type": "object",
                    "properties": {"b": {"type": "integer"}},
                    "required": ["b"]
                }
            }
        });

        let removed = sanitize_for_strict(&mut schema);

        assert!(removed.is_empty());
        assert_eq!(
            schema["properties"]["mode"]["enum"],
            json!([{"type": "object", "properties": {"a": {"type": "string"}}}])
        );
        assert_eq!(schema["properties"]["mode"]["default"], json!({"const": 1}));
        assert_eq!(schema["$defs"]["properties"], json!({"type": "string"}));
        assert_eq!(schema["$defs"]["span"]["additionalProperties"], false);
        assert_eq!(schema["$defs"]["span"]["required"], json!(["b"]));
    }

    #[test]
    fn strict_sanitize_strips_dependent_required_tables() {
        // Conditional dependency tables are outside the documented strict
        // subset; stripping one only widens what the endpoint accepts, the
        // same treatment as `patternProperties` and the length bounds.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "required": ["a"],
            "dependentRequired": {"a": ["b"]}
        });

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(removed, vec!["b".to_string()]);
        assert!(schema.get("dependentRequired").is_none());
        assert_eq!(schema["required"], json!(["a"]));
    }

    #[test]
    fn strict_sanitize_strips_undocumented_dependency_and_bound_keywords() {
        // Draft-7 `dependencies` (both the name-table and the schema form),
        // `uniqueItems`, and the property/containment count bounds are not
        // in the documented subset; dropping one only widens what the
        // endpoint accepts, the same treatment as `dependentRequired`.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "required": ["a"],
            "dependencies": {"a": ["b"]},
            "uniqueItems": true,
            "minProperties": 1,
            "maxProperties": 5,
            "minContains": 1,
            "maxContains": 3
        });

        let removed = sanitize_for_strict(&mut schema);

        assert!(schema.get("dependencies").is_none());
        assert!(schema.get("uniqueItems").is_none());
        assert!(schema.get("minProperties").is_none());
        assert!(schema.get("maxProperties").is_none());
        assert!(schema.get("minContains").is_none());
        assert!(schema.get("maxContains").is_none());
        assert_eq!(schema["required"], json!(["a"]));
        assert_eq!(removed, vec!["b".to_string()]);
    }

    #[test]
    fn strict_sanitize_strips_undocumented_formats_and_draft4_exclusive_bounds() {
        // `format` is documented with an enumerated set (email, hostname,
        // ipv4, ipv6, uuid); any other value ships a constraint outside
        // that list, so it is dropped — dropping a format only widens what
        // the endpoint accepts. A draft-4 boolean
        // `exclusiveMinimum`/`Maximum` is likewise outside the documented
        // numeric form, while the numeric form stays.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "when": {"type": "string", "format": "date-time"},
                "mail": {"type": "string", "format": "email"},
                "host": {"type": "string", "format": "uuid"},
                "score": {
                    "type": "number",
                    "minimum": 0,
                    "exclusiveMinimum": true,
                    "maximum": 10
                },
                "weight": {
                    "type": "number",
                    "exclusiveMinimum": 0,
                    "exclusiveMaximum": 100
                }
            },
            "required": ["when", "mail", "host", "score", "weight"]
        });

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(removed, Vec::<String>::new());
        assert!(
            schema["properties"]["when"].get("format").is_none(),
            "an undocumented format is dropped: {}",
            schema["properties"]["when"]
        );
        assert_eq!(schema["properties"]["mail"]["format"], "email");
        assert_eq!(schema["properties"]["host"]["format"], "uuid");
        assert!(
            schema["properties"]["score"]
                .get("exclusiveMinimum")
                .is_none(),
            "a draft-4 boolean exclusiveMinimum is dropped"
        );
        assert_eq!(schema["properties"]["weight"]["exclusiveMinimum"], 0);
        assert_eq!(schema["properties"]["weight"]["exclusiveMaximum"], 100);
        assert_eq!(schema["properties"]["weight"]["type"], "number");
    }

    #[test]
    fn strict_sanitize_drops_a_null_const_instead_of_landing_it_on_enum() {
        // `const: null` cannot be represented (null is not a supported
        // type), and landing it on `enum` would ship a null member. The
        // compatibility gate fails such a tool closed; for direct
        // `sanitize_for_strict` calls the strip drops the const outright
        // instead of manufacturing `enum: [null]`.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "fixed": {"type": "string", "const": null},
                "kept": {"type": "integer", "const": 3}
            },
            "required": ["fixed", "kept"]
        });

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(
            schema["properties"]["fixed"],
            json!({"type": "string"}),
            "a null const is dropped, not turned into a null enum member"
        );
        assert_eq!(schema["properties"]["kept"]["const"], 3);
        assert_eq!(removed, Vec::<String>::new());
    }

    #[test]
    fn strict_sanitize_walks_the_documented_def_spelling_as_a_name_map() {
        // The DeepSeek documents spell the definitions map `$def`
        // (singular). It must be walked as a name map like
        // `$defs`/`definitions`, so a definition named after a stripped
        // keyword survives intact instead of being deleted or renamed when
        // its map is visited as if it were a schema.
        let mut schema = json!({
            "type": "object",
            "properties": {"item": {"$ref": "#/$def/const"}},
            "required": ["item"],
            "$def": {
                "const": {"type": "string"},
                "patternProperties": {"type": "object"},
                "span": {
                    "type": "object",
                    "properties": {"b": {"type": "integer"}},
                    "required": ["b"]
                }
            }
        });

        let removed = sanitize_for_strict(&mut schema);

        assert!(removed.is_empty());
        assert_eq!(
            schema["$def"]["const"],
            json!({"type": "string"}),
            "a definition named `const` keeps its schema instead of becoming an `enum` key"
        );
        assert_eq!(
            schema["$def"]["patternProperties"]["type"],
            json!("object"),
            "a definition named `patternProperties` is not deleted by the keyword strip"
        );
        assert_eq!(schema["$def"]["span"]["additionalProperties"], false);
        assert_eq!(schema["$def"]["span"]["required"], json!(["b"]));
    }

    #[test]
    fn strict_sanitize_dedupes_optionals_shared_across_levels() {
        // The same optional name at two levels is reported once.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "trim": {"type": "boolean"},
                "outer": {
                    "type": "object",
                    "properties": {
                        "trim": {"type": "boolean"},
                        "alpha": {"type": "string"}
                    },
                    "required": ["alpha"]
                }
            },
            "required": ["outer"]
        });

        let removed = sanitize_for_strict(&mut schema);

        assert_eq!(removed, vec!["trim".to_string()]);
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
    #[error("Moonshot function parameters contain an invalid MFJS default")]
    InvalidDefault,
    #[error("Moonshot function parameters contain an invalid MFJS range")]
    InvalidRange,
}

/// Normalize a complete Kimi / Moonshot `function.parameters` object.
///
/// Function parameters have an additional MFJS constraint: the root must end
/// as a plain `type: "object"` schema. Root composition is flattened using the
/// same compatibility pass as Responses and xAI, while supported nested
/// `anyOf` branches remain nested. Internal root `$ref` values are resolved and
/// inlined before normalization so we never manufacture the invalid
/// `type + allOf($ref)` shape rejected by MFJS.
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

    // A function schema's root cannot carry MFJS composition because it must
    // simultaneously be `type: object`. Flatten the actual root shape used by
    // apply_patch and retain its dropped required-group contract as a prompt
    // note for the model.
    // MFJS has no conditional keywords. The shared compatibility pass drops
    // them with a bounded description note instead of failing the whole turn.
    let constraint_note = sanitize_for_responses(parameters);

    // Restore nullable unions collapsed by the registry's provider-neutral
    // sanitizer, translate MFJS-safe scalar const values, and normalize nested
    // composition. This changes model-facing schema guidance only; each tool
    // keeps responsibility for validating its own arguments. allOf fails closed.
    normalize_kimi_compatibility(parameters, true)?;

    // MFJS requires `type` to live inside each anyOf branch, never alongside
    // the union keyword. The root is composition-free at this point, so this
    // only adjusts valid nested unions.
    sanitize_for_kimi(parameters);

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
/// `UnsupportedKeyword`, `UnsupportedNestedAllOf` for a nested `allOf`, or
/// `InvalidSchemaNode` for tuple-form `items` — before a stranded empty
/// `items` node inside one could matter. `dependentSchemas` is the one
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
        if !is_mfjs_enum_literal(&constant) {
            return Err(KimiParameterSchemaError::UnsupportedConstLiteral);
        }
        if !obj.contains_key("type") {
            let inferred = mfjs_literal_kind(&constant)
                .ok_or(KimiParameterSchemaError::UnsupportedConstLiteral)?;
            let schema_type = match inferred {
                MfjsLiteralKind::Integer => "integer",
                MfjsLiteralKind::Number => "number",
                MfjsLiteralKind::String => "string",
            };
            obj.insert("type".to_string(), Value::String(schema_type.to_string()));
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

fn is_mfjs_enum_literal(value: &Value) -> bool {
    value.is_string() || value.is_number()
}

/// Validate one fully normalized MFJS function-parameters schema.
///
/// Every error is a fixed enum variant: schemas can originate in MCP or
/// runtime tools and may contain private names or values, so diagnostics must
/// never echo a keyword, property, reference, or literal from the document.
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
    if !mfjs_schema_can_terminate(parameters, parameters, &mut visiting_refs, 0)? {
        return Err(KimiParameterSchemaError::NonTerminatingReference);
    }

    validate_mfjs_expanded_depth(parameters, parameters, 0, &mut HashSet::new(), 0)?;
    if let Some(definitions) = root.get("$defs").and_then(Value::as_object) {
        for definition in definitions.values() {
            validate_mfjs_expanded_depth(definition, parameters, 0, &mut HashSet::new(), 0)?;
        }
    }
    Ok(())
}

const MFJS_MAX_ANY_OF_ITEMS: usize = 10;
const MFJS_MAX_OBJECT_DEPTH: usize = 5;
const MFJS_MAX_TOTAL_PROPERTIES: usize = 100;
const MFJS_MAX_TOTAL_ENUM_VALUES: usize = 500;
const MFJS_ENUM_LENGTH_CHECK_THRESHOLD: usize = 250;
const MFJS_MAX_ENUM_STRING_LENGTH: usize = 7_500;
const MFJS_MAX_SCHEMA_BYTES: usize = 120_000;
const MFJS_MAX_STRUCTURAL_DEPTH: usize = 64;
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

        const ALLOWED_KEYWORDS: &[&str] = &[
            "$id",
            "$ref",
            "$defs",
            "anyOf",
            "properties",
            "additionalProperties",
            "items",
            "type",
            "enum",
            "required",
            "maxLength",
            "minLength",
            "maximum",
            "minimum",
            "maxItems",
            "minItems",
            "title",
            "description",
            "default",
        ];
        if obj
            .keys()
            .any(|keyword| !ALLOWED_KEYWORDS.contains(&keyword.as_str()))
        {
            return Err(KimiParameterSchemaError::UnsupportedKeyword);
        }

        for annotation in ["title", "description"] {
            if obj.get(annotation).is_some_and(|value| !value.is_string()) {
                return Err(KimiParameterSchemaError::InvalidKeywordValue);
            }
        }
        if obj.get("$id").is_some_and(|value| !value.is_string())
            || (!is_root && obj.contains_key("$id"))
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
                matches!(key, "$ref" | "title" | "description")
                    || (is_root && matches!(key, "$defs" | "$id"))
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
                matches!(key, "anyOf" | "title" | "description")
                    || (is_root && matches!(key, "$defs" | "$id"))
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
        if let Some(default) = obj.get("default") {
            validate_mfjs_default(default, schema_type)?;
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

        validate_mfjs_bounds(obj, schema_type)?;
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
    if is_root && matches!(keyword, "$defs" | "$id") {
        return true;
    }
    if matches!(keyword, "type" | "title" | "description") {
        return true;
    }
    match schema_type {
        "object" => matches!(keyword, "properties" | "required" | "additionalProperties"),
        "array" => matches!(keyword, "items" | "minItems" | "maxItems"),
        "string" => matches!(keyword, "enum" | "default" | "minLength" | "maxLength"),
        "number" | "integer" => {
            matches!(keyword, "enum" | "default" | "minimum" | "maximum")
        }
        "boolean" | "null" => keyword == "default",
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

fn validate_mfjs_default(value: &Value, schema_type: &str) -> Result<(), KimiParameterSchemaError> {
    let valid = match schema_type {
        "boolean" => value.is_boolean(),
        "number" => value.is_number(),
        "integer" => mfjs_integer_value(value).is_some(),
        "string" => value.is_string(),
        "null" => value.is_null(),
        _ => false,
    };
    if valid {
        Ok(())
    } else {
        Err(KimiParameterSchemaError::InvalidDefault)
    }
}

fn mfjs_integer_value(value: &Value) -> Option<f64> {
    let value = value.as_number()?.as_f64()?;
    (value.is_finite() && value.fract() == 0.0 && value.abs() <= MFJS_MAX_SAFE_INTEGER)
        .then_some(value)
}

fn validate_mfjs_bounds(
    obj: &Map<String, Value>,
    schema_type: &str,
) -> Result<(), KimiParameterSchemaError> {
    validate_mfjs_unsigned_range(obj, schema_type, "string", "minLength", "maxLength")?;
    validate_mfjs_unsigned_range(obj, schema_type, "array", "minItems", "maxItems")?;

    let minimum = obj.get("minimum");
    let maximum = obj.get("maximum");
    if minimum.is_some() || maximum.is_some() {
        let parse_bound = |value: &Value| match schema_type {
            "integer" => mfjs_integer_value(value),
            "number" => value.as_number().and_then(serde_json::Number::as_f64),
            _ => None,
        };
        let minimum = minimum
            .map(|value| parse_bound(value).ok_or(KimiParameterSchemaError::InvalidRange))
            .transpose()?;
        let maximum = maximum
            .map(|value| parse_bound(value).ok_or(KimiParameterSchemaError::InvalidRange))
            .transpose()?;
        if minimum.zip(maximum).is_some_and(|(min, max)| min > max) {
            return Err(KimiParameterSchemaError::InvalidRange);
        }
    }
    Ok(())
}

fn validate_mfjs_unsigned_range(
    obj: &Map<String, Value>,
    schema_type: &str,
    expected_type: &str,
    minimum_keyword: &str,
    maximum_keyword: &str,
) -> Result<(), KimiParameterSchemaError> {
    let minimum = obj.get(minimum_keyword);
    let maximum = obj.get(maximum_keyword);
    if minimum.is_none() && maximum.is_none() {
        return Ok(());
    }
    if schema_type != expected_type {
        return Err(KimiParameterSchemaError::InvalidRange);
    }
    let minimum = minimum
        .map(|value| value.as_u64().ok_or(KimiParameterSchemaError::InvalidRange))
        .transpose()?;
    let maximum = maximum
        .map(|value| value.as_u64().ok_or(KimiParameterSchemaError::InvalidRange))
        .transpose()?;
    if minimum.zip(maximum).is_some_and(|(min, max)| min > max) {
        return Err(KimiParameterSchemaError::InvalidRange);
    }
    Ok(())
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

fn mfjs_schema_can_terminate(
    schema: &Value,
    document: &Value,
    visiting_refs: &mut HashSet<String>,
    structural_depth: usize,
) -> Result<bool, KimiParameterSchemaError> {
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
        let terminates =
            mfjs_schema_can_terminate(target, document, visiting_refs, structural_depth + 1)?;
        visiting_refs.remove(reference);
        return Ok(terminates);
    }

    if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
        for branch in branches {
            let mut branch_refs = visiting_refs.clone();
            if mfjs_schema_can_terminate(branch, document, &mut branch_refs, structural_depth + 1)?
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
                )? {
                    return Ok(false);
                }
            }
            Ok(true)
        }
        Some("array") => {
            // An absent or non-integer `minItems` reads as 0. Non-integer
            // values are unreachable here only because `validate_mfjs_bounds`
            // rejects them (InvalidRange) on every node before the terminator
            // runs — keep that call order if the walkers are reordered.
            if obj.get("minItems").and_then(Value::as_u64).unwrap_or(0) == 0 {
                return Ok(true);
            }
            let items = obj
                .get("items")
                .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;
            mfjs_schema_can_terminate(items, document, visiting_refs, structural_depth + 1)
        }
        Some(schema_type) if is_mfjs_concrete_type(schema_type) => Ok(true),
        _ => Err(KimiParameterSchemaError::MissingType),
    }
}

fn validate_mfjs_expanded_depth(
    schema: &Value,
    document: &Value,
    property_depth: usize,
    visiting_refs: &mut HashSet<String>,
    structural_depth: usize,
) -> Result<(), KimiParameterSchemaError> {
    if property_depth > MFJS_MAX_OBJECT_DEPTH || structural_depth > MFJS_MAX_STRUCTURAL_DEPTH {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    let obj = schema
        .as_object()
        .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;

    if let Some(reference) = obj.get("$ref").and_then(Value::as_str) {
        if !visiting_refs.insert(reference.to_string()) {
            return Ok(());
        }
        let target = resolve_mfjs_reference(document, reference)
            .ok_or(KimiParameterSchemaError::InvalidReference)?;
        let result = validate_mfjs_expanded_depth(
            target,
            document,
            property_depth,
            visiting_refs,
            structural_depth + 1,
        );
        visiting_refs.remove(reference);
        return result;
    }

    if let Some(properties) = obj.get("properties").and_then(Value::as_object) {
        for property in properties.values() {
            validate_mfjs_expanded_depth(
                property,
                document,
                property_depth + 1,
                visiting_refs,
                structural_depth + 1,
            )?;
        }
    }
    if let Some(items) = obj.get("items") {
        validate_mfjs_expanded_depth(
            items,
            document,
            property_depth,
            visiting_refs,
            structural_depth + 1,
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
            visiting_refs,
            structural_depth + 1,
        )?;
    }
    if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
        for branch in branches {
            validate_mfjs_expanded_depth(
                branch,
                document,
                property_depth,
                visiting_refs,
                structural_depth + 1,
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
    // available only in valid nested anyOf positions.

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
                {"type": "object", "properties": {"id": {"type": "integer"}}},
                {"type": "object", "properties": {"name": {"type": "string"}}}
            ]
        });
        sanitize_for_kimi_parameters(&mut schema).unwrap();
        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert_eq!(schema["properties"]["id"]["type"], "integer");
        assert_eq!(schema["properties"]["name"]["type"], "string");
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
        let mut schema = json!({
            "type": "object",
            "properties": {
                "private-toggle-8172": {"type": "boolean", "const": true}
            }
        });
        let original = schema.clone();

        let error = sanitize_for_kimi_parameters(&mut schema).unwrap_err();

        assert_eq!(error, KimiParameterSchemaError::UnsupportedConstLiteral);
        assert!(!error.to_string().contains("private-toggle-8172"));
        assert_eq!(schema, original, "a rejected schema must remain reusable");
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
    fn kimi_parameters_min_items_beside_empty_items_is_position_dependent() {
        // Terminator-checked position (a required property): the repaired
        // node must still terminate, and with `minItems >= 1` the terminator
        // requires an `items` keyword — the tool keeps failing closed, with
        // the terminator's variant.
        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "items": {}, "minItems": 1}},
            "required": ["a"]
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("minItems >= 1 cannot terminate without items at a checked position");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidSchemaNode),
            "the terminator's variant is InvalidSchemaNode, not MissingType: {error:?}"
        );

        // `minItems: 0` terminates trivially at the same checked position,
        // and its only MFJS-legal shape repairs to the equivalent
        // unconstrained form.
        let mut schema = json!({
            "type": "object",
            "properties": {"a": {"type": "array", "items": {}, "minItems": 0}},
            "required": ["a"]
        });
        sanitize_for_kimi_parameters(&mut schema).expect("minItems: 0 beside empty items repairs");
        assert_eq!(
            schema["properties"]["a"],
            json!({"type": "array", "minItems": 0})
        );

        // A non-required property position is never revisited by the
        // terminator, so the repaired tool SHIPS — pre-repair it was dropped
        // with MissingType. The shipped shape is instance-set-equivalent:
        // the empty `items` was a no-op constraint and `minItems` survives.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "s": {"type": "array", "items": {}, "minItems": 1}
            }
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("a non-required position ships the equivalent repaired schema");
        assert_eq!(
            schema["properties"]["s"],
            json!({"type": "array", "minItems": 1})
        );

        // The terminator's other two checked positions — an `anyOf` branch
        // and a `$ref` target, each reached from a required property — fail
        // closed the same way, with the terminator's variant.
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
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("minItems >= 1 cannot terminate in a checked anyOf branch");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidSchemaNode),
            "the anyOf branch's variant is InvalidSchemaNode: {error:?}"
        );

        // The anyOf check is branch-order dependent: the terminator stops
        // at the first terminating branch, so a terminating branch ahead
        // of the offending one lets the tool ship — the repaired schema is
        // instance-set-equivalent (the empty `items` was a no-op
        // constraint), which is the desired outcome, but it means the
        // fail-closed guarantee for anyOf holds only when the offending
        // branch is reached.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {
                    "anyOf": [
                        {"type": "string"},
                        {"type": "array", "items": {}, "minItems": 1}
                    ]
                }
            },
            "required": ["a"]
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("a terminating branch ahead of the offending one ships the tool");
        assert_eq!(
            schema["properties"]["a"]["anyOf"][1],
            json!({"type": "array", "minItems": 1}),
            "the shipped branch is the repaired, instance-set-equivalent form"
        );

        let mut schema = json!({
            "type": "object",
            "$defs": {"a": {"type": "array", "items": {}, "minItems": 1}},
            "properties": {"x": {"$ref": "#/$defs/a"}},
            "required": ["x"]
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("minItems >= 1 cannot terminate at a checked $ref target");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidSchemaNode),
            "the $ref target's variant is InvalidSchemaNode: {error:?}"
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
        let mut too_many_branches = json!({
            "type": "object",
            "properties": {
                "choice": {
                    "anyOf": (0..11)
                        .map(|_| json!({"type": "string"}))
                        .collect::<Vec<_>>()
                }
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_branches).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let string_property = || json!({"type": "string"});
        let mut root_properties = Map::new();
        for index in 0..51 {
            root_properties.insert(format!("root_{index}"), string_property());
        }
        let mut definition_properties = Map::new();
        for index in 0..50 {
            definition_properties.insert(format!("definition_{index}"), string_property());
        }
        let mut too_many_properties = json!({
            "type": "object",
            "properties": Value::Object(root_properties),
            "$defs": {
                "Holder": {
                    "type": "object",
                    "properties": Value::Object(definition_properties)
                }
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_properties).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let enum_values = |start: usize, count: usize| {
            Value::Array((start..start + count).map(|value| json!(value)).collect())
        };
        let mut too_many_enum_values = json!({
            "type": "object",
            "properties": {
                "first": {"type": "integer", "enum": enum_values(0, 250)},
                "second": {"type": "integer", "enum": enum_values(250, 251)}
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_many_enum_values).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_enforces_depth_and_enum_text_limits() {
        let mut too_deep = json!({"type": "string"});
        for index in (0..6).rev() {
            let mut properties = Map::new();
            properties.insert(format!("level_{index}"), too_deep);
            too_deep = json!({
                "type": "object",
                "properties": Value::Object(properties)
            });
        }
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_deep).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let long_values = Value::Array(
            (0..251)
                .map(|index| Value::String(format!("private-enum-{index:04}-xxxxxxxxxxxxxx")))
                .collect(),
        );
        let mut too_much_enum_text = json!({
            "type": "object",
            "properties": {
                "choice": {"type": "string", "enum": long_values}
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut too_much_enum_text).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_validates_default_type_and_placement_without_leaks() {
        let mut valid = json!({
            "type": "object",
            "properties": {
                "enabled": {"type": "boolean", "default": true},
                "count": {"type": "integer", "default": 3},
                "ratio": {"type": "number", "default": 1.5},
                "label": {"type": "string", "default": "default-label"},
                "empty": {"type": "null", "default": null}
            }
        });
        sanitize_for_kimi_parameters(&mut valid).unwrap();

        for (mut invalid, expected) in [
            (
                json!({
                    "type": "object",
                    "properties": {
                        "private-default-3301": {
                            "type": "integer",
                            "default": "private-default-value-3302"
                        }
                    }
                }),
                KimiParameterSchemaError::InvalidDefault,
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "private-untyped-default-3303": {"default": 1}
                    }
                }),
                KimiParameterSchemaError::MissingType,
            ),
            (
                json!({
                    "type": "object",
                    "properties": {
                        "private-object-default-3304": {
                            "type": "object",
                            "default": {}
                        }
                    }
                }),
                KimiParameterSchemaError::InvalidKeywordValue,
            ),
        ] {
            let original = invalid.clone();
            let error = sanitize_for_kimi_parameters(&mut invalid).unwrap_err();
            assert_eq!(error, expected);
            assert!(!error.to_string().contains("private-default"));
            assert_eq!(invalid, original);
        }
    }

    #[test]
    fn kimi_parameters_rejects_inverted_and_fractional_integer_bounds() {
        for mut schema in [
            json!({
                "type": "object",
                "properties": {
                    "value": {"type": "string", "minLength": 5, "maxLength": 4}
                }
            }),
            json!({
                "type": "object",
                "properties": {
                    "value": {"type": "array", "minItems": 3, "maxItems": 2}
                }
            }),
            json!({
                "type": "object",
                "properties": {
                    "value": {"type": "number", "minimum": 10, "maximum": 9}
                }
            }),
            json!({
                "type": "object",
                "properties": {
                    "value": {"type": "integer", "minimum": 1.5}
                }
            }),
            json!({
                "type": "object",
                "properties": {
                    "value": {"type": "integer", "maximum": 2.5}
                }
            }),
        ] {
            assert_eq!(
                sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
                KimiParameterSchemaError::InvalidRange
            );
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
}
