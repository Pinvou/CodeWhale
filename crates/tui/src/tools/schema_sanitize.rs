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
                        "const" | "enum" => {}
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
        values.retain(|candidate| allowed.contains(candidate));
    }
    values.retain(|candidate| {
        declared.is_none_or(|expected| json_value_is_type(candidate, expected))
    });
    if values.is_empty() {
        return None;
    }
    Some(values)
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

/// A branch's surviving `required` group: the string names, sorted and
/// deduplicated.
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
        if self.witness_provable(left, left_group, &self.enclosing)
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
                && self.witness_provable(left, left_group, &self.enclosing)
                && left_set.iter().any(|candidate| {
                    !right_set.contains(candidate)
                        && self.enclosing_entry_admits(name.as_str(), candidate)
                        && self.branches.iter().enumerate().all(|(index, other)| {
                            index == left_index
                                || other
                                    .get("properties")
                                    .and_then(|props| props.get(name.as_str()))
                                    .and_then(pinned_value_set)
                                    .is_some_and(|set| !set.contains(candidate))
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
        let left_props = left.get("properties").and_then(Value::as_object);
        left_group
            .iter()
            .copied()
            .chain(enclosing.required.iter().copied())
            .all(|field| {
                let entries = [
                    left_props.and_then(|props| props.get(field)),
                    enclosing.props.and_then(|props| props.get(field)),
                ];
                entries.into_iter().flatten().all(entry_is_value_permissive)
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
            return pinned == candidate;
        }
        if let Some(values) = obj.get("enum").and_then(Value::as_array) {
            return values.contains(candidate);
        }
        if let Some(expected) = obj.get("type").and_then(Value::as_str) {
            return json_value_is_type(candidate, expected);
        }
        false
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
    // they also constrain the shared fields identically. Partially pruned
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
    // compare equal (review 5812238335). Branches whose constraints on
    // the shared fields still DIVERGE after that normalization stay
    // untouched: a const/enum pin
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
        // Constraint signature for the shared fields only: the branch's
        // own property entries for those names, with two normalizations
        // for constraints the containing root already implies for every
        // root-valid instance (review 5812238335): a vacuous `{}` entry
        // constrains nothing, and an entry identical to the root's own
        // schema for that field adds nothing the root has not already
        // required. Both normalize to absent so root-implied variants
        // compare equal; any other divergence keeps its verbatim shape
        // and the branch stays untouched (conservative pass-through on
        // that bounded fail-open remainder, disclosed in review
        // 5811598796's reply — deeper subset relations are a solver).
        let signature = match branch.get("properties").and_then(Value::as_object) {
            Some(props) => {
                let mut shared: serde_json::Map<String, Value> = serde_json::Map::new();
                for (key, value) in props {
                    if !names.contains(&key.as_str()) {
                        continue;
                    }
                    let vacuous = value.as_object().is_some_and(|entry| entry.is_empty());
                    let root_identical = root_props
                        .and_then(|rp| rp.get(key.as_str()))
                        .is_some_and(|root_entry| root_entry == value);
                    if !vacuous && !root_identical {
                        shared.insert(key.clone(), value.clone());
                    }
                }
                Value::Object(shared)
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
    // R3: a branch carrying a dead inner union cannot match.
    for (index, branch) in branches.iter().enumerate() {
        if let Some(inner) = walk.subtree_defect(branch, &format!("{path}.oneOf[{index}]")) {
            return Some(format!(
                "R3 branch {index} at {path} carries a dead inner union: {inner}"
            ));
        }
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
/// present-but-malformed (non-array) union value, an empty union, and
/// branches without an explicit object `type` — xAI's own 400 message for
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
    required.iter().any(|entry| {
        !entry
            .as_str()
            .is_some_and(|name| known_keys.iter().any(|known| *known == name))
    })
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        // shared requirement survives on both).
        let r2_shared = json!({
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
        let reason = oneof_semantic_defect(&r2_shared).expect("R2 shared group must fire");
        assert!(
            reason.starts_with("R2 2 branches share the required group"),
            "unexpected shared-group reason: {reason}"
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        assert_eq!(note, None);
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
        // fallback, keeping the original witness valid.
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
            "a valid instance of the original union must stay valid: {schema}"
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
