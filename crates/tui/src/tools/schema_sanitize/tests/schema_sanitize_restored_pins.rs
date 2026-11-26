// Restored base-side test pins for `schema_sanitize.rs` — round-23 FIX WAVE (PR #147).
//
// The round-22 rebase reconstruction of this PR silently dropped 80 of the
// base tree's `mod tests` fns. This module re-grafts them VERBATIM from
// `crates/tui/src/tools/schema_sanitize.rs` at the PR base, so the test-name
// census and the historical pins are restored exactly. Name-for-name the set
// is: base-minus-head census (by test fn name) minus the two tests the PR's
// own mechanisms supersede — the full reconciliation lives in the PR's
// review records (no in-tree REPORT.md ships) (a third qualified-name-only
// entry,
// `xai_union_defect_walk_computes_each_subtree_once`, survives byte-identical
// at head under `kimi_tests` and is therefore not re-grafted here).
//
// WIRING (applied by the integrating lane, not by this file): inside
// `mod tests` in `schema_sanitize.rs`, declare
//
//     #[path = "schema_sanitize_restored_pins.rs"]
//     mod restored_base_pins;
//
// Because this module is a child of `mod tests`, `use super::*;` below
// reaches `mod tests`' imports and the file-scope items they re-export
// (`sanitize`, the `sanitize_for_*` passes,
// `oneof_semantic_defect[_with_computations]`, `strip_annotation_keywords`,
// `Value`) plus `serde_json::json`, without touching any production code.
//
// FIDELITY NOTES (round-23 verification, static — cargo not run, per lane
// instructions):
// - The xAI pass and the required-context/instance-scope machinery are
//   code-identical between base and head (`sanitize_for_xai_parameters` and
//   every helper through `oneof_node_defect`, `sanitize` itself), and no
//   fixture below routes through the head-side collapse collision guards
//   (verified per fixture: every single-element and nullable-union fixture
//   here lacks parent/branch key collisions, so the guards are inert).
// - `sanitize_for_anthropic_messages` at head enriches the note (group-name
//   lists, the STRICT_NOTE_MAX_GROUPS cap with a counted remainder, and the
//   may/must verb). Every specific-note fixture in the restored anthropic
//   tests carries a non-empty `required` on every group-bearing branch, so
//   the verb stays "must be" and every expected string holds verbatim; the
//   generic-note cases render identically (`root_composition_generic_note`
//   is code-identical). Zero assertion adaptations were required.
// - `responses_constraint_note_covers_a_single_dropped_group` pins the
//   round-23-restored `root_composition_constraint_note` semantics (aggregate
//   every composition keyword, disclose single dropped groups, keep the
//   round-15 cap/select-first-render hardening); it passes as-is against that
//   restored implementation.
// - The release-line harness fns (`previous_line_pipeline`,
//   `current_line_pipeline`) are nested inside
//   `anthropic_messages_pass_matches_the_previous_release_line_on_the_recorded_tools`,
//   exactly as in base; no external helper had to be copied. Their
//   dependencies (`crate::tools::apply_patch::ApplyPatchTool`,
//   `crate::tools::finance::FinanceTool`,
//   `crate::tools::schema_canonicalize::canonicalize_schema`,
//   `crate::tools::spec::ToolSpec`, `jsonschema` dev-dependency at 0.46) all
//   exist unchanged at head.
// - Not restored (superseded by the PR's mechanisms, per the round-22 audit):
//   `strict_sanitize_preserves_optional_properties_as_nullable` (superseded
//   by optional removal + reporting) and
//   `strict_mode_rejects_nested_unsupported_composition` (superseded by the
//   fail-closed typeless-composition strict fixes).
//
// Every test fn name below matches the base tree exactly and is absent from
// head's `schema_sanitize.rs` (collision check performed for all 80).

use super::*;
use serde_json::json;

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
fn empty_property_map_merged_from_a_nullable_arm_is_a_declared_scope() {
    // Provenance is captured after the nullable-union collapse: an arm
    // carrying an explicitly EMPTY properties map merges that map into
    // the scope, and it reads as a declaration — strictly local, like
    // any directly declared empty map — not as a synthetic map to
    // inherit through (review 5897819279). The untyped sibling branch
    // at the same instance shows the inherited reading still applies
    // when nothing was declared.
    let mut schema = json!({
        "type": "object",
        "properties": {
            "a": {"type": "string"},
            "b": {"type": "string"}
        },
        "oneOf": [
            {
                "anyOf": [
                    {"type": "object", "properties": {}},
                    {"type": "null"}
                ],
                "oneOf": [
                    {"required": ["a"]},
                    {"type": "string"}
                ]
            },
            {
                "oneOf": [
                    {"required": ["a"]},
                    {"type": "string"}
                ]
            },
            {"type": "string"}
        ]
    });
    sanitize(&mut schema);
    assert_eq!(
        schema["oneOf"][0]["oneOf"],
        json!([{}, {"type": "string"}]),
        "the merged explicit empty map is a declared scope: required prunes locally: {schema}"
    );
    assert_eq!(
        schema["oneOf"][1]["oneOf"],
        json!([{"required": ["a"]}, {"type": "string"}]),
        "the undeclared sibling branch inherits the ancestor names: {schema}"
    );
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
    // root into a plain object schema as before. (Round 29: it also
    // stays NOTE-SILENT — "exactly one of zero alternatives" is not a
    // contract the flatten relaxed; required-less compositions with
    // branches do disclose now.)
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
    // Round 29: bare (required-less) branches now disclose the
    // exactly-one relaxation — the flatten still merged alternatives the
    // authored schema made exclusive, even with no required group to
    // label.
    assert!(
        note.as_deref().is_some_and(|n| n.contains("Exactly one")),
        "the bare-branch flatten is disclosed: {note:?}"
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
    // Round 29: a required-less composition discloses the exactly-one
    // relaxation even when the branches are annotation-only — the
    // flatten still merged alternatives the authored schema made
    // exclusive (this pin's own comment says validation saw two
    // identical exactly-one alternatives).
    assert!(
        note.as_deref().is_some_and(|n| n.contains("Exactly one")),
        "the exactly-one flatten is disclosed: {note:?}"
    );
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
    // Round 29: the required-less composition discloses the exactly-one
    // relaxation even though the groups were pruned upstream — the
    // flatten still merged the two alternatives.
    assert!(
        note.as_deref().is_some_and(|n| n.contains("Exactly one")),
        "the pruned-branch flatten is disclosed: {note:?}"
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
        // Round 29: the required-less composition discloses the
        // exactly-one relaxation (the flatten merged the alternatives).
        assert!(
            note.as_deref().is_some_and(|n| n.contains("Exactly one")),
            "the floored-union flatten is disclosed: {note:?}"
        );
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
