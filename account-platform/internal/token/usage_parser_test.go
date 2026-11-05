package token

import "testing"

func TestProviderUsageRequiresActualCounts(t *testing.T) {
	for name, payload := range map[string]string{
		"missing both":              `{"usage":{}}`,
		"missing prompt":            `{"usage":{"completion_tokens":0}}`,
		"missing completion":        `{"usage":{"prompt_tokens":0}}`,
		"null prompt":               `{"usage":{"prompt_tokens":null,"completion_tokens":0}}`,
		"null completion":           `{"usage":{"prompt_tokens":0,"completion_tokens":null}}`,
		"missing usage":             `{}`,
		"null usage":                `{"usage":null}`,
		"negative prompt":           `{"usage":{"prompt_tokens":-1,"completion_tokens":0}}`,
		"negative completion":       `{"usage":{"prompt_tokens":0,"completion_tokens":-1}}`,
		"negative cached":           `{"usage":{"prompt_tokens":1,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":-1}}}`,
		"cached exceeds raw prompt": `{"usage":{"prompt_tokens":1,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":2}}}`,
		"fraction":                  `{"usage":{"prompt_tokens":1.5,"completion_tokens":0}}`,
		"quoted count":              `{"usage":{"prompt_tokens":"1","completion_tokens":0}}`,
		"overflow":                  `{"usage":{"prompt_tokens":9223372036854775808,"completion_tokens":0}}`,
		"malformed cached":          `{"usage":{"prompt_tokens":1,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":"1"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got, found := parseUsageJSON([]byte(payload)); found {
				t.Fatalf("unproven supplier counts accepted: %#v", got)
			}
		})
	}
}
func TestProviderUsageNormalizesExclusiveInput(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		want          Usage
	}{
		{"explicit zero", `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`, Usage{}},
		{"optional details", `{"usage":{"prompt_tokens":3,"completion_tokens":2}}`, Usage{InputTokens: 3, OutputTokens: 2}},
		{"optional cache count", `{"usage":{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{}}}`, Usage{InputTokens: 3, OutputTokens: 2}},
		{"high cache", `{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":90}}}`, Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 90}},
		{"all cache", `{"usage":{"prompt_tokens":100,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":100}}}`, Usage{OutputTokens: 5, CachedInputTokens: 100}},
		{"unknown extension", `{"other":"supported extension","usage":{"prompt_tokens":3,"completion_tokens":2,"future":9}}`, Usage{InputTokens: 3, OutputTokens: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found := parseUsageJSON([]byte(tc.payload))
			if !found || got != tc.want {
				t.Fatalf("valid counts = %#v found=%v want=%#v", got, found, tc.want)
			}
		})
	}
}
