//! Compare the production mirror update with the authoritative parser at every
//! delivered prefix, including reads between deltas and terminal mutations.
use super::*;

fn chunks(text: &str, width: usize) -> Vec<&str> {
    let mut parts = Vec::new();
    let mut start = 0;
    while start < text.len() {
        let mut end = (start + width).min(text.len());
        while !text.is_char_boundary(end) {
            end += 1;
        }
        parts.push(&text[start..end]);
        start = end;
    }
    parts
}

fn assert_mirror(baseline: &ToolUseState, candidate: &ToolUseState) {
    assert_eq!(candidate.input_buffer, baseline.input_buffer);
    assert_eq!(
        candidate.input, baseline.input,
        "prefix: {:?}",
        baseline.input_buffer
    );
    assert_eq!(candidate.input_parse_error, baseline.input_parse_error);
}

fn compare_delta(baseline: &mut ToolUseState, candidate: &mut ToolUseState, delta: &str) {
    baseline.input_buffer.push_str(delta);
    let expected_some = if let Some(value) = parse_tool_input(&baseline.input_buffer) {
        baseline.input = value;
        true
    } else {
        false
    };
    candidate.input_buffer.push_str(delta);
    assert_eq!(
        candidate.refresh_partial_input(delta),
        expected_some,
        "Some/None disposition at prefix {:?}",
        baseline.input_buffer
    );
    assert_mirror(baseline, candidate);
}

fn compare_trace(text: &str, width: usize) {
    let mut baseline = tool_state(json!({"initial": "retained until parse succeeds"}), "");
    let mut candidate = baseline.clone();
    for delta in chunks(text, width) {
        compare_delta(&mut baseline, &mut candidate, delta);
    }
    assert_eq!(final_tool_input(&candidate), final_tool_input(&baseline));
}

#[test]
fn partial_input_differential_prefixes_preserve_parser_disposition_and_mirror() {
    let fixtures = [
        r#"{"a":1,"b":"unfinished"#,
        r#"{"s":"aaa[1,2]bbbbbbbbbbbb"}"#,
        // Completing a raw root segment changes precedence over the array,
        // even though the root is malformed and the old mirror remains Some.
        r#"{"s":"[1] more text } trailing"#,
        r#"{"s":"} extra [1]more"}"#,
        "```json\n{\"a\":1}\n```",
        r#""{\"a\":1}""#,
        "{not json",
        r#"{"a":1} trailing"x"#,
        r#"{"a":"\u1234\n\\\"text"}"#,
        r#"{"a":"\uZZZZmore"}"#,
        r#"{"a":"\q invalid escape more"}"#,
        "{\"a\":\"raw\u{0001}control\"}",
        "{\"a\":\"raw\nline\tcontrol\"}",
        r#"{"a":"foo`bar"}"#,
        // Fence stripping after a closed raw root can change quote parity.
        "{\"a\": \"}\"}\n```z\" tail",
        // Quote-unaware closer removal can merge escape runs. The invalid
        // escape must disable reuse before that repair can change the value.
        r#"{"k": "\]\" tail"#,
        r#"{"中文🙂":"text🙂","nested":[{"a":true}],}"#,
        r#"[1,"text"]"#,
        " ",
        "",
    ];
    for fixture in fixtures {
        for width in [1, 2, 7, 64] {
            compare_trace(fixture, width);
        }
    }

    // Fixed-seed generated inputs cover both serialized values and malformed
    // quote/escape/segment combinations without requiring a fuzz dependency.
    let mut seed = 37_u64;
    let alphabet = b"{}[]\"\\`,:abcdef0123 \n\t";
    for case in 0..64 {
        seed = seed.wrapping_mul(6364136223846793005).wrapping_add(1);
        let content = [
            "中文🙂 plain",
            "\\\"\n[]{}",
            "[1,2] after",
            "} then [3]",
            "`fence",
        ][seed as usize % 5]
            .repeat(4);
        compare_trace(
            &json!({"content": content, "number": case, "nested": [{"a":true}]}).to_string(),
            1 + case % 31,
        );
        let mut malformed = String::from("{");
        for _ in 0..64 {
            seed = seed.wrapping_mul(6364136223846793005).wrapping_add(1);
            malformed.push(alphabet[(seed >> 24) as usize % alphabet.len()] as char);
        }
        compare_trace(&malformed, 1 + case % 13);
    }
    // Exercise repair-size boundaries with coarse deltas to keep this focused
    // regression cheap; the small fixtures above cover every UTF-8 prefix.
    let overhead = json!({"content":"", "meta":{"a":[1,2]}}).to_string().len();
    for size in [1024 * 1024 - 1, 1024 * 1024, 1024 * 1024 + 1] {
        let text = json!({"content": "x".repeat(size - overhead), "meta": {"a":[1,2]}}).to_string();
        assert_eq!(text.len(), size);
        compare_trace(&text, 65536);
    }
}

#[test]
fn partial_input_differential_repair_parity_at_adversarial_delta_boundaries() {
    for deltas in [
        vec!["{\"a\": \"}", "\"}\n```z\""],
        vec![r#"{"k": ""#, "\\", "]", "\\\"", " tail"],
    ] {
        let mut baseline = tool_state(json!({}), "");
        let mut candidate = baseline.clone();
        for delta in deltas {
            compare_delta(&mut baseline, &mut candidate, delta);
        }
    }
}

#[test]
fn partial_input_proof_reuses_dispositions_and_rechecks_changed_segments() {
    use super::partial_input::PartialInputProof;

    for (prefix, expected_some) in [(r#"{"content":""#, false), (r#"{"s":"[1]"#, true)] {
        let mut proof = PartialInputProof::default();
        let mut buffer = prefix.to_owned();
        assert_eq!(proof.reuse(&buffer, prefix), None);
        assert_eq!(parse_tool_input(&buffer).is_some(), expected_some);
        proof.record(expected_some);
        buffer.push_str(" more");
        assert_eq!(proof.reuse(&buffer, " more"), Some(expected_some));
        assert_eq!(parse_tool_input(&buffer).is_some(), expected_some);

        // A newly completed raw root takes precedence over an extracted array.
        buffer.push_str("}");
        assert_eq!(proof.reuse(&buffer, "}"), None);
        let some = parse_tool_input(&buffer).is_some();
        proof.record(some);
        buffer.push_str(" tail");
        assert_eq!(proof.reuse(&buffer, " tail"), Some(some));
        assert_eq!(parse_tool_input(&buffer).is_some(), some);
    }
}

#[test]
fn partial_input_proof_rejects_stale_buffers_and_explicit_invalidation() {
    use super::partial_input::PartialInputProof;

    let prefix = r#"{"content":""#;
    let mut certified = PartialInputProof::default();
    assert_eq!(certified.reuse(prefix, prefix), None);
    certified.record(false);
    assert_eq!(
        certified.clone().reuse(&format!("{prefix}x"), "x"),
        Some(false)
    );

    // Wrong previous length and wrong suffix are independent stale hints.
    for (buffer, delta) in [(format!("{prefix}xx"), "x"), (format!("{prefix}x"), "y")] {
        let mut proof = certified.clone();
        assert_eq!(proof.reuse(&buffer, delta), None);
    }
    let mut invalidated = certified;
    invalidated.invalidate();
    assert_eq!(invalidated.reuse(&format!("{prefix}x"), "x"), None);
}

#[test]
fn partial_input_differential_interleaving_continuation_and_new_attempt() {
    let mut baseline = [
        tool_state(json!({}), ""),
        tool_state(json!({"initial":2}), ""),
    ];
    let mut candidate = baseline.clone();
    for (index, delta) in [
        (0, r#"{"content":""#),
        (1, r#"{"s":"[1]"#),
        (0, "中文🙂 text"),
        (1, " more"),
        (0, " continuation"),
        (1, "} selected segment changes"),
        (0, r#"","meta":{"a":1}}"#),
    ] {
        compare_delta(&mut baseline[index], &mut candidate[index], delta);
        for index in 0..2 {
            assert_mirror(&baseline[index], &candidate[index]);
        }
    }
    // Reused provider ID must acquire fresh state with a different initial
    // value, rather than carrying the previous attempt's certificate/mirror.
    baseline[0] = tool_state(json!({"retry":true}), "");
    candidate[0] = baseline[0].clone();
    for delta in [r#"{"retry":""#, "fresh", " value", r#""}"#] {
        compare_delta(&mut baseline[0], &mut candidate[0], delta);
    }
}

#[test]
fn partial_input_differential_invalidation_preserves_terminal_mutations_and_late_delta() {
    let mut baseline = tool_state(json!({}), "");
    let mut candidate = baseline.clone();
    for delta in [r#"{"s":"[1]"#, " unfinished"] {
        compare_delta(&mut baseline, &mut candidate, delta);
    }
    // These writes model block-stop error tagging or schema normalization.
    // They must survive when the next full evaluation returns None, and be
    // replaced when the authoritative parser returns Some (the extracted array).
    candidate.invalidate_input_parse_hint();
    baseline.input = json!({"terminal":"mutation"});
    baseline.input_parse_error = Some("terminal parse error".into());
    candidate.input = baseline.input.clone();
    candidate.input_parse_error = baseline.input_parse_error.clone();
    compare_delta(&mut baseline, &mut candidate, " late text");
    compare_delta(&mut baseline, &mut candidate, "} changes selected segment");

    baseline = tool_state(json!({}), "");
    candidate = baseline.clone();
    for delta in [r#"{"content":""#, "unfinished"] {
        compare_delta(&mut baseline, &mut candidate, delta);
    }
    candidate.invalidate_input_parse_hint();
    baseline.input = json!({"raw_arguments":baseline.input_buffer});
    baseline.input_parse_error = Some("malformed final input".into());
    candidate.input = baseline.input.clone();
    candidate.input_parse_error = baseline.input_parse_error.clone();
    compare_delta(&mut baseline, &mut candidate, " late unfinished text");
    assert!(candidate.input.get("raw_arguments").is_some());

    // A snapshot/buffer replacement uses a fresh proof; initial-input-only
    // finalization remains authoritative when no delta ever arrives.
    baseline = tool_state(json!({"initial_only":true}), "");
    candidate = baseline.clone();
    assert_eq!(final_tool_input(&candidate), json!({"initial_only":true}));
    compare_delta(&mut baseline, &mut candidate, r#"{"new":""#);
    compare_delta(&mut baseline, &mut candidate, "content");
}
