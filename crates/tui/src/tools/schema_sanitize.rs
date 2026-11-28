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
//! compatibility passes where their request shapes need it, and a narrower
//! Anthropic Messages pass that strips only the root composition keywords
//! that validator is recorded rejecting.
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
//!    this module takes the narrower face. Pure annotations are stripped;
//!    bounds are real constraints and fail closed instead of being removed
//!    into a wider accepted instance set.
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
    if !schema_within_structural_depth_limit(schema) {
        return;
    }
    sanitize_in_instance(schema, &[]);
}

/// Provider-neutral recursion protection for the shared registry. This guard
/// deliberately has no MFJS byte budget: other providers may accept documents
/// larger than 120000 bytes. The iterative walk cannot overflow the call stack.
pub(crate) fn schema_within_structural_depth_limit(schema: &Value) -> bool {
    let mut pending = vec![(schema, 0usize)];
    while let Some((value, depth)) = pending.pop() {
        if depth > MFJS_MAX_STRUCTURAL_DEPTH {
            return false;
        }
        match value {
            Value::Object(map) => {
                pending.extend(map.values().map(|child| (child, depth + 1)));
            }
            Value::Array(items) => {
                pending.extend(items.iter().map(|child| (child, depth + 1)));
            }
            _ => {}
        }
    }
    true
}

/// Moonshot's raw-input admission check, applied before cloning or recursive
/// normalization. Serialization happens only after depth safety is established.
pub(crate) fn schema_within_mfjs_local_budgets(schema: &Value) -> bool {
    schema_within_structural_depth_limit(schema)
        && serde_json::to_vec(schema).is_ok_and(|encoded| encoded.len() <= MFJS_MAX_SCHEMA_BYTES)
}

struct RelaxedResourceTask<'a> {
    value: &'a Value,
    property_depth: usize,
    structural_depth: usize,
}

fn push_relaxed_resource_tasks<'a>(
    pending: &mut Vec<RelaxedResourceTask<'a>>,
    value: &'a Value,
    property_depth: usize,
    structural_depth: usize,
) {
    if let Value::Array(items) = value {
        pending.extend(items.iter().map(|item| RelaxedResourceTask {
            value: item,
            property_depth,
            structural_depth: structural_depth + 1,
        }));
    } else {
        pending.push(RelaxedResourceTask {
            value,
            property_depth,
            structural_depth: structural_depth + 1,
        });
    }
}

/// Validate only the local resource budgets for the explicit strict:false
/// Moonshot path. This performs no MFJS keyword/type validation and never
/// mutates the schema. References stay unexpanded and literal instance data is
/// never interpreted as a subschema; unsupported composition is still walked
/// so its resource use cannot hide from the budgets.
pub(crate) fn validate_kimi_relaxed_resource_budgets(
    schema: &Value,
) -> Result<(), KimiParameterSchemaError> {
    if !schema_within_mfjs_local_budgets(schema) {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }

    let mut pending = vec![RelaxedResourceTask {
        value: schema,
        property_depth: 0,
        structural_depth: 0,
    }];
    let mut work = 0usize;
    let mut total_properties = 0usize;
    let mut total_enum_values = 0usize;
    while let Some(RelaxedResourceTask {
        value,
        property_depth,
        structural_depth,
    }) = pending.pop()
    {
        work = work
            .checked_add(1)
            .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
        if work > MFJS_MAX_EXPANDED_STATES
            || property_depth > MFJS_MAX_OBJECT_DEPTH
            || structural_depth > MFJS_MAX_STRUCTURAL_DEPTH
        {
            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
        }
        let Value::Object(obj) = value else {
            continue;
        };
        if obj
            .get("anyOf")
            .and_then(Value::as_array)
            .is_some_and(|branches| branches.len() > MFJS_MAX_ANY_OF_ITEMS)
        {
            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
        }

        for (key, child) in obj {
            match key.as_str() {
                "properties" => {
                    if let Some(properties) = child.as_object() {
                        total_properties = total_properties
                            .checked_add(properties.len())
                            .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
                        if total_properties > MFJS_MAX_TOTAL_PROPERTIES {
                            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
                        }
                        for property in properties.values() {
                            pending.push(RelaxedResourceTask {
                                value: property,
                                property_depth: property_depth + 1,
                                structural_depth: structural_depth + 1,
                            });
                        }
                    } else {
                        push_relaxed_resource_tasks(
                            &mut pending,
                            child,
                            property_depth,
                            structural_depth,
                        );
                    }
                }
                "$defs" | "definitions" => {
                    if let Some(definitions) = child.as_object() {
                        for definition in definitions.values() {
                            pending.push(RelaxedResourceTask {
                                value: definition,
                                property_depth: 0,
                                structural_depth: structural_depth + 1,
                            });
                        }
                    } else {
                        push_relaxed_resource_tasks(
                            &mut pending,
                            child,
                            property_depth,
                            structural_depth,
                        );
                    }
                }
                "patternProperties" | "dependentSchemas" => {
                    if let Some(schemas) = child.as_object() {
                        for subschema in schemas.values() {
                            pending.push(RelaxedResourceTask {
                                value: subschema,
                                property_depth,
                                structural_depth: structural_depth + 1,
                            });
                        }
                    } else {
                        push_relaxed_resource_tasks(
                            &mut pending,
                            child,
                            property_depth,
                            structural_depth,
                        );
                    }
                }
                "default" | "const" | "examples" => {}
                "enum" => {
                    if let Some(values) = child.as_array() {
                        total_enum_values = total_enum_values
                            .checked_add(values.len())
                            .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
                        if total_enum_values > MFJS_MAX_TOTAL_ENUM_VALUES {
                            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
                        }
                        // The per-string length budget arms at any entry
                        // count: gating it on a minimum enum size would let a
                        // small enum carry one oversized string the wire
                        // rejects (round-4 review).
                        let mut encoded_length = 0usize;
                        for value in values {
                            let literal_length = value
                                .as_str()
                                .map(str::len)
                                .unwrap_or_else(|| value.to_string().len());
                            encoded_length = encoded_length
                                .checked_add(literal_length)
                                .ok_or(KimiParameterSchemaError::ResourceLimitExceeded)?;
                        }
                        if encoded_length > MFJS_MAX_ENUM_STRING_LENGTH {
                            return Err(KimiParameterSchemaError::ResourceLimitExceeded);
                        }
                    }
                }
                "$ref" | "required" => {}
                _ => push_relaxed_resource_tasks(
                    &mut pending,
                    child,
                    property_depth,
                    structural_depth,
                ),
            }
        }
    }
    Ok(())
}

/// Sanitize a schema for one JSON instance scope.
///
/// `enclosing_properties` carries the property names declared by the
/// enclosing object schema for the *same* instance: a `required` array
/// inside a composition branch refers to that instance, so a required-only
/// branch such as `oneOf: [{required: ["patch"]}, ...]` (the builtin
/// `apply_patch` shape) keeps its entries — they resolve against the root
/// `properties` — instead of being pruned into `{}`, which under `oneOf`
/// semantics would reject every instance (all-empty branches match
/// everything, so exactly-one-match is unsatisfiable). Every other keyword
/// (`properties.*`, `items`, `additionalProperties`, ...) validates a
/// different instance, so the context resets there and only genuinely
/// dangling names are pruned.
fn sanitize_in_instance(schema: &mut Value, enclosing_properties: &[String]) {
    // Whether THIS schema declared a properties map before the injection
    // pass: capture AFTER the nullable-union collapse (a merged arm's map —
    // even an explicitly empty one — is the schema's own declaration,
    // review thread on 5897819279) but BEFORE
    // `inject_properties_on_bare_objects`, whose synthetic map is not a
    // declaration (review threads on 5896057559 and 5897491074: treating
    // it as one discarded the inherited context for composition
    // intermediates).
    collapse_nullable_unions(schema);
    let declared_properties = schema
        .as_object()
        .is_some_and(|obj| obj.contains_key("properties"));
    inject_properties_on_bare_objects(schema);
    prune_dangling_required(schema, enclosing_properties, declared_properties);
    collapse_single_element_unions(schema);
    // Recurse into all sub-schemas
    if let Some(obj) = schema.as_object_mut() {
        // The context this scope hands its same-instance applicators: a
        // DECLARED `properties` map replaces the inherited names, while an
        // object with no declaration of its own introduces no property
        // scope and passes the inherited names through — a nested
        // composition inside an untyped or bare-typed `{oneOf: [...]}`
        // branch still resolves against the ancestor that declared the
        // properties (review threads on 5896057559 and 5897491074: the
        // injected compatibility map must not read as a declaration, or
        // the inner required-only branches prune into `{}` and no valid
        // argument survives).
        let local_properties: Vec<String> = match obj.get("properties") {
            Some(props) if declared_properties => props
                .as_object()
                .map(|props| props.keys().cloned().collect())
                .unwrap_or_default(),
            Some(props) if props.as_object().is_some_and(|m| !m.is_empty()) => {
                // A collapse merged a union arm's real property map into
                // this scope: that map is the schema's own declaration.
                props
                    .as_object()
                    .map(|props| props.keys().cloned().collect())
                    .unwrap_or_default()
            }
            _ => enclosing_properties.to_vec(),
        };
        for (key, v) in obj.iter_mut() {
            if matches!(
                key.as_str(),
                "oneOf" | "anyOf" | "allOf" | "not" | "if" | "then" | "else"
            ) {
                sanitize_in_instance(v, &local_properties);
            } else if key == "dependentSchemas" {
                // Draft 2020-12: each dependentSchemas value applies to the
                // SAME instance as the object schema holding it, so a
                // dependency naming only fields declared at this scope
                // (`{required: ["confirm"]}` against the enclosing
                // properties) must resolve here instead of pruning into an
                // all-accepting `{}` — the compatible-Messages lane would
                // otherwise forward a contract that admits a `mode`-only
                // argument with no `confirm` at all (review 5910284697).
                // The name keys route instances; they are not schema
                // scopes, so the reset applies inside the map's values only.
                if let Some(dep_map) = v.as_object_mut() {
                    for (_name, dep) in dep_map.iter_mut() {
                        sanitize_in_instance(dep, &local_properties);
                    }
                }
            } else if matches!(key.as_str(), "default" | "enum" | "const") {
                // Instance data, not schema: each enum entry and every const
                // or default value is a literal JSON value the tool can
                // receive, so a schema-shaped entry (`enum: [{"required":
                // ["x"]}]` for a property named `required`) must reach the
                // wire byte-for-byte. The generic recursion used to normalize
                // those literal objects — pruning their `required` into an
                // all-accepting `{}` — excluding an argument the original
                // contract admitted and advertising a different one (review
                // 5911391551).
            } else if matches!(
                key.as_str(),
                "properties" | "patternProperties" | "$defs" | "definitions"
            ) {
                // A name→schema map: the keys route names and are never
                // keywords. The generic walk dispatched a property
                // literally named `dependentSchemas` as the keyword, treated
                // its schema as a dependency map, walked the enum literal
                // inside as schemas, and pruned the literal object's
                // `required` into an all-accepting `{}` — reversing what
                // the contract accepts (review 5986025535). Walk each VALUE
                // as a schema node in its own right; the empty context
                // matches the previous treatment of property subtrees.
                if let Some(name_map) = v.as_object_mut() {
                    for (_name, prop_schema) in name_map.iter_mut() {
                        sanitize_in_instance(prop_schema, &[]);
                    }
                } else {
                    sanitize_in_instance(v, &[]);
                }
            } else {
                sanitize_in_instance(v, &[]);
            }
        }
    } else if let Some(arr) = schema.as_array_mut() {
        // Composition branch lists arrive as the array value of a
        // composition keyword, whose context the parent already chose —
        // thread it so each branch prunes against the enclosing
        // properties. Non-composition arrays enter with an empty context
        // and keep it.
        for v in arr.iter_mut() {
            sanitize_in_instance(v, enclosing_properties);
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
        if !schema_within_structural_depth_limit(&tool.input_schema) {
            tool.strict = None;
            all_strict = false;
            continue;
        }
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
    if !schema_within_structural_depth_limit(schema) {
        return;
    }
    sanitize(schema);
    enforce_strict_subset(schema);
}

/// Sanitize a tool `parameters` schema for xAI chat completions.
///
/// xAI rejects a parameters root that is neither an object schema nor a
/// root-level `anyOf`/`oneOf` union whose every branch is itself an object
/// schema ("tool parameter root must be an object type"), naming the tool in
/// the 400. The live incident recorded in upstream fix `ab6c68a4d`
/// (2026-07-19, grok-4.5's first tool-bearing request) died on `apply_patch`'s
/// `oneOf` required-groups (`patch` | `replace` | `changes`). xAI's 2026-10-10
/// function-calling documentation
/// (https://docs.x.ai/developers/tools/function-calling) has since relaxed
/// the rule — "A root `anyOf` or `oneOf` also works when every branch is
/// itself an object" — so that documented-good shape now passes through with
/// only the shared normalization pass (its dangling-`required` prune
/// included) and the dependent-keyword drop — but only when that shared pass
/// would keep every branch's `required` group. An object branch that
/// declares its group as a bare `required` (the names living in the root's
/// shared `properties`, as raw MCP servers commonly ship) would be pruned
/// into a constraint-free `{type:"object",properties:{}}`; two identically
/// gutted branches make an exactly-one `oneOf` unsatisfiable for every
/// instance, a worse fate than the flattening pass's disclosed
/// note-only degradation. Such unions, and everything else that never
/// qualified (non-object roots, unions with a non-object branch,
/// composite roots mixing a union with `allOf`/`enum`/`not`, both union
/// keywords at once, malformed union values), still go through the
/// Responses-API flattening pass.
///
/// Pass-through is granted on semantic usability, not structural
/// distinctness: pairwise-distinct projected branches can still serve no
/// instance (review 5802378120: the registry pre-prune of cross-branch
/// `required` groups leaves two required-free object branches that both
/// match `{}`; review 5802577370: a nested `oneOf` the recursive cleanup
/// collapses into identical branches kills its whole parent branch). The
/// rehearsal clone is therefore judged by [`oneof_semantic_defect`], and
/// only a union it cannot kill passes through; a killed union takes the
/// flattening fallback, which keeps the shared root properties usable.
/// The kill reason is emitted at debug level; the user-facing note stays
/// the flattening pass's own.
pub fn sanitize_for_xai_parameters(parameters: &mut Value) -> Option<String> {
    if !schema_within_structural_depth_limit(parameters) {
        return parameters
            .as_object()
            .and_then(root_composition_constraint_note);
    }
    if root_union_is_all_object_branches(parameters) && union_branches_keep_required(parameters) {
        // Decide on the EFFECTIVE post-normalization shape: the recursive
        // cleanup below can manufacture branch equality that the raw input
        // does not show (review 5800015446: branch `required` groups naming
        // their own declared nested object get that nested object's groups
        // pruned and empty properties injected, collapsing initially
        // distinct alternatives into identical bare objects). Rehearse the
        // exact transformation on a clone and only commit to pass-through
        // when the normalized union stays semantically usable; otherwise the
        // original (still unmutated) parameters take the flattening
        // fallback, which keeps the shared root properties usable.
        let mut normalized = parameters.clone();
        drop_dependent_keywords(&mut normalized);
        sanitize(&mut normalized);
        // Annotation-only divergence (title/description/examples/…) keeps raw
        // values unequal without making the validation constraints different
        // (review 5800814557: gutted registry branches differing only in
        // annotations passed the raw-equality check and still reached xAI as
        // two validation-equivalent exactly-one alternatives). Compare
        // validation-relevant shapes only; the real parameters keep their
        // annotations either way.
        strip_annotation_keywords(&mut normalized);
        if let Some(reason) = oneof_semantic_defect(&normalized) {
            tracing::debug!("xAI union takes the flattening pass: {reason}");
        } else {
            let dependent_note = drop_dependent_keywords(parameters);
            sanitize(parameters);
            return dependent_note;
        }
    }
    sanitize_for_responses(parameters)
}

/// Recursively remove the JSON-Schema annotation keywords (spec §7: they
/// carry no validation semantics) so branch comparison sees
/// validation-relevant shape only.
///
/// The walk distinguishes schema positions from data positions (review
/// 5802577370, inline P3): the values of `properties` / `$defs` /
/// `patternProperties` / `dependentSchemas` are schemas keyed by *field
/// names*, so the map itself is never stripped — a real property named
/// `title` is validation surface, not an annotation — and only each
/// value is visited as a schema. The values of `const` / `enum` are
/// *instance data*, so they are skipped wholesale instead of having
/// their `title`-shaped keys rewritten. Annotation keywords themselves
/// are still removed only at schema nodes.
fn strip_annotation_keywords(value: &mut Value) {
    match value {
        Value::Object(obj) => {
            for key in [
                "title",
                "description",
                "default",
                "examples",
                "$comment",
                "deprecated",
                "readOnly",
                "writeOnly",
            ] {
                obj.remove(key);
            }
            for (key, child) in obj.iter_mut() {
                match key.as_str() {
                    // Name → schema maps: the keys are field names, never
                    // schema keywords; only the values are schemas.
                    "properties" | "$defs" | "patternProperties" | "dependentSchemas" => {
                        if let Some(map) = child.as_object_mut() {
                            for subschema in map.values_mut() {
                                strip_annotation_keywords(subschema);
                            }
                        } else {
                            // Malformed keyword value: keep the historical
                            // walk instead of guessing its shape.
                            strip_annotation_keywords(child);
                        }
                    }
                    // Instance data: stripping inside it would corrupt the
                    // very constraint the value expresses.
                    "const" | "enum" => {}
                    _ => strip_annotation_keywords(child),
                }
            }
        }
        Value::Array(items) => {
            for item in items {
                strip_annotation_keywords(item);
            }
        }
        _ => {}
    }
}

/// Semantic usability judgment for an exactly-one union, run on the
/// already-normalized, annotation-stripped rehearsal clone (see
/// [`sanitize_for_xai_parameters`]).
///
/// Structural pairwise-distinctness (reviews 5799828607, 5800814557,
/// 5802378120) reached its boundary: two more Registry→xAI forms pass it
/// while the union still serves no instance. Review 5802378120 (form A):
/// the registry pre-prune of branch `required` groups naming *cross-branch*
/// shared fields leaves two required-free object branches — pairwise
/// distinct, yet both match `{}`, so the exactly-one contract already
/// fails there. Review 5802577370 (form B): a nested `oneOf` the
/// recursive cleanup collapses into identical branches is unsatisfiable,
/// which kills its whole parent branch and with it the union's payload.
/// The judgment therefore walks every `oneOf` node (root or nested) and
/// fires — without becoming a general JSON-Schema solver — when:
///
/// - R1: two projected branches are deep-equal (every matching instance
///   matches both, every other matches neither — the union accepts
///   nothing),
/// - R2: at least two branches are required-free object branches — the
///   `required` group was the branch's only cross-branch discriminator,
///   and neither an optional property map nor any `minProperties` floor
///   restores the distinction (floors 0, 1 and 2 all leave every
///   shared-root-valid instance clearing them matching both alternatives;
///   reviews 5802378120, 5808313704, 5811366579),
/// - R2 (shared surviving group, review 5811598796): object branches with
///   equal surviving `required` groups and identically-normalized
///   shared-field entries (see the arm below),
/// - R2 (pairwise admission, reviews 5812488975, 5814012140): an
///   all-object union where some ordered branch pair stays
///   indistinguishable over the root-valid domain (see the arm below),
/// - R3: a branch's subtree contains a dead inner `oneOf` (the branch
///   cannot match, so the union loses that alternative's payload).
///
/// The walk distinguishes schema positions from instance data exactly
/// like [`strip_annotation_keywords`] (a property named `oneOf`, or
/// object data under `const`/`enum`, must never be read as a union), and
/// needs no artificial depth bound: the document is finite and `$ref`
/// values are strings that are never followed.
///
/// Returns `Some(reason)` naming the rule and structural path that
/// killed the union (paths carry keyword names and indices only, never
/// field names or instance values, so the string is safe to log), or
/// `None` when pass-through stays safe.
fn oneof_semantic_defect(schema: &Value) -> Option<String> {
    let mut walk = DefectWalk::default();
    walk.subtree_defect(schema, "root")
}

/// Counts actual computations, so tests can pin the single-traversal
/// bound (`xai_union_defect_walk_computes_each_subtree_once`).
#[cfg(test)]
fn oneof_semantic_defect_with_computations(schema: &Value) -> (Option<String>, usize) {
    let mut walk = DefectWalk::default();
    let reason = walk.subtree_defect(schema, "root");
    (reason, walk.computed)
}

/// Single-computation carrier for the defect walk. The R3 recursion and
/// the outer keyword walk used to visit the same branch subtrees
/// independently, so every healthy nesting level doubled the work —
/// exponential on a linear-size document (reviews 5814012140,
/// 5814046611). Memoizing per node keeps each subtree at one computation.
///
/// The tree is never mutated during the walk, so a node's address is a
/// stable key, and a `serde_json` tree has single-parent nodes: every
/// visit to the same node arrives with the same path (the R3 call site
/// formats `{path}.oneOf[{index}]`, exactly the path the outer walk
/// reaches that branch by), so cached reasons are path-exact.
#[derive(Default)]
struct DefectWalk {
    memo: std::collections::HashMap<usize, Option<String>>,
    computed: usize,
}

impl DefectWalk {
    fn subtree_defect(&mut self, schema: &Value, path: &str) -> Option<String> {
        let key = (schema as *const Value) as usize;
        if let Some(cached) = self.memo.get(&key) {
            return cached.clone();
        }
        self.computed += 1;
        let reason = self.compute_defect(schema, path);
        self.memo.insert(key, reason.clone());
        reason
    }

    fn compute_defect(&mut self, schema: &Value, path: &str) -> Option<String> {
        match schema {
            Value::Object(obj) => {
                if let Some(branches) = obj.get("oneOf").and_then(Value::as_array)
                    && let Some(reason) = oneof_node_defect(branches, obj, path, self)
                {
                    return Some(reason);
                }
                for (key, child) in obj {
                    match key.as_str() {
                        // Name → schema maps: field names are not schemas, so
                        // the map is never read as a union-bearing node.
                        "properties" | "$defs" | "patternProperties" | "dependentSchemas" => {
                            if let Some(map) = child.as_object() {
                                for subschema in map.values() {
                                    if let Some(reason) =
                                        self.subtree_defect(subschema, &format!("{path}.{key}.*"))
                                    {
                                        return Some(reason);
                                    }
                                }
                            }
                        }
                        // Instance data is never a union.
                        "default" | "const" | "enum" => {}
                        _ => {
                            if let Some(reason) =
                                self.subtree_defect(child, &format!("{path}.{key}"))
                            {
                                return Some(reason);
                            }
                        }
                    }
                }
                None
            }
            Value::Array(items) => {
                for (index, item) in items.iter().enumerate() {
                    if let Some(reason) = self.subtree_defect(item, &format!("{path}[{index}]")) {
                        return Some(reason);
                    }
                }
                None
            }
            _ => None,
        }
    }
}

/// The enclosing-schema context the pairwise admission arm normalizes
/// against: the object holding the `oneOf` keyword contributes its own
/// `required` names (instances the root validates carry them for free)
/// and its `properties` entries (the per-field constraints a witness
/// value must stay admissible under).
#[derive(Default)]
struct PairAdmission<'a> {
    enclosing: SchemaView<'a>,
    /// Whether the enclosing schema constrains instances through nothing
    /// beyond `type`/`properties`/`required` (own-level composition aside):
    /// every omission-witness arm (1–4) runs behind this gate, because a
    /// `minProperties` floor with `additionalProperties: false` can make
    /// every omission impossible while the keys stay otherwise ordinary
    /// (review 4580299113), and a sibling composition on the container is
    /// a same-instance assertion the bounded witness never reads (review
    /// 5940770432).
    enclosing_omission_clean: bool,
    /// Every branch of the union under judgment. The pin arm's witness
    /// must fail not only the pair's right branch but every OTHER branch
    /// together (review 4511007764): pairwise-distinct witnesses — one
    /// value outside the right branch here, a different value outside the
    /// right branch there — can coexist while no single instance works
    /// against all branches at once (`kind` enums `{a,b}`, `{b,c}`,
    /// `{a,c}` pass every ordered pair yet admit no value).
    branches: &'a [Value],
}

/// The bounded slice of a schema node the admission arms read: its
/// `required` names, its `properties` map, and whether the node carries
/// only structural keys — `type`, `properties`, `required`, plus the
/// union keywords themselves (the structure under judgment, not a
/// witness constraint). A sibling `allOf`/`not`/`minProperties` at the
/// node could reject the arms' constructed witnesses, so such nodes keep
/// their pairs unproven and the union takes the flattening fallback.
struct SchemaView<'a> {
    required: std::collections::BTreeSet<&'a str>,
    props: Option<&'a serde_json::Map<String, Value>>,
    structural: bool,
}

impl<'a> Default for SchemaView<'a> {
    fn default() -> Self {
        Self {
            required: std::collections::BTreeSet::new(),
            props: None,
            structural: true,
        }
    }
}

impl<'a> SchemaView<'a> {
    fn of(schema: Option<&'a Value>) -> Self {
        match schema.and_then(Value::as_object) {
            Some(obj) => Self::of_object(obj),
            // An absent entry constrains nothing, so a witness never
            // collides with it.
            None => Self::default(),
        }
    }

    fn of_object(obj: &'a serde_json::Map<String, Value>) -> Self {
        Self {
            required: obj
                .get("required")
                .and_then(Value::as_array)
                .map(|required| required.iter().filter_map(Value::as_str).collect())
                .unwrap_or_default(),
            props: obj.get("properties").and_then(Value::as_object),
            structural: obj.keys().all(|key| {
                matches!(
                    key.as_str(),
                    "type" | "properties" | "required" | "oneOf" | "anyOf"
                )
            }),
        }
    }
}

/// Whether a declared `type` string admits a concrete default a witness
/// could carry. Unknown or non-scalar spellings (type arrays, exotic
/// names) keep the conservative answer: no provable default.
fn type_has_default_value(expected: &str) -> bool {
    matches!(
        expected,
        "string" | "boolean" | "object" | "array" | "null" | "number" | "integer"
    )
}

/// Whether an entry constrains its field no further than a witness value
/// can satisfy: only `type`, `const`, and `enum` keys, each of which
/// admits a concrete value (a type default, the pinned value, an enum
/// member). A `minLength`, `pattern`, or any other validation keyword
/// could reject the witness, so the entry keeps its pair unproven.
/// Contradictory spellings are fail-closed too (reviews 4509283666 and
/// 4541468073): a `{"type":"string","const":42}` pin, a `const` outside a
/// co-present `enum`, an empty enum, or an enum whose members all violate
/// the declared type admits NO instance, so it is exactly the shape a
/// witness must not be proven against.
fn entry_is_value_permissive(entry: &Value) -> bool {
    let Some(obj) = entry.as_object() else {
        return false;
    };
    if !obj
        .keys()
        .all(|key| matches!(key.as_str(), "type" | "const" | "enum"))
    {
        return false;
    }
    let declared = obj.get("type").and_then(Value::as_str);
    if obj.contains_key("type") && declared.is_none_or(|expected| !type_has_default_value(expected))
    {
        return false;
    }
    if obj.contains_key("const") || obj.contains_key("enum") {
        return pinned_value_set(entry).is_some();
    }
    true
}

/// A concrete default the witness can carry for a declared type — the same
/// bounded grammar [`type_has_default_value`] admits, so callers only see
/// these spellings (`number` and `integer` share the integral default).
fn type_default_value(expected: &str) -> Value {
    match expected {
        "boolean" => Value::Bool(true),
        "object" => Value::Object(Map::new()),
        "array" => Value::Array(Vec::new()),
        "null" => Value::Null,
        "number" | "integer" => Value::Number(serde_json::Number::from(1)),
        _ => Value::String(String::new()),
    }
}

/// Whether two co-present entries for the SAME field admit one concrete
/// common value.
///
/// The omission witness carries the field once, so its value must satisfy
/// both entries; two entries that are value-permissive in isolation can
/// still have disjoint value sets — an enclosing `{"type":"string"}`
/// beside a branch `{"type":"number"}` — and proving each side
/// independently would then witness an instance no root-valid object can
/// realize, leaving the preserved root union unsatisfiable (review
/// 5886022727). Within the bounded grammar: pin sets (`const` / `enum`,
/// already jointly constrained by their own declared type) must
/// intersect; a pin set against a bare type entry needs one pinned value
/// of that type; two type entries need one shared default (`number` and
/// `integer` share `1`; disjoint spellings like `string` and `number`
/// share none). Entries outside that grammar fail closed first — a shared
/// verbatim `minLength` could still reject every default the grammar
/// could construct.
fn entries_admit_common_value(left: &Value, right: &Value) -> bool {
    // Anything outside the bounded grammar keeps the pair unproven, even
    // when both sides agree verbatim: a shared `minLength` could still
    // reject the grammar's type default.
    if !entry_is_value_permissive(left) || !entry_is_value_permissive(right) {
        return false;
    }
    let left_set = pinned_value_set(left);
    let right_set = pinned_value_set(right);
    let left_type = left.get("type").and_then(Value::as_str);
    let right_type = right.get("type").and_then(Value::as_str);
    match (left_set, right_set) {
        (Some(left), Some(right)) => left.iter().any(|candidate| {
            right
                .iter()
                .any(|allowed| schema_value_equal(allowed, candidate))
        }),
        (Some(values), None) => values.iter().any(|candidate| {
            right_type.is_none_or(|expected| json_value_is_type(candidate, expected))
        }),
        (None, Some(values)) => values.iter().any(|candidate| {
            left_type.is_none_or(|expected| json_value_is_type(candidate, expected))
        }),
        (None, None) => match (left_type, right_type) {
            (None, _) | (_, None) => true,
            (Some(expected), Some(other)) => {
                json_value_is_type(&type_default_value(expected), other)
            }
        },
    }
}

/// Whether a schema node's own keys are structural only (see
/// [`SchemaView`]) — the left side of an admission arm must be, or its
/// witness could be rejected by a keyword the arms never read.
fn shape_is_structural(schema: &Value) -> bool {
    schema.as_object().is_some_and(|obj| {
        obj.keys().all(|key| {
            matches!(
                key.as_str(),
                "type" | "properties" | "required" | "oneOf" | "anyOf"
            )
        })
    })
}

/// Whether the schema carries a composition keyword at its OWN level. The
/// witness arms evaluate only the outer `required`/`properties` and the
/// carried field entries' value pins, so an `anyOf` beside the admitted
/// keywords — the CR's shape (review 4574653781) — constrains the same
/// instance through constraints the arms never read, and the witness
/// stays unproven (fail closed to the flattening fallback). Own-level
/// composition is also the complete same-instance chain: `properties.*` /
/// `items` / `additionalProperties` subtrees are DIFFERENT instances
/// (their constraints are judged by the permissiveness checks), so
/// nothing deeper can constrain the witnessed object.
fn schema_carries_unproven_composition(schema: &Value) -> bool {
    schema.as_object().is_some_and(|obj| {
        obj.contains_key("oneOf") || obj.contains_key("anyOf") || obj.contains_key("allOf")
    })
}

/// The pin arm's value sets: `const` contributes the single value, `enum`
/// the whole list; an entry carrying further validation keywords beside
/// the pin could reject its own pinned value, so it stops being a
/// witness-able pin. Anything else is not an instance-data pin. A pin
/// that contradicts its own constraints keeps no witness-able value:
/// no instance satisfies it, so treating it as a pin would ship an
/// unsatisfiable union. That covers a `const` outside a co-present
/// `enum` (`{"const":"a","enum":["b"]}`, review 4541468073 — both
/// keywords apply, so the admitted set is their intersection), a
/// `const` contradicting its declared `type` (`{"type":"string","const":42}`,
/// review 4509283666), an empty enum, and an enum whose members all
/// violate the declared type. A non-string `type` spelling (type arrays)
/// is outside the bounded grammar and stays fail-closed.
fn pinned_value_set(entry: &Value) -> Option<Vec<Value>> {
    let obj = entry.as_object()?;
    if !obj
        .keys()
        .all(|key| matches!(key.as_str(), "type" | "const" | "enum"))
    {
        return None;
    }
    let declared = obj.get("type").and_then(Value::as_str);
    if obj.contains_key("type") && declared.is_none() {
        return None;
    }
    let mut values = if let Some(pinned) = obj.get("const") {
        vec![pinned.clone()]
    } else {
        let values = obj.get("enum").and_then(Value::as_array)?;
        if values.is_empty() {
            return None;
        }
        values.clone()
    };
    // `const` and `enum` constrain jointly: a co-present `enum` admits the
    // pinned const only when the const is one of its members.
    if let Some(allowed) = obj.get("enum").and_then(Value::as_array) {
        values.retain(|candidate| {
            allowed
                .iter()
                .any(|allowed| schema_value_equal(allowed, candidate))
        });
    }
    values.retain(|candidate| {
        declared.is_none_or(|expected| json_value_is_type(candidate, expected))
    });
    if values.is_empty() {
        return None;
    }
    Some(values)
}

/// Exact JSON Schema numeric equality. Integer/integer pairs compare
/// through their stored i64/u64 values — distinct pins such as
/// `9007199254740992` and `9007199254740993` both collapse to the same
/// f64, and a rounding comparison would declare a real discriminator
/// equal, flatten the union, and drop one branch's previously valid
/// input (review 4579521065). When a float participates, JSON Schema
/// still equates `1` with `1.0`, but an integer converts through f64
/// losslessly only within ±2^53; outside that window equality is UNKNOWN
/// (`None`), never evidence of inequality. Merge callers must abandon the
/// transformation when they encounter this result.
fn schema_number_equal(a: &serde_json::Number, b: &serde_json::Number) -> Option<bool> {
    let a_int = a
        .as_i64()
        .map(i128::from)
        .or_else(|| a.as_u64().map(i128::from));
    let b_int = b
        .as_i64()
        .map(i128::from)
        .or_else(|| b.as_u64().map(i128::from));
    if let (Some(x), Some(y)) = (a_int, b_int) {
        return Some(x == y);
    }
    let (fa, fb) = (a.as_f64(), b.as_f64());
    if fa != fb {
        return Some(false);
    }
    let converts_exactly = |int_side: Option<i128>| match int_side {
        Some(value) => value.abs() <= 9_007_199_254_740_992,
        None => true,
    };
    (converts_exactly(a_int) && converts_exactly(b_int)).then_some(true)
}

/// Whether two pin values are PROVABLY disjoint. Pairwise exclusion is an
/// affirmative proof, so unknown must not masquerade as disjoint: an
/// integer outside ±2^53 beside a float of the same rounded value is
/// mathematically equal or different beyond what f64 can decide, and
/// treating the comparison as unequal would "prove" a discriminator
/// between semantically equivalent branches and keep an unsatisfiable
/// union (review thread PRRT_kwDOTh_H9863shgR). Numbers with float
/// participation outside the exact window are therefore NOT provably
/// disjoint; compound values prove disjointness positively through
/// [`compound_values_provably_disjoint`] (review 5916351834).
fn pin_values_provably_disjoint(a: &Value, b: &Value) -> bool {
    match (a.as_number(), b.as_number()) {
        (Some(x), Some(y)) => numbers_provably_disjoint(x, y),
        _ => compound_values_provably_disjoint(a, b),
    }
}

/// The bounded numeric disjointness proof for a scalar pair: distinct
/// integers are provably distinct through their stored values, differing
/// f64 projections are provably distinct, and an f64-equal mixed/in-window
/// pair stays unproven (mathematically equal as far as the representation
/// decides).
fn numbers_provably_disjoint(x: &serde_json::Number, y: &serde_json::Number) -> bool {
    let xi = x
        .as_i64()
        .map(i128::from)
        .or_else(|| x.as_u64().map(i128::from));
    let yi = y
        .as_i64()
        .map(i128::from)
        .or_else(|| y.as_u64().map(i128::from));
    if let (Some(p), Some(q)) = (xi, yi) {
        return p != q;
    }
    let (fx, fy) = (x.as_f64(), y.as_f64());
    if fx != fy {
        return true;
    }
    // f64-equal with a float (or an in-window integer) side:
    // mathematically equal as far as the representation decides.
    false
}

/// Disjointness for compound (and leaf) pairs, proved POSITIVELY: two
/// containers are provably unequal when their shape differs or any child
/// pair is provably disjoint. Negating recursive equality would promote an
/// UNKNOWN child comparison — e.g. `9007199254740994` beside
/// `9007199254740994.0` wrapped in otherwise-equal arrays — into a fake
/// proof of disjointness, keeping equivalent branches and shipping an
/// unsatisfiable exactly-one (review 5916351834). Leaves route numeric
/// pairs through the bounded numeric proof; every other leaf kind has an
/// exact equality, so representation inequality is affirmative proof.
fn compound_values_provably_disjoint(a: &Value, b: &Value) -> bool {
    match (a, b) {
        (Value::Array(x), Value::Array(y)) => {
            x.len() != y.len()
                || x.iter()
                    .zip(y.iter())
                    .any(|(p, q)| compound_values_provably_disjoint(p, q))
        }
        (Value::Object(x), Value::Object(y)) => {
            x.len() != y.len()
                || x.keys().any(|key| !y.contains_key(key))
                || x.iter().any(|(key, value)| {
                    y.get(key)
                        .is_some_and(|other| compound_values_provably_disjoint(value, other))
                })
        }
        _ => match (a.as_number(), b.as_number()) {
            (Some(x), Some(y)) => numbers_provably_disjoint(x, y),
            _ => a != b,
        },
    }
}

/// JSON Schema semantic equality for pin values. JSON Schema `const`/`enum`
/// membership treats `1` and `1.0` as the same number, while serde_json's
/// derived equality compares across integer/float variants as unequal —
/// representation equality would falsely prove two semantically equivalent
/// branches disjoint and keep an unsatisfiable union (review 4578150947).
/// Boolean consumers retain their conservative unknown-as-not-equal behavior.
/// Schema intersection must instead use [`schema_value_comparison`] directly
/// so an unknown comparison aborts the merge rather than deleting literals.
fn schema_value_equal(left: &Value, right: &Value) -> bool {
    schema_value_comparison(left, right).unwrap_or(false)
}

/// Three-state JSON Schema equality: `Some(true)` is proven equal,
/// `Some(false)` is proven different, and `None` is undecidable with the
/// available numeric representation.
fn schema_value_comparison(left: &Value, right: &Value) -> Option<bool> {
    match (left, right) {
        (Value::Number(a), Value::Number(b)) => schema_number_equal(a, b),
        (Value::Array(a), Value::Array(b)) => {
            if a.len() != b.len() {
                return Some(false);
            }
            for (x, y) in a.iter().zip(b.iter()) {
                if !schema_value_comparison(x, y)? {
                    return Some(false);
                }
            }
            Some(true)
        }
        (Value::Object(a), Value::Object(b)) => {
            if a.len() != b.len() {
                return Some(false);
            }
            for (key, value) in a {
                let Some(other) = b.get(key) else {
                    return Some(false);
                };
                if !schema_value_comparison(value, other)? {
                    return Some(false);
                }
            }
            Some(true)
        }
        _ => Some(left == right),
    }
}

/// Bounded type check for the pin arm's enclosing-admissibility test.
/// `integer` only admits integral numbers, so a float candidate fails it
/// conservatively.
fn json_value_is_type(candidate: &Value, expected: &str) -> bool {
    match expected {
        "string" => candidate.is_string(),
        "boolean" => candidate.is_boolean(),
        "object" => candidate.is_object(),
        "array" => candidate.is_array(),
        "null" => candidate.is_null(),
        "number" => candidate.is_number(),
        "integer" => candidate.is_i64() || candidate.is_u64(),
        _ => false,
    }
}

/// Type check for the instance-data narrowing path. It must agree with the
/// module's own MFJS integer ruling ([`mfjs_integer_value`]: finite, zero
/// fractional part, within the interop window), under which an f64 spelling
/// with a zero fractional part IS an integral value on the wire. Raw serde
/// integrality — [`json_value_is_type`]'s stricter witness-proof test —
/// would silently drop such a member from a narrowed enum and shrink the
/// contract the original schema admitted (round-4 review).
fn instance_data_is_type(candidate: &Value, expected: &str) -> bool {
    match expected {
        "integer" => mfjs_integer_value(candidate).is_some(),
        _ => json_value_is_type(candidate, expected),
    }
}

/// A branch's surviving `required` group: the string names, sorted and
/// deduplicated.
/// The branch's surviving required group when it is non-empty.
fn constrained_group(branch: &Value) -> Option<Vec<&str>> {
    let group = branch_required_names(branch);
    (!group.is_empty()).then_some(group)
}

fn branch_required_names(branch: &Value) -> Vec<&str> {
    let mut names: Vec<&str> = branch
        .get("required")
        .and_then(Value::as_array)
        .map(|required| required.iter().filter_map(Value::as_str).collect())
        .unwrap_or_default();
    names.sort_unstable();
    names.dedup();
    names
}

impl<'a> PairAdmission<'a> {
    /// Conservative admission domain for one ordered branch pair over the
    /// root-valid domain (reviews 5812488975, 5814012140): the pair is
    /// admissible only when a bounded arm proves an instance exists that
    /// matches `left` and fails `right` — otherwise every root-valid
    /// instance may match both and exactly-one holds for no instance.
    /// This is deliberately NOT a general subsumption solver: each arm's
    /// witness is an omission witness (carry the left branch's group plus
    /// everything the enclosing schema requires, omit one discriminating
    /// name — or pin one discriminating value on a shared field), and
    /// only a value-permissive left shape can prove such a witness (see
    /// [`Self::witness_provable`]). Anything unproven takes the
    /// protective flattening fallback, which keeps the shared root
    /// properties usable.
    #[allow(clippy::too_many_arguments)]
    fn pair_discriminated(
        &self,
        left_index: usize,
        left: &Value,
        right: &Value,
        left_group: &[&str],
        right_group: &[&str],
        depth: usize,
    ) -> bool {
        // Arm 1 — group discrimination: the right branch requires a name
        // the left branch does not and the enclosing schema does not
        // either, so a root-valid instance may omit it (failing the right
        // branch) while carrying the left branch's group and everything
        // the enclosing schema requires. This also kills the review
        // 5814012140 form: two different groups the root requires in full.
        //
        // The enclosing bound is the same `enclosing_omission_clean` gate
        // Arms 3/4 already run behind: a sibling composition on the object
        // holding the judged union is a same-instance assertion the
        // bounded witness never reads (review 5940770432). A raw-MCP `not`
        // predicate there declares its `required` with no local
        // `properties`, so the recursive prune guts it into `not:{}` — an
        // alternative that rejects EVERY instance, the constructed
        // omission witness included — while the witness proof only sees
        // the container's `required`/`properties` entries and would call
        // the pair discriminated. The union then takes the flattening
        // fallback, whose merged root keeps the original instance usable.
        if self.enclosing_omission_clean
            && self.witness_provable(left, left_group, &self.enclosing)
            && right_group
                .iter()
                .any(|name| !left_group.contains(name) && !self.enclosing.required.contains(name))
        {
            return true;
        }
        let (Some(left_props), Some(right_props)) = (
            left.get("properties").and_then(Value::as_object),
            right.get("properties").and_then(Value::as_object),
        ) else {
            return false;
        };
        for (name, left_entry) in left_props {
            let Some(right_entry) = right_props.get(name.as_str()) else {
                continue;
            };
            // Arm 2 — pin discrimination: both sides pin the field's
            // value set (`const`/`enum`), and ONE left-admitted value
            // survives the enclosing entry for the field AND every other
            // branch's pin together (review 4511007764). Ordered pairs
            // checked in isolation could each pick a different witness —
            // `kind` enums `{a,b}`, `{b,c}`, `{a,c}` pass every pair that
            // way — while no instance works against all branches at once.
            // The witness value must therefore be absent from every other
            // branch's pin set for this field; a branch that does not pin
            // the field cannot be proven to reject the witness, so the
            // pair stays unproven and the union takes the flattening
            // fallback (fail closed).
            if let (Some(left_set), Some(right_set)) =
                (pinned_value_set(left_entry), pinned_value_set(right_entry))
                // The same enclosing bound as Arm 1 (review 5940770432):
                // a sibling composition on the container could reject the
                // pinned witness just as it rejects the omission witness.
                && self.enclosing_omission_clean
                && self.witness_provable(left, left_group, &self.enclosing)
                && left_set.iter().any(|candidate| {
                    right_set
                        .iter()
                        .all(|value| pin_values_provably_disjoint(value, candidate))
                        && self.enclosing_entry_admits(name.as_str(), candidate)
                        && self.branches.iter().enumerate().all(|(index, other)| {
                            index == left_index
                                || other
                                    .get("properties")
                                    .and_then(|props| props.get(name.as_str()))
                                    .and_then(pinned_value_set)
                                    .is_some_and(|set| {
                                        set.iter().all(|value| {
                                            pin_values_provably_disjoint(value, candidate)
                                        })
                                    })
                        })
                })
            {
                return true;
            }
            // Arm 3 — nested group discrimination (depth-bounded): both
            // entries are object schemas whose own surviving groups differ
            // by a name neither entry requires and the enclosing entry for
            // the field does not require either, so the sub-instance may
            // omit it and fail the right entry while carrying the left
            // entry's group. This keeps genuinely self-contained nested
            // discriminators (the satisfiable control of
            // `xai_sanitize_flattens_oneof_distinct_only_before_recursive_normalization`)
            // shipping, while a root-stronger or root-required nested
            // group stays kill-worthy.
            let enclosing_entry = SchemaView::of(
                self.enclosing
                    .props
                    .and_then(|props| props.get(name.as_str())),
            );
            if depth < 8
                // The witness is a ROOT-valid object: the containing root
                // must stay within the bounded keyword model (a
                // `maxProperties` floor can forbid carrying the nested
                // witness beside the branch's required names, and a closed
                // root can forbid the branch-only field entirely — review
                // 5928255936), and the whole containing object — the
                // branch's own required names plus everything the root
                // requires, with the nested witness as the field's value —
                // must be constructible against the root's own entries the
                // same way the group arms prove their root-level witnesses.
                && self.enclosing_omission_clean
                && self.arm3_root_witness_provable(left, left_group, name.as_str())
                && self.enclosing_entry_admits_object_witness(name.as_str())
                && is_object_branch(left_entry)
                && is_object_branch(right_entry)
                && self.witness_provable(
                    left_entry,
                    &branch_required_names(left_entry),
                    &enclosing_entry,
                )
                && nested_entries_discriminated(left_entry, right_entry, &enclosing_entry)
            {
                return true;
            }
        }
        // Arm 4 — broad-matcher pair (review thread PRRT_kwDOTh_H9863sevQ):
        // one branch constrains nothing beyond objectness (structural keys,
        // no surviving required group, no property entries) while the other
        // carries a non-empty surviving required group. Instances omitting
        // that group match the broad branch and fail the constrained one,
        // so the union's exactly-one accepted set is non-empty and
        // pass-through preserves the original contract verbatim — while
        // the flattening fallback merges the constrained branch's fields
        // into the root and silently widens it ({"x":"v"} flipping from
        // rejected to accepted). The multi-branch pin overlap that demands
        // two-directional proofs is untouched: those branches carry their
        // own required groups, so no branch here is broad.
        let broad = |branch: &Value| -> bool {
            // Own-level composition is excluded along with the rest: a
            // branch carrying an `anyOf` whose entries require the sibling's
            // group accepts exactly the sibling's objects while this
            // predicate would otherwise call it broad, and the pairwise
            // proof preserves an impossible union (review 4579889873) —
            // the same-instance blindness the witness guard exists for.
            !schema_carries_unproven_composition(branch)
                && shape_is_structural(branch)
                && branch
                    .get("required")
                    .and_then(Value::as_array)
                    .is_none_or(|required| required.is_empty())
                && branch
                    .get("properties")
                    .and_then(Value::as_object)
                    .is_none_or(|props| props.is_empty())
        };

        // The omission witness must stay ROOT-VALID: a group name the
        // enclosing schema itself requires cannot be omitted, and when the
        // enclosing schema requires the constrained branch's whole group,
        // every root-valid instance matches both branches and exactly-one
        // accepts nothing over the root-valid domain (review 4580074704 —
        // the valid pre-prune dependentRequired example stays
        // reproducible). The arm fires only when the group carries at
        // least one name free of the enclosing requirement.
        let free_of_enclosing = |group: &[&str]| -> bool {
            group
                .iter()
                .any(|name| !self.enclosing.required.contains(name))
        };
        if !self.enclosing_omission_clean {
            return false;
        }
        if let Some(group) = constrained_group(right)
            && broad(left)
            && free_of_enclosing(&group)
        {
            return true;
        }
        if let Some(group) = constrained_group(left)
            && broad(right)
            && free_of_enclosing(&group)
        {
            return true;
        }
        false
    }

    /// Whether the arms' omission witness exists for `left` under
    /// `enclosing`: an instance carrying `left_group` plus every name in
    /// the enclosing `required` is only guaranteed to match both when
    /// both shapes carry structural keys only and every carried field's
    /// entry is value-permissive (a concrete satisfying value provably
    /// exists: a type default, the pinned value, an enum member). A
    /// restrictive `additionalProperties` floor, a `minLength` on a
    /// carried field, or any other validation keyword could reject the
    /// witness, so the pair stays unproven and the union takes the
    /// flattening fallback.
    fn witness_provable(
        &self,
        left: &Value,
        left_group: &[&str],
        enclosing: &SchemaView<'a>,
    ) -> bool {
        if !shape_is_structural(left) || !enclosing.structural {
            return false;
        }
        // A composition keyword at the left shape's own level constrains
        // the same instance through constraints the witness arms never
        // read (review 4574653781): the group arm builds an omission
        // witness from the outer `required`/`properties` alone, so two
        // branches whose nested `anyOf` entries require the OTHER field
        // are semantically equivalent while both omission witnesses appear
        // provable. An unproven composition keeps the pair unproven — the
        // union takes the protective flattening fallback instead of
        // shipping an unsatisfiable exactly-one.
        if schema_carries_unproven_composition(left) {
            return false;
        }
        let left_props = left.get("properties").and_then(Value::as_object);
        left_group
            .iter()
            .copied()
            .chain(enclosing.required.iter().copied())
            .all(|field| {
                let left_entry = left_props.and_then(|props| props.get(field));
                let enclosing_entry = enclosing.props.and_then(|props| props.get(field));
                match (left_entry, enclosing_entry) {
                    (None, None) => true,
                    (Some(entry), None) | (None, Some(entry)) => entry_is_value_permissive(entry),
                    // The witness carries the field once, so one value
                    // must satisfy BOTH entries; two entries that are
                    // permissive in isolation can still admit no common
                    // value, and each side proved independently would
                    // then witness an instance no root-valid object can
                    // realize.
                    (Some(left_entry), Some(enclosing_entry)) => {
                        entries_admit_common_value(left_entry, enclosing_entry)
                    }
                }
            })
    }

    /// Arm 2 witness check: is `candidate` admissible under the enclosing
    /// schema's own entry for this field? Only trivially-checkable entry
    /// shapes admit a witness (absent, empty, a matching `const`, a
    /// matching `enum` membership, or a matching `type`); anything with
    /// further validation keywords refuses to prove the witness and the
    /// pair falls back to flattening.
    fn enclosing_entry_admits(&self, name: &str, candidate: &Value) -> bool {
        let Some(entry) = self.enclosing.props.and_then(|props| props.get(name)) else {
            return true;
        };
        let Some(obj) = entry.as_object() else {
            return false;
        };
        if obj.is_empty() {
            return true;
        }
        if obj.len() != 1 {
            return false;
        }
        if let Some(pinned) = obj.get("const") {
            return schema_value_equal(pinned, candidate);
        }
        if let Some(values) = obj.get("enum").and_then(Value::as_array) {
            return values
                .iter()
                .any(|value| schema_value_equal(value, candidate));
        }
        if let Some(expected) = obj.get("type").and_then(Value::as_str) {
            return json_value_is_type(candidate, expected);
        }
        false
    }

    /// Arm 3 witness root-validity: the nested witness sub-instance is an
    /// OBJECT (both entries are object branches), so the CONTAINING root
    /// field's own value constraint must admit object sub-instances. A root
    /// declaring `payload: {type: string}` while the branches declare
    /// `payload` as objects makes every root-valid sub-instance a string —
    /// the nested witness matches no root-valid instance and the projected
    /// union keeps no usable argument (review 5916351834). Only entries
    /// within the bounded model (declared `type` absent or `"object"`, plus
    /// the object-schema keywords the [`SchemaView`] checks already
    /// validate the witness against) admit the witness; anything else — a
    /// scalar `type`, a `const`/`enum`, a `minProperties` floor — could
    /// reject the object witness and takes the protective flattening
    /// fallback.
    fn enclosing_entry_admits_object_witness(&self, name: &str) -> bool {
        let Some(entry) = self.enclosing.props.and_then(|props| props.get(name)) else {
            return true;
        };
        let Some(obj) = entry.as_object() else {
            return false;
        };
        let type_admits = match obj.get("type") {
            None => true,
            Some(Value::String(expected)) => expected == "object",
            // Type arrays and other exotic spellings are outside the
            // bounded model.
            Some(_) => false,
        };
        type_admits
            && obj
                .keys()
                .all(|key| matches!(key.as_str(), "type" | "properties" | "required"))
    }

    /// Whether a ROOT-valid containing object exists for the Arm 3 nested
    /// witness: the branch's own required names plus everything the root
    /// requires, carried once, with the containing field's value being the
    /// sub-instance the nested proof already constructs. Fields other than
    /// the containing one go through the scalar common-value grammar; the
    /// containing field itself is covered by
    /// [`Self::enclosing_entry_admits_object_witness`] plus the nested
    /// proof, and the root's keyword boundedness is the
    /// `enclosing_omission_clean` gate (a `maxProperties` floor or a closed
    /// root can otherwise forbid carrying the witness at all — review
    /// 5928255936).
    fn arm3_root_witness_provable(&self, left: &Value, left_group: &[&str], name: &str) -> bool {
        if !shape_is_structural(left) || !self.enclosing.structural {
            return false;
        }
        if schema_carries_unproven_composition(left) {
            return false;
        }
        let left_props = left.get("properties").and_then(Value::as_object);
        left_group
            .iter()
            .copied()
            .chain(self.enclosing.required.iter().copied())
            .filter(|field| *field != name)
            .all(|field| {
                let left_entry = left_props.and_then(|props| props.get(field));
                let enclosing_entry = self.enclosing.props.and_then(|props| props.get(field));
                match (left_entry, enclosing_entry) {
                    (None, None) => true,
                    (Some(entry), None) | (None, Some(entry)) => entry_is_value_permissive(entry),
                    (Some(left_entry), Some(enclosing_entry)) => {
                        entries_admit_common_value(left_entry, enclosing_entry)
                    }
                }
            })
    }
}

/// Arm 3 check: the entries' own `required` groups differ by a name
/// neither entry requires and the enclosing entry for the field does not
/// require either.
fn nested_entries_discriminated(
    left_entry: &Value,
    right_entry: &Value,
    enclosing_entry: &SchemaView<'_>,
) -> bool {
    let left_group = branch_required_names(left_entry);
    let right_group = branch_required_names(right_entry);
    right_group.iter().any(|candidate| {
        !left_group.contains(candidate) && !enclosing_entry.required.contains(candidate)
    })
}

/// R2 (pairwise admission): an all-object union ships only when EVERY
/// ordered branch pair stays discriminable over the root-valid domain by
/// one of the bounded [`PairAdmission`] arms. Reviews 5812488975 and
/// 5814012140 each shipped a form the earlier arms missed: a root
/// strictly stronger than both branch-local shared-field constraints
/// (`minLength: 2` dominating `{}` and `{"type":"string"}` leaves unequal
/// signatures that both admit every root-valid instance), and two
/// different surviving groups the enclosing root already requires in
/// full. The judgment runs on the post-preprocessing projection — the
/// shape that actually reaches the wire — and a union with any
/// non-object branch is outside the domain (its exactly-one analysis is
/// a general schema question; R1 and the required-free arm still cover
/// its pinned degenerate shapes).
fn unions_lack_pairwise_discrimination(
    branches: &[Value],
    enclosing: &serde_json::Map<String, Value>,
    path: &str,
) -> Option<String> {
    if branches.iter().any(|branch| !is_object_branch(branch)) {
        return None;
    }
    let admission = PairAdmission {
        enclosing: SchemaView::of_object(enclosing),
        enclosing_omission_clean: enclosing
            .keys()
            .all(|key| matches!(key.as_str(), "type" | "properties" | "required" | "oneOf")),
        branches,
    };
    let groups: Vec<Vec<&str>> = branches.iter().map(branch_required_names).collect();
    for (left, left_group) in groups.iter().enumerate() {
        for (right, right_group) in groups.iter().enumerate() {
            if left == right {
                continue;
            }
            if !admission.pair_discriminated(
                left,
                &branches[left],
                &branches[right],
                left_group,
                right_group,
                0,
            ) {
                return Some(format!(
                    "R2 branches {left} and {right} are not pairwise discriminated \
                     over the root-valid domain at {path}"
                ));
            }
        }
    }
    None
}

/// Judge one `oneOf` node's branches by R1/R2/R3; `enclosing` is the
/// object holding the `oneOf` keyword, whose `properties` (when present)
/// is the root-level constraint set the shared-group arm normalizes
/// branch-local entries against and whose `required`/`properties` feed
/// the pairwise admission arm.
fn oneof_node_defect(
    branches: &[Value],
    enclosing: &serde_json::Map<String, Value>,
    path: &str,
    walk: &mut DefectWalk,
) -> Option<String> {
    // R1: identical branches.
    for (left, left_branch) in branches.iter().enumerate() {
        for (right, right_branch) in branches.iter().enumerate().skip(left + 1) {
            if left_branch == right_branch {
                return Some(format!(
                    "R1 identical branches {left} and {right} at {path}"
                ));
            }
        }
    }
    // R2: two required-free object branches break the exactly-one contract.
    // A branch without a surviving `required` group constrains instances
    // only through its optional property map and any `minProperties`
    // floor; two such branches admit a common instance set (everything
    // clearing both floors whose fields lie in the union of their maps —
    // which the shared root's own fields already are) and reject the rest
    // together, so exactly-one holds for no instance. Floors cannot
    // restore the distinction (review 5811366579: floors 0, 1 and 2 on
    // the erased cross-required shape all leave every admitted instance
    // matching both alternatives), and whether a group was never declared
    // or erased by the registry/rehearsal pruning is indistinguishable at
    // this layer — the xAI pass sees the schema after the registry's own
    // sanitize has already dropped cross-branch groups (reviews
    // 5802378120, 5808313704). Required-free shapes take the compatible
    // flattening fallback.
    // A required-free branch is exempt from the count when its property
    // pins on some shared field are disjoint from every other
    // required-free branch's pins on that same field (reviews
    // 4509301139 and 4509370633): `oneOf` branches discriminated by
    // `kind: {"const":"email"}` vs `{"const":"sms"}` — no surviving
    // `required`, yet each pin value matches exactly one branch — are a
    // usable documented union, and flattening it would keep only the
    // first branch's `kind` pin and reject the other variant. The pins
    // go through `pinned_value_set`, so a contradictory or empty pin
    // fails the exemption and the branch stays counted (fail closed).
    // The exempted pairs must then still be PROVEN discriminable by the
    // pairwise admission below — where the pin arm now demands a single
    // witness that survives every other branch together — so an
    // overlapping multi-branch pin set still falls back instead of
    // shipping an unsatisfiable union.
    let required_free: Vec<&Value> = branches
        .iter()
        .filter(|branch| empty_matching_object_branch(branch))
        .collect();
    let pin_discriminated = |left: &Value, right: &Value| -> bool {
        let (Some(left_props), Some(right_props)) = (
            left.get("properties").and_then(Value::as_object),
            right.get("properties").and_then(Value::as_object),
        ) else {
            return false;
        };
        left_props.iter().any(|(name, left_entry)| {
            let Some(left_set) = pinned_value_set(left_entry) else {
                return false;
            };
            let Some(right_set) = right_props.get(name.as_str()).and_then(pinned_value_set) else {
                return false;
            };
            left_set
                .iter()
                .all(|candidate| !right_set.contains(candidate))
        })
    };
    let non_discriminating = required_free
        .iter()
        .filter(|branch| {
            !required_free
                .iter()
                .all(|other| std::ptr::eq(**branch, *other) || pin_discriminated(branch, other))
        })
        .count();
    if non_discriminating >= 2 {
        return Some(format!(
            "R2 {non_discriminating} non-discriminating branches at {path}"
        ));
    }
    // R2 (shared surviving group, review 5811598796): object branches whose
    // surviving `required` groups are EQUAL are non-discriminating when
    // they also constrain every field identically. Partially pruned
    // groups keep exactly this shape: the cross-branch discriminators
    // (`b` / `a`) are dropped because the branch's own `properties` does
    // not declare them, while the locally declared common requirement
    // (`c`) survives on both branches — and both branches describe `c`
    // the same way (`{type:"string"}`). Every root-valid instance then
    // matches both alternatives: it always carries the shared group, each
    // branch's property map admits the sibling's fields as additional,
    // and the identical shared-field constraints accept the same values —
    // so no instance matches exactly one. Branch-local shared-field
    // entries that the containing root already implies (vacuous `{}`, or
    // identical to the root's own schema for that field) normalize to
    // absent, so root-implied variants of the same surviving group
    // compare equal (review 5812238335). The signature reads every local
    // property, not just the shared group's fields: branches whose
    // OPTIONAL pins diverge (`kind` const `email` vs `sms`) stay
    // untouched, because each tagged instance matches exactly one branch
    // and the pairwise pin proof below is what judges them (review
    // 5886022727 — collapsing such branches to one signature flattened a
    // usable union and kept only one pin). Branches whose constraints on
    // the shared fields still DIVERGE after that normalization likewise
    // stay untouched: a const/enum pin
    // (`xai_sanitize_passes_const_differentiated_union_through_without_touching_consts`)
    // or a nested subconstraint
    // (`xai_sanitize_flattens_oneof_distinct_only_before_recursive_normalization`)
    // keeps exactly-one meaningful for the instances it differentiates,
    // and comparing subschema equivalence beyond deep-equality would be a
    // general solver. Branches with different groups are likewise
    // untouched: an instance carrying one group but not the other still
    // matches exactly one, and instances carrying both were already
    // ambiguous in the original (the union's own semantics, not a
    // preprocessing loss).
    let root_props = enclosing.get("properties").and_then(Value::as_object);
    let mut group_shapes: std::collections::BTreeMap<(String, String), usize> =
        std::collections::BTreeMap::new();
    for branch in branches {
        if branch.get("type").and_then(Value::as_str) != Some("object") {
            continue;
        }
        let Some(required) = branch.get("required").and_then(Value::as_array) else {
            continue;
        };
        if required.is_empty() {
            continue;
        }
        let mut names: Vec<&str> = required.iter().filter_map(Value::as_str).collect();
        names.sort_unstable();
        names.dedup();
        if names.is_empty() {
            continue;
        }
        // Constraint signature over the branch's whole property map: the
        // SHARED required fields carry two normalizations for constraints
        // the containing root already implies for every root-valid
        // instance (review 5812238335): a vacuous `{}` entry constrains
        // nothing, and an entry identical to the root's own schema for
        // that field adds nothing the root has not already required. Both
        // normalize to absent so root-implied variants compare equal; any
        // other divergence keeps its verbatim shape and the branch stays
        // untouched (conservative pass-through on that bounded fail-open
        // remainder, disclosed in review 5811598796's reply — deeper
        // subset relations are a solver). Every OTHER local property
        // ships verbatim (review 5886022727): an optional pin
        // (`kind: {"const":"email"}` vs `{"const":"sms"}`) discriminates
        // the branches — each tagged instance matches exactly one — so
        // branches differing there must not compare equal, or this arm
        // declares them equivalent before the pairwise pin proof can run,
        // the flattening fallback keeps only one pin, and the other valid
        // variant is rejected with no note. Annotation-only divergence is
        // already stripped upstream, so verbatim here is
        // validation-relevant; grouped signatures stay deterministic
        // through the BTreeMap ordering.
        let signature = match branch.get("properties").and_then(Value::as_object) {
            Some(props) => {
                let mut shared = std::collections::BTreeMap::new();
                for (key, value) in props {
                    if names.contains(&key.as_str()) {
                        let vacuous = value.as_object().is_some_and(|entry| entry.is_empty());
                        let root_identical = root_props
                            .and_then(|rp| rp.get(key.as_str()))
                            .is_some_and(|root_entry| root_entry == value);
                        if !vacuous && !root_identical {
                            shared.insert(key.clone(), value.clone());
                        }
                    } else {
                        shared.insert(key.clone(), value.clone());
                    }
                }
                Value::Object(shared.into_iter().collect())
            }
            None => Value::Object(serde_json::Map::new()),
        };
        let group_key = (names.join("\u{1}"), signature.to_string());
        *group_shapes.entry(group_key).or_default() += 1;
    }
    if let Some(((group, _), count)) = group_shapes.iter().find(|(_, count)| **count >= 2) {
        return Some(format!(
            "R2 {count} branches share the required group {group:?} at {path}"
        ));
    }
    // R3: a branch carrying a dead inner union cannot match. Named BEFORE
    // the pairwise admission arm: the witness guard (review 4574653781)
    // leaves every branch carrying nested composition unproven, so a dead
    // inner union would otherwise always be shadowed by the coarser R2
    // naming — the R3 recursion is the more specific diagnosis of the
    // same defect, and both rules flatten (this function only labels;
    // the production caller flattens whenever any rule fires).
    for (index, branch) in branches.iter().enumerate() {
        if let Some(inner) = walk.subtree_defect(branch, &format!("{path}.oneOf[{index}]")) {
            return Some(format!(
                "R3 branch {index} at {path} carries a dead inner union: {inner}"
            ));
        }
    }
    // R2 (pairwise admission, reviews 5812488975 and 5814012140): the
    // shared-group arm above only fires on signatures that normalize
    // equal. Two more registry forms slip past it while the union serves
    // no root-valid instance: a root STRICTLY STRONGER than both
    // branch-local shared-field entries (unequal signatures, both
    // implied), and two different surviving groups the enclosing root
    // already requires in full. Every ordered pair must therefore stay
    // discriminable by a bounded admission arm; the first unproven pair
    // takes the compatible flattening fallback.
    if let Some(reason) = unions_lack_pairwise_discrimination(branches, enclosing, path) {
        return Some(reason);
    }
    None
}

/// R2 trigger: an object branch with no surviving `required` group. The
/// group was the branch's only cross-branch discriminator; whatever
/// remains (an optional property map, any `minProperties` floor) admits a
/// common tail of the shared root's instances with every sibling
/// required-free branch. Floors are deliberately NOT consulted: they
/// cannot restore an erased group, and a floor's only sound role
/// (excluding `{}` for a never-required branch — review 5811366579
/// corrected the earlier reading that floor 1 admits the empty object; it
/// excludes it) still leaves the pair matching each other on every
/// shared-root-valid instance above the floors.
fn empty_matching_object_branch(branch: &Value) -> bool {
    branch.get("type").and_then(Value::as_str) == Some("object")
        && branch
            .get("required")
            .and_then(Value::as_array)
            .is_none_or(|required| required.is_empty())
}

/// Whether the schema's root is exactly a single `oneOf`/`anyOf` union whose
/// every branch declares `type: "object"` — the shape xAI documents as valid
/// for a parameters root ("A root `anyOf` or `oneOf` also works when every
/// branch is itself an object").
///
/// Any root whose composition is not a single documented-good union stays on
/// the protective flattening pass: sibling root `allOf`/`enum`/`not`, both
/// union keywords at once (the doc names one union, not an intersection), a
/// present-but-malformed (non-array) union value, an empty union,
/// a declared root `type` other than `"object"` (a non-object root cannot
/// satisfy an object branch, so the union would ship with no valid
/// instance — review 5886022727), and branches without an explicit object
/// `type` — xAI's own 400 message for
/// `apply_patch`'s bare required-groups called that union "an anyOf/oneOf
/// union with a non-object branch". Other root-level keywords (annotations,
/// `$defs`, `$ref`, `if`/`then`/`else`, …) are not root composition
/// operators: they ride along beside the union exactly as they rode the
/// flattening pass before.
fn root_union_is_all_object_branches(schema: &Value) -> bool {
    let Some(obj) = schema.as_object() else {
        return false;
    };
    if obj.contains_key("allOf") || obj.contains_key("enum") || obj.contains_key("not") {
        return false;
    }
    // The root's own declared `type` participates too (review 5886022727):
    // a declared non-object root cannot satisfy an object branch, so
    // pass-through would ship a root whose every instance fails. Absent
    // types stay eligible — the preserved branches force the effective
    // object shape — while an explicitly incompatible or malformed root
    // takes the compatibility fallback, which forces `type: "object"` and
    // merges the branch properties into a usable object.
    if obj
        .get("type")
        .is_some_and(|declared| declared.as_str() != Some("object"))
    {
        return false;
    }
    let mut unions = 0;
    for key in ["oneOf", "anyOf"] {
        let Some(branches) = obj.get(key).and_then(Value::as_array) else {
            if obj.contains_key(key) {
                // Present but not an array: the flattening pass removes the
                // malformed key instead of shipping it to the wire.
                return false;
            }
            continue;
        };
        if branches.is_empty() || branches.iter().any(|branch| !is_object_branch(branch)) {
            return false;
        }
        unions += 1;
    }
    unions == 1
}

fn is_object_branch(branch: &Value) -> bool {
    branch.get("type").and_then(Value::as_str) == Some("object")
}

/// Whether the shared normalization pass keeps every branch `required` of the
/// root union intact.
///
/// `prune_dangling_required` drops `required` entries the branch's own
/// `properties` does not declare, and `inject_properties_on_bare_objects`
/// gives a bare object branch an empty `properties` first — so a branch whose
/// group constraint is a bare `required` over the root's shared `properties`
/// reaches the wire constraint-free. In an exactly-one `oneOf`, identically
/// gutted branches accept the same instances, so the union admits no
/// instance at all; such a union must fall back to the flattening pass,
/// which re-derives the parameter-groups note from the pristine schema
/// before the prune runs.
fn union_branches_keep_required(schema: &Value) -> bool {
    let Some(obj) = schema.as_object() else {
        return true;
    };
    for key in ["oneOf", "anyOf"] {
        let branches_keep_required = obj
            .get(key)
            .and_then(Value::as_array)
            .map(|branches| {
                branches
                    .iter()
                    .all(|branch| !branch_required_would_be_pruned(branch))
            })
            // A present-but-malformed union value never reaches here (the
            // shape guard already routed it to the flattening pass).
            .unwrap_or(true);
        if !branches_keep_required {
            return false;
        }
    }
    true
}

/// Mirror `prune_dangling_required`: `true` when at least one `required`
/// entry of this schema node would be pruned because it is not a key of the
/// node's own `properties`.
fn branch_required_would_be_pruned(branch: &Value) -> bool {
    let Some(obj) = branch.as_object() else {
        return false;
    };
    let Some(required) = obj.get("required").and_then(Value::as_array) else {
        return false;
    };
    let known_keys: Vec<&String> = obj
        .get("properties")
        .and_then(Value::as_object)
        .map(|props| props.keys().collect())
        .unwrap_or_default();
    let known_keys: std::collections::HashSet<&str> =
        known_keys.into_iter().map(|known| known.as_str()).collect();
    required
        .iter()
        .any(|entry| !entry.as_str().is_some_and(|name| known_keys.contains(name)))
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
    if !schema_within_structural_depth_limit(schema) {
        return schema
            .as_object()
            .and_then(root_composition_constraint_note);
    }
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
    // The map here is derived from the merged composition arms — the
    // schema's own post-collapse declaration — so it keeps local authority.
    prune_dangling_required(schema, &[], true);
    combine_constraint_notes(constraint_note, dependent_note)
}

/// Narrow the root-composition keywords the Anthropic Messages validator is
/// recorded rejecting, and leave every other keyword untouched.
///
/// The genuine Anthropic endpoint has returned HTTP 400 "input_schema does
/// not support oneOf, allOf, or anyOf at the top level" for tool schemas
/// carrying a root-level composition since at least 2025-07: commit
/// `f8e757322` quotes a live failure, CHANGELOG 0.9.0 records the same
/// rationale, and anthropics/claude-code#4886 / #30212 report the identical
/// server-side error. Unlike [`sanitize_for_responses`], this pass strips
/// only those three root keywords: root `enum`/`not`,
/// `dependentRequired`/`dependentSchemas`, and the declared root type ship
/// verbatim because no record rejects them. Root composition branch
/// properties are merged into the object and the dropped exactly-one /
/// at-least-one constraint is surfaced as a description note so the model
/// still knows which parameter groups apply. Every stripped composition is
/// surfaced: its specific required-group note when the branches carry two
/// or more labeled groups, otherwise the generic sentence — merged
/// properties or not (review 4539837534): a union of
/// `additionalProperties:false` branches whose properties merge into the
/// root accepts a mixture no original branch admits, so the model must
/// still be told the branch contract was relaxed. The notes aggregate
/// across all stripped keywords (review 5898539144): a first-match note
/// left a root stacking `oneOf` and `anyOf` advertising only the
/// exactly-one contract while the wire schema dropped both.
///
/// The shared [`sanitize`] normalization still applies first, so a root
/// nullable union can collapse (rewriting the root `type`), a bare object
/// gains `properties`, and dangling `required` entries are pruned — only
/// the composition-keyword scope is narrower than [`sanitize_for_responses`].
/// A root that is not a JSON object at all (malformed server or MCP output)
/// is replaced by an empty object exactly as in [`sanitize_for_responses`]:
/// the API defines `input_schema` as a JSON Schema object, and a declared
/// non-object schema such as a string-enum root still ships verbatim.
///
/// Returns the same style of description note as [`sanitize_for_responses`].
pub fn sanitize_for_anthropic_messages(schema: &mut Value) -> Option<String> {
    // The specific required-group sentences for the keywords that carry
    // two or more labeled groups, captured at entry exactly as the
    // first-match note read them; which keywords actually SURVIVE to be
    // stripped is decided after the shared normalization (a nullable-union
    // collapse can erase a composition entirely, and a composition that
    // never reaches the wire takes no note with it).
    let mut specific_notes: std::collections::HashMap<&'static str, String> =
        std::collections::HashMap::new();
    if let Some(obj) = schema.as_object() {
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
                specific_notes.insert(
                    key,
                    format!(
                        "{prefix} of these parameter groups must be provided: {}.",
                        groups.join(" | ")
                    ),
                );
            }
        }
    }

    if !schema_within_structural_depth_limit(schema) {
        let notes: Vec<String> = ["oneOf", "anyOf", "allOf"]
            .iter()
            .filter(|key| schema.get(*key).is_some())
            .map(|key| {
                specific_notes
                    .get(key)
                    .cloned()
                    .unwrap_or_else(|| root_composition_generic_note(key).to_string())
            })
            .collect();
        return (!notes.is_empty()).then(|| notes.join(" "));
    }

    sanitize(schema);

    if !schema.is_object() {
        *schema = Value::Object(Map::new());
    }

    let Some(obj) = schema.as_object_mut() else {
        return specific_notes.values().next().cloned();
    };

    let stripped_compositions: Vec<&'static str> = ["oneOf", "anyOf", "allOf"]
        .iter()
        .filter(|key| obj.contains_key(**key))
        .copied()
        .collect();
    merge_root_composition_properties(obj);
    obj.remove("oneOf");
    obj.remove("anyOf");
    obj.remove("allOf");
    if !stripped_compositions.is_empty() && !obj.contains_key("type") {
        obj.insert("type".into(), Value::String("object".to_string()));
    }
    // Same provenance as the strict flatten above: the merged map is the
    // schema's own declaration, so it keeps local authority.
    prune_dangling_required(schema, &[], true);
    // One note per STRIPPED root composition keyword, in the recorded
    // keyword order: the specific sentence when that keyword carried two or
    // more labeled groups, otherwise the generic sentence (review 4539837534
    // — merged properties do not compensate for the dropped composition, and
    // a property-less composition erases the argument contract outright).
    // Aggregating across ALL stripped keywords keeps the model informed of
    // every dropped constraint (review 5898539144: the pass strips all
    // three, but a first-match note advertised only the first).
    let notes: Vec<String> = stripped_compositions
        .iter()
        .map(|key| {
            specific_notes
                .get(key)
                .cloned()
                .unwrap_or_else(|| root_composition_generic_note(key).to_string())
        })
        .collect();
    if notes.is_empty() {
        None
    } else {
        Some(notes.join(" "))
    }
}

fn strict_schema_supported(schema: &Value) -> bool {
    if !schema_within_structural_depth_limit(schema) {
        return false;
    }
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
        return obj.iter().any(|(key, value)| {
            !matches!(key.as_str(), "default" | "enum" | "const")
                && has_strict_incompatible_composition(value, false)
        });
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
            // Only a bare null branch has an obvious nullable spelling. Extra
            // constraints on that branch would need their own intersection.
            if nulls[0].as_object().is_none_or(|branch| branch.len() != 1) {
                continue;
            }
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
            let mut outer = obj.clone();
            outer.remove(key);
            // These siblings constrain null or are forbidden union siblings
            // in the strict pipeline. Do not smuggle them into one branch
            // through an OpenAPI-style nullable marker.
            if outer.keys().any(|key| {
                matches!(
                    key.as_str(),
                    "default"
                        | "enum"
                        | "const"
                        | "$ref"
                        | "anyOf"
                        | "oneOf"
                        | "allOf"
                        | "not"
                        | "if"
                        | "then"
                        | "else"
                        | "dependentSchemas"
                )
            }) {
                continue;
            }
            let parent_has_type = outer.contains_key("type");
            let outer_declines_null = outer.get("nullable") == Some(&Value::Bool(false));
            let Some(non_null) = nons[0].as_object() else {
                continue;
            };
            let Some(mut merged) = merge_schema_intersection(outer, non_null) else {
                continue;
            };
            // A concrete parent type already excludes the null branch. The
            // exact intersection is therefore just the non-null schema.
            if !parent_has_type {
                if outer_declines_null {
                    // The parent explicitly forbids null while a null branch
                    // survives the merge: the intersection is unsatisfiable,
                    // so decline instead of widening the parent back to
                    // nullable (review 6061318874).
                    continue;
                }
                merged.insert("nullable".into(), Value::Bool(true));
            }
            *obj = merged;
        }
    }
}

/// Work ceiling for one merge: comparisons counted across the whole
/// recursion. The enum intersection is O(outer × inner) per collision, and
/// hostile or merely oversized schemas (uncapped on the shared registry
/// lane) could previously spend unbounded time here on every request build.
/// Exceeding the ceiling declines the merge — the original composition is
/// preserved, which is the merge's own fail-closed contract (review
/// 6061318874).
const MFJS_MAX_MERGE_WORK: usize = 100_000;

/// Merge two schemas that constrain the same instance. Disjoint keywords
/// combine directly; collisions need a real intersection or the merge is
/// declined. Returning `None` leaves the original composition untouched.
///
/// Takes `outer` by value: the merge builds its result from the caller's
/// working copy in place, so a declined merge just drops the copy instead of
/// paying a second full deep clone of the node on every attempt (round-6
/// review).
fn merge_schema_intersection(
    outer: Map<String, Value>,
    inner: &Map<String, Value>,
) -> Option<Map<String, Value>> {
    let mut work = 0usize;
    merge_schema_intersection_bounded(outer, inner, &mut work)
}

fn merge_schema_intersection_bounded(
    outer: Map<String, Value>,
    inner: &Map<String, Value>,
    work: &mut usize,
) -> Option<Map<String, Value>> {
    // Declaring a new named property changes the domain of a constraining
    // additionalProperties keyword on either side. Merging different property
    // sets would therefore turn previously rejected keys into accepted ones.
    let outer_properties = schema_property_names(&outer);
    let inner_properties = schema_property_names(inner);
    let property_sets_differ = match (&outer_properties, &inner_properties) {
        (Some(outer), Some(inner)) => outer != inner,
        _ => true,
    };
    if property_sets_differ
        && (additional_properties_constrains(&outer) || additional_properties_constrains(inner))
    {
        return None;
    }

    // A `$ref` cannot be merged beside constraining keywords: the exact
    // intersection needs the reference target's constraints (unknown here),
    // and the plausible spelling — `type` beside `$ref` — is exactly the
    // sibling pair the MFJS ref-sibling whitelist rejects into a misleading
    // `InvalidReference`. Declining keeps the original composition, whose
    // union form is exactly representable (round-4 review). Annotations
    // beside the reference are the whitelist's own allowance.
    if (outer.contains_key("$ref") || inner.contains_key("$ref"))
        && outer
            .keys()
            .chain(inner.keys())
            .any(|key| !matches!(key.as_str(), "$ref" | "description"))
    {
        return None;
    }

    // `outer` is the caller's private working copy, so the merged result is
    // built in place — no second deep clone of the node per attempt.
    let mut merged = outer;
    for (key, inner_value) in inner {
        let Some(outer_value) = merged.get(key).cloned() else {
            merged.insert(key.clone(), inner_value.clone());
            continue;
        };
        if schema_value_comparison(&outer_value, inner_value)? {
            continue;
        }
        match key.as_str() {
            "type" => {
                let outer_type = outer_value.as_str()?;
                let inner_type = inner_value.as_str()?;
                merged.insert(
                    key.clone(),
                    Value::String(intersect_schema_types(outer_type, inner_type)?.to_string()),
                );
            }
            "enum" => {
                let outer_values = outer_value.as_array()?;
                let inner_values = inner_value.as_array()?;
                // The cap bounds element visits, not pair counts: each
                // charged pair comparison recursively walks both operands,
                // so compound members must charge by their serialized size
                // or ~100k pairs of large values cost ~10^10 element visits
                // on the byte-uncapped registry lane (round-6 review).
                // Sizes are computed once per element — O(document)
                // overall — and the pre-arm whole-array equality shortcut
                // above stays a single linear pass per key.
                let operand_size = |value: &Value| {
                    serde_json::to_vec(value)
                        .map(|encoded| encoded.len().max(1))
                        .unwrap_or(usize::MAX)
                };
                let outer_sizes: Vec<usize> = outer_values.iter().map(operand_size).collect();
                let inner_sizes: Vec<usize> = inner_values.iter().map(operand_size).collect();
                let mut intersection = Vec::new();
                for (outer_index, candidate) in outer_values.iter().enumerate() {
                    let mut matched = false;
                    for (inner_index, value) in inner_values.iter().enumerate() {
                        *work = work
                            .saturating_add(outer_sizes[outer_index].max(inner_sizes[inner_index]));
                        if *work > MFJS_MAX_MERGE_WORK {
                            return None;
                        }
                        if schema_value_comparison(value, candidate)? {
                            matched = true;
                            break;
                        }
                    }
                    if matched {
                        intersection.push(candidate.clone());
                    }
                }
                if intersection.is_empty() {
                    return None;
                }
                merged.insert(key.clone(), Value::Array(intersection));
            }
            "required" => {
                let outer_required = outer_value.as_array()?;
                let inner_required = inner_value.as_array()?;
                if !outer_required.iter().all(Value::is_string)
                    || !inner_required.iter().all(Value::is_string)
                {
                    return None;
                }
                // Membership goes through a set: a bare `contains` scan per
                // inner name is O(|outer| × |inner|) string compares with no
                // accounting, and the shared registry lane caps neither the
                // schema nor the names. Every inner name is charged to the
                // same work ceiling as the enum arm; tripping it declines the
                // merge exactly like the enum arm (round-4 review).
                let mut seen: std::collections::HashSet<&str> =
                    outer_required.iter().filter_map(Value::as_str).collect();
                let mut required = outer_required.clone();
                for name in inner_required {
                    *work = work.saturating_add(1);
                    if *work > MFJS_MAX_MERGE_WORK {
                        return None;
                    }
                    if let Some(name) = name.as_str()
                        && seen.insert(name)
                    {
                        required.push(Value::String(name.to_owned()));
                    }
                }
                merged.insert(key.clone(), Value::Array(required));
            }
            "properties" => {
                let outer_properties = outer_value.as_object()?;
                let inner_properties = inner_value.as_object()?;
                let mut properties = outer_properties.clone();
                for (name, inner_schema) in inner_properties {
                    if let Some(outer_schema) = properties.get(name).cloned() {
                        let intersected = if schema_value_comparison(&outer_schema, inner_schema)? {
                            inner_schema.clone()
                        } else {
                            match (outer_schema.as_object(), inner_schema.as_object()) {
                                (Some(outer), Some(inner)) => Value::Object(
                                    merge_schema_intersection_bounded(outer.clone(), inner, work)?,
                                ),
                                _ => return None,
                            }
                        };
                        properties.insert(name.clone(), intersected);
                    } else {
                        properties.insert(name.clone(), inner_schema.clone());
                    }
                }
                merged.insert(key.clone(), Value::Object(properties));
            }
            "$defs" => {
                let outer_definitions = outer_value.as_object()?;
                let inner_definitions = inner_value.as_object()?;
                for (name, definition) in outer_definitions {
                    if let Some(inner) = inner_definitions.get(name)
                        && !schema_value_comparison(definition, inner)?
                    {
                        return None;
                    }
                }
                let mut definitions = outer_definitions.clone();
                definitions.extend(inner_definitions.clone());
                merged.insert(key.clone(), Value::Object(definitions));
            }
            _ => return None,
        }
    }
    if !narrow_instance_data_to_intersected_type(&mut merged) {
        return None;
    }
    Some(merged)
}

/// Narrow enum/const instance data to the intersected type. A `type` that
/// the intersection produced (for example `number` ∩ `integer` → `integer`)
/// must also constrain the surviving members: keeping a fractional enum
/// value beside an `integer` type yields a schema MFJS's value-type check
/// rejects even though the exact intersection is representable (review
/// 4689524156). An instance-data member the final type can never admit is
/// removed, under the schema's own integer ruling
/// ([`instance_data_is_type`]) so an integral f64 spelling such as `3.0`
/// survives the narrowing it would fail only under raw serde integrality.
/// Returns `false` when nothing survives — an emptied enum, or a
/// const incompatible with the final type — meaning no instance satisfies
/// the intersection and the merge must be declined.
fn narrow_instance_data_to_intersected_type(merged: &mut Map<String, Value>) -> bool {
    let Some(final_type) = merged
        .get("type")
        .and_then(Value::as_str)
        .map(str::to_owned)
    else {
        return true;
    };
    if let Some(values) = merged.get("enum").and_then(Value::as_array).cloned() {
        let narrowed: Vec<Value> = values
            .into_iter()
            .filter(|value| instance_data_is_type(value, &final_type))
            .collect();
        if narrowed.is_empty() {
            return false;
        }
        merged.insert("enum".into(), Value::Array(narrowed));
    }
    if let Some(constant) = merged.get("const") {
        let constant = constant.clone();
        if !instance_data_is_type(&constant, &final_type) {
            return false;
        }
    }
    true
}

fn schema_property_names(schema: &Map<String, Value>) -> Option<std::collections::BTreeSet<&str>> {
    match schema.get("properties") {
        None => Some(std::collections::BTreeSet::new()),
        Some(Value::Object(properties)) => Some(properties.keys().map(String::as_str).collect()),
        Some(_) => None,
    }
}

fn additional_properties_constrains(schema: &Map<String, Value>) -> bool {
    match schema.get("additionalProperties") {
        None | Some(Value::Bool(true)) => false,
        Some(Value::Object(schema)) if schema.is_empty() => false,
        Some(_) => true,
    }
}

fn intersect_schema_types<'a>(outer: &'a str, inner: &'a str) -> Option<&'a str> {
    if outer == inner || (outer == "integer" && inner == "number") {
        Some(outer)
    } else if outer == "number" && inner == "integer" {
        Some(inner)
    } else {
        None
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
    // A required-bearing bare object is always a composition branch on the
    // supported lanes (strict validation rejects compositions outright, so
    // the synthetic map buys nothing there), while persisting the map
    // poisons the pruning twice over: this pass's pruner would read it as a
    // local declaration and strip `required` entries that resolve against
    // the enclosing properties (review 5910284697), and later passes cannot
    // distinguish the synthetic map from a declared one — the branch would
    // re-prune into `{}` on the second sanitize. Skipped here, the branch
    // stays property-less and the pass stays idempotent.
    if obj.contains_key("required") {
        return;
    }
    // A composition-bearing bare object never reaches the strict-mode
    // validator this injection exists for (strict mode rejects root and
    // nested compositions outright), so the synthetic map buys nothing
    // there — while persisting it manufactures provenance the next
    // sanitize pass mistakes for a declared empty scope and re-prunes the
    // nested required-only branches (review thread on 5897819279: the
    // registry sanitize injects, then sanitize_for_responses sanitizes the
    // same schema again). Skipped here, the branch stays property-less,
    // the inherited-context rule keeps forwarding ancestor names, and the
    // pass is idempotent.
    if obj.contains_key("oneOf") || obj.contains_key("anyOf") || obj.contains_key("allOf") {
        return;
    }
    obj.insert("properties".into(), Value::Object(Map::new()));
}

/// Remove entries from `required` that aren't keys in `properties`.
///
/// An object with no local `properties` declaration at all (a required-only
/// composition branch) resolves names against `enclosing_properties` — the
/// same-instance property names declared by the enclosing object schema —
/// before a name is declared dangling. A local `properties` key, even empty,
/// keeps the strictly local resolution (the DeepSeek strict validator reads
/// required against the schema object it validates, so an injected empty
/// properties map must not silently adopt the enclosing names).
///
/// `declared_local` carries the PRE-injection declaration state: a
/// synthetic map that `inject_properties_on_bare_objects` added after that
/// capture is not the schema's own declaration, so a typed required-only
/// branch must still resolve against the enclosing names instead of being
/// pruned into an all-accepting `{type: "object"}` — both branches of a
/// oneOf would become identical and exactly-one semantics would admit
/// nothing (review 5910284697).
fn prune_dangling_required(
    schema: &mut Value,
    enclosing_properties: &[String],
    declared_local: bool,
) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    // Collect known property names first (immutable borrow), then prune.
    let local_keys: Vec<String> = obj
        .get("properties")
        .and_then(|v| v.as_object())
        .map(|props| props.keys().cloned().collect())
        .unwrap_or_default();
    // Set membership keeps the required prune linear in the schema size on
    // the shared (uncapped-by-bytes) registry lane instead of an
    // O(required × properties) scan per node (round-5 review).
    let known_keys: std::collections::HashSet<&str> =
        if obj.contains_key("properties") && declared_local {
            local_keys.iter().map(String::as_str).collect()
        } else {
            enclosing_properties.iter().map(String::as_str).collect()
        };
    let Some(required) = obj.get_mut("required").and_then(|v| v.as_array_mut()) else {
        return;
    };
    required.retain(|entry| entry.as_str().is_some_and(|k| known_keys.contains(k)));
    if required.is_empty() {
        obj.remove("required");
    }
}

/// Collapse a single-branch composition only when the branch and every
/// sibling can be represented by their exact intersection.
fn collapse_single_element_unions(schema: &mut Value) {
    let Some(obj) = schema.as_object_mut() else {
        return;
    };
    for key in ["oneOf", "allOf", "anyOf"] {
        if obj.contains_key("default") {
            continue;
        }
        let single = match obj.get(key).and_then(|v| v.as_array()) {
            Some(arr) if arr.len() == 1 => arr[0].clone(),
            _ => continue,
        };
        let Some(inner) = single.as_object() else {
            continue;
        };
        let mut outer = obj.clone();
        outer.remove(key);
        // An outer with no remaining keywords contributes nothing, so the
        // exact intersection is the branch itself — the property-set guard
        // below has nothing to protect (a root `anyOf:[object]` must
        // collapse onto its branch rather than be dropped as root
        // composition, review 4689524141).
        if outer.is_empty() {
            *obj = inner.clone();
            continue;
        }
        if let Some(merged) = merge_schema_intersection(outer, inner) {
            *obj = merged;
        }
    }
}

fn enforce_strict_subset(schema: &mut Value) {
    if let Some(obj) = schema.as_object_mut() {
        strip_unsupported_strict_keywords(obj);
        if is_object_schema(obj) {
            let originally_required = required_names(obj);
            // Set membership keeps the strict-subset pass linear instead of
            // an O(properties × required) scan per node (round-5 review).
            let required_set: std::collections::HashSet<&str> =
                originally_required.iter().map(String::as_str).collect();
            let properties = ensure_properties_object(obj);
            let mut property_names: Vec<String> = properties.keys().cloned().collect();
            property_names.sort();
            for property_name in &property_names {
                if !required_set.contains(property_name.as_str())
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

        for (key, value) in obj.iter_mut() {
            if !matches!(key.as_str(), "default" | "enum" | "const") {
                enforce_strict_subset(value);
            }
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
    // Aggregate the required-group note for EVERY root composition keyword,
    // in the recorded keyword order: this pass strips all three keywords
    // but a first-match note left the model ignorant of every dropped
    // constraint after the first (review 5898539144 — a root stacking
    // `oneOf` and `anyOf` advertised only the exactly-one contract on the
    // wire).
    let mut notes: Vec<String> = Vec::new();
    for (key, prefix) in [
        ("oneOf", "Exactly one"),
        ("anyOf", "At least one"),
        ("allOf", "All"),
    ] {
        let Some(items) = obj.get(key).and_then(Value::as_array) else {
            continue;
        };
        // A union alternative that admits the empty instance keeps omission
        // legal on the flattened wire schema, so a "must" hint would walk
        // the model onto a path the schema does not require (review
        // 4604994801: the unconstrained `{type:"object"}` alternative
        // accepts `{}` while the note claimed a group must be provided).
        // allOf keeps the strong wording — every branch constrains the
        // same instance, so each surviving group genuinely is required.
        let optional = key != "allOf" && items.iter().any(alternative_admits_omission);
        let mut groups: Vec<String> = items.iter().filter_map(required_group_label).collect();
        groups.sort();
        groups.dedup();
        // Every dropped group must be disclosed, including a single one:
        // with one distinct label the flatten still relaxed the original
        // constraint (e.g. only one of two branches required it), and
        // suppressing the note there used to hide the loosening entirely.
        if !groups.is_empty() {
            let verb = if optional { "may be" } else { "must be" };
            notes.push(format!(
                "{prefix} of these parameter groups {verb} provided: {}.",
                groups.join(" | ")
            ));
        }
    }
    if notes.is_empty() {
        None
    } else {
        Some(notes.join(" "))
    }
}

/// The generic sentence for one stripped root composition keyword: the
/// model-facing compensation for a composition whose branches carry no
/// labeled required groups (review 4539837534).
fn root_composition_generic_note(key: &str) -> &'static str {
    match key {
        "oneOf" => {
            "Exactly one of the original argument alternatives must be provided (top-level `oneOf` is not supported by this API and was removed)."
        }
        "anyOf" => {
            "At least one of the original argument alternatives must be provided (top-level `anyOf` is not supported by this API and was removed)."
        }
        _ => {
            "All of the original argument alternatives must be satisfied (top-level `allOf` is not supported by this API and was removed)."
        }
    }
}

/// Whether one composition alternative admits an instance carrying no
/// group at all: an object-typed (or type-unconstrained) alternative with
/// no surviving `required` accepts `{}` itself. A scalar-typed
/// alternative or a `false` schema admits no empty instance and keeps the
/// strong wording.
fn alternative_admits_omission(item: &Value) -> bool {
    let Some(obj) = item.as_object() else {
        return item == &Value::Bool(true);
    };
    obj.get("type").is_none_or(|declared| declared == "object")
        && obj
            .get("required")
            .and_then(Value::as_array)
            .is_none_or(|required| required.is_empty())
}

/// Strip `dependentSchemas` / `dependentRequired` for the compatibility
/// providers whose existing contract explicitly permits that relaxation.
/// Moonshot does not call this helper: conditional requirements are real
/// constraints and its strict pipeline rejects them instead.
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
            for (key, value) in obj.iter_mut() {
                if !matches!(key.as_str(), "default" | "enum" | "const") {
                    dropped |= strip_dependent_keywords(value);
                }
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
    fn required_only_composition_branches_resolve_against_the_enclosing_properties() {
        // `required` inside a composition branch refers to the SAME instance
        // the enclosing schema validates, so a required-only branch
        // (`{required: ["patch"]}` with the field declared in the enclosing
        // `properties`) is not dangling: pruning it would leave `oneOf:
        // [{}, {}, {}]`, whose every branch matches every instance — under
        // exactly-one semantics that contract admits no instance at all
        // (review 5885719142: the registry handed exactly that shape for
        // the builtin apply_patch tool, and the verbatim Messages lanes
        // forwarded it).
        let mut schema = json!({
            "type": "object",
            "properties": {
                "patch": {"type": "string"},
                "replace": {"type": "array"}
            },
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["replace"]}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["oneOf"],
            json!([{"required": ["patch"]}, {"required": ["replace"]}]),
            "required-only branches must keep their enclosing-resolvable names"
        );
    }

    #[test]
    fn genuinely_dangling_branch_required_still_prunes_against_the_enclosing_properties() {
        // The fallback resolves names only against what the enclosing
        // schema actually declares: a branch requiring a name that exists
        // nowhere stays dangling and is pruned, keeping the DeepSeek
        // strict-validator workaround intact.
        let mut schema = json!({
            "type": "object",
            "properties": {"patch": {"type": "string"}},
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["ghost"]}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["oneOf"],
            json!([{"required": ["patch"]}, {}]),
            "an enclosing-unresolvable branch name must still prune"
        );
    }

    #[test]
    fn nested_instance_scopes_do_not_inherit_the_enclosing_required_context() {
        // `properties.*` and `items` validate DIFFERENT instances: a
        // required-only branch inside them must not resolve against the
        // outer object's property names, or a genuinely dangling nested
        // name would survive on the strength of an unrelated sibling.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "patch": {"type": "string"},
                "handler": {
                    "type": "object",
                    "properties": {"kind": {"type": "string"}},
                    "oneOf": [
                        {"required": ["ghost"]},
                        {"type": "string"}
                    ]
                }
            }
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["properties"]["handler"]["oneOf"],
            json!([{}, {"type": "string"}]),
            "a nested instance scope resets the property context"
        );
    }

    #[test]
    fn inherited_property_names_flow_through_nested_same_instance_composition() {
        // A composition branch with no local `properties` introduces no
        // property scope of its own: the ancestor's declared names must
        // reach compositions nested inside it, or a bare
        // `{oneOf: [...]}` intermediate branch prunes its inner
        // required-only branches into `{}` — the inner union becomes
        // unsatisfiable and the sibling non-object branch is incompatible
        // with the object root, so no argument stays valid (review
        // 5896057559). `{"a":"x"}` is valid before sanitization and the
        // schema must keep admitting it.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "oneOf": [
                        {"required": ["a"]},
                        {"required": ["b"]}
                    ]
                },
                {"type": "string"}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["oneOf"][0]["oneOf"],
            json!([{"required": ["a"]}, {"required": ["b"]}]),
            "the nested required-only branches must resolve against the root properties: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the sanitized schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "the object form must stay valid after sanitization: {schema}"
        );
    }

    #[test]
    fn injected_compatibility_map_is_not_a_declared_property_scope() {
        // `inject_properties_on_bare_objects` inserts a synthetic empty
        // `properties` map on a bare typed object BEFORE the context is
        // derived, so an intermediate `{type: "object", oneOf: [...]}`
        // branch carries Some(empty) even though the schema declared
        // nothing locally: reading that map as a declaration discarded the
        // inherited names and the inner required-only branches pruned into
        // `{}` — the inner union became unsatisfiable and the sibling
        // non-object branch is incompatible with the object root (review
        // 5897491074). `{"a":"x"}` is valid before sanitization and must
        // stay valid after.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "oneOf": [
                        {"required": ["a"]},
                        {"required": ["b"]}
                    ]
                },
                {"type": "string"}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["oneOf"][0]["oneOf"],
            json!([{"required": ["a"]}, {"required": ["b"]}]),
            "the typed-object intermediate must forward the inherited names: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the sanitized schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "the object form must stay valid after sanitization: {schema}"
        );
    }

    #[test]
    fn sanitize_is_idempotent_for_typed_composition_intermediates() {
        // The registry sanitizes first, and `sanitize_for_responses`
        // sanitizes the same schema again: a synthetic compatibility map
        // injected on pass one must not read as a declaration on pass two,
        // or the typed-composition intermediate re-prunes its nested
        // required-only branches on the second pass (review 5897819279).
        // Composition-bearing bare typed objects now receive no synthetic
        // map at all — the strict validator the injection exists for
        // rejects compositions outright — so the pass is idempotent and
        // the inherited context survives.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "oneOf": [
                        {"required": ["a"]},
                        {"required": ["b"]}
                    ]
                },
                {"type": "string"}
            ]
        });
        sanitize(&mut schema);
        let once = schema.clone();
        sanitize(&mut schema);
        assert_eq!(
            schema, once,
            "a second sanitize pass must be a no-op here: {schema}"
        );
        assert!(
            !schema["oneOf"][0]
                .as_object()
                .expect("object branch")
                .contains_key("properties"),
            "the composition-bearing bare typed branch must not gain a synthetic map: {schema}"
        );
        assert_eq!(
            schema["oneOf"][0]["oneOf"],
            json!([{"required": ["a"]}, {"required": ["b"]}]),
            "the nested required-only branches must survive both passes: {schema}"
        );
    }

    #[test]
    fn typed_required_only_union_branches_keep_their_declared_contract() {
        // A typed required-only branch (`{type: "object", required: ["a"]}`)
        // under a root that declares `a` and `b` admits an a-only object.
        // The bare-object injection used to add a synthetic empty
        // `properties` map BEFORE the pruner ran, the pruner read that map
        // as the branch's own declaration, and both branches pruned into
        // identical all-accepting `{type: "object"}` schemas — under
        // exactly-one semantics the union then admits nothing at all
        // (review 5910284697). The pre-injection declaration distinction
        // must survive the pruning, the branch must not gain a synthetic
        // map, and the pass must stay idempotent.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {"type": "object", "required": ["a"]},
                {"type": "object", "required": ["b"]}
            ]
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["oneOf"],
            json!([
                {"type": "object", "required": ["a"]},
                {"type": "object", "required": ["b"]}
            ]),
            "typed required-only branches must keep their required groups: {schema}"
        );
        let once = schema.clone();
        sanitize(&mut schema);
        assert_eq!(
            schema, once,
            "a second sanitize pass must be a no-op: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the sanitized union must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "the a-only form must stay valid after sanitization: {schema}"
        );
        assert!(
            !validator.is_valid(&json!({"a": "x", "b": "y"})),
            "an object satisfying both branches must stay rejected: {schema}"
        );
    }

    #[test]
    fn dependent_schemas_resolve_against_the_same_instance_properties() {
        // Draft 2020-12: each dependentSchemas value applies to the SAME
        // instance as the object schema holding it, so a dependency naming
        // only fields declared at that scope — `{required: ["confirm"]}`
        // against the root `properties` — keeps its entries. The traversal
        // used to reset the context at `dependentSchemas` and again at its
        // name map, pruning the dependency into an all-accepting `{}` and
        // letting a `mode`-only argument ship without the advertised
        // confirmation (review 5910284697).
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {"type": "string"},
                "confirm": {"type": "boolean"}
            },
            "dependentSchemas": {
                "mode": {"required": ["confirm"]}
            }
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["dependentSchemas"]["mode"],
            json!({"required": ["confirm"]}),
            "the dependency must keep its root-declared required entry: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the sanitized schema must compile");
        assert!(
            !validator.is_valid(&json!({"mode": "x"})),
            "a mode-only argument must be rejected by the kept dependency: {schema}"
        );
        assert!(
            validator.is_valid(&json!({"mode": "x", "confirm": true})),
            "the confirmed form must stay valid: {schema}"
        );
        assert!(
            validator.is_valid(&json!({"confirm": true})),
            "the dependency must stay inactive without its trigger key: {schema}"
        );
    }

    #[test]
    fn keyword_named_property_schemas_are_walked_as_schemas_not_keyword_payloads() {
        // Review 5986025535: a property literally named `dependentSchemas`
        // is a NAME in the properties map, not the keyword. The generic walk
        // dispatched the name as the keyword, treated the property's schema
        // as a dependency map, walked the enum literal inside as schemas,
        // and pruned the literal object's `required` into an all-accepting
        // `{}` — the advertised enum changed from `[{"required": ["x"]}]`
        // to `[{}]`, so the original argument stopped validating while `{}`
        // started. The map keys must never be dispatched as keywords; each
        // property schema is a schema node in its own right.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "dependentSchemas": {"enum": [{"required": ["x"]}]}
            }
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["properties"]["dependentSchemas"]["enum"],
            json!([{"required": ["x"]}]),
            "the enum literal under a keyword-shaped property name must stay verbatim: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the sanitized schema must compile");
        assert!(
            validator.is_valid(&json!({"dependentSchemas": {"required": ["x"]}})),
            "the original enum-admitted argument must stay valid: {schema}"
        );
        assert!(
            !validator.is_valid(&json!({"dependentSchemas": {}})),
            "the corrupted-admission value must stay rejected: {schema}"
        );
    }

    #[test]
    fn object_valued_enum_entries_are_instance_data_and_stay_verbatim() {
        // Review 5911391551: each enum entry is a literal argument value,
        // not a subschema — a schema-shaped entry (`enum:
        // [{"required": ["x"]}]` beside a property literally named
        // `required`) must reach the wire byte-for-byte. The generic
        // recursion used to normalize the literal object, pruning its
        // `required` into an all-accepting `{}` and excluding the exact
        // argument the original contract admitted. The same holds for a
        // schema-shaped `const`.
        let mut enum_schema = json!({
            "type": "object",
            "properties": {
                "required": {"type": "array", "items": {"type": "string"}}
            },
            "enum": [{"required": ["x"]}]
        });
        sanitize(&mut enum_schema);
        assert_eq!(
            enum_schema["enum"],
            json!([{"required": ["x"]}]),
            "the enum entry is instance data and must stay verbatim: {enum_schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&enum_schema)
            .expect("the sanitized schema must compile");
        assert!(
            validator.is_valid(&json!({"required": ["x"]})),
            "the original enum-admitted argument must stay valid: {enum_schema}"
        );

        // The const value is instance data under the same rule: a
        // schema-shaped const must not be normalized either.
        let mut const_schema = json!({
            "type": "object",
            "properties": {
                "required": {"type": "array", "items": {"type": "string"}}
            },
            "const": {"required": ["x"], "mode": "fast"}
        });
        sanitize(&mut const_schema);
        assert_eq!(
            const_schema["const"],
            json!({"required": ["x"], "mode": "fast"}),
            "the const value is instance data and must stay verbatim: {const_schema}"
        );
    }

    #[test]
    fn nullable_union_with_same_instance_composition_sibling_does_not_collapse() {
        let mut schema = json!({
            "anyOf": [
                {"type": "object", "properties": {}},
                {"type": "null"}
            ],
            "oneOf": [
                {"required": ["a"]},
                {"type": "string"}
            ]
        });

        sanitize(&mut schema);

        assert!(schema["anyOf"].is_array());
        assert!(schema.get("nullable").is_none());
    }

    #[test]
    fn nullable_parent_type_and_non_null_siblings_keep_and_semantics() {
        let mut schema = json!({
            "type": "string",
            "minLength": 2,
            "anyOf": [
                {"type": "string", "maxLength": 4},
                {"type": "null"}
            ]
        });

        sanitize(&mut schema);

        assert_eq!(schema["type"], "string");
        assert_eq!(schema["minLength"], 2);
        assert_eq!(schema["maxLength"], 4);
        assert!(schema.get("nullable").is_none());
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn single_branch_union_intersects_conflicting_enum_siblings() {
        let mut schema = json!({
            "type": "string",
            "enum": ["a", "b"],
            "anyOf": [{"type": "string", "enum": ["b", "c"], "minLength": 1}]
        });

        sanitize(&mut schema);

        assert_eq!(schema["enum"], json!(["b"]));
        assert_eq!(schema["minLength"], 1);
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn numeric_type_intersection_narrows_enum_members_to_the_result_type() {
        // Review 4689524156: `number` ∩ `integer` is `integer`, so the
        // surviving enum must drop fractional members instead of shipping a
        // schema the MFJS value-type check rejects wholesale.
        let mut schema = json!({
            "type": "number",
            "enum": [0.5, 1, 2],
            "anyOf": [{"type": "integer"}]
        });

        sanitize(&mut schema);

        assert_eq!(schema["type"], "integer");
        assert_eq!(schema["enum"], json!([1, 2]));
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn numeric_type_intersection_declines_when_no_enum_member_survives() {
        let mut schema = json!({
            "type": "number",
            "enum": [0.5],
            "anyOf": [{"type": "integer"}]
        });
        let original = schema.clone();

        sanitize(&mut schema);

        assert_eq!(schema, original, "an unsatisfiable intersection declines");
    }

    #[test]
    fn type_narrowing_preserves_integral_float_enum_members() {
        // Round-4 review: the narrowing test must agree with the module's own
        // MFJS integer ruling (`mfjs_integer_value`), under which an f64
        // spelling with a zero fractional part IS an integral value. Raw
        // serde integrality used to drop `3.0` and silently shrink the
        // admitted contract to `[4]`, while a fractional member still goes.
        let mut schema = json!({
            "type": "number",
            "enum": [3.0, 4, 0.5],
            "anyOf": [{"type": "integer"}]
        });

        sanitize(&mut schema);

        assert_eq!(schema["type"], "integer");
        assert_eq!(schema["enum"], json!([3.0, 4]));
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn type_narrowing_preserves_an_integral_float_const() {
        let mut schema = json!({
            "type": "number",
            "const": 3.0,
            "anyOf": [{"type": "integer"}]
        });

        sanitize(&mut schema);

        assert_eq!(schema["type"], "integer");
        assert_eq!(schema["const"], json!(3.0));
        assert!(schema.get("anyOf").is_none());
    }

    #[test]
    fn type_narrowing_follows_the_safe_integer_window_exactly() {
        // The narrowing ruling must stay aligned with `mfjs_integer_value`:
        // a negative f64 at the -(2^53-1) floor is an admitted integral
        // value, while a u64-spelled value one past the window fails the
        // same ruling the MFJS enum check applies and is dropped instead of
        // shipping a schema validation rejects wholesale.
        let mut schema = json!({
            "type": "number",
            "enum": [-9_007_199_254_740_991.0, 9_007_199_254_740_993u64, 0.5],
            "anyOf": [{"type": "integer"}]
        });

        sanitize(&mut schema);

        assert_eq!(schema["type"], "integer");
        assert_eq!(schema["enum"], json!([-9_007_199_254_740_991.0]));
    }

    #[test]
    fn merge_declines_unknown_large_number_equality_without_deleting_enum_values() {
        let integer = json!(9_007_199_254_740_994_i64);
        let float = json!(9_007_199_254_740_994.0);
        assert_eq!(
            schema_number_equal(integer.as_number().unwrap(), float.as_number().unwrap()),
            None
        );
        assert_eq!(
            schema_value_comparison(&json!([integer.clone()]), &json!([float.clone()])),
            None
        );

        let mut schema = json!({
            "type": "number",
            "enum": [integer, 1],
            "anyOf": [{
                "type": "number",
                "enum": [float, 1.0]
            }]
        });
        let original = schema.clone();

        sanitize(&mut schema);

        assert_eq!(
            schema, original,
            "unknown numeric equality must preserve the union and every enum literal"
        );
    }

    #[test]
    fn required_merge_unions_deduplicated_names_in_order() {
        let merge = |outer: Value, inner: Value| {
            merge_schema_intersection(
                outer.as_object().cloned().unwrap(),
                &inner.as_object().cloned().unwrap(),
            )
        };
        let merged = merge(
            json!({"type": "object", "required": ["b", "a"]}),
            json!({"type": "object", "required": ["a", "c", "a"]}),
        )
        .expect("a required-vs-required merge is an exact intersection");

        assert_eq!(
            merged.get("required"),
            Some(&json!(["b", "a", "c"])),
            "outer order stands, unseen inner names append in order"
        );
    }

    #[test]
    fn required_merge_completes_fifty_thousand_names_under_the_cap() {
        // Round-4 review: the `required` arm's bare `contains` scan was
        // O(|outer| × |inner|) string compares with no work accounting —
        // ≈1.25e9 compares on this shape, uncapped on the shared registry
        // lane. With the set-backed scan and the per-name charge the merge
        // completes into the exact union.
        let inner_names: Vec<Value> = (0..50_000)
            .map(|index| Value::String(format!("private-field-{index}")))
            .collect();
        let merged = merge_schema_intersection(
            json!({"type": "object", "required": ["private-field-anchor"]})
                .as_object()
                .cloned()
                .unwrap(),
            &json!({"type": "object", "required": inner_names})
                .as_object()
                .cloned()
                .unwrap(),
        )
        .expect("50_000 charged name comparisons sit under the cap");

        let required = merged.get("required").and_then(Value::as_array).unwrap();
        assert_eq!(required.len(), 50_001);
        assert_eq!(required[0], json!("private-field-anchor"));
    }

    #[test]
    fn required_merge_declines_when_charged_work_exceeds_the_cap() {
        // The band pin for the required arm's work ceiling, mirroring the
        // enum-merge band pattern: 100_001 charged inner names — one past
        // MFJS_MAX_MERGE_WORK — must decline the merge (returning `None`
        // preserves the original composition, the merge's fail-closed
        // contract), and loosening the cap would complete this merge and
        // flip the assertion.
        let inner_names: Vec<Value> = (0..100_001)
            .map(|index| Value::String(format!("private-field-{index}")))
            .collect();
        let merged = merge_schema_intersection(
            json!({"type": "object", "required": ["private-field-anchor"]})
                .as_object()
                .cloned()
                .unwrap(),
            &json!({"type": "object", "required": inner_names})
                .as_object()
                .cloned()
                .unwrap(),
        );

        assert!(
            merged.is_none(),
            "over-cap required merge work must decline instead of scanning"
        );
    }

    #[test]
    fn merge_declines_rather_than_placing_a_ref_beside_constraining_keywords() {
        // Round-4 review: merging a `$ref` with a constraining keyword would
        // manufacture a schema the MFJS ref-sibling whitelist rejects (a
        // misleading `InvalidReference` for a resolvable reference). The
        // merge declines so the original union form survives; annotations
        // beside the reference stay mergeable.
        let merged = merge_schema_intersection(
            json!({"type": "object"}).as_object().cloned().unwrap(),
            &json!({"$ref": "#/$defs/A"}).as_object().cloned().unwrap(),
        );
        assert!(merged.is_none(), "type beside $ref is not mergeable");

        let annotated = merge_schema_intersection(
            json!({"description": "payload"})
                .as_object()
                .cloned()
                .unwrap(),
            &json!({"$ref": "#/$defs/A"}).as_object().cloned().unwrap(),
        )
        .expect("an annotation beside a $ref is the whitelist's own allowance");
        assert_eq!(annotated.get("$ref"), Some(&json!("#/$defs/A")));
        assert_eq!(annotated.get("description"), Some(&json!("payload")));
    }

    #[test]
    fn property_union_does_not_widen_constraining_additional_properties() {
        for (outer_closed, inner_closed) in [(true, false), (false, true)] {
            let mut schema = json!({
                "type": "object",
                "properties": {
                    "payload": {
                        "type": "object",
                        "properties": {"a": {"type": "string"}},
                        "anyOf": [{
                            "type": "object",
                            "properties": {"b": {"type": "string"}}
                        }]
                    }
                }
            });
            if outer_closed {
                schema["properties"]["payload"]["additionalProperties"] = json!(false);
            }
            if inner_closed {
                schema["properties"]["payload"]["anyOf"][0]["additionalProperties"] = json!(false);
            }

            sanitize(&mut schema);

            let payload = &schema["properties"]["payload"];
            assert!(payload["anyOf"].is_array(), "unsafe merge must be declined");
            assert!(payload["properties"].get("b").is_none());
        }
    }

    #[test]
    fn default_literals_are_instance_data_in_every_shared_walk() {
        let literal = json!({
            "required": ["x"],
            "oneOf": [{}, {}],
            "dependentRequired": {"x": ["y"]},
            "properties": {"x": {"type": "string"}}
        });
        let mut schema = json!({
            "type": "object",
            "properties": {"value": {"type": "object"}},
            "default": literal.clone()
        });

        sanitize(&mut schema);
        sanitize_for_strict(&mut schema);

        assert_eq!(schema["default"], literal);
        assert_eq!(drop_dependent_keywords(&mut schema), None);
        assert_eq!(oneof_semantic_defect(&schema), None);
        assert!(!has_strict_incompatible_composition(&schema, true));
    }

    #[test]
    fn recursive_public_entry_points_fail_closed_on_overdepth_schemas() {
        let mut literal = json!("leaf");
        for _ in 0..=MFJS_MAX_STRUCTURAL_DEPTH {
            literal = json!({"nested": literal});
        }
        let plain = json!({"type": "object", "default": literal});

        for (name, entry) in [
            ("generic", sanitize as fn(&mut Value)),
            ("strict", sanitize_for_strict as fn(&mut Value)),
            (
                "kimi",
                (|schema: &mut Value| {
                    // Overdepth input trips the entry-point depth guard, so
                    // the result is `Ok` here; the pointer table only needs
                    // the fail-closed "no mutation" behavior under test.
                    sanitize_for_kimi(schema)
                        .expect("overdepth input must take the guard's silent Ok path");
                }) as fn(&mut Value),
            ),
        ] {
            let mut schema = plain.clone();
            entry(&mut schema);
            assert_eq!(schema, plain, "{name} must leave overdepth input unchanged");
        }
        assert!(!strict_schema_supported(&plain));

        let mut tools = vec![test_tool("deep", plain.clone())];
        tools[0].strict = Some(true);
        assert!(!prepare_tools_for_strict_mode(&mut tools));
        assert_eq!(tools[0].strict, None);
        assert_eq!(tools[0].input_schema, plain);

        let mut responses = plain.clone();
        assert_eq!(sanitize_for_responses(&mut responses), None);
        assert_eq!(responses, plain);
        let mut xai = plain.clone();
        assert_eq!(sanitize_for_xai_parameters(&mut xai), None);
        assert_eq!(xai, plain);
        let mut anthropic = plain.clone();
        assert_eq!(sanitize_for_anthropic_messages(&mut anthropic), None);
        assert_eq!(anthropic, plain);

        let mut literal = json!("leaf");
        for _ in 0..=MFJS_MAX_STRUCTURAL_DEPTH {
            literal = json!({"nested": literal});
        }
        let composed = json!({
            "default": literal,
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["replace"]}
            ]
        });
        let expected =
            "Exactly one of these parameter groups must be provided: `patch` | `replace`."
                .to_string();
        let mut responses = composed.clone();
        assert_eq!(
            sanitize_for_responses(&mut responses).as_deref(),
            Some(expected.as_str())
        );
        assert_eq!(responses, composed);
        let mut xai = composed.clone();
        assert_eq!(
            sanitize_for_xai_parameters(&mut xai).as_deref(),
            Some(expected.as_str())
        );
        assert_eq!(xai, composed);
        let mut anthropic = composed.clone();
        assert_eq!(
            sanitize_for_anthropic_messages(&mut anthropic).as_deref(),
            Some(expected.as_str())
        );
        assert_eq!(anthropic, composed);
    }

    #[test]
    fn branch_with_local_properties_keeps_strictly_local_required_resolution() {
        // A branch declaring its own `properties` (the self-contained
        // alternative shape) resolves `required` locally exactly as before:
        // the DeepSeek strict validator reads required against the branch's
        // own advertised properties, so an injected or declared empty
        // properties map must not silently adopt the enclosing names.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "patch": {"type": "string"},
                "handler": {
                    "type": "object",
                    "properties": {"kind": {"type": "string"}},
                    "required": ["kind", "patch"],
                    "oneOf": [
                        {"type": "object", "properties": {"kind": {"type": "string"}}, "required": ["kind", "patch"]},
                        {"type": "string"}
                    ]
                }
            }
        });
        sanitize(&mut schema);
        assert_eq!(
            schema["properties"]["handler"]["required"],
            json!(["kind"]),
            "a local-properties branch prunes against its own map only"
        );
        assert_eq!(
            schema["properties"]["handler"]["oneOf"][0]["required"],
            json!(["kind"]),
            "the local-properties branch alternative prunes the same way"
        );
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
    fn anthropic_messages_sanitize_strips_only_recorded_root_keywords() {
        // The recorded 400 names only top-level oneOf/allOf/anyOf
        // (f8e757322; CHANGELOG 0.9.0; anthropics/claude-code#4886/#30212).
        // Root enum/not, dependent keywords, and the declared root type are
        // not in any record, so the narrowed pass preserves all of them.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {"type": "string"}
            },
            "not": {"type": "integer"},
            "dependentRequired": {"mode": ["confirm"]},
            "dependentSchemas": {
                "mode": {
                    "properties": {"confirm": {"type": "boolean"}},
                    "required": ["confirm"]
                }
            },
            "oneOf": [
                {"required": ["patch"]},
                {"required": ["changes"]}
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("oneOf").is_none());
        assert!(schema.get("anyOf").is_none());
        assert!(schema.get("allOf").is_none());
        assert_eq!(schema["not"], json!({"type": "integer"}));
        assert_eq!(schema["dependentRequired"], json!({"mode": ["confirm"]}));
        assert_eq!(
            schema["dependentSchemas"],
            json!({
                "mode": {
                    "properties": {"confirm": {"type": "boolean"}},
                    "required": ["confirm"]
                }
            }),
            "a well-formed dependent schema ships byte-identically: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `changes` | `patch`.")
        );
    }

    #[test]
    fn anthropic_messages_sanitize_keeps_root_enum_and_non_object_roots() {
        let mut enum_root = json!({
            "type": "string",
            "enum": ["compact", "full"]
        });

        let note = sanitize_for_anthropic_messages(&mut enum_root);

        assert_eq!(
            enum_root,
            json!({
                "type": "string",
                "enum": ["compact", "full"]
            })
        );
        assert_eq!(note, None);
    }

    #[test]
    fn anthropic_messages_sanitize_merges_branch_properties_and_notes_groups() {
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {
                        "patch": {"type": "string"}
                    },
                    "required": ["patch"]
                },
                {
                    "type": "object",
                    "properties": {
                        "changes": {"type": "array", "items": {"type": "string"}}
                    },
                    "required": ["changes"]
                }
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert!(schema.get("anyOf").is_none());
        assert!(schema["properties"].get("patch").is_some());
        assert!(schema["properties"].get("changes").is_some());
        assert_eq!(
            schema["type"], "object",
            "the merge leaves a populated object schema whose root declared no type, so the object type is restored: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some("At least one of these parameter groups must be provided: `changes` | `patch`.")
        );
    }

    #[test]
    fn anthropic_messages_sanitize_notes_the_relaxed_union_when_branches_merge_without_required() {
        // Review 4539837534: merged properties do not compensate for the
        // dropped composition. An `anyOf` of two closed object branches whose
        // properties merge into the root — each with
        // `additionalProperties:false` and no `required`, so no required-group
        // note exists — originally admitted an object with exactly one of the
        // two fields; the merged root accepts both. The model must still be
        // told the branch contract was relaxed via the generic note.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"file": {"type": "string"}},
                    "additionalProperties": false
                },
                {
                    "type": "object",
                    "properties": {"url": {"type": "string"}},
                    "additionalProperties": false
                }
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert!(schema.get("anyOf").is_none(), "{schema}");
        assert!(schema["properties"].get("file").is_some(), "{schema}");
        assert!(schema["properties"].get("url").is_some(), "{schema}");
        assert!(
            schema.get("additionalProperties").is_none(),
            "branch closures must not leak onto the merged root: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "At least one of the original argument alternatives must be provided (top-level `anyOf` is not supported by this API and was removed)."
            ),
            "the relaxed at-least-one contract must be surfaced even though properties merged: {note:?}"
        );
    }

    #[test]
    fn anthropic_messages_sanitize_does_not_inject_type_without_a_root_composition() {
        // Type restoration is scoped to stripped compositions: a declared
        // typeless root with nothing to strip ships exactly as declared.
        let mut schema = json!({"properties": {"x": {"type": "string"}}});

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert_eq!(
            schema,
            json!({"properties": {"x": {"type": "string"}}}),
            "a typeless root without a composition must not gain a type: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn anthropic_messages_sanitize_replaces_non_schema_roots_with_empty_objects() {
        // The API defines input_schema as a JSON Schema object; a root that
        // is an array or scalar (malformed server or MCP output) cannot be
        // one, so the pass replaces it with an empty object exactly as the
        // Responses pass does instead of shipping the raw value to the
        // validator. A DECLARED non-object schema (string-enum root) is an
        // object and keeps shipping verbatim.
        let mut array_root = json!([{"type": "string"}]);

        let note = sanitize_for_anthropic_messages(&mut array_root);

        assert_eq!(
            array_root,
            json!({}),
            "an array root is replaced, not shipped verbatim: {array_root}"
        );
        assert_eq!(note, None);

        let mut scalar_root = json!("browse");

        let note = sanitize_for_anthropic_messages(&mut scalar_root);

        assert_eq!(
            scalar_root,
            json!({}),
            "a scalar root is replaced, not shipped verbatim: {scalar_root}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn anthropic_messages_sanitize_pins_the_post_merge_required_prune() {
        // A single-branch oneOf collapses inside sanitize(), which spreads
        // the branch required groups to the root AFTER sanitize's own
        // dangling-required prune has run; only the explicit post-merge
        // prune removes the ghost entry.
        let mut schema = json!({
            "oneOf": [{
                "type": "object",
                "properties": {"a": {"type": "string"}},
                "required": ["a", "ghost"]
            }]
        });

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert_eq!(
            schema["required"],
            json!(["a"]),
            "the explicit post-merge prune removes required entries spread after sanitize's prune: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn anthropic_messages_sanitize_strips_root_all_of_and_notes_groups() {
        // The recorded 400 names allOf alongside oneOf/anyOf; this pins the
        // strip, the merge, and the All-prefix note for a root allOf.
        let mut schema = json!({
            "allOf": [
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
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut schema);

        assert!(schema.get("allOf").is_none());
        assert_eq!(schema["type"], "object");
        assert!(
            schema["properties"].get("path").is_some() && schema["properties"].get("url").is_some()
        );
        assert_eq!(
            note.as_deref(),
            Some("All of these parameter groups must be provided: `path` | `url`.")
        );
    }

    #[test]
    fn anthropic_messages_sanitize_keeps_the_shared_registry_normalization() {
        // The narrowed pass still runs the shared registry normalization
        // (sanitize): the previous release line collapsed root nullable
        // unions through it, and without that anchor a root
        // `anyOf:[X, null]` would ship the recorded-rejection union shape
        // verbatim. Bare-object properties injection and dangling-required
        // pruning ride the same call.
        let mut nullable = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"q": {"type": "string"}}
                },
                {"type": "null"}
            ]
        });
        let note = sanitize_for_anthropic_messages(&mut nullable);
        assert_eq!(
            nullable,
            json!({
                "type": "object",
                "properties": {"q": {"type": "string"}},
                "nullable": true
            }),
            "a root nullable union must collapse instead of shipping the recorded shape: {nullable}"
        );
        assert_eq!(note, None);

        let mut bare = json!({"type": "object"});
        let note = sanitize_for_anthropic_messages(&mut bare);
        assert_eq!(
            bare,
            json!({"type": "object", "properties": {}}),
            "a bare object root keeps its properties injection: {bare}"
        );
        assert_eq!(note, None);

        let mut dangling = json!({
            "type": "object",
            "properties": {"a": {"type": "string"}},
            "required": ["a", "ghost"]
        });
        let note = sanitize_for_anthropic_messages(&mut dangling);
        assert_eq!(
            dangling["required"],
            json!(["a"]),
            "required naming absent properties is pruned on this lane too: {dangling}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn anthropic_messages_sanitize_restores_the_object_floor_for_property_less_compositions() {
        // A composition whose branches carry no properties — an allOf of
        // $refs (the classic OpenAPI-to-MCP shape), required-only or boolean
        // branches — merges nothing, and without the type floor the wire
        // would carry a constraint-free typeless schema with no note. The
        // previous release line always forced type:"object"; keep that floor
        // for every stripped composition and surface a generic note when the
        // merge contributed nothing.
        let mut refs = json!({
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "$defs": {
                "patch": {"type": "object", "properties": {"patch": {"type": "string"}}}
            },
            "allOf": [
                {"$ref": "#/$defs/patch"},
                {"$ref": "#/$defs/missing"}
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut refs);

        assert_eq!(
            refs["type"], "object",
            "a stripped composition must not ship a typeless root even when no properties merged: {refs}"
        );
        assert!(
            refs.get("allOf").is_none(),
            "the recorded keyword is still stripped: {refs}"
        );
        assert!(
            refs.get("properties").is_none(),
            "no properties are invented when the branches reference $defs: {refs}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "All of the original argument alternatives must be satisfied (top-level `allOf` is not supported by this API and was removed)."
            ),
            "the erased argument contract is surfaced as a note: {note:?}"
        );

        let mut booleans = json!({"oneOf": [true, false]});

        let note = sanitize_for_anthropic_messages(&mut booleans);

        assert_eq!(
            booleans,
            json!({"type": "object"}),
            "boolean branches merge nothing but keep the object floor: {booleans}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of the original argument alternatives must be provided (top-level `oneOf` is not supported by this API and was removed)."
            ),
            "the note names the dropped exactly-one constraint: {note:?}"
        );
    }

    #[test]
    fn anthropic_messages_sanitize_preserves_nested_compositions() {
        // The recorded 400 names only top-level composition keywords, so the
        // strip is root-only exactly like the Responses pass; a nested oneOf
        // inside a property must reach the wire untouched. Without this pin
        // a mutation widening the strip tree-wide stays green.
        let mut nested = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}},
                {"type": "object", "properties": {"b": {"type": "integer"}}}
            ],
            "properties": {
                "payload": {
                    "oneOf": [
                        {"type": "object", "properties": {"x": {"type": "string"}}, "required": ["x"]},
                        {"type": "object", "properties": {"y": {"type": "integer"}}, "required": ["y"]}
                    ]
                }
            }
        });

        let note = sanitize_for_anthropic_messages(&mut nested);

        assert!(
            nested.get("oneOf").is_none(),
            "the root oneOf is stripped: {nested}"
        );
        assert_eq!(
            nested["properties"]["payload"]["oneOf"][0]["required"],
            json!(["x"]),
            "a nested composition is outside the recorded root-only scope: {nested}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of the original argument alternatives must be provided (top-level `oneOf` is not supported by this API and was removed)."
            ),
            "the root branches carry properties but no required groups, so the dropped exactly-one contract is surfaced as the generic note (review 4539837534): {note:?}"
        );
    }

    #[test]
    fn anthropic_messages_sanitize_keeps_root_properties_over_same_named_branch_properties() {
        // merge_root_composition_properties resolves a same-named property
        // first-wins: a property declared at the root keeps its own
        // definition over a branch's redeclaration. Pin the precedence so a
        // silent flip to last-wins cannot narrow the declared contract.
        let mut precedence = json!({
            "type": "object",
            "properties": {"mode": {"type": "string"}},
            "oneOf": [
                {"type": "object", "properties": {"mode": {"type": "integer"}}},
                {"type": "object", "properties": {"extra": {"type": "string"}}}
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut precedence);

        assert_eq!(
            precedence["properties"]["mode"],
            json!({"type": "string"}),
            "the root-declared property wins over a same-named branch property: {precedence}"
        );
        assert_eq!(
            precedence["properties"]["extra"],
            json!({"type": "string"}),
            "branch properties without a root collision still merge: {precedence}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of the original argument alternatives must be provided (top-level `oneOf` is not supported by this API and was removed)."
            ),
            "the branches merge properties without required groups, so no group note exists and the generic note still names the dropped union (review 4539837534): {note:?}"
        );
    }

    #[test]
    fn anthropic_messages_sanitize_notes_every_stripped_composition_keyword() {
        // A root carrying two compositions strips both, and the notes
        // aggregate in the recorded keyword order (review 5898539144: the
        // first-match note told the model only about `a` | `b` while the
        // wire schema also dropped the at-least-one `c` | `d` constraint,
        // so the model could emit arguments outside the original
        // contract).
        let mut both = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]},
                {"type": "object", "properties": {"b": {"type": "string"}}, "required": ["b"]}
            ],
            "anyOf": [
                {"type": "object", "properties": {"c": {"type": "string"}}, "required": ["c"]},
                {"type": "object", "properties": {"d": {"type": "string"}}, "required": ["d"]}
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut both);

        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `a` | `b`. At least one of these parameter groups must be provided: `c` | `d`."
            ),
            "both dropped constraints must be surfaced: {note:?}"
        );
        assert!(
            both.get("oneOf").is_none() && both.get("anyOf").is_none(),
            "both compositions are stripped: {both}"
        );

        // A stripped keyword whose branches carry no labeled groups
        // contributes its generic sentence beside the sibling's specific
        // one instead of being silently dropped by the aggregation.
        let mut mixed = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]},
                {"type": "object", "properties": {"b": {"type": "string"}}, "required": ["b"]}
            ],
            "anyOf": [
                {"type": "object", "properties": {"e": {"type": "string"}}},
                {"type": "object", "properties": {"f": {"type": "string"}}}
            ]
        });

        let note = sanitize_for_anthropic_messages(&mut mixed);

        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `a` | `b`. At least one of the original argument alternatives must be provided (top-level `anyOf` is not supported by this API and was removed)."
            ),
            "the generic sentence fills in for the group-less keyword: {note:?}"
        );
    }

    #[test]
    fn anthropic_messages_pass_matches_the_previous_release_line_on_the_recorded_tools() {
        // The PR's headline behavior claim: apply_patch and finance — the
        // two model-visible built-ins whose root compositions produced the
        // recorded Anthropic 400 — ship byte-identically to the previous
        // release line, which ran sanitize_for_responses on every lane. The
        // registry pre-pass (registry.rs) both lines share runs first. The
        // notes intentionally diverge after review 4539837534: the registry
        // pre-pass leaves no required groups, so the previous line surfaced
        // nothing while this pass names the dropped union contract — the
        // model-facing compensation the narrowed pass exists to keep.
        use crate::tools::apply_patch::ApplyPatchTool;
        use crate::tools::finance::FinanceTool;
        use crate::tools::schema_canonicalize::canonicalize_schema;
        use crate::tools::spec::ToolSpec as _;

        fn previous_line_pipeline(mut schema: Value) -> (Value, Option<String>) {
            sanitize(&mut schema);
            canonicalize_schema(&mut schema);
            let note = sanitize_for_responses(&mut schema);
            (schema, note)
        }

        fn current_line_pipeline(mut schema: Value) -> (Value, Option<String>) {
            sanitize(&mut schema);
            canonicalize_schema(&mut schema);
            let note = sanitize_for_anthropic_messages(&mut schema);
            (schema, note)
        }

        for (name, previous_input, current_input) in [
            (
                "apply_patch",
                ApplyPatchTool.input_schema(),
                ApplyPatchTool.input_schema(),
            ),
            (
                "finance",
                FinanceTool::new().input_schema(),
                FinanceTool::new().input_schema(),
            ),
        ] {
            let (previous, previous_note) = previous_line_pipeline(previous_input);
            let (current, current_note) = current_line_pipeline(current_input);

            assert_eq!(
                previous, current,
                "{name}: the narrowed pass must match the previous release line"
            );
            assert_eq!(
                previous.to_string(),
                current.to_string(),
                "{name}: key order must match too (prompt-cache stability)"
            );
            // Bytes stay identical; the note is the deliberate divergence:
            // the previous line dropped the union contract silently, this
            // pass names it whenever no required-group note exists.
            match (previous_note, current_note) {
                (previous_note, Some(current_note))
                    if previous_note.is_none() || previous_note == Some(current_note.clone()) => {}
                (previous_note, current_note) => panic!(
                    "{name}: notes diverged beyond the reviewed generic-note gap: \
                     previous={previous_note:?} current={current_note:?}"
                ),
            }
        }
    }

    #[test]
    fn xai_sanitize_flattens_apply_patch_root_one_of() {
        // The exact shape that produced the live 400:
        // "apply_patch: tool parameter root must be an object type (root
        // schema is an anyOf/oneOf union with a non-object branch)". The
        // branches carry no explicit `type`, so xAI counts them as
        // non-object branches even after its 2026-10-10 relaxation.
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
    fn xai_sanitize_passes_root_union_of_object_branches_through() {
        // The documented-good shape from xAI's 2026-10-10 function-calling
        // page: "A root `anyOf` or `oneOf` also works when every branch is
        // itself an object". Folding it into one merged object would erase
        // the variant contract the tool documented.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "kind": {"const": "email"},
                        "address": {"type": "string"}
                    },
                    "required": ["kind", "address"]
                },
                {
                    "type": "object",
                    "properties": {
                        "kind": {"const": "sms"},
                        "phone": {"type": "string"}
                    },
                    "required": ["kind", "phone"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("oneOf")
            .and_then(Value::as_array)
            .expect("root oneOf must survive");
        assert_eq!(branches.len(), 2, "both object variants must survive");
        assert_eq!(branches[0]["required"], json!(["kind", "address"]));
        assert_eq!(branches[1]["required"], json!(["kind", "phone"]));
        assert!(
            schema.get("type").is_none(),
            "the root must not gain a forced object type: {schema}"
        );
        assert!(
            schema.get("properties").is_none(),
            "branch properties must not be merged into the root: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_still_flattens_root_union_with_non_object_branch() {
        // A union with any non-object branch still "cannot be compiled into
        // a tool-call grammar" per the 2026-10-10 doc, so the flattening
        // pass must stay for it.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                },
                {"type": "string"}
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(schema.get("anyOf").is_none());
        assert!(schema["properties"].get("path").is_some());
        // The shared flattener's note gate now discloses even a single
        // required group (the merged Moonshot/xAI composition), so the
        // mixed object/non-object union surfaces the surviving branch's
        // contract instead of flattening silently.
        assert_eq!(
            note.as_deref(),
            Some("At least one of these parameter groups must be provided: `path`.")
        );
    }

    #[test]
    fn xai_sanitize_passes_root_anyof_of_object_branches_through() {
        // The relaxation names `anyOf` alongside `oneOf`; pin that the anyOf
        // spelling of the documented-good shape takes the same pass-through
        // route instead of being folded.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                },
                {
                    "type": "object",
                    "properties": {"content": {"type": "string"}},
                    "required": ["content"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("anyOf")
            .and_then(Value::as_array)
            .expect("root anyOf must survive");
        assert_eq!(branches.len(), 2, "both object variants must survive");
        assert!(
            schema.get("type").is_none(),
            "no forced root type: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_flattens_object_union_with_root_all_of_sibling() {
        // The doc blesses "a root anyOf or oneOf", not a union crossed with
        // other root composition keywords. A sibling `allOf` is undocumented,
        // so the protective flattening pass must stay and strip it exactly as
        // before the relaxation.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"mode": {"const": "fast"}},
                    "required": ["mode"]
                },
                {
                    "type": "object",
                    "properties": {"mode": {"const": "slow"}},
                    "required": ["mode"]
                }
            ],
            "allOf": [
                {"properties": {"extra": {"type": "string"}}},
                {"required": ["extra"]}
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none(),
            "union must be flattened: {schema}"
        );
        assert!(
            schema.get("allOf").is_none(),
            "root allOf must be stripped: {schema}"
        );
        assert!(schema["properties"].get("mode").is_some());
        // Merged single-group disclosure (#143): flattening a union whose
        // branches share one surviving distinct required group discloses the
        // relaxation instead of staying silent below the two-group gate.
        // The aggregated note pass (review 5898539144) discloses every
        // dropped composition keyword: the oneOf single surviving group AND
        // the stripped allOf group.
        assert_eq!(
            note.as_deref(),
            Some(
                "Exactly one of these parameter groups must be provided: `mode`. All of these parameter groups must be provided: `extra`."
            )
        );
    }

    #[test]
    fn xai_sanitize_flattens_object_union_with_root_enum_and_not_siblings() {
        // Same composite-root rule for `enum` and `not`: outside the relaxed
        // rule, so the flattening pass removes them as it always did.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": "a"}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "b"}},
                    "required": ["kind"]
                }
            ],
            "enum": [{"kind": "a"}, {"kind": "b"}],
            "not": {"type": "string"}
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("anyOf").is_none(),
            "union must be flattened: {schema}"
        );
        assert!(
            schema.get("enum").is_none(),
            "root enum must be stripped: {schema}"
        );
        assert!(
            schema.get("not").is_none(),
            "root not must be stripped: {schema}"
        );
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("At least one of these parameter groups must be provided: `kind`.")
        );
    }

    #[test]
    fn xai_sanitize_flattens_when_both_union_keywords_are_present() {
        // {"oneOf": [...], "anyOf": [...]} is an intersection of two unions,
        // not the single documented "root anyOf or oneOf". Keep it on the
        // protective flattening pass instead of shipping an undocumented
        // composite grammar shape.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ],
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"c": {"type": "string"}},
                    "required": ["c"]
                },
                {
                    "type": "object",
                    "properties": {"d": {"type": "string"}},
                    "required": ["d"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none(),
            "oneOf must be flattened: {schema}"
        );
        assert!(
            schema.get("anyOf").is_none(),
            "anyOf must be flattened: {schema}"
        );
        for name in ["a", "b", "c", "d"] {
            assert!(
                schema["properties"].get(name).is_some(),
                "branch property {name} must merge into the root: {schema}"
            );
        }
        assert!(
            note.as_deref()
                .is_some_and(|note| note.contains("parameter groups")),
            "the merged union's required groups must surface as a note: {note:?}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_malformed_union_values() {
        // A present-but-non-array union value is garbage the flattening pass
        // removes; it must not ride along a qualifying sibling union to the
        // wire. A lone malformed union is equally non-documentable.
        let mut with_sibling = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ],
            "anyOf": null
        });
        let note = sanitize_for_xai_parameters(&mut with_sibling);
        assert_eq!(with_sibling["type"], "object");
        assert!(
            with_sibling.get("anyOf").is_none(),
            "malformed anyOf must not survive: {with_sibling}"
        );
        assert!(
            note.as_deref()
                .is_some_and(|note| note.contains("parameter groups")),
            "the surviving union's required groups must surface as a note: {note:?}"
        );

        let mut lone = json!({"oneOf": null});
        let note = sanitize_for_xai_parameters(&mut lone);
        assert_eq!(lone["type"], "object");
        assert!(
            lone.get("oneOf").is_none(),
            "malformed oneOf must be removed: {lone}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_flattens_empty_root_union() {
        // An empty union describes nothing; the flattening pass turns the
        // root into a plain object schema as before.
        let mut schema = json!({"oneOf": []});

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none(),
            "empty union must be removed: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_union_passes_through_with_dependent_keyword_note() {
        // The pass-through route keeps the dependent-keyword drop: the union
        // survives AND the dropped conditional requirements surface as the
        // constraint note appended to the tool description.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": "email"}, "address": {"type": "string"}},
                    "required": ["kind", "address"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "sms"}, "phone": {"type": "string"}},
                    "required": ["kind", "phone"]
                }
            ],
            "dependentRequired": {"kind": ["address"]}
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_some(),
            "the union must still pass through: {schema}"
        );
        assert!(
            schema.get("dependentRequired").is_none(),
            "dependent keywords must be dropped on the pass-through route: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some(
                "This provider cannot express conditional requirements from the original \
                 schema. Honor any such requirements documented by the tool when calling it."
            )
        );
    }

    /// Fork-distinct behavior pin (parent-repo fork register): the xAI
    /// parameters lane keeps xAI's documented-good root union of object
    /// branches intact while every composite or non-conforming root shape
    /// stays on the flattening pass, exactly as before the 2026-10-10
    /// relaxation. If either half drifts, the fork's xAI wire contract has
    /// changed and the parent register entry must be revisited.
    #[test]
    fn forkguard_xai_root_union_contract_passes_documented_and_flattens_composites() {
        // Documented-good: survives with branches intact.
        let mut good = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": "email"}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "sms"}},
                    "required": ["kind"]
                }
            ]
        });
        let good_note = sanitize_for_xai_parameters(&mut good);
        assert!(
            good.get("anyOf").is_some(),
            "documented union must survive: {good}"
        );
        assert_eq!(good_note, None);

        // Incident fixture: bare required-groups still flatten (protective).
        use crate::tools::spec::ToolSpec as _;
        let mut incident = crate::tools::apply_patch::ApplyPatchTool.input_schema();
        assert!(
            incident.get("oneOf").is_some(),
            "fixture must match the tool"
        );
        let incident_note = sanitize_for_xai_parameters(&mut incident);
        assert_eq!(incident["type"], "object");
        assert!(
            incident.get("oneOf").is_none(),
            "incident union must flatten"
        );
        assert!(
            incident_note
                .as_deref()
                .is_some_and(|note| note.contains("parameter groups")),
            "incident fixture must keep its required-group note: {incident_note:?}"
        );

        // Composite: union + sibling allOf still flattens (undocumented).
        let mut composite = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"mode": {"const": "fast"}},
                    "required": ["mode"]
                },
                {
                    "type": "object",
                    "properties": {"mode": {"const": "slow"}},
                    "required": ["mode"]
                }
            ],
            "allOf": [{"required": ["mode"]}]
        });
        sanitize_for_xai_parameters(&mut composite);
        assert_eq!(composite["type"], "object");
        assert!(composite.get("oneOf").is_none());
        assert!(composite.get("allOf").is_none());
    }

    #[test]
    fn xai_sanitize_hoists_single_element_object_union() {
        // A len-1 union of one object branch qualifies for the pass-through
        // guard, and the shared pass collapses it to the equivalent plain
        // object root — the one pass-through input whose output is not a
        // union. Pin that collapse instead of an accidental union leak.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "single-element union must collapse to the plain object: {schema}"
        );
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["required"], json!(["path"]));
        assert!(schema["properties"].get("path").is_some());
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_flattens_union_with_bare_required_branches() {
        // Object-typed branches whose only constraint is a bare `required`
        // (the property names live in the root's shared `properties`) used to
        // take the pass-through route, where the shared pass pruned each
        // branch into an identical `{type:"object",properties:{}}` — an
        // exactly-one `oneOf` no instance can satisfy. Such unions now stay
        // on the protective flattening pass: the union is dropped, the
        // constraint survives as the parameter-groups note, and the output
        // stays satisfiable.
        let mut schema = json!({
            "oneOf": [
                {"type": "object", "required": ["patch"]},
                {"type": "object", "required": ["changes"]}
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none(),
            "the gutted union must not reach the wire: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `changes` | `patch`.")
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_whose_bare_required_groups_live_in_root_properties() {
        // Raw-MCP regression (reviews 5788075613 / 5790040245 / 5796658406):
        // `McpPool::to_api_tools` registers the server schema verbatim, and
        // servers commonly declare the union groups against the root's
        // shared `properties`. Pass-through let the shared pass prune both
        // bare-required branches into identical empty objects, making the
        // exactly-one `oneOf` unsatisfiable for every instance — a valid
        // `{"a":"x"}` that originally matched exactly one branch matched both
        // gutted branches and validated against nothing. The flattening pass
        // keeps the root's shared `properties`, re-derives the constraint
        // note, and keeps valid instances valid.
        let mut schema = json!({
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {"type": "object", "required": ["a"]},
                {"type": "object", "required": ["b"]}
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the gutted union must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the root's shared properties must survive the flattening: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `a` | `b`.")
        );
        // JSON-Schema semantics: the flattened root must keep accepting an
        // instance the original union accepted through exactly one branch.
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "a valid instance of the original union must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_oneof_whose_bare_branches_arrive_already_identical() {
        // Registry-to-provider regression (review 5799828607): the ToolRegistry
        // registration path runs the general sanitize on every tool schema
        // BEFORE the wire helpers see it, so a shared-root-properties required
        // union reaches this helper with the branch groups already pruned —
        // the keep-required guard sees nothing left to prune, and two
        // identical bare object branches passed through as an unsatisfiable
        // exactly-one. Identical branches are unsatisfiable however they got
        // that way, so the oneOf flattens; a satisfiable anyOf with duplicate
        // branches keeps the pass-through behavior.
        let mut registry_sanitized = json!({
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {"type": "object", "properties": {}},
                {"type": "object", "properties": {}}
            ]
        });
        // Simulate the actual registry preprocessing, exactly as the
        // registration path applies it (tools/registry.rs).
        sanitize(&mut registry_sanitized);

        let mut schema = registry_sanitized;
        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the identical-branch oneOf must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the root's shared properties must stay usable after the fallback: {schema}"
        );
        assert_eq!(
            note, None,
            "bare branches carry no required groups, so the note is empty: {note:?}"
        );

        // The satisfiable twin: an anyOf with the same duplicate bare branches
        // (nothing to prune, never exactly-one) still passes through.
        let mut anyof_schema = json!({
            "properties": {"a": {"type": "string"}},
            "anyOf": [
                {"type": "object", "properties": {}},
                {"type": "object", "properties": {}}
            ]
        });
        let anyof_note = sanitize_for_xai_parameters(&mut anyof_schema);
        assert!(
            anyof_schema.get("anyOf").is_some(),
            "a satisfiable duplicate-branch anyOf keeps pass-through: {anyof_schema}"
        );
        assert_eq!(anyof_note, None);
    }

    #[test]
    fn xai_sanitize_flattens_oneof_distinct_only_before_recursive_normalization() {
        // Review 5800015446: the raw input's branch `required` groups name
        // their own declared nested object, so the input-level checks all
        // pass — yet the recursive cleanup prunes each nested object's
        // dangling group and injects empty properties, collapsing the two
        // initially distinct alternatives into identical bare objects and an
        // unsatisfiable exactly-one. The pass-through decision must be based
        // on the effective normalized shape; the flattening fallback keeps
        // the shared root `inner` property usable for a valid instance.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "inner": {
                    "type": "object",
                    "properties": {
                        "a": {"type": "string"},
                        "b": {"type": "string"}
                    }
                }
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"inner": {"type": "object", "required": ["a"]}},
                    "required": ["inner"]
                },
                {
                    "type": "object",
                    "properties": {"inner": {"type": "object", "required": ["b"]}},
                    "required": ["inner"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the normalized-identical union must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("inner").is_some(),
            "the shared root inner property must survive the fallback: {schema}"
        );
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `inner`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"inner": {"a": "x"}})),
            "a valid instance of the original union must stay valid: {schema}"
        );

        // The satisfiable control: branches whose nested groups are
        // self-contained (required names live in the same nested object's own
        // properties) stay distinct after normalization and keep the
        // pass-through behavior.
        let mut self_contained = json!({
            "type": "object",
            "properties": {
                "inner": {
                    "type": "object",
                    "properties": {
                        "a": {"type": "string"},
                        "b": {"type": "string"}
                    }
                }
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"inner": {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]}},
                    "required": ["inner"]
                },
                {
                    "type": "object",
                    "properties": {"inner": {"type": "object", "properties": {"b": {"type": "string"}}, "required": ["b"]}},
                    "required": ["inner"]
                }
            ]
        });
        let control_note = sanitize_for_xai_parameters(&mut self_contained);
        assert!(
            self_contained.get("oneOf").is_some(),
            "a self-contained union that stays distinct after normalization keeps pass-through: {self_contained}"
        );
        assert_eq!(control_note, None);
    }

    #[test]
    fn xai_sanitize_flattens_oneof_gutted_branches_that_differ_only_in_annotations() {
        // Review 5800814557: the registry sanitize/canonicalizer strips the
        // branch requirements but keeps `title`/`description`, so raw-equality
        // sees two distinct branches while validation sees two identical
        // exactly-one alternatives. Annotation-only divergence must not
        // rescue an unsatisfiable union; the flattening fallback keeps the
        // shared root properties usable.
        let mut registry_sanitized = json!({
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {"type": "object", "title": "Group A", "description": "use a"},
                {"type": "object", "title": "Group B", "description": "use b"}
            ]
        });
        // Simulate the registry preprocessing exactly (tools/registry.rs):
        // the gutting happens upstream, the annotations survive.
        sanitize(&mut registry_sanitized);
        assert!(
            registry_sanitized["oneOf"][0].get("title").is_some(),
            "fixture precondition: the registry pass keeps annotations: {registry_sanitized}"
        );

        let mut schema = registry_sanitized;
        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the annotation-only-distinct union must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the root's shared properties must stay usable after the fallback: {schema}"
        );
        assert_eq!(note, None);
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "a valid instance of the original union must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_still_coerces_scalar_root_to_object() {
        // The doc's 400 verdict names scalar/array roots, so they must stay on
        // the protective flattening pass and its object coercion — a guard
        // regression that admitted every root would ship the scalar verbatim.
        let mut schema = json!({"type": "string"});

        sanitize_for_xai_parameters(&mut schema);

        assert_eq!(
            schema,
            json!({"type": "object", "properties": {}}),
            "scalar root must take the flattening pass: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_cross_branch_required_union_via_keep_required_belt() {
        // Review 5802378120 form A, raw: each branch's `required` group
        // names a field the OTHER branch declares, under the root's shared
        // `properties`. The keep-required belt sees both groups as prunable
        // and routes the raw shape straight to the flattening pass —
        // nothing below the belt even runs.
        let mut schema = json!({
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["b"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["a"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the raw cross-branch union must flatten via the keep-required belt: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the root's shared properties must survive the flattening: {schema}"
        );
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `a` | `b`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "an instance the original union accepted through exactly one branch must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_registry_pruned_cross_branch_required_union() {
        // Review 5802378120 form A through the real Registry→xAI
        // preprocessing (tools/registry.rs applies sanitize + canonicalize
        // before the wire helpers): the cross-branch groups are already
        // pruned, so the keep-required belt sees nothing left to prune, and
        // the projected branches stay pairwise-distinct — the exact hole
        // the pairwise check could not close. The semantic judgment kills
        // the union by R2 (two required-free object branches both match the
        // empty object, so the exactly-one contract fails there) and the
        // flattening fallback keeps the originally valid a-only argument
        // valid.
        let mut registry_processed = json!({
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["b"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["a"]
                }
            ]
        });
        // Simulate the actual registry preprocessing sequence
        // (tools/registry.rs): the provider-neutral sanitize, then the
        // byte-level canonicalizer.
        sanitize(&mut registry_processed);
        crate::tools::schema_canonicalize::canonicalize_schema(&mut registry_processed);
        assert!(
            registry_processed["oneOf"][0].get("required").is_none()
                && registry_processed["oneOf"][1].get("required").is_none(),
            "fixture precondition: the registry pre-prune gutted both groups: {registry_processed}"
        );
        assert_ne!(
            registry_processed["oneOf"][0], registry_processed["oneOf"][1],
            "fixture precondition: the gutted branches stay pairwise-distinct"
        );

        let mut schema = registry_processed;
        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the empty-matching branch pair must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the root's shared properties must stay usable after the fallback: {schema}"
        );
        assert_eq!(
            note, None,
            "the pruned branches carry no required groups, so the note is empty: {note:?}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "the original a-only argument must stay valid on the flattened schema: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_whose_branch_carries_dead_inner_oneof() {
        // Review 5802577370 form B: the patch/noop union's branches are
        // kept apart by `kind.const`, but the patch branch carries an inner
        // required-group oneOf. The recursive cleanup prunes both inner
        // groups into identical bare objects, the inner exactly-one dies
        // (R1), and R3 propagates the death to the whole patch branch and
        // then to the root union — pass-through would lose the valid
        // patch-branch arguments. The flattening fallback keeps them valid.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "kind": {"const": "patch"},
                        "a": {"type": "string"},
                        "b": {"type": "string"}
                    },
                    "required": ["kind"],
                    "oneOf": [
                        {"type": "object", "required": ["a"]},
                        {"type": "object", "required": ["b"]}
                    ]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "noop"}},
                    "required": ["kind"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "a union with a branch-killing inner oneOf must flatten: {schema}"
        );
        for name in ["kind", "a", "b"] {
            assert!(
                schema["properties"].get(name).is_some(),
                "branch property {name} must merge into the flattened root: {schema}"
            );
        }
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `kind`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"kind": "patch", "a": "x"})),
            "the patch-branch argument must stay valid on the flattened schema: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_passes_union_with_title_named_field_through() {
        // Review 5802577370 inline P3: the annotation strip used to
        // recurse into `properties` maps as if their keys were schema
        // keywords, deleting a real field named `title` from the projected
        // branch. The projection must keep the field (the branches stay
        // validation-distinct) and the union must keep its pass-through
        // contract.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"title": {"type": "string"}},
                    "required": ["title"]
                },
                {
                    "type": "object",
                    "properties": {"name": {"type": "string"}},
                    "required": ["name"]
                }
            ]
        });
        let mut projected = schema.clone();
        strip_annotation_keywords(&mut projected);
        assert!(
            projected["oneOf"][0]["properties"].get("title").is_some(),
            "a field named title is validation surface, not an annotation: {projected}"
        );

        let note = sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("oneOf")
            .and_then(Value::as_array)
            .expect("the title-named-field union must keep pass-through");
        assert_eq!(branches[0]["required"], json!(["title"]));
        assert_eq!(branches[1]["required"], json!(["name"]));
        assert_eq!(note, None);
    }

    #[test]
    fn xai_sanitize_passes_const_differentiated_union_through_without_touching_consts() {
        // The sharp P3 pass-through pin: the branches differ only in
        // `const` values whose keys collide with annotation names. The old
        // strip rewrote both consts into empty objects, made the branches
        // compare equal, and wrongly flattened a genuinely differentiated
        // union; with instance data off limits, the union keeps
        // pass-through and its const values verbatim.
        let mut schema = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"config": {"const": {"title": "A"}}},
                    "required": ["config"]
                },
                {
                    "type": "object",
                    "properties": {"config": {"const": {"title": "B"}}},
                    "required": ["config"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("oneOf")
            .and_then(Value::as_array)
            .expect("a const-differentiated union must keep pass-through");
        assert_eq!(
            branches[0]["properties"]["config"]["const"],
            json!({"title": "A"}),
            "const values are instance data and must reach the wire verbatim: {schema}"
        );
        assert_eq!(
            branches[1]["properties"]["config"]["const"],
            json!({"title": "B"}),
            "const values are instance data and must reach the wire verbatim: {schema}"
        );
        assert_eq!(note, None);
    }

    #[test]
    fn strip_annotation_keywords_keeps_field_names_and_instance_data() {
        // P3 pins: name→schema maps carry field names (a `$defs` entry
        // named `title` survives), `const`/`enum` values are instance data
        // (zero touch), while genuine schema-level annotations — including
        // on nested property schemas — still go.
        let mut schema = json!({
            "title": "outer",
            "description": "outer description",
            "properties": {
                "title": {
                    "type": "string",
                    "description": "kept field, stripped annotation"
                },
                "config": {"const": {"title": "T", "description": "D"}},
                "mode": {"enum": [{"title": "A"}, {"title": "B"}]}
            },
            "$defs": {
                "title": {"type": "string"}
            }
        });
        strip_annotation_keywords(&mut schema);
        assert!(
            schema.get("title").is_none() && schema.get("description").is_none(),
            "schema-level annotations still go: {schema}"
        );
        let title_field = &schema["properties"]["title"];
        assert_eq!(
            title_field["type"], "string",
            "the field named title must survive the strip: {schema}"
        );
        assert!(
            title_field.get("description").is_none(),
            "the property schema's own annotation is still an annotation: {schema}"
        );
        assert_eq!(
            schema["properties"]["config"]["const"],
            json!({"title": "T", "description": "D"}),
            "const values are instance data: zero touch: {schema}"
        );
        assert_eq!(
            schema["properties"]["mode"]["enum"],
            json!([{"title": "A"}, {"title": "B"}]),
            "enum values are instance data: zero touch: {schema}"
        );
        assert_eq!(
            schema["$defs"]["title"],
            json!({"type": "string"}),
            "a $defs entry named title is a definition, not an annotation: {schema}"
        );
    }

    #[test]
    fn oneof_semantic_defect_names_rule_and_position() {
        // R1: identical projected branches.
        let r1 = json!({
            "oneOf": [
                {"type": "object", "properties": {}},
                {"type": "object", "properties": {}}
            ]
        });
        let reason = oneof_semantic_defect(&r1).expect("R1 must fire");
        assert!(
            reason.starts_with("R1 identical branches 0 and 1 at root"),
            "unexpected R1 reason: {reason}"
        );

        // R2: two required-free object branches both match the empty
        // object.
        let r2 = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}},
                {"type": "object", "properties": {"b": {"type": "string"}}}
            ]
        });
        let reason = oneof_semantic_defect(&r2).expect("R2 must fire");
        assert!(
            reason.starts_with("R2 2 non-discriminating branches at root"),
            "unexpected R2 reason: {reason}"
        );

        // R2 floor semantics (review 5811366579): floors cannot restore an
        // erased or absent cross-branch discriminator. `minProperties: 0`
        // is equivalent to omitting the keyword (JSON Schema §6.5.2); a
        // floor of 1 or more excludes `{}` but still leaves the pair
        // matching each other on every shared-root-valid instance above
        // the floors — required-free shapes flatten regardless of floor.
        let reason = oneof_semantic_defect(&r2).expect("R2 must fire");
        assert!(
            reason.starts_with("R2 2 non-discriminating branches at root"),
            "unexpected R2 reason: {reason}"
        );
        for floor in [0, 1, 2] {
            let r2_floored = json!({
                "oneOf": [
                    {
                        "type": "object",
                        "properties": {"a": {"type": "string"}},
                        "minProperties": floor
                    },
                    {
                        "type": "object",
                        "properties": {"b": {"type": "string"}},
                        "minProperties": floor
                    }
                ]
            });
            assert!(
                oneof_semantic_defect(&r2_floored).is_some(),
                "required-free branches are non-discriminating at floor {floor}"
            );
        }

        // R2 shared surviving group (review 5811598796): two object
        // branches whose surviving `required` groups are EQUAL lose
        // exactly-one for every instance — partially pruned groups keep
        // this shape (cross-branch discriminators dropped, the locally
        // shared requirement survives on both). The branches differ only
        // in a keyword the signature never reads (floors — review
        // 5811366579), so their property maps normalize equal and this
        // rule is what judges them. A branch that still declares a
        // differing OPTIONAL PROPERTY is validation surface and routes to
        // the pairwise admission arm instead (review 5886022727 — the
        // shared-group signature reads the whole property map); the
        // companion assertion below pins that such a shape is still a
        // defect, just a pairwise-named one.
        let r2_shared = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"c": {"type": "string"}},
                    "required": ["c"],
                    "minProperties": 1
                },
                {
                    "type": "object",
                    "properties": {"c": {"type": "string"}},
                    "required": ["c"],
                    "minProperties": 2
                }
            ]
        });
        let reason = oneof_semantic_defect(&r2_shared).expect("R2 shared group must fire");
        assert!(
            reason.starts_with("R2 2 branches share the required group"),
            "unexpected shared-group reason: {reason}"
        );
        let r2_shared_with_optional = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["c"]
                }
            ]
        });
        assert!(
            oneof_semantic_defect(&r2_shared_with_optional).is_some(),
            "a shared group with differing optional fields must still take a defect"
        );
        // Different groups stay untouched: an instance carrying one group
        // but not the other still matches exactly one.
        let r2_distinct = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ]
        });
        assert_eq!(
            oneof_semantic_defect(&r2_distinct),
            None,
            "branches with different required groups must stay discriminating"
        );

        // R2 root-implied shared entries (review 5812238335): a vacuous
        // `{}` entry and an entry identical to the containing root's own
        // schema for the field both constrain nothing beyond the root for
        // root-valid instances — they normalize to absent and the equal
        // groups fire.
        let r2_root_implied = json!({
            "type": "object",
            "properties": {"c": {"type": "string"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {}},
                    "required": ["c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["c"]
                }
            ]
        });
        assert!(
            oneof_semantic_defect(&r2_root_implied).is_some(),
            "root-implied shared entries must compare equal"
        );

        // R3: a dead inner oneOf kills its branch and propagates to the
        // root. The death is manufactured by the same normalization the
        // rehearsal runs, so the fixture is normalized first.
        let r3 = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "kind": {"const": "patch"},
                        "a": {"type": "string"},
                        "b": {"type": "string"}
                    },
                    "required": ["kind"],
                    "oneOf": [
                        // The inner required names must be genuinely
                        // dangling: the context-aware pruning keeps a
                        // required-only branch whose names resolve against
                        // the enclosing declared properties, so `a`/`b`
                        // (declared above) would survive and the union
                        // would not die. `zz1`/`zz2` resolve nowhere and
                        // prune into all-accepting `{}` on both sides.
                        {"type": "object", "required": ["zz1"]},
                        {"type": "object", "required": ["zz2"]}
                    ]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "noop"}},
                    "required": ["kind"]
                }
            ]
        });
        let mut normalized = r3;
        sanitize(&mut normalized);
        strip_annotation_keywords(&mut normalized);
        let reason = oneof_semantic_defect(&normalized).expect("R3 must fire");
        assert!(
            reason.starts_with("R3 branch 0 at root carries a dead inner union: R1"),
            "unexpected R3 reason: {reason}"
        );

        // A dead inner oneOf under a nested property propagates too, while
        // a property named `oneOf` is never misread as a union.
        let nested = json!({
            "type": "object",
            "properties": {
                "oneOf": {"type": "string"},
                "config": {
                    "oneOf": [
                        {"type": "object", "properties": {}},
                        {"type": "object", "properties": {}}
                    ]
                }
            }
        });
        let reason = oneof_semantic_defect(&nested).expect("the nested dead union must be found");
        assert!(
            reason.starts_with("R1 identical branches 0 and 1 at root.properties.*"),
            "unexpected nested reason: {reason}"
        );

        // Healthy unions and non-exactly-one composition stay usable.
        let healthy = json!({
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": "a"}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "b"}},
                    "required": ["kind"]
                }
            ]
        });
        assert_eq!(oneof_semantic_defect(&healthy), None);
        let anyof_duplicates = json!({
            "anyOf": [
                {"type": "object", "properties": {}},
                {"type": "object", "properties": {}}
            ]
        });
        assert_eq!(
            oneof_semantic_defect(&anyof_duplicates),
            None,
            "exactly-one rules do not apply to anyOf"
        );
    }

    #[test]
    fn xai_sanitize_registry_path_flattens_minproperties_floored_cross_required_union() {
        // Reviews 5808313704 and 5811366579: the registry's preprocessing
        // (sanitize THEN canonicalize_schema, the exact production order of
        // tools/registry.rs:278-279) prunes both cross-branch required
        // groups; the surviving optional-property maps differ, R1 stays
        // silent, and no `minProperties` floor restores the erased
        // distinction — with floors 1 and 2 alike, every shared-root-valid
        // instance clearing the floors matches both alternatives and the
        // rest match neither, so exactly-one holds for no instance. The
        // full registry path must land on the flattening fallback, keeping
        // each original union's valid witness valid.
        for floor in [1, 2] {
            let mut schema = json!({
                "type": "object",
                "properties": {
                    "a": {"type": "string"},
                    "b": {"type": "string"}
                },
                "oneOf": [
                    {
                        "type": "object",
                        "properties": {"a": {"type": "string"}},
                        "required": ["b"],
                        "minProperties": floor
                    },
                    {
                        "type": "object",
                        "properties": {"b": {"type": "string"}},
                        "required": ["a"],
                        "minProperties": floor
                    }
                ]
            });
            sanitize(&mut schema);
            crate::tools::schema_canonicalize::canonicalize_schema(&mut schema);

            let note = sanitize_for_xai_parameters(&mut schema);

            assert_eq!(schema["type"], "object");
            assert!(
                schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
                "the floored (floor {floor}) cross-required union must not reach the wire: {schema}"
            );
            assert!(
                schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
                "the root's shared properties must survive: {schema}"
            );
            assert_eq!(note, None);
            let validator = jsonschema::options()
                .with_draft(jsonschema::Draft::Draft202012)
                .build(&schema)
                .expect("the flattened schema must compile");
            // Floor 1's original witness needs one property; floor 2's needs
            // two — the second mirrors review 5811366579's {"a":"x","extra":"y"}.
            let witness = if floor == 1 {
                json!({"a": "x"})
            } else {
                json!({"a": "x", "extra": "y"})
            };
            assert!(
                validator.is_valid(&witness),
                "a valid instance of the original union must stay valid at floor {floor}: {schema}"
            );
        }
    }

    #[test]
    fn xai_sanitize_registry_path_flattens_shared_required_cross_branch_union() {
        // Review 5811598796: the registry preprocess drops the cross-branch
        // discriminators (b / a — not declared in the branch's own
        // properties) while the locally declared common requirement (c)
        // survives on BOTH branches. R1 is silent (the optional property
        // maps differ), the required-free R2 is silent, R3 is silent, and
        // the keep-required belt sees only the retained c — yet every
        // root-valid instance carries c and matches both alternatives, so
        // exactly-one holds for no instance. The full production order
        // (sanitize THEN canonicalize_schema, registry.rs:278-279) must
        // land on the flattening fallback, keeping {"a":"x","c":"z"} valid.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"},
                "c": {"type": "string"}
            },
            "required": ["c"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["b", "c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["a", "c"]
                }
            ]
        });
        sanitize(&mut schema);
        crate::tools::schema_canonicalize::canonicalize_schema(&mut schema);

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the shared-required union must not reach the wire: {schema}"
        );
        assert_eq!(
            schema["required"],
            json!(["c"]),
            "the root's own requirement must survive"
        );
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `c`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x", "c": "z"})),
            "a valid instance of the original union must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_registry_path_flattens_root_implied_shared_required_union() {
        // Review 5812238335: the branch-local shared-field signatures
        // diverge (`c:{}` versus `c:{{"type":"string"}}`), but the
        // containing root itself requires `c` and declares it a string —
        // both constraints are implied by the root for every root-valid
        // instance, so the unequal signatures are equivalent and the
        // shared-group arm must fire. The full production order
        // (sanitize THEN canonicalize_schema, registry.rs:278-279) lands
        // on the flattening fallback, keeping {"a":"x","c":"z"} valid.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"},
                "c": {"type": "string"}
            },
            "required": ["c"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {}},
                    "required": ["b", "c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["a", "c"]
                }
            ]
        });
        sanitize(&mut schema);
        crate::tools::schema_canonicalize::canonicalize_schema(&mut schema);

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the root-implied shared-required union must not reach the wire: {schema}"
        );
        assert_eq!(
            schema["required"],
            json!(["c"]),
            "the root's own requirement must survive"
        );
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `c`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x", "c": "z"})),
            "a valid instance of the original union must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_registry_path_flattens_root_stronger_shared_required_union() {
        // Review 5812488975: the shared-group arm normalizes vacuous and
        // root-identical entries away, but a branch-local entry that is
        // STRICTLY WEAKER than the root's own schema for the field
        // (`{"type":"string"}` under a root `minLength: 2`) is neither, so
        // the signatures stay unequal and the equal surviving groups
        // (`[c]` on both branches after the cross-required prune) shipped.
        // The root is stronger than both branch constraints, so every
        // root-valid instance matches both alternatives and exactly-one
        // holds for no instance. The full production order (sanitize THEN
        // canonicalize_schema, registry.rs:278-279) must land on the
        // flattening fallback, keeping {"a":"x","c":"zz"} valid.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"},
                "c": {"type": "string", "minLength": 2}
            },
            "required": ["c"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {}},
                    "required": ["b", "c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "string"}},
                    "required": ["a", "c"]
                }
            ]
        });
        sanitize(&mut schema);
        crate::tools::schema_canonicalize::canonicalize_schema(&mut schema);

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "the root-stronger shared-required union must not reach the wire: {schema}"
        );
        assert_eq!(
            schema["required"],
            json!(["c"]),
            "the root's own requirement must survive"
        );
        assert_eq!(
            schema["properties"]["c"]["minLength"], 2,
            "the root's stronger constraint must survive the fallback: {schema}"
        );
        // Merged single-group disclosure (#143): the flattening pass now
        // discloses one surviving distinct required group instead of staying
        // silent below the two-group gate.
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `c`.")
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x", "c": "zz"})),
            "a valid instance of the original union must stay valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_whose_groups_the_root_already_requires() {
        // Review 5814012140: two object branches whose surviving required
        // groups differ ([a] versus [b]) pass every existing arm, but the
        // enclosing root REQUIRES both names — every root-valid instance
        // carries both groups and matches both alternatives, so exactly-one
        // holds for no instance while the union still ships unflattened.
        // The raw registered-tool path (no prune needed: both branches
        // declare the fields they require) must take the flattening
        // fallback. The pristine union itself admits nothing here, so the
        // final assertion checks FALLBACK USABILITY — the merged schema
        // keeps the instance valid — not original-input preservation
        // (review 5940770432's P3: only the transformed schema is
        // compiled; the instance was never valid against the pristine
        // union).
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "required": ["a", "b"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "a union whose groups the root already requires must not reach the wire: {schema}"
        );
        assert_eq!(
            schema["required"],
            json!(["a", "b"]),
            "the root's own requirement must survive"
        );
        assert_eq!(
            note.as_deref(),
            Some("Exactly one of these parameter groups must be provided: `a` | `b`."),
            "the flattening fallback must keep the parameter-groups note the \
             branches' own declared groups support"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x", "b": "y"})),
            "the flattening fallback must keep the instance usable: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_beside_a_sibling_anyof_the_prune_guts() {
        // Review 5940770432: the object holding the judged union may also
        // carry a sibling composition. A raw-MCP `not` predicate declares
        // its `required` with no local `properties`, so the recursive prune
        // guts `not:{required:["z"]}` into `not:{}` — an alternative that
        // rejects EVERY instance. Both sibling alternatives then reject
        // every instance the inner oneOf's omission witness would carry,
        // while the bounded witness proof only reads the container's
        // `required`/`properties` entries. The container's own-level
        // `anyOf` is an unproven same-instance assertion, so the inner
        // union stays unproven and the whole root takes the flattening
        // fallback, whose merged root keeps the original instance usable:
        // the pristine root here accepts `{"a":"x"}`, and the gutted
        // pass-through would have shipped a schema admitting nothing.
        let branch = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ],
            "anyOf": [
                {"not": {"required": ["z"]}},
                {"not": {"required": ["w"]}}
            ]
        });
        let mut schema = json!({
            "type": "object",
            "anyOf": [branch.clone(), branch]
        });

        let _ = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "a union beside an unproven sibling composition must not reach \
             the wire: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})),
            "the pristine root accepts this instance, so the flattened \
             fallback must keep it usable: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_whose_pins_a_sibling_anyof_voids() {
        // Review 5940770432's pin-arm form: the gutted sibling composition
        // voids the pin arm's witness the same way it voids the omission
        // witness — the pinned value the proof constructs is an instance
        // the `not:{}` alternatives reject, so the pair stays unproven and
        // the root takes the flattening fallback.
        let branch = json!({
            "type": "object",
            "properties": {"kind": {"type": "string"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": "x"}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": "y"}},
                    "required": ["kind"]
                }
            ],
            "anyOf": [
                {"not": {"required": ["z"]}},
                {"not": {"required": ["w"]}}
            ]
        });
        let mut schema = json!({
            "type": "object",
            "anyOf": [branch.clone(), branch]
        });

        let _ = sanitize_for_xai_parameters(&mut schema);

        assert_eq!(schema["type"], "object");
        assert!(
            schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
            "a pin pair whose witness an unproven sibling composition \
             rejects must not reach the wire: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"kind": "x"})),
            "the pristine root accepts this pin, so the flattened fallback \
             must keep it usable: {schema}"
        );
    }

    #[test]
    fn xai_union_defect_walk_computes_each_subtree_once() {
        // Reviews 5814012140 and 5814046611: the R3 recursion plus the
        // outer keyword walk used to visit every branch subtree
        // independently, so each healthy nesting level doubled the work —
        // exponential on a linear-size document, run synchronously during
        // xAI request preparation for every tool-bearing request. The
        // walk memoizes per node, so the computation count stays at the
        // document's node count no matter how deep healthy unions nest.
        fn nested_union(depth: usize) -> Value {
            let child = if depth == 0 {
                // A plain leaf object: the deepest branch carries no
                // further union, and the walk must still have visited it
                // once per node, not once per ancestor path.
                json!({
                    "type": "object",
                    "properties": {"kind": {"const": "leaf"}},
                    "required": ["kind"]
                })
            } else {
                json!({
                    "type": "object",
                    "oneOf": [
                        {
                            "type": "object",
                            "properties": {
                                "kind": {"const": "A"},
                                "child": nested_union(depth - 1)
                            },
                            "required": ["kind"]
                        },
                        {
                            "type": "object",
                            "properties": {"kind": {"const": "B"}},
                            "required": ["kind"]
                        }
                    ]
                })
            };
            json!({
                "type": "object",
                "properties": {"child": child},
                "oneOf": [
                    {
                        "type": "object",
                        "properties": {"kind": {"const": "A"}, "child": {}},
                        "required": ["kind"]
                    },
                    {
                        "type": "object",
                        "properties": {"kind": {"const": "B"}},
                        "required": ["kind"]
                    }
                ]
            })
        }
        fn count_value_nodes(value: &Value) -> usize {
            match value {
                Value::Object(map) => 1 + map.values().map(count_value_nodes).sum::<usize>(),
                Value::Array(items) => 1 + items.iter().map(count_value_nodes).sum::<usize>(),
                _ => 1,
            }
        }
        // Both branches pin `kind` with disjoint consts and the enclosing
        // nodes never constrain the field, so every level is genuinely
        // discriminated — the walk must report healthy, not short-circuit
        // on a defect.
        let deep = nested_union(10);
        let (reason, computed) = oneof_semantic_defect_with_computations(&deep);
        assert_eq!(reason, None, "the deep healthy union must stay usable");
        assert!(
            computed <= count_value_nodes(&deep),
            "each subtree must be computed at most once: {computed} computations for {} nodes",
            count_value_nodes(&deep)
        );
    }

    #[test]
    fn xai_sanitize_flattens_union_whose_witness_fields_carry_further_constraints() {
        // The admission arms' witnesses are omission witnesses: they carry
        // the left branch's group plus everything the enclosing schema
        // requires. A witness is only provable when both shapes stay
        // structural and every carried field's entry is value-permissive —
        // a restrictive `additionalProperties` on the left branch, or a
        // `minLength` on a carried field's entry, could reject the
        // witness, and an unproven pair takes the flattening fallback
        // instead of shipping a possibly unsatisfiable union.
        let cases = [
            // The enclosing schema requires `g`; the left branch does not
            // declare it and closes its own property set, so the witness
            // (forced to carry `g`) cannot match it.
            json!({
                "type": "object",
                "properties": {
                    "a": {"type": "string"},
                    "b": {"type": "string"},
                    "g": {"type": "string"}
                },
                "required": ["g"],
                "oneOf": [
                    {
                        "type": "object",
                        "properties": {"a": {"type": "string"}},
                        "required": ["a"],
                        "additionalProperties": false
                    },
                    {
                        "type": "object",
                        "properties": {"b": {"type": "string"}},
                        "required": ["b"]
                    }
                ]
            }),
            // The left branch's carried field carries a `minLength` the
            // witness's type default does not satisfy.
            json!({
                "type": "object",
                "properties": {
                    "a": {"type": "string", "minLength": 5},
                    "b": {"type": "string"},
                    "g": {"type": "string"}
                },
                "required": ["g"],
                "oneOf": [
                    {
                        "type": "object",
                        "properties": {"a": {"type": "string", "minLength": 5}},
                        "required": ["a"]
                    },
                    {
                        "type": "object",
                        "properties": {"b": {"type": "string"}},
                        "required": ["b"]
                    }
                ]
            }),
        ];
        for case in cases {
            let mut schema = case.clone();
            let note = sanitize_for_xai_parameters(&mut schema);
            assert_eq!(
                schema["type"], "object",
                "an unprovable-witness union must take the flattening fallback: {schema}"
            );
            assert!(
                schema.get("oneOf").is_none() && schema.get("anyOf").is_none(),
                "an unprovable-witness union must not reach the wire: {schema}"
            );
            let validator = jsonschema::options()
                .with_draft(jsonschema::Draft::Draft202012)
                .build(&schema)
                .expect("the flattened schema must compile");
            assert!(
                validator.is_valid(&json!({"a": "xxxxx", "b": "y", "g": "z"})),
                "a valid instance of the original union must stay valid: {case}"
            );
            assert_eq!(
                note.as_deref(),
                Some("Exactly one of these parameter groups must be provided: `a` | `b`."),
                "the flattening fallback keeps the parameter-groups note: {case}"
            );
        }
    }

    /// Review 4509283666: a pin that contradicts its own declared `type`
    /// admits NO instance, so it is not a witness-able pin. Both
    /// contradictory spellings — a `const` of the wrong type and an enum
    /// whose every member violates the type — must fail the pin arm and
    /// take the flattening fallback instead of shipping a union whose
    /// branch cannot be satisfied by any instance.
    #[test]
    fn xai_sanitize_takes_the_fallback_for_contradictory_pin_type_branch_pins() {
        let cases = [
            // const of the wrong type
            json!({
                "oneOf": [
                    {"type": "object", "properties": {"a": {"type": "string", "const": 42}}, "required": ["a"]},
                    {"type": "object", "properties": {"b": {"type": "string", "const": "keep"}}, "required": ["b"]}
                ]
            }),
            // enum whose members all violate the declared type
            json!({
                "oneOf": [
                    {"type": "object", "properties": {"a": {"type": "string", "enum": [1, 2]}}, "required": ["a"]},
                    {"type": "object", "properties": {"b": {"type": "string", "const": "keep"}}, "required": ["b"]}
                ]
            }),
            // empty enum: no value admits it at all
            json!({
                "oneOf": [
                    {"type": "object", "properties": {"a": {"enum": []}}, "required": ["a"]},
                    {"type": "object", "properties": {"b": {"type": "string", "const": "keep"}}, "required": ["b"]}
                ]
            }),
        ];
        for case in cases {
            let mut schema = case.clone();
            let note = sanitize_for_xai_parameters(&mut schema);
            assert!(
                schema.get("oneOf").is_none(),
                "a contradictory pin must fail the witness proof and take the flattening fallback: {schema}"
            );
            assert!(
                note.as_deref()
                    .is_some_and(|note| note.contains("parameter groups")),
                "the flattening fallback keeps the parameter-groups note: {case} -> {note:?}"
            );
        }
    }

    /// Review 4541468073 / 4541483630: `const` and a co-present `enum`
    /// constrain jointly, so a `const` outside its `enum`
    /// (`{"const":"a","enum":["b"]}`) admits NO instance and must not
    /// ground an omission witness. Branches whose pins contradict each
    /// other that way must fail the witness proof and take the flattening
    /// fallback instead of shipping a union no instance can satisfy — and
    /// a consistent `const` inside its `enum` must keep passing through.
    #[test]
    fn xai_sanitize_takes_the_fallback_for_const_outside_enum_branch_pins() {
        // Both branches pin a const their own enum excludes: neither admits
        // an instance, yet each const value is absent from the sibling's
        // enum, so a const-only witness proof would wrongly pass the pair.
        let mut both = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string", "const": "first", "enum": ["second"]}}, "required": ["a"]},
                {"type": "object", "properties": {"b": {"type": "string", "const": "second", "enum": ["first"]}}, "required": ["b"]}
            ]
        });
        let note = sanitize_for_xai_parameters(&mut both);
        assert!(
            both.get("oneOf").is_none(),
            "a const outside its enum admits no instance and must take the flattening fallback: {both}"
        );
        assert!(
            note.as_deref()
                .is_some_and(|note| note.contains("parameter groups")),
            "the flattening fallback keeps the parameter-groups note: {note:?}"
        );

        // One contradictory branch is enough: its pin proves nothing, so
        // the pair stays unproven and the union falls back.
        let mut one_sided = json!({
            "oneOf": [
                {"type": "object", "properties": {"a": {"type": "string", "const": "first", "enum": ["second"]}}, "required": ["a"]},
                {"type": "object", "properties": {"b": {"type": "string", "const": "keep"}}, "required": ["b"]}
            ]
        });
        sanitize_for_xai_parameters(&mut one_sided);
        assert!(
            one_sided.get("oneOf").is_none(),
            "one contradictory pin must still fail the witness proof: {one_sided}"
        );

        // A const inside its enum is a consistent pin: a disjoint pair of
        // such pins keeps the documented pass-through.
        let mut consistent = json!({
            "oneOf": [
                {"type": "object", "properties": {"kind": {"const": "email", "enum": ["email", "web"]}}},
                {"type": "object", "properties": {"kind": {"const": "sms", "enum": ["sms", "web"]}}}
            ]
        });
        sanitize_for_xai_parameters(&mut consistent);
        let branches = consistent.get("oneOf").and_then(Value::as_array);
        assert!(
            branches.is_some_and(|branches| branches.len() == 2),
            "consistent const-in-enum pins must keep pass-through: {consistent}"
        );
    }

    #[test]
    fn xai_sanitize_takes_the_fallback_for_nested_composition_branch_witnesses() {
        // Review 4574653781: a branch may carry its own `anyOf` whose
        // entries require the OTHER field. The witness arms evaluate only
        // the outer `required`/`properties`, so both ordered pairs built an
        // apparent omission witness while the two branches were in fact
        // semantically equivalent — the retained union accepted nothing. An
        // unproven composition at the branch's own level keeps the pair
        // unproven and the union takes the protective flattening fallback,
        // whose merged object admits the instances the original contract
        // served.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {}, "b": {}},
                    "required": ["a"],
                    "anyOf": [
                        {"type": "object", "properties": {"b": {}}, "required": ["b"]},
                        {"type": "object", "properties": {"b": {}}, "required": ["b"]}
                    ]
                },
                {
                    "type": "object",
                    "properties": {"a": {}, "b": {}},
                    "required": ["b"],
                    "anyOf": [
                        {"type": "object", "properties": {"a": {}}, "required": ["a"]},
                        {"type": "object", "properties": {"a": {}}, "required": ["a"]}
                    ]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "the semantically equivalent union must not reach the wire: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "the shared root properties must survive the fallback: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x"})) && validator.is_valid(&json!({"b": "y"})),
            "the flattened fallback must keep both field forms usable: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_takes_the_fallback_for_numeric_pins_equal_under_schema_semantics() {
        // Review 4578150947: JSON Schema treats `1` and `1.0` as the same
        // number, while serde_json's derived equality compares across
        // integer/float variants as unequal. Two branches pinned to
        // `kind: {const: 1}` and `{const: 1.0}` with opposite
        // `dependentRequired` rules have valid original inputs, but the
        // deliberate dependent-keyword drop makes the branches equivalent —
        // and representation equality falsely proved them disjoint, keeping
        // the now-unsatisfiable `oneOf` on the wire. Pin membership must use
        // schema-semantic equality, so the pair stays unproven and the
        // flattening fallback ships a usable contract.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "kind": {"type": "integer"},
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": 1}, "a": {"type": "string"}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": 1.0}, "b": {"type": "string"}},
                    "required": ["kind"]
                }
            ],
            "dependentRequired": {"kind": ["a", "b"]}
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "the semantically equivalent numeric pins must not keep the union: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"kind": 1})),
            "a kind-only argument must stay usable after the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_keeps_distinct_big_integer_pins_discriminated() {
        // Review 4579521065: distinct integer pins such as 9007199254740992
        // and 9007199254740993 collapse to the same f64, so a rounding
        // comparison declared the real discriminator equal and flattened
        // the union — which retains only one pin and rejects the other
        // branch's previously valid input. Integers compare exactly, so
        // the union keeps pass-through with both branches, and a compiled
        // validator accepts either pin value.
        let mut schema = json!({
            "type": "object",
            "properties": {"kind": {"type": "integer"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9007199254740992u64}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9007199254740993u64}},
                    "required": ["kind"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_some(),
            "exact integer pins are a real discriminator and must keep the union: {schema}"
        );
        assert_eq!(note, None);
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the shipped schema must compile");
        assert!(
            validator.is_valid(&json!({"kind": 9007199254740992u64}))
                && validator.is_valid(&json!({"kind": 9007199254740993u64})),
            "both branch inputs must stay valid: {schema}"
        );

        // An integer whose f64 conversion is lossy beside a float of the
        // same rounded value: equality is declined (fail closed), so the
        // equivalent-after-drop pair still takes the flattening fallback
        // instead of a collided proof.
        let mut collided = json!({
            "type": "object",
            "properties": {"kind": {"type": "integer"}, "a": {"type": "string"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9007199254740993u64}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9.007199254740993e15}},
                    "required": ["kind"]
                }
            ],
            "dependentRequired": {"kind": ["a"]}
        });
        sanitize_for_xai_parameters(&mut collided);
        assert!(
            collided.get("oneOf").is_none(),
            "equality outside the exact-integer window must be declined, not collided: {collided}"
        );
    }

    #[test]
    fn xai_sanitize_keeps_the_broad_and_narrow_object_union_through() {
        // Review thread PRRT_kwDOTh_H9863sevQ: one branch constrains
        // nothing beyond objectness while the other requires `x` — the
        // union IS satisfiable (`{}` matches only the broad branch;
        // `{"x":"v"}` matches both and exactly-one rejects it). The
        // two-directional witness demand used to flatten this shape,
        // merging `x` into the root and silently widening the contract
        // (`{"x":"v"}` accepted, `{"x":3}` still accepted). The
        // broad-matcher arm keeps the union verbatim, so the original
        // accepted and rejected sets survive untouched.
        let mut schema = json!({
            "type": "object",
            "oneOf": [
                {"type": "object"},
                {
                    "type": "object",
                    "properties": {"x": {"type": "string"}},
                    "required": ["x"]
                }
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_some(),
            "the satisfiable broad+narrow union must keep pass-through: {schema}"
        );
        assert_eq!(note, None);
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the shipped schema must compile");
        assert!(
            validator.is_valid(&json!({})),
            "an object matching only the broad branch stays valid: {schema}"
        );
        assert!(
            !validator.is_valid(&json!({"x": "v"})),
            "an object matching both branches stays rejected: {schema}"
        );
        assert!(
            validator.is_valid(&json!({"x": 3})),
            "an object failing the narrow branch's type stays valid: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_takes_the_fallback_when_mixed_number_equality_is_unknown() {
        // Review thread PRRT_kwDOTh_H9863shgR: `9007199254740994` and
        // `9.007199254740994e15` are mathematically equal but only their
        // f64 values agree — the integer exceeds the ±2^53 exact window,
        // so disjointness is NOT provable. A root `type: number` union
        // distinguishing these pins via branch-local `dependentSchemas`
        // loses that distinction when the dependent keywords drop; treating
        // the unknown comparison as disjoint "proved" a discriminator
        // between now-equivalent branches and shipped an unsatisfiable
        // union. Unknown equality keeps the pair unproven and the union
        // takes the flattening fallback.
        // The branches arrive at the admission arms with the dependent
        // keywords already stripped (the pass-through drops them with the
        // constraint note), so only the mixed-number pins remain to
        // distinguish them.
        let mut schema = json!({
            "type": "object",
            "properties": {"kind": {"type": "number"}, "a": {"type": "string"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9007199254740994u64}},
                    "required": ["kind"]
                },
                {
                    "type": "object",
                    "properties": {"kind": {"const": 9.007199254740994e15}},
                    "required": ["kind"]
                }
            ],
            "dependentRequired": {"kind": ["a"]}
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "an unknown mixed-number comparison must not prove disjointness: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_declines_the_nested_witness_when_the_root_forbids_the_containing_object() {
        // Review 5928255936 (P2): Arm3 proved the nested a/b omission witness
        // for `payload` without checking that any ROOT-valid object can carry
        // it. With the root requiring `flag` under `maxProperties: 1`, no
        // root-valid instance can hold `payload` beside `flag`, so every
        // root-valid object matches both projected branches (payload omitted)
        // and the preserved oneOf accepts nothing. The flattened fallback
        // keeps the flag-only arguments usable. The branches declare their
        // required names locally, which is what routes the pair into Arm3 at
        // all.
        let mut schema = json!({
            "type": "object",
            "properties": {"flag": {"type": "string"}},
            "required": ["flag"],
            "maxProperties": 1,
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"a": {"type": "string"}},
                            "required": ["a"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                },
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"b": {"type": "string"}},
                            "required": ["b"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a nested witness no root-valid object can carry must not keep the union: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"flag": "on"})),
            "the flag-only argument must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_declines_the_nested_witness_when_a_carried_field_has_disjoint_types() {
        // Review 5928255936 (P2, carried-field form): the root-level witness
        // object carries the branch's required names too. When the branch
        // and the root declare one of those names with disjoint types, no
        // concrete value satisfies both entries, so the containing object
        // cannot be constructed and the nested proof must not fire.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "flag": {"type": "string"},
                "extra": {"type": "string"}
            },
            "required": ["flag", "extra"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "extra": {"type": "number"},
                        "payload": {
                            "type": "object",
                            "properties": {"a": {"type": "string"}},
                            "required": ["a"]
                        }
                    },
                    "required": ["flag", "extra"],
                    "dependentRequired": {"flag": ["payload"]}
                },
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "extra": {"type": "number"},
                        "payload": {
                            "type": "object",
                            "properties": {"b": {"type": "string"}},
                            "required": ["b"]
                        }
                    },
                    "required": ["flag", "extra"],
                    "dependentRequired": {"flag": ["payload"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "disjoint carried-field types must not keep the union: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"flag": "on", "extra": "s"})),
            "the root-valid argument must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_declines_the_nested_witness_when_the_root_anyof_forbids_the_field() {
        // Review 5928255936 (P2, root-composition form): the root's own
        // `anyOf` can forbid the containing field outright, so the bounded
        // keyword gate must treat a root carrying `anyOf` as outside the
        // witness model even though the field-entry checks alone would admit
        // the object. Every root-valid instance here is flag-only, which
        // matches both projected branches.
        let mut schema = json!({
            "type": "object",
            "properties": {"flag": {"type": "string"}},
            "required": ["flag"],
            "anyOf": [
                {"not": {"required": ["payload"]}},
                {"not": {"required": ["payload"], "properties": {"flag": {"type": "string"}}}}
            ],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"a": {"type": "string"}},
                            "required": ["a"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                },
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"b": {"type": "string"}},
                            "required": ["b"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a root anyOf forbidding the containing field must not keep the union: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"flag": "on"})),
            "the flag-only argument must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_declines_the_nested_witness_under_a_closed_root() {
        // Review 5928255936 (P2, closed-root form): with the root forbidding
        // undeclared properties, the branch-only `payload` field cannot exist
        // in any root-valid instance; the hypothetical payload object is not
        // a witness and the projected oneOf keeps no usable argument.
        let mut schema = json!({
            "type": "object",
            "properties": {"flag": {"type": "string"}},
            "required": ["flag"],
            "additionalProperties": false,
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"a": {"type": "string"}},
                            "required": ["a"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                },
                {
                    "type": "object",
                    "properties": {
                        "flag": {"type": "string"},
                        "payload": {
                            "type": "object",
                            "properties": {"b": {"type": "string"}},
                            "required": ["b"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["payload"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a closed root must not keep a union whose witness is branch-only: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"flag": "on"})),
            "the flag-only argument must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_keeps_compound_pin_disjointness_unproven_for_unknown_children() {
        // Review 5916351834 (P2-1): the scalar mixed-number fix declines
        // equality outside the ±2^53 window, but the compound fall-through
        // negated recursive equality into a proof of disjointness. Wrapping
        // the same mathematically-equal pins in arrays
        // (`[9007199254740994]` vs `[9007199254740994.0]`) therefore
        // "proved" a discriminator between branches the rehearsal leaves
        // equivalent: the original `{"tag":[9007199254740994],"a":"x"}`
        // matches only the first branch, while every root-valid instance
        // matches both (dependencies dropped) or neither (scalar pins
        // differ) — the shipped oneOf accepts nothing. Compound disjointness
        // must be proved positively (shape mismatch or a provably-disjoint
        // child), never by negating an unknown.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "tag": {"type": "array"},
                "a": {"type": "string"},
                "b": {"type": "string"}
            },
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"tag": {"const": [9007199254740994u64]}},
                    "required": ["tag"],
                    "dependentRequired": {"tag": ["a"]}
                },
                {
                    "type": "object",
                    "properties": {"tag": {"const": [9.007199254740994e15]}},
                    "required": ["tag"],
                    "dependentRequired": {"tag": ["b"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "an unknown comparison inside compound pins must not prove disjointness: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"tag": [9007199254740994u64], "a": "x"})),
            "the original argument must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_declines_the_nested_witness_under_a_scalar_typed_root_field() {
        // Review 5916351834 (P2-2): Arm 3 proved the nested a/b omission
        // witness against the branch entries alone; the CONTAINING root
        // field's own value constraint never entered the proof. With the
        // root declaring `payload: {type: string}` while both branches
        // declare `payload` as objects carrying distinct required groups,
        // every root-valid instance either omits payload (matches both
        // branches) or carries a string payload (matches neither) — the
        // projected exactly-one accepts nothing, while the flattening
        // fallback keeps the root contract usable. The witness must be
        // root-valid: an object-shaped nested witness under a scalar-typed
        // root field takes the protective fallback.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "flag": {"type": "string"},
                "payload": {"type": "string"},
                "x": {"type": "string"},
                "y": {"type": "string"}
            },
            "required": ["flag"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {
                        "payload": {
                            "type": "object",
                            "properties": {"a": {"type": "string"}},
                            "required": ["a"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["x"]}
                },
                {
                    "type": "object",
                    "properties": {
                        "payload": {
                            "type": "object",
                            "properties": {"b": {"type": "string"}},
                            "required": ["b"]
                        }
                    },
                    "required": ["flag"],
                    "dependentRequired": {"flag": ["y"]}
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a nested witness must stay root-valid under the containing field's constraint: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"flag": "on", "x": "x"})),
            "the root-valid instance must stay valid under the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_the_composition_broad_branch_pair() {
        // Review 4579889873: a branch whose only same-instance constraint
        // lives in its own `anyOf` has no top-level `required`/`properties`,
        // so the broad-matcher arm used to call it broad beside a
        // constrained sibling — but after normalization both branches
        // accept exactly the same objects and the preserved outer `oneOf`
        // accepts none. A broad branch must carry no own-level
        // composition; this pair takes the flattening fallback.
        let branch_with_anyof = |group: &str| {
            json!({
                "type": "object",
                "anyOf": [
                    {"type": "object", "properties": {group: {"type": "string"}}, "required": [group]},
                    {"type": "object", "properties": {group: {"type": "string"}}, "required": [group]}
                ]
            })
        };
        let mut schema = json!({
            "type": "object",
            "properties": {"x": {"type": "string"}},
            "oneOf": [
                branch_with_anyof("x"),
                {
                    "type": "object",
                    "properties": {"x": {"type": "string"}},
                    "required": ["x"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "the composition-broad pair must not keep the impossible union: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"x": "v"})),
            "the merged contract must keep the shared form usable: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_the_broad_pair_when_the_enclosing_requires_the_group() {
        // Review 4580074703: when the ENCLOSING schema requires the same
        // fields the constrained branch requires, every root-valid
        // instance matches both branches — the omission witness cannot be
        // root-valid — and the preserved union would accept nothing. The
        // broad-matcher arm must stay silent here and let the flattening
        // fallback ship a usable merged contract.
        let mut schema = json!({
            "type": "object",
            "properties": {"x": {"type": "string"}},
            "required": ["x"],
            "oneOf": [
                {"type": "object"},
                {
                    "type": "object",
                    "properties": {"x": {"type": "string"}},
                    "required": ["x"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a group fully required by the enclosing schema leaves no exactly-one instance: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"x": "v"})),
            "the merged contract keeps the root-valid form usable: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_the_broad_pair_under_enclosing_floor_constraints() {
        // Review 4580299113: the omission witness can be impossible even
        // without an enclosing `required` — `minProperties: 1` beside
        // `additionalProperties: false` with only root property `x` forces
        // every root-valid instance to carry `x`, so the broad+narrow pair
        // matches both branches everywhere and the preserved union accepts
        // nothing. Enclosing schemas carrying instance-existence keywords
        // beyond the bounded shape keep the arm silent; the flattening
        // fallback ships the merged contract.
        let mut schema = json!({
            "type": "object",
            "properties": {"x": {"type": "string"}},
            "minProperties": 1,
            "additionalProperties": false,
            "oneOf": [
                {"type": "object"},
                {
                    "type": "object",
                    "properties": {"x": {"type": "string"}},
                    "required": ["x"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "an enclosing floor can make every omission impossible: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"x": "v"})),
            "the merged contract keeps the root-valid form usable: {schema}"
        );
    }

    /// Reviews 4509301139 and 4509370637: a required-free object pair whose
    /// `kind` const-pins are disjoint is a usable documented union — each
    /// pin value matches exactly one branch — so it must keep pass-through
    /// instead of being flattened (the fallback merges only the FIRST
    /// branch's `kind` pin into the root and would reject the `sms`
    /// variant). Both variants must survive verbatim.
    #[test]
    fn xai_sanitize_keeps_required_free_kind_discriminated_union_through() {
        let mut schema = json!({
            "oneOf": [
                {"type": "object", "properties": {"kind": {"const": "email"}}},
                {"type": "object", "properties": {"kind": {"const": "sms"}}}
            ]
        });

        let note = sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("oneOf")
            .and_then(Value::as_array)
            .expect("a pin-discriminated required-free union must keep pass-through: {schema}");
        assert_eq!(branches.len(), 2, "both variants must survive: {schema}");
        assert_eq!(
            branches[0]["properties"]["kind"]["const"],
            json!("email"),
            "the email variant must survive verbatim: {schema}"
        );
        assert_eq!(
            branches[1]["properties"]["kind"]["const"],
            json!("sms"),
            "the sms variant must not be flattened away: {schema}"
        );
        assert_eq!(note, None);
    }

    /// The exemption above must not open a wider door: a required-free
    /// branch whose pins are NOT disjoint from every sibling (or a pin
    /// set that only pairwise-discriminates, review 4511007764) still
    /// counts as non-discriminating or fails the pairwise admission, and
    /// the union takes the flattening fallback. `kind` enums `{a,b}`,
    /// `{b,c}`, `{a,c}` pass every ordered pair with per-pair witnesses
    /// yet admit no instance against all branches together.
    #[test]
    fn xai_sanitize_takes_the_fallback_when_no_single_witness_survives_all_branches() {
        let mut schema = json!({
            "oneOf": [
                {"type": "object", "properties": {"kind": {"enum": ["a", "b"]}}, "required": ["kind"]},
                {"type": "object", "properties": {"kind": {"enum": ["b", "c"]}}, "required": ["kind"]},
                {"type": "object", "properties": {"kind": {"enum": ["a", "c"]}}, "required": ["kind"]}
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "overlapping multi-branch pin sets must take the flattening fallback: {schema}"
        );

        // The required-free count layer refuses the same overlap too:
        // three required-free branches whose only pins overlap on `kind`
        // cannot all be exempt, so the count fires before any witness.
        let mut required_free = json!({
            "oneOf": [
                {"type": "object", "properties": {"kind": {"enum": ["a", "b"]}}},
                {"type": "object", "properties": {"kind": {"enum": ["b", "c"]}}},
                {"type": "object", "properties": {"kind": {"enum": ["a", "c"]}}}
            ]
        });
        sanitize_for_xai_parameters(&mut required_free);
        assert!(
            required_free.get("oneOf").is_none(),
            "overlapping required-free pins must take the flattening fallback: {required_free}"
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
    fn xai_sanitize_flattens_union_whose_shared_witness_field_admits_no_common_value() {
        // The omission witness carries the left branch's group AND the
        // enclosing required once, so one value must satisfy BOTH entries
        // for the shared field. Two independently value-permissive entries
        // with disjoint types — the root's `c: string` beside each
        // branch's `c: number` — admit no such value, so every proof must
        // fail and the union takes the flattening fallback (review
        // 5886022727) instead of passing an unsatisfiable root union
        // through.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string"},
                "b": {"type": "string"},
                "c": {"type": "string"}
            },
            "required": ["c"],
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}, "c": {"type": "number"}},
                    "required": ["a", "c"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}, "c": {"type": "number"}},
                    "required": ["b", "c"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert!(
            schema.get("oneOf").is_none(),
            "a joint-constraint union with disjoint shared types must take \
             the flattening fallback: {schema}"
        );
        assert_eq!(
            schema["properties"]["c"],
            json!({"type": "string"}),
            "the fallback keeps the root's usable c:string entry: {schema}"
        );
        let validator = jsonschema::options()
            .with_draft(jsonschema::Draft::Draft202012)
            .build(&schema)
            .expect("the flattened schema must compile");
        assert!(
            validator.is_valid(&json!({"a": "x", "c": "s"})),
            "the flattened fallback keeps a usable c:string object: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_flattens_object_union_under_a_declared_non_object_root_type() {
        // The eligibility guard reads the root's own declared type too: a
        // `string` root cannot satisfy an object branch, so pass-through
        // would ship a root with no valid instance (review 5886022727).
        // The compatibility fallback forces the object type and merges
        // the branch properties, keeping both fields usable.
        let mut schema = json!({
            "type": "string",
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "required": ["a"]
                },
                {
                    "type": "object",
                    "properties": {"b": {"type": "string"}},
                    "required": ["b"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        assert_eq!(
            schema["type"], "object",
            "a declared non-object root takes the compatibility fallback: {schema}"
        );
        assert!(
            schema.get("anyOf").is_none(),
            "the ineligible union is flattened: {schema}"
        );
        assert!(
            schema["properties"].get("a").is_some() && schema["properties"].get("b").is_some(),
            "both branch fields stay usable after the fallback: {schema}"
        );
    }

    #[test]
    fn xai_sanitize_keeps_optional_pin_discriminated_shared_group_union_through() {
        // Two branches sharing the required group `c` but pinning their
        // optional `kind` fields to different values ARE discriminated:
        // each tagged instance matches exactly one branch. The
        // shared-group signature must therefore read the branches'
        // optional entries too — declaring them equivalent takes the
        // fallback, the fallback keeps only one pin, and the other valid
        // variant is rejected with no note (review 5886022727).
        let mut schema = json!({
            "type": "object",
            "properties": {"c": {"type": "string"}},
            "oneOf": [
                {
                    "type": "object",
                    "properties": {"c": {"type": "string"}, "kind": {"const": "email"}},
                    "required": ["c"]
                },
                {
                    "type": "object",
                    "properties": {"c": {"type": "string"}, "kind": {"const": "sms"}},
                    "required": ["c"]
                }
            ]
        });

        sanitize_for_xai_parameters(&mut schema);

        let branches = schema
            .get("oneOf")
            .expect("a pin-discriminated shared-group union must ship: {schema}");
        assert_eq!(
            branches[0]["properties"]["kind"],
            json!({"const": "email"}),
            "the email variant must survive production preprocessing: {schema}"
        );
        assert_eq!(
            branches[1]["properties"]["kind"],
            json!({"const": "sms"}),
            "the sms variant must survive production preprocessing: {schema}"
        );
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
        // entirely. The unconstrained object alternative accepts `{}`, so
        // the disclosure stays OPTIONAL — the wire schema permits omitting
        // the group, and a "must" hint would walk the model onto an
        // unnecessary path (review 4604994801).
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
            Some("At least one of these parameter groups may be provided: `path`.")
        );
    }
}

/// Intersect a parent `type` with every nested `anyOf` branch. Branches with
/// a provably disjoint type are removed; an untyped `$ref` branch ships
/// verbatim only when its resolved target carries the parent type exactly —
/// any other `$ref` (unresolvable, strictly broader than the intersection,
/// or without a plain-string target type) aborts the whole node so
/// validation rejects the retained `type` + `anyOf` pair instead of a
/// branch whose parent constraint lost its wire carrier — array-form
/// `type` branches are narrowed to the intersection exactly, and
/// bool-schema branches are projected exactly (a `false` branch
/// is removed, a `true` branch collapses the union onto the parent type) so
/// a representable contract does not fail validation as a whole. A parent
/// type that conflicts with EVERY branch is reported as
/// [`KimiParameterSchemaError::ContradictoryParentUnion`] instead of leaving
/// an emptied union for validation to misreport as a resource failure.
/// `oneOf` is deliberately not rewritten.
pub fn sanitize_for_kimi(schema: &mut serde_json::Value) -> Result<(), KimiParameterSchemaError> {
    // Recursion protection for the walk below: the guard runs once here
    // instead of at every recursion node — re-walking the full subtree per
    // node made the pass O(n·d) with a transient allocation per node, and
    // the entry budgets already bound the root depth (round-6 review).
    // Mutations below can only add depth bounded by the input structure,
    // and `validate_mfjs_parameters` re-checks the final depth.
    if !schema_within_structural_depth_limit(schema) {
        return Ok(());
    }
    // Branch-level `$ref` projection resolves against the root `$defs`.
    // Snapshot them once (the lane's byte budget bounds the copy) so the
    // walk can read the author's declared targets without borrowing against
    // the in-flight mutations.
    let defs = schema
        .get("$defs")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default();
    sanitize_kimi_node(schema, &defs)
}

fn sanitize_kimi_node(
    schema: &mut serde_json::Value,
    defs: &serde_json::Map<String, Value>,
) -> Result<(), KimiParameterSchemaError> {
    if let Some(obj) = schema.as_object_mut() {
        for map_key in ["properties", "$defs"] {
            if let Some(children) = obj.get_mut(map_key).and_then(Value::as_object_mut) {
                for child in children.values_mut() {
                    sanitize_kimi_node(child, defs)?;
                }
            }
        }
        if let Some(items) = obj.get_mut("items") {
            sanitize_kimi_node(items, defs)?;
        }
        if let Some(additional) = obj.get_mut("additionalProperties")
            && additional.is_object()
        {
            sanitize_kimi_node(additional, defs)?;
        }

        let parent_type = obj.get("type").and_then(Value::as_str).map(str::to_string);
        if parent_type.is_some() && obj.get("anyOf").is_some_and(Value::is_array) {
            // Bool-schema branches fail MFJS validation verbatim and would
            // take the whole tool with them; the exact projections are
            // trivial. A `true` branch admits every instance, so the whole
            // union is the parent type alone and the union keyword goes —
            // an emptied union would itself fail validation. A `false`
            // branch contributes nothing and is dropped before the typed
            // walk; an all-`false` union is unsatisfiable and reports the
            // same contradiction the walk reports for a fully disjoint
            // union (round-4 review).
            let union_is_vacuous =
                obj.get("anyOf")
                    .and_then(Value::as_array)
                    .is_some_and(|branches| {
                        branches
                            .iter()
                            .any(|branch| matches!(branch, Value::Bool(true)))
                    });
            if union_is_vacuous {
                obj.remove("anyOf");
            } else if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
                branches.retain(|branch| !branch.is_boolean());
                if branches.is_empty() {
                    return Err(KimiParameterSchemaError::ContradictoryParentUnion);
                }
                // All-or-nothing: a branch that cannot be projected exactly
                // (an unresolvable or over-broad `$ref`, an unrepresentable
                // `type` form) aborts the node, which is left as
                // `{type, anyOf}` for the validator's union-sibling whitelist
                // to reject with its precise diagnosis. The projection is
                // only written back when every branch carries the exact
                // intersection — a branch must never ship after its parent
                // constraint lost its wire carrier (round-5 review).
                if project_typed_union_branches(branches, parent_type.as_deref().unwrap(), defs)? {
                    obj.remove("type");
                }
            }
        }
        // The projection above runs BEFORE this descent: a branch killed by
        // its own nested union is dropped there, so the walk below never
        // enters a branch whose subtree is unsatisfiable — walking it first
        // raised `ContradictoryParentUnion` for the whole tool even though
        // the enclosing union keeps live branches (round-6 review).
        if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
            for branch in branches {
                sanitize_kimi_node(branch, defs)?;
            }
        }
        if parent_type.is_some()
            && obj.get("anyOf").is_some()
            && obj
                .get("properties")
                .and_then(Value::as_object)
                .is_some_and(Map::is_empty)
        {
            obj.remove("properties");
        }
    }
    Ok(())
}

/// Outcome of projecting one branch of a typed union onto the parent type.
#[derive(Debug)]
enum BranchProjection {
    /// The branch carries the exact intersection after in-place projection.
    Keep,
    /// The branch is provably disjoint from the parent type.
    Drop,
    /// The branch cannot be projected exactly; the enclosing union must be
    /// left verbatim so its retained `type` beside `anyOf` fails validation
    /// closed instead of a constraint-free branch shipping.
    AbortNode,
}

/// Project every branch of a typed union all-or-nothing. Returns whether the
/// projection applied: `false` means some branch could not be represented
/// exactly and the caller must leave the union's `type` in place (the
/// validator then rejects the `type`-beside-`anyOf` pair with its precise
/// diagnosis). A provably disjoint branch is dropped; a union whose branches
/// all vanish reports
/// [`KimiParameterSchemaError::ContradictoryParentUnion`]. The projected
/// branches are only written back once every branch projected, so an abort
/// never leaves a half-projected union behind.
fn project_typed_union_branches(
    branches: &mut Vec<Value>,
    parent_type: &str,
    defs: &serde_json::Map<String, Value>,
) -> Result<bool, KimiParameterSchemaError> {
    let original = branches.clone();
    let mut kept = Vec::with_capacity(original.len());
    for mut branch in original {
        match intersect_kimi_branch_type(&mut branch, parent_type, defs) {
            Ok(BranchProjection::Keep) => kept.push(branch),
            Ok(BranchProjection::Drop) => {}
            Ok(BranchProjection::AbortNode) => return Ok(false),
            Err(error) => return Err(error),
        }
    }
    if kept.is_empty() {
        return Err(KimiParameterSchemaError::ContradictoryParentUnion);
    }
    *branches = kept;
    Ok(true)
}

/// Project a `$ref` branch of a typed union. The wire allows `$ref` only
/// beside annotations and `type` only beside the union keyword, so the
/// parent constraint has no carrier of its own: it survives only inside the
/// resolved target. An untyped `$ref` therefore ships verbatim when its
/// target provably carries the intersection, drops when it is provably
/// disjoint, and aborts the node whenever exactness cannot be proven
/// (round-5 review: verbatim-keeping used to widen the wire).
fn project_ref_branch(
    obj: &mut serde_json::Map<String, Value>,
    parent_type: &str,
    defs: &serde_json::Map<String, Value>,
) -> Result<BranchProjection, KimiParameterSchemaError> {
    let Some(target) = obj
        .get("$ref")
        .and_then(Value::as_str)
        .and_then(|reference| resolve_kimi_ref_target(defs, reference))
    else {
        return Ok(BranchProjection::AbortNode);
    };
    match target.get("type").and_then(Value::as_str) {
        Some(target_type) => match intersect_schema_types(parent_type, target_type) {
            // The target itself carries the intersection exactly: the
            // reference ships verbatim (the shape round-4 pinned).
            Some(effective) if effective == target_type => Ok(BranchProjection::Keep),
            // The intersection is strictly narrower than the target; no
            // verbatim spelling is exact, so the node aborts closed.
            Some(_) => Ok(BranchProjection::AbortNode),
            None => Ok(BranchProjection::Drop),
        },
        // A target without a plain-string `type` cannot prove exactness.
        None => Ok(BranchProjection::AbortNode),
    }
}

/// Resolve a branch-level `#/$defs/<name>` reference against the root
/// `$defs`, admitting one decoded single-name segment exactly like
/// [`resolve_mfjs_reference`] does (round-4 review).
fn resolve_kimi_ref_target<'a>(
    defs: &'a serde_json::Map<String, Value>,
    reference: &str,
) -> Option<&'a Value> {
    let name = reference.strip_prefix("#/$defs/")?;
    let decoded = name.replace("~1", "/").replace("~0", "~");
    if decoded.is_empty() || decoded.contains('/') {
        return None;
    }
    defs.get(&decoded).filter(|target| target.is_object())
}

/// Apply a same-instance parent type to one branch of a typed union. Nested
/// unions receive the constraint recursively instead of gaining an invalid
/// `type + anyOf` pair. An untyped `$ref` branch is projected through its
/// resolved target (see [`project_ref_branch`]); a `$ref` that already
/// carries a `type` keeps the plain typed-branch path, where a disjoint
/// spelling is dropped and a satisfiable one is stamped for the validator's
/// ref-sibling whitelist to judge. Array-form `type` members are narrowed to
/// their exact intersection with the parent instead of shipping un-narrowed.
/// Bool-schema branches are projected exactly: `false` admits no instance
/// and is removed, `true` admits every instance so the union collapses onto
/// its effective type. A branch that cannot be projected exactly reports
/// [`BranchProjection::AbortNode`]; a branch whose own nested union has no
/// live branch left is itself unsatisfiable and drops from the enclosing
/// union, which surfaces the
/// [`KimiParameterSchemaError::ContradictoryParentUnion`] diagnosis only
/// when no live branch remains there.
fn intersect_kimi_branch_type(
    branch: &mut Value,
    parent_type: &str,
    defs: &serde_json::Map<String, Value>,
) -> Result<BranchProjection, KimiParameterSchemaError> {
    let Some(obj) = branch.as_object_mut() else {
        // Non-object branches stay for the validator to reject verbatim.
        return Ok(BranchProjection::Keep);
    };
    if obj.contains_key("$ref") && !obj.contains_key("type") {
        return project_ref_branch(obj, parent_type, defs);
    }
    let type_node = obj.get("type").cloned();
    let effective_type = match type_node.as_ref() {
        Some(Value::String(branch_type)) => {
            match intersect_schema_types(parent_type, branch_type) {
                Some(effective) => effective.to_string(),
                None => return Ok(BranchProjection::Drop),
            }
        }
        Some(Value::Array(members)) => {
            // Exact projection of an array-form `type`: narrow to the single
            // MFJS carrier the surviving members share. Every survivor is
            // either the parent type itself or a strictly narrower subtype,
            // so the broadest survivor — the parent whenever any member
            // survives as it — subsumes the rest, and the branch stamps that
            // plain string. Re-shipping the narrowed array failed MFJS
            // validation outright (`MissingType` standalone, the
            // union-sibling whitelist beside a nested `anyOf`), dropping the
            // whole tool (round-6 review); verbatim-keeping used to ship the
            // branch un-narrowed, dropping the parent constraint (round-5
            // review, same class as the `$ref` carrier hole). Falling
            // through — instead of returning early — lets a nested union
            // take the same projection the plain-string path uses, so
            // `type` never ships beside the union keyword.
            let mut survivor: Option<String> = None;
            for member in members {
                match member.as_str() {
                    Some(member_type) => {
                        if let Some(effective) = intersect_schema_types(parent_type, member_type) {
                            let effective = effective.to_string();
                            if effective == parent_type {
                                survivor = Some(effective);
                                break;
                            }
                            match &survivor {
                                Some(existing) if *existing != effective => {
                                    // Unreachable over the JSON type lattice:
                                    // only the parent subsumes a narrowed
                                    // survivor. Fail closed rather than pick.
                                    return Ok(BranchProjection::AbortNode);
                                }
                                _ => survivor = Some(effective),
                            }
                        }
                    }
                    None => return Ok(BranchProjection::AbortNode),
                }
            }
            match survivor {
                Some(effective_type) => effective_type,
                None => return Ok(BranchProjection::Drop),
            }
        }
        // An unrepresentable `type` form cannot be intersected exactly.
        Some(_) => return Ok(BranchProjection::AbortNode),
        None => parent_type.to_string(),
    };
    if obj.get("anyOf").is_some_and(Value::is_array) {
        // Both bool spellings fail MFJS validation verbatim and would take
        // the whole tool with them, while the exact projections are trivial:
        // a `true` branch makes the union admit everything, so the branch is
        // exactly its effective type and the union keyword goes; a `false`
        // branch contributes nothing and is dropped before the typed walk —
        // an all-`false` union is unsatisfiable and reports the same
        // contradiction the walk reports for a fully disjoint union
        // (round-4 review).
        let union_is_vacuous = obj
            .get("anyOf")
            .and_then(Value::as_array)
            .is_some_and(|branches| {
                branches
                    .iter()
                    .any(|branch| matches!(branch, Value::Bool(true)))
            });
        if union_is_vacuous {
            obj.remove("anyOf");
            obj.insert("type".into(), Value::String(effective_type));
            return Ok(BranchProjection::Keep);
        }
        if let Some(branches) = obj.get_mut("anyOf").and_then(Value::as_array_mut) {
            branches.retain(|branch| !branch.is_boolean());
            if branches.is_empty() {
                // Every nested branch was a provably disjoint `false`, so
                // this branch admits no instance under the parent: it drops
                // from the enclosing union instead of failing the whole
                // tool. The walk that owns the enclosing union surfaces
                // `ContradictoryParentUnion` only when no live branch
                // remains (round-6 review).
                return Ok(BranchProjection::Drop);
            }
            match project_typed_union_branches(branches, &effective_type, defs) {
                Ok(true) => {}
                Ok(false) => return Ok(BranchProjection::AbortNode),
                Err(KimiParameterSchemaError::ContradictoryParentUnion) => {
                    // The nested walk only reports this when every nested
                    // branch was provably disjoint, so this branch is
                    // unsatisfiable under the parent and drops exactly; the
                    // enclosing union keeps its live branches and reports
                    // the diagnosis itself if none remain.
                    return Ok(BranchProjection::Drop);
                }
                Err(error) => return Err(error),
            }
        }
        obj.remove("type");
    } else {
        obj.insert("type".into(), Value::String(effective_type));
    }
    Ok(BranchProjection::Keep)
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
    #[error("Moonshot function parameters contain a contradictory typed union branch")]
    ContradictoryParentUnion,
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
/// Function parameters must have a plain object root in the strict pipeline.
/// Every root union is rejected, including a union reached through `$ref`;
/// no Responses-style property merge or first-wins flattening is applied.
/// Internal root `$ref` values are inlined only when neither the root nor a
/// chained reference carries siblings that could be lost or override it.
///
/// One pipeline serves every Moonshot route (api.moonshot.cn / ai platform,
/// api.kimi.com and api.kimi.ai coding endpoints): the documents behind those
/// routes all point at the same MFJS specification, so the wire contract is
/// uniform. Pure annotations outside that specification are stripped. Real
/// constraints and composition without an exact supported representation
/// reject the tool instead of being dropped into a wider schema.
///
/// Unsupported, unresolved, cyclic, and non-object root references fail
/// closed with a non-secret diagnostic instead of being sent to Moonshot.
///
/// MFJS differences from JSON Schema:
/// <https://github.com/MoonshotAI/walle/blob/main/docs/mfjs-walle-vs-draft-2020-12.md>
pub fn sanitize_for_kimi_parameters(
    parameters: &mut Value,
) -> Result<(), KimiParameterSchemaError> {
    if !schema_within_mfjs_local_budgets(parameters) {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    // Work on a clone so a rejected schema remains byte-for-byte unchanged for
    // callers that retain the catalog and retry against another provider.
    let mut candidate = parameters.clone();
    sanitize_kimi_parameters_candidate(&mut candidate)?;
    validate_mfjs_parameters(&candidate)?;
    *parameters = candidate;
    Ok(())
}

fn sanitize_kimi_parameters_candidate(
    parameters: &mut Value,
) -> Result<(), KimiParameterSchemaError> {
    if contains_dependent_keywords(parameters) {
        return Err(KimiParameterSchemaError::UnsupportedKeyword);
    }
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
    // Root composition is not rejected here: `normalize_kimi_compatibility`
    // first collapses exact singleton unions onto their branch (an
    // `anyOf:[object]` root is the equivalent plain object and must not be
    // dropped — the final root checks below still reject every surviving
    // union), so an early rejection would preempt the collapse (review
    // 4689524141).
    normalize_kimi_compatibility(parameters, true)?;
    sanitize_for_kimi(parameters)?;

    // Only annotations are removed. Bounds and dependent keywords are real
    // constraints, so they remain for validation to reject rather than being
    // stripped into a wider schema.
    strip_mfjs_advisory_keywords(parameters);
    remove_empty_mfjs_items_schemas(parameters);

    let Some(root) = parameters.as_object_mut() else {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    };
    if root.contains_key("oneOf") || root.contains_key("allOf") || root.contains_key("$ref") {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    }
    if root.contains_key("anyOf") || root.get("type").and_then(Value::as_str) != Some("object") {
        return Err(KimiParameterSchemaError::RootMustBeObject);
    }
    root.entry("properties")
        .or_insert_with(|| Value::Object(Map::new()));

    Ok(())
}

fn contains_dependent_keywords(schema: &Value) -> bool {
    match schema {
        Value::Object(obj) => {
            if obj.contains_key("dependentRequired") || obj.contains_key("dependentSchemas") {
                return true;
            }
            obj.iter().any(|(key, child)| {
                if matches!(key.as_str(), "default" | "enum" | "const") {
                    return false;
                }
                if matches!(
                    key.as_str(),
                    "properties" | "$defs" | "definitions" | "patternProperties"
                ) {
                    return child
                        .as_object()
                        .is_some_and(|map| map.values().any(contains_dependent_keywords));
                }
                contains_dependent_keywords(child)
            })
        }
        Value::Array(items) => items.iter().any(contains_dependent_keywords),
        _ => false,
    }
}

/// Remove the advisory keywords the MFJS specification does not support from
/// every schema node of a Kimi parameters document.
///
/// Only lossless annotations (`title`, `$comment`, `$id`) are removed. The
/// specification's supported keyword set is closed, and min/max bounds are
/// validation constraints rather than advisory hints: they stay in the
/// document so `validate_mfjs_parameters` rejects them. Pattern, format,
/// prefixItems, exclusive bounds, and unsupported composition fail the same
/// way. Source-comparison note: walle's `FutureKeywords` are not a shipping
/// basis for widening this whitelist.
///
/// The traversal visits schema positions only, never instance data beneath
/// `default`, `enum`, or `const`.
///
/// The traversal matches `remove_empty_mfjs_items_schemas`: property values,
/// `$defs` entries, `anyOf` branches, object-form `additionalProperties`, and
/// `items` are the schema-value positions. `oneOf`/`allOf` need no visit:
/// this runs after `normalize_kimi_compatibility`, which has already
/// converted or rejected every such node.
fn strip_mfjs_advisory_keywords(value: &mut Value) {
    const ADVISORY_KEYWORDS: [&str; 3] = ["title", "$comment", "$id"];
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

/// Drop every empty `items` schema node (`"items": {}`) from a Kimi
/// parameters document.
///
/// Empty-schema nodes sit exactly where schema values live: property values,
/// `items`, object-form `additionalProperties`, `anyOf` branches, and `$defs`
/// entries. `properties` itself is a name map, so its own emptiness is legal
/// and only its values are visited. `oneOf` needs no visit: normalization
/// either merges a single branch exactly or rejects the composition.
/// Schema values under keywords MFJS rejects (`contains`, `prefixItems`,
/// `patternProperties`, `not`, `if`/`then`/`else`, `propertyNames`,
/// `unevaluatedProperties`, `unevaluatedItems`, nested `allOf`, tuple-form
/// `items`) need no visit: the keyword itself fails closed —
/// `UnsupportedKeyword` at validation for the unlisted keywords,
/// `UnsupportedNestedAllOf` for a nested `allOf` and `InvalidSchemaNode`
/// for tuple-form `items` already in `normalize_kimi_compatibility` —
/// before a stranded empty `items` node inside one could matter.
/// `dependentSchemas` is rejected before this traversal runs.
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
    let annotation = |key: &str| matches!(key, "description" | "title" | "$id" | "$comment");
    if document_root
        .keys()
        .any(|key| key != "$ref" && key != "$defs" && !annotation(key))
    {
        return Err(KimiParameterSchemaError::UnsupportedRootReference);
    }
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
            // Intermediate descriptions are annotations too, but silently
            // dropping them would lose tool guidance. Keep the strict path
            // simple: chained references must be bare references.
            if target.len() != 1 {
                return Err(KimiParameterSchemaError::UnsupportedRootReference);
            }
            reference = next_ref
                .as_str()
                .ok_or(KimiParameterSchemaError::UnsupportedRootReference)?;
            continue;
        }
        if target.get("type").and_then(Value::as_str) != Some("object") {
            return Err(KimiParameterSchemaError::ReferencedRootMustBeObject);
        }
        if target.contains_key("$defs") {
            return Err(KimiParameterSchemaError::UnsupportedRootReference);
        }
        break target.clone();
    };

    let mut inlined = resolved;
    for (key, value) in document_root {
        if key == "$defs" || annotation(key) {
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
    if matches!(schema, Value::Bool(_)) {
        // A bool schema carries no keywords to normalize. Its fate is the
        // parent-union walk's exact projection on a typed union and the
        // validator's own bool-schema rejection everywhere else — not the
        // malformed-node error a keyword-less object would earn (round-4
        // review).
        return Ok(());
    }
    if !is_root {
        collapse_nullable_unions(schema);
    }
    collapse_single_element_unions(schema);
    let Some(obj) = schema.as_object_mut() else {
        return Err(KimiParameterSchemaError::InvalidSchemaNode);
    };

    if obj.contains_key("allOf") {
        return Err(KimiParameterSchemaError::UnsupportedNestedAllOf);
    }
    if obj.contains_key("oneOf") {
        return Err(KimiParameterSchemaError::ConflictingNestedUnion);
    }

    if let Some(constant) = obj.remove("const") {
        // A string/number const translates onto the MFJS-supported enum
        // landing verbatim. A bool/null const has no supported equivalent:
        // its enum would fail validation, so accepting the translation here
        // could only widen the author's pin (`const: true` becoming "any
        // boolean") if a later pass erased it. Fail closed immediately.
        if !is_mfjs_scalar_const(&constant) {
            return Err(KimiParameterSchemaError::UnsupportedConstLiteral);
        }
        if !obj.contains_key("type") {
            // Type inference stays limited to MFJS-supported enum values;
            // unsupported literals were rejected above.
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
            let agrees = existing.as_array().is_some_and(|values| {
                values
                    .iter()
                    .any(|value| schema_value_equal(value, &constant))
            });
            if !agrees {
                return Err(KimiParameterSchemaError::ConflictingLiteralConstraint);
            }
        }
        obj.insert("enum".to_string(), Value::Array(vec![constant]));
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
            // null was never a valid wire instance there, so the marker
            // admits nothing the wire does not already admit and is dropped
            // exactly. The registry collapse stamps this marker on every
            // `anyOf: [{type: object, ...}, {type: null}]` root (the
            // standard Pydantic-Optional shape), so erroring here dropped
            // those tools from every Moonshot request instead of shipping
            // the exact object schema (round-6 review).
        }
        Some(Some(true)) => {
            let non_null = Value::Object(std::mem::take(obj));
            *schema = serde_json::json!({
                "anyOf": [non_null, {"type": "null"}]
            });
        }
        Some(None) => return Err(KimiParameterSchemaError::InvalidNullable),
    }

    let obj = schema
        .as_object_mut()
        .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;
    if obj.contains_key("anyOf") {
        if obj
            .get("anyOf")
            .and_then(Value::as_array)
            .is_none_or(Vec::is_empty)
        {
            return Err(KimiParameterSchemaError::InvalidKeywordValue);
        }
        if obj
            .get("properties")
            .and_then(Value::as_object)
            .is_some_and(Map::is_empty)
        {
            obj.remove("properties");
        }
        // The registry collapse retains union compositions that carry
        // instance data verbatim (the round-6 carve-out), so without this
        // distribution the strict lane dropped every tool with the common
        // `Optional[T] = default` property shape on the union-sibling
        // whitelist, although the exact wire form exists: move
        // `default`/`enum` onto the branches that provably admit them (base
        // shipped exactly this distributed form by flattening first).
        // Instance data nothing admits stays on the node for the sibling
        // whitelist to reject, exactly as before (fail-closed).
        distribute_kimi_union_instance_data(obj);
        if obj.keys().any(|key| {
            !matches!(key.as_str(), "anyOf" | "type" | "description")
                && !(is_root && key == "$defs")
        }) {
            return Err(KimiParameterSchemaError::InvalidKeywordValue);
        }
    } else if is_root && !obj.contains_key("type") {
        obj.insert("type".into(), Value::String("object".into()));
    }

    normalize_kimi_child_schemas(schema)?;
    Ok(())
}

/// Move node-level instance data beside a retained `anyOf` onto the
/// branches that provably admit it. `default` annotates instance data, so
/// admitting branches gain it verbatim and the accepted instance set is
/// untouched; `enum` is a constraint, so each admitting branch carries the
/// subset of values its type admits and a branch admitting none is dropped
/// exactly (it admits no instance under the original node-level enum).
/// Branches that already constrain the same instance data, `$ref`/bool
/// branches, and unprovable type forms cannot take the distribution and
/// stay as-is; if that leaves any value or key undistributed, the key stays
/// on the node for the union-sibling whitelist to reject — the same
/// fail-closed outcome as before this helper existed (round-7 review).
fn distribute_kimi_union_instance_data(obj: &mut serde_json::Map<String, Value>) {
    if let Some(value) = obj.get("default").cloned() {
        let branches = obj.get_mut("anyOf").and_then(Value::as_array_mut);
        if let Some(branches) = branches
            && distribute_single_instance_value(branches, "default", &value)
        {
            obj.remove("default");
        }
    }
    if let Some(Value::Array(values)) = obj.get("enum").cloned() {
        let branches = obj.get_mut("anyOf").and_then(Value::as_array_mut);
        if let Some(branches) = branches
            && distribute_enum_across_branches(branches, &values)
        {
            obj.remove("enum");
        }
    }
}

/// Whether one branch provably admits an instance value for distribution.
fn branch_admits_instance(branch: &Value, value: &Value) -> bool {
    let Some(branch_obj) = branch.as_object() else {
        return false;
    };
    if branch_obj.contains_key("const")
        || branch_obj.contains_key("enum")
        || branch_obj.contains_key("$ref")
        || branch_obj.contains_key("anyOf")
        || branch_obj.contains_key("oneOf")
        || branch_obj.contains_key("allOf")
    {
        return false;
    }
    match branch_obj.get("type") {
        None => true,
        Some(Value::String(branch_type)) => instance_data_is_type(value, branch_type),
        Some(Value::Array(members)) => members.iter().any(|member| {
            member
                .as_str()
                .is_some_and(|t| instance_data_is_type(value, t))
        }),
        Some(_) => false,
    }
}

/// Annotate every admitting branch with one instance-data value. Returns
/// whether at least one branch admitted it.
fn distribute_single_instance_value(branches: &mut [Value], key: &str, value: &Value) -> bool {
    let mut distributed = false;
    for branch in branches.iter_mut() {
        if branch_admits_instance(branch, value)
            && let Some(branch_obj) = branch.as_object_mut()
        {
            branch_obj.insert(key.to_string(), value.clone());
            distributed = true;
        }
    }
    distributed
}

/// Distribute a node-level `enum` across the branches: each admitting
/// branch carries the values its type admits, and a branch admitting no
/// value is dropped exactly. Returns whether every value was admitted by at
/// least one surviving branch and at least one branch survives.
fn distribute_enum_across_branches(branches: &mut Vec<Value>, values: &[Value]) -> bool {
    let mut per_branch: Vec<Option<Vec<Value>>> = Vec::with_capacity(branches.len());
    for _ in branches.iter() {
        per_branch.push(None);
    }
    for value in values {
        let mut admitted = false;
        for (index, branch) in branches.iter().enumerate() {
            if branch_admits_instance(branch, value) {
                admitted = true;
                per_branch[index]
                    .get_or_insert_with(Vec::new)
                    .push(value.clone());
            }
        }
        if !admitted {
            return false;
        }
    }
    let mut kept = Vec::new();
    for (branch, subset) in branches.drain(..).zip(per_branch) {
        if let Some(values) = subset {
            let mut branch_obj = match branch.as_object() {
                Some(branch_obj) => branch_obj.clone(),
                None => return false,
            };
            branch_obj.insert("enum".to_string(), Value::Array(values));
            kept.push(Value::Object(branch_obj));
        }
    }
    if kept.is_empty() {
        return false;
    }
    *branches = kept;
    true
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
/// bool/null consts have no supported equivalent and fail closed like
/// object/array consts.
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
/// wins. Annotations are stripped upstream; bounds fail validation.
pub fn validate_mfjs_parameters(parameters: &Value) -> Result<(), KimiParameterSchemaError> {
    if !schema_within_mfjs_local_budgets(parameters) {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    let root = parameters
        .as_object()
        .ok_or(KimiParameterSchemaError::RootMustBeObject)?;
    if root.contains_key("oneOf")
        || root.contains_key("anyOf")
        || root.contains_key("allOf")
        || root.contains_key("$ref")
        || root.get("type").and_then(Value::as_str) != Some("object")
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
    let mut termination_state = MfjsTerminationState::default();
    if !mfjs_schema_can_terminate(
        parameters,
        parameters,
        &mut visiting_refs,
        0,
        &mut termination_state,
    )? {
        return Err(KimiParameterSchemaError::NonTerminatingReference);
    }
    if let Some(definitions) = root.get("$defs").and_then(Value::as_object) {
        for definition in definitions.values() {
            visiting_refs.clear();
            if !mfjs_schema_can_terminate(
                definition,
                parameters,
                &mut visiting_refs,
                0,
                &mut termination_state,
            )? {
                return Err(KimiParameterSchemaError::NonTerminatingReference);
            }
        }
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
// MaxSchemaSize=120000 below already matches. Local rules deliberately take
// the stricter intersection and must not be relaxed merely because walle
// accepts a shape: the enum-length budget arms at any entry count rather
// than walle's 250-value floor (a small enum must not carry one oversized
// string), and MFJS_MAX_TOTAL_ENUM_VALUES is document-wide rather than
// per-node. MFJS_MAX_STRUCTURAL_DEPTH is also an engine-local safety guard;
// raw input is checked iteratively before clone or recursive normalization.
const MFJS_MAX_ANY_OF_ITEMS: usize = 500;
const MFJS_MAX_OBJECT_DEPTH: usize = 30;
const MFJS_MAX_TOTAL_PROPERTIES: usize = 3_000;
const MFJS_MAX_TOTAL_ENUM_VALUES: usize = 1_000;
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
        // set. The Kimi candidate pipeline keeps bounds deliberately (only
        // annotations are stripped upstream), so a bounds-carrying schema
        // reaches and fails THIS rejection — which is exactly why tools
        // carrying such keywords must opt into the explicit strict:false
        // relaxed lane at their ToolSpec boundary (review 6061318874).
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
    // Placement caveat (review 4604994865): the earlier `$ref`-sibling and
    // union-sibling whitelists below still refuse `default`, so a default
    // beside either form drops the whole tool. That restriction predates
    // the default support and stays disclosed rather than widened here.
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

    // The per-string length budget arms at any entry count: gating it on a
    // minimum enum size would let a small enum carry one oversized string
    // the wire rejects (round-4 review).
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
    // Admission and resolution must judge one spelling: the JSON Pointer
    // lookup below decodes `~1`→`/` and `~0`→`~` (serde_json applies the
    // same replace order), so the single-name rule runs on the decoded form.
    // A literal `~1` in a `$defs` key stays reachable through its escaped
    // `~01` spelling, while an escaped slash can no longer pass the raw-name
    // check and then resolve elsewhere — deeper into the document or into a
    // misleading `InvalidReference` (round-4 review).
    let decoded = name.replace("~1", "/").replace("~0", "~");
    if decoded.is_empty() || decoded.contains('/') {
        return None;
    }
    document
        .pointer(&reference[1..])
        .filter(|target| target.is_object())
}

/// Termination is a pure function of the node: the walk only descends into
/// schemas a finite instance is actually forced through (required object
/// properties, arrays with a positive `minItems`), so an untainted verdict
/// memoizes on node identity and structural depth alone. Nodes whose subtree
/// hit the reference-cycle guard keep their path context — a guard early
/// return is only valid for the active reference set — so they are never
/// memoized and instead re-walk per path under the strict total-state
/// budget. This keeps shared acyclic reference DAGs linear without paying
/// one owned active-path key per visited path (review 4689524122).
#[derive(Default)]
struct MfjsTerminationState {
    memo: std::collections::HashMap<(usize, usize), bool>,
    work: usize,
    cycle_guard_hits: usize,
}

fn mfjs_schema_can_terminate(
    schema: &Value,
    document: &Value,
    visiting_refs: &mut HashSet<String>,
    structural_depth: usize,
    state: &mut MfjsTerminationState,
) -> Result<bool, KimiParameterSchemaError> {
    if structural_depth > MFJS_MAX_STRUCTURAL_DEPTH {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }
    let obj = schema
        .as_object()
        .ok_or(KimiParameterSchemaError::InvalidSchemaNode)?;

    if let Some(reference) = obj.get("$ref").and_then(Value::as_str) {
        if !visiting_refs.insert(reference.to_string()) {
            state.cycle_guard_hits += 1;
            return Ok(false);
        }
        let target = resolve_mfjs_reference(document, reference)
            .ok_or(KimiParameterSchemaError::InvalidReference)?;
        let terminates =
            mfjs_schema_can_terminate(target, document, visiting_refs, structural_depth + 1, state);
        visiting_refs.remove(reference);
        return terminates;
    }

    let key = (schema as *const Value as usize, structural_depth);
    if let Some(cached) = state.memo.get(&key) {
        return Ok(*cached);
    }
    state.work += 1;
    if state.work > MFJS_MAX_EXPANDED_STATES {
        return Err(KimiParameterSchemaError::ResourceLimitExceeded);
    }

    let guards_before = state.cycle_guard_hits;
    let result = (|| {
        if let Some(branches) = obj.get("anyOf").and_then(Value::as_array) {
            for branch in branches {
                let mut branch_refs = visiting_refs.clone();
                if mfjs_schema_can_terminate(
                    branch,
                    document,
                    &mut branch_refs,
                    structural_depth + 1,
                    state,
                )? {
                    return Ok(true);
                }
            }
            return Ok(false);
        }

        match obj.get("type").and_then(Value::as_str) {
            Some("object") => {
                // walle's termination test is instance-based ("whether a
                // finite instance exists"): optional properties and
                // unconstrained additional keys are omission exits, so only
                // REQUIRED properties can force a recursion that never
                // bottoms out. A required name with no declared property is
                // a malformed node and fails closed.
                if let Some(required) = obj.get("required").and_then(Value::as_array)
                    && !required.is_empty()
                {
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
                            state,
                        )? {
                            return Ok(false);
                        }
                    }
                }
                Ok(true)
            }
            Some("array") => {
                // An array whose lower bound is zero admits the empty
                // instance, so a cyclic `items` schema still terminates;
                // only a positive `minItems` forces an element (and only
                // MFJS-unsupported bounds ever reach here unsanitized, so
                // an absent keyword reads as zero).
                let min_items = obj.get("minItems").and_then(Value::as_u64).unwrap_or(0);
                if min_items == 0 {
                    return Ok(true);
                }
                // MFJS allows `items` to be absent, and an unconstrained
                // array can always be satisfied, so absent `items` counts
                // as terminating instead of failing with InvalidSchemaNode.
                let Some(items) = obj.get("items") else {
                    return Ok(true);
                };
                mfjs_schema_can_terminate(
                    items,
                    document,
                    visiting_refs,
                    structural_depth + 1,
                    state,
                )
            }
            Some(schema_type) if is_mfjs_concrete_type(schema_type) => Ok(true),
            _ => Err(KimiParameterSchemaError::MissingType),
        }
    })();
    if let Ok(terminates) = result
        && state.cycle_guard_hits == guards_before
    {
        state.memo.insert(key, terminates);
    }
    result
}

/// Shared state for one [`validate_mfjs_expanded_depth`] traversal.
///
/// `completed` memoizes fully-walked (node identity, depths) states. The
/// walk descends into schemas a finite instance is forced through, so a
/// node's traversal is path-independent and needs no active-reference key;
/// a node whose subtree hit the reference-cycle guard keeps its path
/// context, so its entry is removed again and other paths re-walk it under
/// the strict total-state budget (review 4689524122). `visiting_refs` stays
/// the per-path cycle guard. `work` counts computed states against
/// [`MFJS_MAX_EXPANDED_STATES`].
#[derive(Default)]
struct MfjsExpandedWalkState {
    visiting_refs: HashSet<String>,
    completed: HashSet<(usize, usize, usize)>,
    work: usize,
    cycle_guard_hits: usize,
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
            state.cycle_guard_hits += 1;
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
    // is a stable identity for the memo. Insert before walking so a
    // descendant re-entry skips instead of re-descending; a cycle-tainted
    // node drops its entry after the walk.
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

    let guards_before = state.cycle_guard_hits;
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
        validate_mfjs_expanded_depth(items, document, property_depth, structural_depth + 1, state)?;
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
    if state.cycle_guard_hits != guards_before {
        state.completed.remove(&state_key);
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
    fn kimi_sanitize_intersects_parent_type_and_drops_contradictory_branches() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "handle": {
                    "type": "string",
                    "anyOf": [
                        {"type": "string"},
                        {"type": "null"},
                        {"type": "integer"}
                    ]
                }
            }
        });
        sanitize_for_kimi(&mut schema).expect("satisfiable schema must sanitize");
        let handle = &schema["properties"]["handle"];
        assert!(!handle.as_object().unwrap().contains_key("type"));
        let any_of = handle["anyOf"].as_array().unwrap();
        assert_eq!(any_of.len(), 1);
        assert_eq!(any_of[0]["type"], "string");
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

        sanitize_for_kimi(&mut schema).expect("satisfiable schema must sanitize");

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
    fn mfjs_termination_prepass_memoizes_required_shared_reference_dags() {
        // Required properties are what the instance-based termination rule
        // forces the walk through, so the shared-DAG shape below is the
        // real worst case. The node+depth memo keeps the walk linear
        // instead of exploring 2^depth duplicate paths.
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

        validate_mfjs_parameters(&schema)
            .expect("the context-aware termination memo must keep a shared DAG linear");
    }

    #[test]
    fn mfjs_termination_memo_includes_active_reference_context() {
        // The cycle must run through a REQUIRED property to be real under
        // the instance-based termination rule: an optional property is an
        // omission exit. The first context's false verdict is guard-tainted,
        // so it must not leak into the context without R.
        let document = json!({
            "type": "object",
            "properties": {},
            "$defs": {
                "R": {"type": "string"},
                "Shared": {
                    "type": "object",
                    "required": ["again"],
                    "properties": {"again": {"$ref": "#/$defs/R"}}
                }
            }
        });
        let shared = document.pointer("/$defs/Shared").unwrap();
        let mut visiting = HashSet::from(["#/$defs/R".to_string()]);
        let mut state = MfjsTerminationState::default();

        assert!(
            !mfjs_schema_can_terminate(shared, &document, &mut visiting, 0, &mut state)
                .expect("an active R is a cycle in the first context")
        );
        visiting.clear();
        assert!(
            mfjs_schema_can_terminate(shared, &document, &mut visiting, 0, &mut state)
                .expect("R terminates when it is no longer active"),
            "a false result cached under active R must not leak into a context without R"
        );
    }

    #[test]
    fn mfjs_termination_admits_recursion_through_an_optional_property() {
        // Review 4689524112: walle's termination test is instance-based —
        // omitting an optional property is a finite instance, so recursion
        // through it terminates and must not fail the tool.
        let schema = json!({
            "type": "object",
            "properties": {"next": {"$ref": "#/$defs/node"}},
            "$defs": {
                "node": {
                    "type": "object",
                    "properties": {
                        "value": {"type": "string"},
                        "next": {"$ref": "#/$defs/node"}
                    }
                }
            }
        });

        validate_mfjs_parameters(&schema)
            .expect("optional-property recursion has a finite instance");
    }

    #[test]
    fn mfjs_termination_admits_recursion_through_an_unbounded_array() {
        // The empty array is a finite instance when no positive minItems
        // forces an element, so a recursive `items` schema terminates.
        let schema = json!({
            "type": "object",
            "properties": {"children": {"type": "array", "items": {"$ref": "#/$defs/node"}}},
            "$defs": {
                "node": {
                    "type": "object",
                    "properties": {
                        "name": {"type": "string"},
                        "children": {"type": "array", "items": {"$ref": "#/$defs/node"}}
                    }
                }
            }
        });

        validate_mfjs_parameters(&schema).expect("empty-array recursion terminates");
    }

    #[test]
    fn mfjs_termination_still_rejects_recursion_through_required_properties() {
        // A required property forces at least one more nesting level, so the
        // cycle never bottoms out — exactly walle's rejected shape.
        let schema = json!({
            "type": "object",
            "required": ["next"],
            "properties": {"next": {"$ref": "#/$defs/node"}},
            "$defs": {
                "node": {
                    "type": "object",
                    "required": ["next"],
                    "properties": {"next": {"$ref": "#/$defs/node"}}
                }
            }
        });

        assert_eq!(
            validate_mfjs_parameters(&schema).unwrap_err(),
            KimiParameterSchemaError::NonTerminatingReference
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
    fn mfjs_expanded_walk_memo_includes_active_reference_context() {
        let mut deep = json!({"type": "string"});
        for _ in 0..MFJS_MAX_OBJECT_DEPTH + 1 {
            deep = json!({"type": "object", "properties": {"next": deep}});
        }
        let document = json!({
            "type": "object",
            "properties": {},
            "$defs": {
                "R": {"type": "object", "properties": {"deep": deep}},
                "Shared": {
                    "type": "object",
                    "properties": {"again": {"$ref": "#/$defs/R"}}
                }
            }
        });
        let shared = document.pointer("/$defs/Shared").unwrap();
        let mut state = MfjsExpandedWalkState::default();
        state.visiting_refs.insert("#/$defs/R".to_string());

        validate_mfjs_expanded_depth(shared, &document, 0, 0, &mut state)
            .expect("the active R reference skips its subtree in the first context");
        state.visiting_refs.clear();

        assert_eq!(
            validate_mfjs_expanded_depth(shared, &document, 0, 0, &mut state),
            Err(KimiParameterSchemaError::ResourceLimitExceeded),
            "a cache entry created under an active R must not hide R's deep subtree"
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

        sanitize_for_kimi(&mut schema).expect("satisfiable schema must sanitize");

        let outer_item = &schema["anyOf"][0];
        assert!(outer_item.get("type").is_none());
        assert_eq!(outer_item["anyOf"][0]["type"], "object");
        assert!(
            !schema.as_object().unwrap().contains_key("type"),
            "outer parent type should be removed"
        );
    }

    #[test]
    fn kimi_sanitize_keeps_untyped_ref_branches_of_a_typed_parent_union() {
        // Round-4 review: an untyped `$ref` branch is exactly representable
        // as a union member when its resolved target carries the parent
        // type. Stamping the parent type onto it used to manufacture the
        // `type` + `$ref` sibling pair the MFJS ref-sibling whitelist
        // rejects, trading a resolvable reference for a misleading
        // `InvalidReference`.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "payload": {
                    "type": "object",
                    "anyOf": [{"$ref": "#/$defs/A"}, {"type": "string"}]
                }
            },
            "$defs": {
                "A": {"type": "object", "properties": {"x": {"type": "string"}}}
            }
        });

        sanitize_for_kimi(&mut schema).expect("a $ref union branch must sanitize");

        let payload = &schema["properties"]["payload"];
        assert!(
            !payload.as_object().unwrap().contains_key("type"),
            "the verbatim reference target carries the parent type exactly"
        );
        let any_of = payload["anyOf"].as_array().unwrap();
        assert_eq!(any_of.len(), 1, "the disjoint string branch is removed");
        assert_eq!(
            any_of[0],
            json!({"$ref": "#/$defs/A"}),
            "the reference branch ships verbatim"
        );
    }

    #[test]
    fn kimi_sanitize_drops_a_ref_branch_disjoint_from_the_typed_parent() {
        // Round-5 review: verbatim-keeping an untyped `$ref` branch whose
        // target does not carry the parent type shipped a constraint-free
        // branch — the parent type has no wire carrier beside `$ref` or the
        // union keyword. A provably disjoint target is a dead branch and is
        // dropped exactly like a disjoint typed branch.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "payload": {
                    "type": "object",
                    "anyOf": [{"$ref": "#/$defs/S"}, {"type": "object"}]
                }
            },
            "$defs": {"S": {"type": "string"}}
        });

        sanitize_for_kimi(&mut schema).expect("a disjoint ref branch projects as dead");

        let payload = &schema["properties"]["payload"];
        assert!(!payload.as_object().unwrap().contains_key("type"));
        assert_eq!(payload["anyOf"], json!([{"type": "object"}]));
    }

    #[test]
    fn kimi_sanitize_aborts_a_node_when_a_ref_target_is_broader_than_the_parent() {
        // The intersection (`integer`) is strictly narrower than the target
        // (`number`); no verbatim spelling is exact, so the node keeps its
        // `type` + `anyOf` pair and the whole tool fails closed at
        // validation instead of widening the wire to every number.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "count": {
                    "type": "integer",
                    "anyOf": [{"$ref": "#/$defs/N"}]
                }
            },
            "$defs": {"N": {"type": "number"}}
        });

        sanitize_for_kimi(&mut schema)
            .expect("the sanitizer defers the rejection to the validator");

        let count = &schema["properties"]["count"];
        assert_eq!(
            count["type"], "integer",
            "the parent type must stay on the node when a branch cannot project"
        );
        assert_eq!(count["anyOf"], json!([{"$ref": "#/$defs/N"}]));

        let mut candidate = json!({
            "type": "object",
            "properties": {
                "count": {"type": "integer", "anyOf": [{"$ref": "#/$defs/N"}]}
            },
            "$defs": {"N": {"type": "number"}}
        });
        let error = sanitize_for_kimi_parameters(&mut candidate)
            .expect_err("an over-broad ref target must fail the tool closed");
        assert_eq!(
            error,
            KimiParameterSchemaError::InvalidKeywordValue,
            "the retained type+anyOf pair is what the union-sibling whitelist rejects"
        );
    }

    #[test]
    fn kimi_sanitize_aborts_a_node_on_an_unresolvable_ref_branch() {
        // Same carrier rule for an unresolvable reference: exactness cannot
        // be proven, so the node keeps `type` + `anyOf` and validation
        // rejects the tool closed.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "count": {
                    "type": "integer",
                    "anyOf": [{"$ref": "#/$defs/Missing"}]
                }
            }
        });

        sanitize_for_kimi(&mut schema)
            .expect("the sanitizer defers the rejection to the validator");

        let count = &schema["properties"]["count"];
        assert_eq!(count["type"], "integer");
        assert_eq!(count["anyOf"], json!([{"$ref": "#/$defs/Missing"}]));

        let mut candidate = json!({
            "type": "object",
            "properties": {
                "count": {"type": "integer", "anyOf": [{"$ref": "#/$defs/Missing"}]}
            }
        });
        assert!(
            sanitize_for_kimi_parameters(&mut candidate).is_err(),
            "an unresolvable branch reference must fail the tool closed"
        );
    }

    #[test]
    fn kimi_sanitize_narrows_array_typed_branches_to_the_parent_intersection() {
        // Same carrier rule for array-form `type` branches: members are
        // narrowed to their exact intersection with the parent, which is
        // always a single MFJS carrier — the parent whenever any member
        // survives as it, else the narrowed subtype — and the projection
        // falls through to the nested-union walk so `type` never ships
        // beside a union keyword. Re-shipping the narrowed array failed
        // validation outright and dropped the whole tool (round-6 review),
        // so this pin runs the full parameters pipeline, validator
        // included.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {
                    "type": "object",
                    "anyOf": [{"type": ["string", "object"]}, {"type": "string"}]
                },
                "b": {"type": "number", "anyOf": [{"type": ["string", "integer"]}]}
            }
        });

        sanitize_for_kimi_parameters(&mut schema)
            .expect("array-form types project onto a shippable wire form");

        let a = &schema["properties"]["a"];
        assert!(!a.as_object().unwrap().contains_key("type"));
        assert_eq!(
            a["anyOf"],
            json!([{"type": "object"}]),
            "the surviving member is narrowed to the intersection as a string"
        );
        let b = &schema["properties"]["b"];
        assert!(!b.as_object().unwrap().contains_key("type"));
        assert_eq!(b["anyOf"], json!([{"type": "integer"}]));

        // The common Optional[X] spelling: an array-form branch beside a
        // nested union narrows onto the carrier string and the nested union
        // projects exactly (null branch dies, the branch `type` is
        // absorbed), so the property ships instead of the tool dropping.
        let mut optional = json!({
            "type": "object",
            "properties": {
                "c": {
                    "type": "object",
                    "anyOf": [{"type": ["object", "null"]}, {"type": "null"}]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut optional)
            .expect("array-form + nested union projects exactly");
        let c = &optional["properties"]["c"];
        assert!(!c.as_object().unwrap().contains_key("type"));
        assert_eq!(
            c["anyOf"],
            json!([{"type": "object"}]),
            "the null branch dies and the array-form member stamps its carrier"
        );

        let mut all_dead = json!({
            "type": "object",
            "properties": {"a": {"type": "object", "anyOf": [{"type": ["string"]}]}}
        });
        assert_eq!(
            sanitize_for_kimi(&mut all_dead).unwrap_err(),
            KimiParameterSchemaError::ContradictoryParentUnion
        );

        // A member that is not a plain string cannot be intersected; the
        // node aborts closed instead of shipping un-narrowed.
        let mut unrepresentable = json!({
            "type": "object",
            "properties": {
                "a": {"type": "object", "anyOf": [{"type": ["string", 3]}]}
            }
        });
        sanitize_for_kimi(&mut unrepresentable)
            .expect("the sanitizer defers the rejection to the validator");
        let a = &unrepresentable["properties"]["a"];
        assert_eq!(a["type"], "object");
        assert_eq!(a["anyOf"], json!([{"type": ["string", 3]}]));
        // The deferral claim itself: the retained form must actually be what
        // the validator rejects, not a shape that slips through.
        let error = validate_mfjs_parameters(&unrepresentable)
            .expect_err("the validator must reject the retained non-string type member");
        assert_eq!(error, KimiParameterSchemaError::InvalidKeywordValue);
    }

    #[test]
    fn kimi_lane_ships_a_dead_nested_union_branch_by_dropping_the_branch() {
        // Round-6 review: a branch killed by its own nested union used to
        // raise the tool-fatal `ContradictoryParentUnion` even though the
        // node is satisfiable — dropping that one branch is the exact
        // projection. The diagnosis is reserved for the node whose whole
        // union dies.
        let mut satisfiable = json!({
            "type": "object",
            "properties": {
                "a": {"type": "object", "anyOf": [
                    {"type": "object"},
                    {"type": "object", "anyOf": [{"type": "null"}]}
                ]}
            }
        });
        sanitize_for_kimi_parameters(&mut satisfiable)
            .expect("the live branch keeps the node satisfiable");
        let a = &satisfiable["properties"]["a"];
        assert!(!a.as_object().unwrap().contains_key("type"));
        assert_eq!(
            a["anyOf"],
            json!([{"type": "object"}]),
            "only the live branch ships"
        );

        // When every branch dies the node is genuinely unsatisfiable and
        // the tool-fatal diagnosis stands.
        let mut all_dead = json!({
            "type": "object",
            "properties": {
                "a": {"type": "object", "anyOf": [
                    {"type": "object", "anyOf": [{"type": "null"}]},
                    {"type": "object", "anyOf": [{"type": "string"}]}
                ]}
            }
        });
        assert_eq!(
            sanitize_for_kimi(&mut all_dead).unwrap_err(),
            KimiParameterSchemaError::ContradictoryParentUnion,
            "a fully dead union still reports the contradiction"
        );
    }

    #[test]
    fn kimi_lane_ships_a_root_nullable_marker_from_the_registry_collapse() {
        // Round-6 review: the shared registry collapse stamps `nullable:
        // true` on every `anyOf: [{type: object, ...}, {type: null}]` root
        // (the standard Pydantic-Optional shape). At an object-only root
        // the marker admits nothing the wire does not already admit, so it
        // is dropped exactly; erroring here dropped those tools from every
        // Moonshot request.
        let mut collapsed = json!({
            "type": "object",
            "properties": {"a": {"type": "string"}},
            "required": ["a"],
            "nullable": true
        });
        sanitize_for_kimi_parameters(&mut collapsed)
            .expect("a root nullable marker ships as the exact object schema");
        assert!(collapsed.get("nullable").is_none(), "the marker is dropped");
        assert_eq!(
            collapsed,
            json!({
                "type": "object",
                "properties": {"a": {"type": "string"}},
                "required": ["a"]
            })
        );

        // End-to-end through the shared registry lane that produces the
        // collapsed form.
        let mut pydantic_root = json!({
            "anyOf": [
                {"type": "object", "properties": {"a": {"type": "string"}}, "required": ["a"]},
                {"type": "null"}
            ]
        });
        sanitize(&mut pydantic_root);
        sanitize_for_kimi_parameters(&mut pydantic_root)
            .expect("the registry-collapsed Optional root ships");
        assert_eq!(
            pydantic_root["type"], "object",
            "the collapsed root keeps its object schema"
        );
    }

    #[test]
    fn kimi_sanitize_absorbs_a_true_branch_of_a_typed_parent_union() {
        // Round-4 review: bool-schema branches fail MFJS validation
        // verbatim and used to take the whole tool with them, while the
        // exact projection of `true` is absorption — the union IS the
        // parent type, including the single-branch case.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string", "anyOf": [true, {"type": "string"}]},
                "b": {"type": "string", "anyOf": [true]}
            }
        });

        sanitize_for_kimi(&mut schema).expect("true branches project exactly");

        assert_eq!(schema["properties"]["a"]["type"], "string");
        assert!(schema["properties"]["a"].get("anyOf").is_none());
        assert_eq!(schema["properties"]["b"]["type"], "string");
        assert!(schema["properties"]["b"].get("anyOf").is_none());
    }

    #[test]
    fn kimi_sanitize_absorbs_a_true_branch_inside_a_nested_union() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "object", "anyOf": [{"anyOf": [false, true]}]}
            }
        });

        sanitize_for_kimi(&mut schema)
            .expect("a nested union with a true branch collapses onto its type");

        let nested = &schema["properties"]["a"]["anyOf"][0];
        assert_eq!(nested["type"], "object");
        assert!(nested.get("anyOf").is_none());
    }

    #[test]
    fn kimi_sanitize_drops_false_branches_and_reports_an_all_false_union() {
        // `false` admits no instance: anyOf X,false ≡ X. The typed walk
        // proceeds on the survivors; a union of only `false` branches is
        // unsatisfiable and reports the same contradiction a fully
        // disjoint union does, instead of leaving an emptied union behind.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "a": {"type": "string", "anyOf": [false, {"type": "string"}]}
            }
        });

        sanitize_for_kimi(&mut schema).expect("the surviving branch keeps the union exact");

        let a = &schema["properties"]["a"];
        assert!(!a.as_object().unwrap().contains_key("type"));
        assert_eq!(a["anyOf"], json!([{"type": "string"}]));

        let mut unsatisfiable = json!({
            "type": "object",
            "properties": {"a": {"type": "string", "anyOf": [false]}}
        });
        assert_eq!(
            sanitize_for_kimi(&mut unsatisfiable).unwrap_err(),
            KimiParameterSchemaError::ContradictoryParentUnion
        );

        let mut nested_unsatisfiable = json!({
            "type": "object",
            "properties": {"a": {"type": "object", "anyOf": [{"anyOf": [false]}]}}
        });
        assert_eq!(
            sanitize_for_kimi(&mut nested_unsatisfiable).unwrap_err(),
            KimiParameterSchemaError::ContradictoryParentUnion
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
        sanitize_for_kimi(&mut schema).expect("satisfiable schema must sanitize");
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

    #[test]
    fn kimi_parameters_rejects_nested_property_union_that_would_widen_additional_properties() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "payload": {
                    "type": "object",
                    "properties": {"a": {"type": "string"}},
                    "additionalProperties": false,
                    "anyOf": [{
                        "type": "object",
                        "properties": {"b": {"type": "string"}}
                    }]
                }
            }
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::InvalidKeywordValue
        );
        assert_eq!(schema, original);
    }

    // Strict and default Moonshot handling rejects every root union. Only the
    // chat layer's explicit strict:false escape may choose another pipeline.

    #[test]
    fn kimi_parameters_rejects_all_object_root_anyof_without_flattening() {
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"],
                    "additionalProperties": false
                },
                {
                    "type": "object",
                    "properties": {"url": {"type": "string"}},
                    "required": ["url"],
                    "additionalProperties": false
                }
            ]
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::RootMustBeObject
        );
        assert_eq!(schema, original);
        assert_eq!(
            validate_mfjs_parameters(&schema).unwrap_err(),
            KimiParameterSchemaError::RootMustBeObject
        );
    }

    #[test]
    fn kimi_parameters_collapses_equivalent_single_allof_root() {
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
    fn kimi_parameters_rejects_root_oneof_without_flattening() {
        let mut schema = json!({
            "oneOf": [
                {"type": "object", "properties": {"id": {"type": "integer"}}},
                {"type": "object", "properties": {"name": {"type": "string"}}}
            ]
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::ConflictingNestedUnion
        );
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_parameters_rejects_root_anyof_with_non_object_branch() {
        let mut schema = json!({
            "anyOf": [
                {"type": "object", "properties": {"path": {"type": "string"}}},
                {"type": "null"}
            ]
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::RootMustBeObject
        );
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_parameters_rejects_root_anyof_with_constraint_siblings() {
        // With the root union no longer rejected before normalization, the
        // surviving multi-branch union fails in the compatibility pass on its
        // disallowed `properties` sibling: MFJS requires `type` inside each
        // branch, so a root-adjacent sibling constraint has no supported
        // representation.
        let mut schema = json!({
            "anyOf": [
                {"type": "object", "properties": {"path": {"type": "string"}}},
                {"type": "object", "properties": {"url": {"type": "string"}}}
            ],
            "properties": {"legacy": {"type": "string"}}
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::InvalidKeywordValue
        );
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_parameters_collapses_singleton_root_anyof_onto_its_branch() {
        // Review 4689524141: an exact singleton root union is the equivalent
        // plain object and must survive instead of being dropped as root
        // composition.
        let mut schema = json!({
            "anyOf": [
                {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"],
                    "additionalProperties": false
                }
            ]
        });

        sanitize_for_kimi_parameters(&mut schema).expect("singleton collapse");

        assert!(schema.get("anyOf").is_none());
        assert_eq!(schema["type"], "object");
        assert_eq!(schema["properties"]["path"]["type"], "string");
        assert_eq!(schema["required"], json!(["path"]));
    }

    #[test]
    fn kimi_parameters_rejects_actual_apply_patch_root_instead_of_flattening() {
        let mut schema = ApplyPatchTool.input_schema();
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::ConflictingNestedUnion
        );
        assert_eq!(schema, original);
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
    fn kimi_parameters_rejects_nested_oneof_instead_of_renaming_it() {
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
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::ConflictingNestedUnion
        );
        assert_eq!(schema, original);
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
    fn kimi_parameters_intersects_scalar_const_with_multi_value_enum() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "kind": {"type": "string", "const": "b", "enum": ["a", "b", "c"]},
                "level": {"type": "number", "const": 2, "enum": [1, 2, 3]}
            }
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert_eq!(schema["properties"]["kind"]["enum"], json!(["b"]));
        assert_eq!(schema["properties"]["level"]["enum"], json!([2]));
        assert!(schema["properties"]["kind"].get("const").is_none());

        let mut disjoint = json!({
            "type": "object",
            "properties": {
                "kind": {"type": "string", "const": "d", "enum": ["a", "b"]}
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut disjoint).unwrap_err(),
            KimiParameterSchemaError::ConflictingLiteralConstraint
        );
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
    fn kimi_parameters_preserves_integral_float_enum_members_on_the_wire() {
        // Round-4 review: `number` ∩ `integer` narrows with the module's own
        // MFJS integer ruling, so the f64-spelled `3.0` stays an admitted
        // value. Raw serde integrality used to drop it during the singleton
        // collapse and silently shrink the wire contract to `[4]`.
        let mut parameters = json!({
            "type": "object",
            "properties": {
                "count": {"type": "integer", "anyOf": [{"type": "number", "enum": [3.0, 4]}]}
            }
        });

        sanitize_for_kimi_parameters(&mut parameters)
            .expect("an integral float enum member is a representable contract");

        assert_eq!(parameters["properties"]["count"]["enum"], json!([3.0, 4]));
        assert!(parameters["properties"]["count"].get("anyOf").is_none());
    }

    #[test]
    fn kimi_parameters_lands_an_integral_float_const_on_the_integer_enum() {
        let mut parameters = json!({
            "type": "object",
            "properties": {
                "count": {"type": "integer", "anyOf": [{"type": "number", "const": 3.0}]}
            }
        });

        sanitize_for_kimi_parameters(&mut parameters)
            .expect("an integral float const lands on the integer enum verbatim");

        assert_eq!(parameters["properties"]["count"]["enum"], json!([3.0]));
        assert!(parameters["properties"]["count"].get("anyOf").is_none());
    }

    #[test]
    fn kimi_parameters_drops_enum_members_outside_the_integer_window_instead_of_failing() {
        // A u64-spelled value one past 2^53-1 fails `mfjs_integer_value`, so
        // narrowing removes it exactly as the MFJS enum check would rule:
        // the surviving member ships instead of the whole tool failing
        // validation on a member the integer contract never admitted.
        let mut parameters = json!({
            "type": "object",
            "properties": {
                "count": {
                    "type": "integer",
                    "anyOf": [{"type": "number", "enum": [9_007_199_254_740_993u64, 4]}]
                }
            }
        });

        sanitize_for_kimi_parameters(&mut parameters)
            .expect("the in-window member keeps the tool shippable");

        assert_eq!(parameters["properties"]["count"]["enum"], json!([4]));
    }

    #[test]
    fn kimi_parameters_ships_a_typed_parent_union_with_a_ref_branch() {
        // Round-4 review: `{type:"object"}` beside a `$ref` is a sibling
        // pair the MFJS whitelist rejects, so the exact union form must
        // survive the whole pipeline and the reference must resolve — the
        // tool ships instead of failing with a misleading
        // `InvalidReference`.
        let mut parameters = json!({
            "type": "object",
            "properties": {
                "payload": {"type": "object", "anyOf": [{"$ref": "#/$defs/A"}]}
            },
            "$defs": {
                "A": {"type": "object", "properties": {"x": {"type": "string"}}}
            }
        });

        sanitize_for_kimi_parameters(&mut parameters)
            .expect("an object-typed $ref union branch is exactly representable");

        // The union that could not collapse keeps its exact form: the
        // reference branch verbatim, no manufactured `type` sibling beside
        // it.
        let payload = &parameters["properties"]["payload"];
        assert_eq!(payload["anyOf"][0], json!({"$ref": "#/$defs/A"}));
        assert!(payload.get("type").is_none());
    }

    #[test]
    fn kimi_parameters_ships_exact_bool_branch_projections_transactionally() {
        // Round-4 review: bool-schema branches used to fail MFJS validation
        // wholesale, dropping the whole tool. The exact projections ship:
        // anyOf X,false ≡ X, and an all-false union is unsatisfiable with
        // the precise contradiction diagnosis — transactionally, leaving a
        // rejected schema byte-for-byte unchanged.
        let mut parameters = json!({
            "type": "object",
            "properties": {
                "doc": {
                    "type": "object",
                    "anyOf": [
                        {"type": "object", "properties": {"x": {"type": "string"}}},
                        false
                    ]
                }
            }
        });

        sanitize_for_kimi_parameters(&mut parameters)
            .expect("the false branch is removed instead of failing validation");

        let doc = &parameters["properties"]["doc"];
        let branches = doc["anyOf"].as_array().unwrap();
        assert_eq!(branches.len(), 1);
        assert_eq!(branches[0]["properties"]["x"]["type"], "string");

        let mut unsatisfiable = json!({
            "type": "object",
            "properties": {"doc": {"type": "object", "anyOf": [false]}}
        });
        let original = unsatisfiable.clone();
        assert_eq!(
            sanitize_for_kimi_parameters(&mut unsatisfiable).unwrap_err(),
            KimiParameterSchemaError::ContradictoryParentUnion
        );
        assert_eq!(unsatisfiable, original, "rejection must be transactional");
    }

    #[test]
    fn kimi_parameters_fails_closed_on_bool_and_null_const() {
        // MFJS supports enum literals of string/number only, and a bool/null
        // const has no supported equivalent. Translating it to `enum: [v]`
        // would hand the node to validation for an unsupported literal and
        // must never ship the bare declared type — silently widening the pin
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
    fn kimi_parameters_empty_items_does_not_make_min_items_advisory() {
        for mut schema in [
            json!({
                "type": "object",
                "properties": {"a": {"type": "array", "items": {}, "minItems": 1}}
            }),
            json!({
                "type": "object",
                "properties": {
                    "a": {"anyOf": [
                        {"type": "array", "items": {}, "minItems": 1},
                        {"type": "string"}
                    ]}
                }
            }),
            json!({
                "type": "object",
                "$defs": {"a": {"type": "array", "items": {}, "minItems": 1}},
                "properties": {"x": {"$ref": "#/$defs/a"}}
            }),
        ] {
            let original = schema.clone();
            assert_eq!(
                sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
                KimiParameterSchemaError::UnsupportedKeyword
            );
            assert_eq!(schema, original);
        }
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
    fn advisory_annotations_are_stripped_without_touching_default_data() {
        let mut schema = json!({
            "title": "Outer annotation",
            "$comment": "why this tool exists",
            "$id": "https://example.com/outer",
            "type": "object",
            "properties": {
                "label": {
                    "title": "Label",
                    "type": "string",
                    "default": {
                        "title": "instance data",
                        "minLength": 7,
                        "dependentRequired": {"x": ["y"]}
                    }
                }
            },
            "required": ["label"]
        });

        sanitize_for_kimi_parameters(&mut schema).unwrap();

        assert!(schema.get("title").is_none());
        assert!(schema.get("$comment").is_none());
        assert!(schema.get("$id").is_none());
        assert!(schema["properties"]["label"].get("title").is_none());
        assert_eq!(
            schema["properties"]["label"]["default"],
            json!({
                "title": "instance data",
                "minLength": 7,
                "dependentRequired": {"x": ["y"]}
            })
        );
        validate_mfjs_parameters(&schema).unwrap();

        let mut reference = schema.clone();
        sanitize_for_kimi_parameters(&mut reference).unwrap();
        assert_eq!(schema, reference);
    }

    #[test]
    fn unsupported_enum_values_fail_closed_and_the_tool_is_dropped() {
        // The MFJS specification supports enum values only as float/int/str
        // scalars of one single type ("枚举值我们只支持基础类型中的 float,
        // int 和 str……不支持混合类型 (List 中的每个枚举值的类型必须相同)").
        // The enum is never stripped: the keyword is a real constraint the
        // engine never re-enforces (the supported plugin path forwards input
        // without revalidating the original schema), so removing it would
        // silently widen the advertised contract — a singleton object enum
        // shipping as "any object with a string mode", a fixed array shape
        // shipping as "any string array" (review 5914018939). The keyword
        // stays and the validator rejects the whole tool — the same
        // fail-closed shape as the boolean/null pins (review 5910671961).
        for (name, property) in [
            (
                "mixed_scalar_kinds",
                json!({"type": "string", "enum": ["fast", 1]}),
            ),
            (
                "object_values",
                json!({"type": "object", "enum": [{"nested": true}]}),
            ),
            ("array_values", json!({"type": "array", "enum": [["a"]]})),
        ] {
            let mut schema = json!({
                "type": "object",
                "properties": { name: property }
            });
            let error = sanitize_for_kimi_parameters(&mut schema)
                .expect_err("an unsupported enum must fail closed, not lose the keyword and widen");
            assert!(
                matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
                "{name}: the kept enum drops via the validator: {error:?}"
            );
        }

        // The review's concrete construction: a required typed object
        // property pinned to a single value keeps its pin and fails closed —
        // the widened schema never reaches the wire.
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {
                    "type": "object",
                    "properties": {"kind": {"type": "string"}},
                    "required": ["kind"],
                    "additionalProperties": false,
                    "enum": [{"kind": "compact"}]
                }
            },
            "required": ["mode"]
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("the fixed object pin must fail closed, not advertise any mode object");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
            "{error:?}"
        );

        // A node whose only keyword is an unsupported `enum` has no type to
        // fall back to: it fails closed with MissingType — the sanitizer
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

        // Homogeneous scalar enums stay shippable: str-only, int-only, and
        // float-only are exactly the document's supported shapes.
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
    fn boolean_enum_pins_fail_closed_instead_of_widening() {
        // A boolean (or null) enum literal is a fixed-argument pin: MFJS
        // cannot represent it, but stripping the keyword used to leave the
        // bare declared type behind — `{"type": "boolean", "enum": [true]}`
        // shipping as "any boolean" — and the supported plugin path
        // forwards input to its handler without revalidating the original
        // enum, so a `false` argument would reach the handler that was
        // authored to see only `true`. Consistently with the bool/null
        // `const` handling, the unrepresentable pin stays on the schema
        // and the validator rejects the whole tool (review 5910671961).
        let mut schema = json!({
            "type": "object",
            "properties": {
                "flag": {"type": "boolean", "enum": [true]}
            }
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("a boolean enum pin must fail closed, not widen");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
            "the kept pin drops via the validator: {error:?}"
        );

        // A mixed scalar/boolean enum keeps its unrepresentable literal too
        // and fails closed the same way, instead of losing the keyword and
        // shipping the bare declared type.
        let mut mixed = json!({
            "type": "object",
            "properties": {
                "mode": {"type": "string", "enum": ["fast", true]}
            }
        });
        let error = sanitize_for_kimi_parameters(&mut mixed)
            .expect_err("a mixed enum with a boolean literal must fail closed");
        assert!(
            matches!(error, KimiParameterSchemaError::InvalidKeywordValue),
            "{error:?}"
        );
    }

    #[test]
    fn kimi_parameters_empty_enum_fails_closed_in_the_validator() {
        // An empty enum accepts nothing, so stripping it would silently
        // widen an unsatisfiable constraint into "anything goes". The
        // sanitizer keeps the keyword and the validator's non-empty rule
        // fails the whole tool — the documented malformed-schema policy.
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

        // A non-array `enum` takes the same road: kept by normalization,
        // then failed closed by the validator.
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
    fn kimi_parameters_keeps_stricter_local_validation_rules() {
        // These are intentional local fail-closed rules, not candidates for
        // relaxation to match a more permissive validator.
        let cases = [
            (
                json!({
                    "type": "object",
                    "properties": {"": {"type": "string"}}
                }),
                KimiParameterSchemaError::InvalidKeywordValue,
            ),
            (
                json!({
                    "type": "object",
                    "properties": {"x": {"type": "string"}},
                    "required": ["x", "x"]
                }),
                KimiParameterSchemaError::InvalidKeywordValue,
            ),
            (
                json!({
                    "type": "object",
                    "properties": {"x": {"enum": ["a"]}}
                }),
                KimiParameterSchemaError::MissingType,
            ),
            (
                json!({
                    "type": "object",
                    "properties": {"x": {"type": ["string", "null"], "enum": ["a"]}}
                }),
                KimiParameterSchemaError::MissingType,
            ),
            // The `Optional[T] = default` union shape is no longer in this
            // table: the round-7 instance-data distribution ships it onto the
            // admitting branch (the exact form base shipped by flattening),
            // pinned by
            // `kimi_lane_ships_optional_property_default_into_the_admitting_branch`.
            (
                json!({
                    "type": "object",
                    "properties": {"x": {"$ref": "#/$defs/X", "default": "a"}},
                    "$defs": {"X": {"type": "string"}}
                }),
                KimiParameterSchemaError::InvalidReference,
            ),
        ];

        for (mut schema, expected) in cases {
            let original = schema.clone();
            assert_eq!(
                sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
                expected
            );
            assert_eq!(schema, original);
        }
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
    fn kimi_parameters_rejects_optional_non_terminating_ref_and_preserves_nullable_exit() {
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
        let original = optional.clone();
        assert_eq!(
            sanitize_for_kimi_parameters(&mut optional).unwrap_err(),
            KimiParameterSchemaError::NonTerminatingReference
        );
        assert_eq!(optional, original);

        let mut unused_cycle = json!({
            "type": "object",
            "properties": {},
            "$defs": {
                "A": {"$ref": "#/$defs/B"},
                "B": {"$ref": "#/$defs/A"}
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut unused_cycle).unwrap_err(),
            KimiParameterSchemaError::NonTerminatingReference
        );

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

        // The encoded-length budget arms on every enum node: 1000 values of
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
    fn kimi_parameters_check_enum_text_on_small_enums_too() {
        // Round-4 review: the per-string length check used to arm only past
        // 250 entries, so a ≤250-value enum could carry one ~100KB string
        // violating MFJS_MAX_ENUM_STRING_LENGTH and still pass local
        // validation. The probe sizes the string between the two budgets —
        // one past 75000 encoded chars, well under the 120000 byte budget —
        // so only the enum-text check can reject it.
        let mut small_enum_over = json!({
            "type": "object",
            "properties": {
                "blob": {
                    "type": "string",
                    "enum": [Value::String("x".repeat(MFJS_MAX_ENUM_STRING_LENGTH + 1))]
                }
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut small_enum_over).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let mut small_enum_at = json!({
            "type": "object",
            "properties": {
                "blob": {
                    "type": "string",
                    "enum": [Value::String("x".repeat(MFJS_MAX_ENUM_STRING_LENGTH))]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut small_enum_at)
            .expect("a single enum string at the exact budget ships");
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
        // MFJS_MAX_SCHEMA_BYTES bounds the raw document before normalization
        // and the normalized document again before validation.
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
    fn kimi_parameters_checks_raw_budget_before_clone_or_annotation_strip() {
        let mut oversized = json!({
            "title": "x".repeat(MFJS_MAX_SCHEMA_BYTES),
            "type": "object",
            "properties": {}
        });
        let original = oversized.clone();
        assert!(schema_within_structural_depth_limit(&oversized));
        assert!(!schema_within_mfjs_local_budgets(&oversized));
        assert_eq!(
            sanitize_for_kimi_parameters(&mut oversized).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        assert_eq!(oversized, original);

        let mut literal = json!("leaf");
        for _ in 0..=MFJS_MAX_STRUCTURAL_DEPTH {
            literal = json!({"nested": literal});
        }
        let mut deep_default = json!({
            "type": "object",
            "properties": {
                "value": {"type": "string", "default": literal}
            }
        });
        let original = deep_default.clone();
        assert_eq!(
            sanitize_for_kimi_parameters(&mut deep_default).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        assert_eq!(deep_default, original);
    }

    #[test]
    fn kimi_relaxed_budgets_reject_501_anyof_and_3001_properties() {
        let branches =
            |count| Value::Array((0..count).map(|_| json!({"type": "string"})).collect());
        let at_any_of_limit = json!({
            "type": ["object", "null"],
            "anyOf": branches(500)
        });
        let original = at_any_of_limit.clone();
        validate_kimi_relaxed_resource_budgets(&at_any_of_limit)
            .expect("500 branches and a non-MFJS type array are resource-valid");
        assert_eq!(at_any_of_limit, original);

        let over_any_of = json!({"anyOf": branches(501)});
        let original = over_any_of.clone();
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&over_any_of).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        assert_eq!(over_any_of, original);

        let properties = |count| {
            let mut properties = Map::new();
            for index in 0..count {
                properties.insert(format!("p{index}"), json!({}));
            }
            Value::Object(properties)
        };
        let over_properties = json!({
            "type": "object",
            "properties": properties(3001)
        });
        let original = over_properties.clone();
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&over_properties).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        assert_eq!(over_properties, original);
    }

    #[test]
    fn kimi_relaxed_budgets_do_not_interpret_default_or_const_as_schemas() {
        let literal = json!({
            "anyOf": (0..501).map(|_| json!({"type": "string"})).collect::<Vec<_>>(),
            "enum": (0..1001).collect::<Vec<_>>()
        });
        let schema = json!({
            "default": literal.clone(),
            "const": literal.clone(),
            "customComposition": {
                "oneOf": [{"allOf": [{"not": {"type": "string"}}]}]
            }
        });

        validate_kimi_relaxed_resource_budgets(&schema)
            .expect("literal budgets and unsupported semantics must not be enforced");
        assert_eq!(schema["default"], literal.clone());
        assert_eq!(schema["const"], literal);
    }

    #[test]
    fn kimi_relaxed_budgets_enforce_property_depth_and_document_enum_limits() {
        let nested = |depth| {
            let mut node = json!({"type": "string"});
            for _ in 0..depth {
                node = json!({"type": "object", "properties": {"next": node}});
            }
            node
        };
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&nested(31)).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        validate_kimi_relaxed_resource_budgets(&nested(30))
            .expect("30 property levels remain within the local budget");

        let enums = |left, right| {
            json!({
                "type": "object",
                "properties": {
                    "left": {"enum": (0..left).collect::<Vec<_>>()},
                    "right": {"enum": (0..right).collect::<Vec<_>>()}
                }
            })
        };
        validate_kimi_relaxed_resource_budgets(&enums(500, 500))
            .expect("1000 enum values across the document are allowed");
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&enums(500, 501)).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_relaxed_budgets_check_enum_text_at_any_entry_count() {
        // Round-4 review: the enum-text budget used to arm only past 250
        // entries, letting a small enum carry one oversized string the wire
        // rejects. The check now arms at any entry count — a single
        // over-budget string fails exactly like an over-budget total, while
        // a moderate enum of the old probe's size still ships.
        let enum_strings = |count, length| {
            Value::Array(
                (0..count)
                    .map(|index| Value::String(format!("{index}-{}", "x".repeat(length))))
                    .collect(),
            )
        };
        validate_kimi_relaxed_resource_budgets(&json!({"enum": enum_strings(250, 200)}))
            .expect("a 250-value enum of moderate strings ships");
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&json!({"enum": enum_strings(251, 300)}))
                .unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
        validate_kimi_relaxed_resource_budgets(&json!({
            "enum": [Value::String("x".repeat(MFJS_MAX_ENUM_STRING_LENGTH))]
        }))
        .expect("a single string at the exact budget ships");
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&json!({
                "enum": [Value::String("x".repeat(MFJS_MAX_ENUM_STRING_LENGTH + 1))]
            }))
            .unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        assert_eq!(
            validate_kimi_relaxed_resource_budgets(
                &json!({"title": "x".repeat(MFJS_MAX_SCHEMA_BYTES)})
            )
            .unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );

        let mut literal = json!("leaf");
        for _ in 0..=MFJS_MAX_STRUCTURAL_DEPTH {
            literal = json!({"nested": literal});
        }
        assert_eq!(
            validate_kimi_relaxed_resource_budgets(&json!({"default": literal})).unwrap_err(),
            KimiParameterSchemaError::ResourceLimitExceeded
        );
    }

    #[test]
    fn kimi_parameters_structural_depth_boundary_is_engine_local() {
        // The raw-input guard counts JSON containers as well as schema nodes,
        // including the root properties map and the scalar type value. With
        // 61 wrapped item levels the leaf sits at JSON depth 64; one more
        // level exceeds the local guard.
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
        let mut at_guard = nested_items(61);
        assert!(schema_within_mfjs_local_budgets(&at_guard));
        sanitize_for_kimi_parameters(&mut at_guard)
            .expect("a leaf at raw JSON depth 64 reaches the engine guard exactly and ships");
        let mut over = nested_items(62);
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
    fn kimi_parameters_rejects_bounds_instead_of_stripping_real_constraints() {
        // Every bound is a real validation constraint, including inverted or
        // fractional spellings. None may be erased into an unconstrained node.
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
            let original = schema.clone();
            assert_eq!(
                sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
                KimiParameterSchemaError::UnsupportedKeyword
            );
            assert_eq!(schema, original, "rejection must be transactional");
            for keyword in keywords {
                assert!(contains_key_anywhere(&schema, keyword));
            }
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
    fn kimi_parameters_rejects_root_and_chained_ref_constraint_siblings() {
        let mut root_sibling = json!({
            "$ref": "#/$defs/FileArgs",
            "$defs": {
                "FileArgs": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                }
            },
            "required": [],
            "properties": {}
        });
        let original = root_sibling.clone();
        assert_eq!(
            sanitize_for_kimi_parameters(&mut root_sibling).unwrap_err(),
            KimiParameterSchemaError::UnsupportedRootReference
        );
        assert_eq!(root_sibling, original);

        let mut chained_sibling = json!({
            "$ref": "#/$defs/Alias",
            "$defs": {
                "Alias": {
                    "$ref": "#/$defs/FileArgs",
                    "required": ["other"]
                },
                "FileArgs": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}},
                    "required": ["path"]
                }
            }
        });
        let original = chained_sibling.clone();
        assert_eq!(
            sanitize_for_kimi_parameters(&mut chained_sibling).unwrap_err(),
            KimiParameterSchemaError::UnsupportedRootReference
        );
        assert_eq!(chained_sibling, original);

        let mut chained_description = json!({
            "$ref": "#/$defs/Alias",
            "$defs": {
                "Alias": {
                    "$ref": "#/$defs/FileArgs",
                    "description": "must not be dropped"
                },
                "FileArgs": {
                    "type": "object",
                    "properties": {"path": {"type": "string"}}
                }
            }
        });
        assert_eq!(
            sanitize_for_kimi_parameters(&mut chained_description).unwrap_err(),
            KimiParameterSchemaError::UnsupportedRootReference
        );
    }

    #[test]
    fn kimi_parameters_rejects_root_ref_to_union() {
        let mut schema = json!({
            "$ref": "#/$defs/Choice",
            "$defs": {
                "Choice": {
                    "type": "object",
                    "anyOf": [
                        {"type": "object", "properties": {"a": {"type": "string"}}},
                        {"type": "object", "properties": {"b": {"type": "string"}}}
                    ]
                }
            }
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::RootMustBeObject
        );
        assert_eq!(schema, original);
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

    #[test]
    fn mfjs_reference_resolution_judges_the_decoded_pointer_segment() {
        // Round-4 review: the JSON Pointer lookup decodes `~1`→`/` and
        // `~0`→`~`, so the single-name admission rule must judge the same
        // decoded spelling. A `$defs` key carrying a literal `~1`/`~0`
        // resolves through its escaped spelling, an escaped slash can no
        // longer pass the raw-name check and resolve elsewhere (deeper into
        // a nested schema or into a misleading `InvalidReference`), and the
        // hostile spellings stay rejected.
        let document = json!({
            "type": "object",
            "properties": {
                "slash": {"$ref": "#/$defs/patch~01config"},
                "tilde": {"$ref": "#/$defs/patch~00config"}
            },
            "$defs": {
                "patch~1config": {
                    "type": "object",
                    "properties": {"a": {"type": "string"}}
                },
                "patch~0config": {
                    "type": "object",
                    "properties": {"b": {"type": "string"}}
                }
            }
        });
        validate_mfjs_parameters(&document)
            .expect("escaped reference spellings must reach their literal keys");

        // The discriminating case: an escaped slash spanning pointer
        // segments used to resolve DEEPER into a live schema — an address
        // the single-segment `$defs` grammar never admitted.
        let mut deep = document.clone();
        deep["properties"] = json!({"probe": {"$ref": "#/$defs/patch~1properties~1a"}});
        deep["$defs"] = json!({
            "patch": {
                "type": "object",
                "properties": {"a": {"type": "string"}}
            }
        });
        assert_eq!(
            validate_mfjs_parameters(&deep).unwrap_err(),
            KimiParameterSchemaError::InvalidReference
        );

        for (hostile, label) in [
            (
                "#/$defs/patch~1config",
                "an escaped slash decodes into a rejected multi-segment name",
            ),
            ("#/$defs/", "an empty name stays rejected"),
        ] {
            let mut probing = document.clone();
            probing["properties"] = json!({"probe": {"$ref": hostile}});
            assert_eq!(
                validate_mfjs_parameters(&probing).unwrap_err(),
                KimiParameterSchemaError::InvalidReference,
                "{label}"
            );
        }
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
    fn kimi_rejects_dependent_keywords_instead_of_flattening_constraints() {
        let mut schema = action_discriminated_schema();
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::UnsupportedKeyword
        );
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_leaves_schemas_without_dependent_keywords_unannotated() {
        let mut schema = json!({
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"]
        });

        sanitize_for_kimi_parameters(&mut schema).expect("plain schema");
        assert_eq!(schema["required"], json!(["path"]));
    }

    #[test]
    fn kimi_rejects_dependent_keywords_nested_under_properties() {
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
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::UnsupportedKeyword
        );
        assert_eq!(schema, original);
    }

    #[test]
    fn kimi_rejects_malformed_dependent_keywords_too() {
        let mut schema = json!({
            "type": "object",
            "properties": {},
            "dependentRequired": false,
            "dependentSchemas": ["invalid"]
        });
        let original = schema.clone();

        assert_eq!(
            sanitize_for_kimi_parameters(&mut schema).unwrap_err(),
            KimiParameterSchemaError::UnsupportedKeyword
        );
        assert_eq!(schema, original);
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
        sanitize_for_kimi_parameters(&mut parameters)
            .expect("the shipped make_pptx schema must pass Moonshot normalization");
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

#[cfg(test)]
mod r7_instance_data_pins {
    use super::*;
    use serde_json::json;

    // Round-7 P2-2: the registry collapse retains the common
    // `Optional[T] = default` union verbatim; the strict lane used to drop
    // the whole tool on the union-sibling whitelist although the exact wire
    // form (default inside the admitting branch) exists — base shipped it
    // by flattening first.
    #[test]
    fn kimi_lane_ships_optional_property_default_into_the_admitting_branch() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {
                    "anyOf": [{"type": "string"}, {"type": "null"}],
                    "default": "fast"
                }
            }
        });
        sanitize_for_kimi_parameters(&mut schema).expect("Optional-with-default must ship exactly");
        assert_eq!(
            schema["properties"]["mode"],
            json!({"anyOf": [
                {"type": "string", "default": "fast"},
                {"type": "null"}
            ]}),
            "the default rides the admitting branch, not the union node"
        );
    }

    #[test]
    fn kimi_lane_distributes_node_enum_across_branches_by_type() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "v": {
                    "anyOf": [{"type": "string"}, {"type": "integer"}],
                    "enum": ["a", 1]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("a fully admitted node enum distributes onto the branches");
        assert_eq!(
            schema["properties"]["v"],
            json!({"anyOf": [
                {"type": "string", "enum": ["a"]},
                {"type": "integer", "enum": [1]}
            ]}),
            "each branch carries exactly its admitted subset"
        );
    }

    #[test]
    fn kimi_lane_drops_a_branch_admitting_no_enum_value_exactly() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "v": {
                    "anyOf": [{"type": "string"}, {"type": "integer"}],
                    "enum": ["a", "b"]
                }
            }
        });
        sanitize_for_kimi_parameters(&mut schema)
            .expect("the string-only enum admits every original instance");
        assert_eq!(
            schema["properties"]["v"],
            json!({"anyOf": [{"type": "string", "enum": ["a", "b"]}]}),
            "the integer branch admits no enum value and drops exactly"
        );
    }

    #[test]
    fn kimi_lane_keeps_undistributable_instance_data_fail_closed() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "mode": {
                    "anyOf": [{"type": "string"}, {"type": "null"}],
                    "default": {"nested": true}
                }
            }
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("an object default admits no branch and must keep failing closed");
        assert_eq!(error, KimiParameterSchemaError::InvalidKeywordValue);
    }

    #[test]
    fn kimi_lane_enum_with_an_unadmitted_value_stays_fail_closed() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "v": {
                    "anyOf": [{"type": "string"}, {"type": "null"}],
                    "enum": ["a", true]
                }
            }
        });
        let error = sanitize_for_kimi_parameters(&mut schema)
            .expect_err("an enum value no branch admits must keep the node-level rejection");
        assert_eq!(error, KimiParameterSchemaError::InvalidKeywordValue);
    }

    #[test]
    fn kimi_lane_distributes_a_null_default_onto_the_null_branch() {
        let mut schema = json!({
            "type": "object",
            "properties": {
                "maybe": {
                    "anyOf": [{"type": "string"}, {"type": "null"}],
                    "default": null
                }
            }
        });
        sanitize_for_kimi_parameters(&mut schema).expect("a null default rides the null branch");
        assert_eq!(
            schema["properties"]["maybe"],
            json!({"anyOf": [
                {"type": "string"},
                {"type": "null", "default": null}
            ]}),
            "the null default rides the admitting null branch"
        );
    }
}
// NOTE: appended inside the file's existing test module boundary is not
// possible from a script; these two tests live in their own module that
// reaches the private walkers through the public `sanitize` seam.

#[cfg(test)]
mod review_wave_tests {
    use serde_json::{Value, json};

    #[test]
    fn nullable_false_parent_declines_the_null_branch_collapse() {
        // Review 6061318874: an explicit `nullable: false` parent intersected
        // with a surviving null branch is unsatisfiable — the collapse must
        // decline (keep the original) instead of widening the parent back to
        // nullable.
        let schema = json!({
            "nullable": false,
            "anyOf": [{"type": "string"}, {"type": "null"}]
        });
        let original = schema.clone();
        let mut worked = schema;
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked, original,
            "an explicit nullable:false must decline the collapse, not widen"
        );
    }

    #[test]
    fn enum_intersection_work_is_capped_and_declines_instead_of_hanging() {
        // Review 6061318874: the enum intersection is O(outer × inner) per
        // collision. Beyond MFJS_MAX_MERGE_WORK comparisons the merge must
        // decline (preserving the union) in bounded time rather than spin.
        let build_enum = |start: u64, count: u64| -> Value {
            Value::Array((start..start + count).map(Value::from).collect())
        };
        let schema = json!({
            "type": "integer",
            "enum": build_enum(0, 30_000),
            "anyOf": [{"type": "integer", "enum": build_enum(30_000, 30_000)}]
        });
        let mut worked = schema.clone();
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked, schema,
            "the over-budget merge must decline, preserving the original union"
        );
    }

    #[test]
    fn enum_intersection_merge_completes_under_the_cap_and_declines_at_it() {
        // The decline pinned by
        // `enum_intersection_work_is_capped_and_declines_instead_of_hanging`
        // fires for any cap value: the disjoint enums there also decline on
        // the empty intersection, and with no cap at all the bounded scan
        // still ends in the same decline. This test pins the cap VALUE. The
        // control performs 50 × 100 candidate comparisons and must COMPLETE
        // into the exact five-value intersection; the band case performs
        // ≈500 × 1000 = 500k comparisons — over the 100_000 cap and under
        // 100_000_000 — so loosening the cap past 500k would complete the
        // band merge and flip this assertion. The five shared values sit at
        // the END of the inner enum so the per-candidate match short-circuit
        // cannot undercut the O(outer × inner) accounting: every outer
        // candidate scans essentially the whole inner enum either way.
        let build_enum = |start: u64, count: u64| -> Value {
            Value::Array((start..start + count).map(Value::from).collect())
        };
        let enum_with_shared_tail =
            |filler_start: u64, filler_count: u64, shared_start: u64, shared_count: u64| -> Value {
                let mut values: Vec<Value> = (filler_start..filler_start + filler_count)
                    .map(Value::from)
                    .collect();
                values.extend((shared_start..shared_start + shared_count).map(Value::from));
                Value::Array(values)
            };

        // Control: well under the cap — the merge must complete.
        let mut worked = json!({
            "type": "integer",
            "enum": build_enum(0, 50),
            "anyOf": [{
                "type": "integer",
                "enum": enum_with_shared_tail(1_000, 95, 45, 5)
            }]
        });
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked,
            json!({"type": "integer", "enum": build_enum(45, 5)}),
            "under the merge-work cap the exact intersection must complete"
        );

        // Band: ≈500k comparisons — over the real cap, under any loosened
        // value a mutation would introduce — so the merge must decline and
        // the ORIGINAL union must be preserved verbatim.
        let band = json!({
            "type": "integer",
            "enum": build_enum(0, 500),
            "anyOf": [{
                "type": "integer",
                "enum": enum_with_shared_tail(1_000, 995, 495, 5)
            }]
        });
        let mut worked = band.clone();
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked, band,
            "≈500k comparisons exceed the 100_000 merge-work cap: the merge \
             must decline and preserve the original union, not collapse it \
             to the five shared values"
        );
    }

    #[test]
    fn enum_merge_work_is_charged_by_comparison_cost_not_pair_count() {
        // Round-6 review: each charged pair comparison recursively walks
        // both operands, so pricing the cap at one unit per pair let ~100k
        // pairs of compound members cost ~10^10 element visits on the
        // byte-uncapped registry lane. The cap is now charged by the
        // operands' serialized size. The discriminating fixture shares one
        // large compound member between the two enum sides, so the pair
        // comparison SHORT-CIRCUITS on equality and a merge under pair-count
        // pricing would complete onto the intersection — while size charging
        // trips the cap mid-loop and declines, preserving the original
        // union. The final assertion is observable only in the declined
        // world, which is what makes this pin bite for the pricing mutation
        // (a pair-count revert fails it).
        let shared = {
            let mut fields = serde_json::Map::new();
            for index in 0..800 {
                fields.insert(
                    format!("field-{index:04}"),
                    Value::String(format!("shared-payload-{index:04}-value-long-suffix")),
                );
            }
            fields.insert("tail".into(), Value::String("shared-tail".to_string()));
            Value::Object(fields)
        };
        let differing_outer = {
            let mut fields = shared.as_object().expect("shared is an object").clone();
            fields.insert("tail".into(), Value::String("outer-a-tail".to_string()));
            Value::Object(fields)
        };
        let differing_inner = {
            let mut fields = shared.as_object().expect("shared is an object").clone();
            fields.insert("tail".into(), Value::String("inner-b-tail".to_string()));
            Value::Object(fields)
        };
        // ~43 KB per operand: the merge attempt charges (shared,shared),
        // then (outer-a,shared), then (outer-a,inner-b) — the third charge
        // crosses the 100_000 cap before its comparison runs, so the merge
        // declines even though its intersection would have been the shared
        // member.
        let compound = json!({
            "type": "object",
            "enum": [shared.clone(), differing_outer],
            "anyOf": [{"type": "object", "enum": [shared, differing_inner]}]
        });
        let mut worked = compound.clone();
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked["enum"], compound["enum"],
            "size charging must decline this merge: three ~36KB-sized pair charges exhaust the 100_000 cap before the third comparison runs, so the original union survives verbatim — pair-count pricing (1 per pair) would complete the merge and collapse the enum onto the shared member, flipping this assertion"
        );
        assert_eq!(
            worked["anyOf"][0]["enum"], compound["anyOf"][0]["enum"],
            "the inner enum must equally survive the declined merge verbatim"
        );

        // Control: the same pair count with tiny members — the pair loop
        // completes (declining only on the empty intersection), so the
        // collapse behaves exactly as it did before the size-charging.
        let small = json!({
            "type": "string",
            "enum": ["outer-a", "outer-b", "outer-c"],
            "anyOf": [{"type": "string", "enum": ["inner-a", "inner-b", "inner-c"]}]
        });
        let mut worked = small.clone();
        crate::tools::schema_sanitize::sanitize(&mut worked);
        assert_eq!(
            worked, small,
            "9 tiny pairs stay under the cap: the merge must decline only \
             on the empty intersection, preserving the union — both \
             spellings differ, so the union form survives identically"
        );
    }

    #[test]
    fn contradictory_parent_union_reports_the_conflict_instead_of_a_resource_limit() {
        // A parent `type` that provably excludes every `anyOf` branch is a
        // schema contradiction. The reconciliation used to leave the emptied
        // union behind for MFJS validation to trip over, surfacing as the
        // misleading `ResourceLimitExceeded` (or a root-shape error); the
        // fail-closed outcome must carry the precise diagnosis instead.
        let expect_contradiction = |schema: &Value, label: &str| {
            let mut candidate = schema.clone();
            let error = crate::tools::schema_sanitize::sanitize_for_kimi_parameters(&mut candidate)
                .expect_err("a parent type excluding every anyOf branch must be rejected");
            assert_eq!(
                error,
                crate::tools::schema_sanitize::KimiParameterSchemaError::ContradictoryParentUnion,
                "{label}: the rejection must name the parent/branch contradiction"
            );
            assert!(
                error
                    .to_string()
                    .contains("contradictory typed union branch"),
                "{label}: the message must name the conflict, got: {error}"
            );
        };
        // The review's shape: the contradiction sits on a property, so the
        // emptied union used to surface at validation as
        // `ResourceLimitExceeded`.
        expect_contradiction(
            &json!({
                "type": "object",
                "properties": {
                    "victim": {
                        "type": "object",
                        "anyOf": [{"type": "string"}, {"type": "integer"}]
                    }
                }
            }),
            "property-level contradiction",
        );
        // The same contradiction at the root.
        expect_contradiction(
            &json!({
                "type": "object",
                "anyOf": [{"type": "string"}, {"type": "integer"}]
            }),
            "root-level contradiction",
        );
        // And inside a branch: the effective parent type flows through the
        // outer branch and conflicts with every nested branch.
        expect_contradiction(
            &json!({
                "type": "object",
                "properties": {
                    "victim": {
                        "type": "object",
                        "anyOf": [{
                            "type": "object",
                            "anyOf": [{"type": "string"}, {"type": "integer"}]
                        }]
                    }
                }
            }),
            "branch-nested contradiction",
        );
    }
}
