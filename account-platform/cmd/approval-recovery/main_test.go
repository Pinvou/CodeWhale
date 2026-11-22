package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRecoveryCommandRequiresIndependentConfigurationAndNeverEmitsReferences(t *testing.T) {
	env := map[string]string{
		"APPROVAL_RECOVERY_DATABASE_URL":  "postgres://dedicated@example.invalid/db",
		"ACCOUNT_HTTP_URL":                "http://account.invalid",
		"BILLING_HTTP_URL":                "http://billing.invalid",
		"TOKEN_GATEWAY_HTTP_URL":          "http://token.invalid",
		"APPROVAL_RECOVERY_SERVICE_TOKEN": "dedicated",
	}
	getenv := func(key string) string { return env[key] }
	for _, tc := range []struct {
		name                string
		args                []string
		remove              string
		wantCode, wantCalls int
	}{
		{"bounded", []string{"--limit", "2"}, "", 0, 1},
		{"missing database", nil, "APPROVAL_RECOVERY_DATABASE_URL", 2, 0},
		{"missing identity", nil, "APPROVAL_RECOVERY_SERVICE_TOKEN", 2, 0},
		{"oversized", []string{"--limit", "201"}, "", 2, 0},
		{"unexpected apply", []string{"--apply"}, "", 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var output bytes.Buffer
			code := run(context.Background(), tc.args, func(key string) string {
				if key == tc.remove {
					return ""
				}
				return getenv(key)
			}, &output, func(_ context.Context, config commandConfig) (commandSummary, error) {
				calls++
				if config.Limit != 2 || config.Credential != "dedicated" {
					t.Fatalf("incorrect configuration: %+v", config)
				}
				return commandSummary{Status: "query_only", Reason: "NO_TERMINAL_AUTHORITY", Candidates: 1, Observed: 1}, nil
			})
			if code != tc.wantCode || calls != tc.wantCalls {
				t.Fatalf("code=%d calls=%d output=%s", code, calls, output.String())
			}
			var result map[string]any
			if json.Unmarshal(output.Bytes(), &result) != nil {
				t.Fatalf("non-JSON result: %s", output.String())
			}
			if _, found := result["approval_request_id"]; found {
				t.Fatal("request reference leaked")
			}
		})
	}
}

func TestRecoveryCommandRedactsActionFailure(t *testing.T) {
	env := map[string]string{
		"APPROVAL_RECOVERY_DATABASE_URL": "postgres://dedicated@example.invalid/db",
		"ACCOUNT_HTTP_URL":               "http://account.invalid", "BILLING_HTTP_URL": "http://billing.invalid",
		"TOKEN_GATEWAY_HTTP_URL": "http://token.invalid", "APPROVAL_RECOVERY_SERVICE_TOKEN": "dedicated",
	}
	var output bytes.Buffer
	code := run(context.Background(), nil, func(key string) string { return env[key] }, &output,
		func(context.Context, commandConfig) (commandSummary, error) {
			return commandSummary{}, errors.New("secret request reference")
		})
	if code != 1 || bytes.Contains(output.Bytes(), []byte("secret")) {
		t.Fatalf("failure leaked: %s", output.String())
	}
}
