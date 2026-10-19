package token

import (
	"encoding/json"
	"testing"
)

func TestConfirmedTerminalUsageAcceptsDirectAndCommittedSettlementSnapshot(t *testing.T) {
	want := Usage{InputTokens: 17, OutputTokens: 5, CachedInputTokens: 4}
	for _, raw := range []string{
		`{"input_tokens":17,"output_tokens":5,"cached_input_tokens":4}`,
		`{"usage":{"input_tokens":17,"output_tokens":5,"cached_input_tokens":4},"actual_microcredits":23,"price_version_id":"historical-price-reference"}`,
	} {
		got, valid := confirmedUsageNumbers([]byte(raw))
		if !valid || got != want {
			t.Fatalf("confirmed usage = %#v, valid=%v; want %#v", got, valid, want)
		}
	}
}

func TestConfirmedTerminalUsageDoesNotInventZeroForMissingOrInvalidEvidence(t *testing.T) {
	for name, raw := range map[string]string{
		"empty":           `{}`,
		"missing input":   `{"output_tokens":0,"cached_input_tokens":0}`,
		"missing output":  `{"input_tokens":0,"cached_input_tokens":0}`,
		"missing cached":  `{"input_tokens":0,"output_tokens":0}`,
		"null input":      `{"input_tokens":null,"output_tokens":0,"cached_input_tokens":0}`,
		"null output":     `{"input_tokens":0,"output_tokens":null,"cached_input_tokens":0}`,
		"null cached":     `{"input_tokens":0,"output_tokens":0,"cached_input_tokens":null}`,
		"null snapshot":   `null`,
		"null wrapper":    `{"usage":null}`,
		"negative input":  `{"input_tokens":-1,"output_tokens":0,"cached_input_tokens":0}`,
		"negative output": `{"input_tokens":0,"output_tokens":-1,"cached_input_tokens":0}`,
		"negative cached": `{"input_tokens":0,"output_tokens":0,"cached_input_tokens":-1}`,
		"fraction":        `{"input_tokens":1.5,"output_tokens":0,"cached_input_tokens":0}`,
		"quoted count":    `{"input_tokens":"1","output_tokens":0,"cached_input_tokens":0}`,
		"overflow":        `{"input_tokens":9223372036854775808,"output_tokens":0,"cached_input_tokens":0}`,
		"unknown shape":   `{"input_tokens":1,"output_tokens":0,"cached_input_tokens":0,"other_usage":9}`,
	} {
		t.Run(name, func(t *testing.T) {
			if usage, valid := confirmedUsageNumbers([]byte(raw)); valid {
				t.Fatalf("unproven terminal evidence became confirmed usage: %#v", usage)
			}
		})
	}
}
func TestConfirmedTerminalUsageUsesExclusiveInput(t *testing.T) {
	for name, want := range map[string]Usage{
		"small cached": {InputTokens: 1, OutputTokens: 0, CachedInputTokens: 2},
		"high cached":  {InputTokens: 10, OutputTokens: 5, CachedInputTokens: 90},
		"all cached":   {InputTokens: 0, OutputTokens: 5, CachedInputTokens: 100},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			got, valid := confirmedUsageNumbers(raw)
			if !valid || got != want {
				t.Fatalf("legitimate exclusive-input evidence rejected: got=%#v valid=%v", got, valid)
			}
		})
	}
}
