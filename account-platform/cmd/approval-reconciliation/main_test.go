package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

func reconciliationEnvironment(key string) string {
	return map[string]string{
		"APPROVAL_RECONCILIATION_DATABASE_URL": "postgres://account-service.invalid/account",
		"BILLING_HTTP_URL":                     "https://billing.invalid",
		"TOKEN_GATEWAY_HTTP_URL":               "https://token.invalid",
		"INTERNAL_SERVICE_TOKEN":               "test-token",
	}[key]
}

func TestRunDefaultsToReadOnlyPlanAndRequiresExplicitApplyConfirmation(t *testing.T) {
	for _, args := range [][]string{{"--apply"}, {"--confirm", applyConfirmation}, {"--limit", "201"}} {
		var output bytes.Buffer
		if code := run(context.Background(), args, reconciliationEnvironment, &output, func(context.Context, commandConfig) (account.ApprovalReconciliationSummary, error) {
			t.Fatal("invalid invocation reached action")
			return account.ApprovalReconciliationSummary{}, nil
		}); code != 2 {
			t.Fatalf("args=%v code=%d output=%s", args, code, output.String())
		}
	}
	var output bytes.Buffer
	called := false
	code := run(context.Background(), nil, reconciliationEnvironment, &output, func(_ context.Context, config commandConfig) (account.ApprovalReconciliationSummary, error) {
		called = true
		if config.Apply || config.Limit != 100 {
			t.Fatalf("config=%+v", config)
		}
		return account.ApprovalReconciliationSummary{DryRun: true, CompleteWithoutBlocker: true}, nil
	})
	var result commandResult
	if code != 0 || !called || json.Unmarshal(output.Bytes(), &result) != nil || result.Status != "planned" || result.Reason != "DRY_RUN_ONLY" {
		t.Fatalf("code=%d called=%v output=%s", code, called, output.String())
	}
	output.Reset()
	code = run(context.Background(), []string{"--apply", "--confirm", applyConfirmation}, reconciliationEnvironment, &output,
		func(_ context.Context, config commandConfig) (account.ApprovalReconciliationSummary, error) {
			if !config.Apply {
				t.Fatal("confirmed run was not apply")
			}
			return account.ApprovalReconciliationSummary{CompleteWithoutBlocker: true}, nil
		})
	if code != 0 || json.Unmarshal(output.Bytes(), &result) != nil || result.Status != "complete" {
		t.Fatalf("code=%d output=%s", code, output.String())
	}
}

func TestRunReportsClosedBlockedAndConfigurationStates(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), nil, func(string) string { return "" }, &output, nil); code != 2 {
		t.Fatalf("missing configuration code=%d output=%s", code, output.String())
	}
	output.Reset()
	if code := run(context.Background(), nil, func(key string) string {
		if key == "WORKLOAD_IDENTITY_TOKEN_FILE" {
			return "relative-token"
		}
		return reconciliationEnvironment(key)
	}, &output, nil); code != 2 {
		t.Fatalf("relative credential code=%d output=%s", code, output.String())
	}
	output.Reset()
	code := run(context.Background(), nil, reconciliationEnvironment, &output, func(context.Context, commandConfig) (account.ApprovalReconciliationSummary, error) {
		return account.ApprovalReconciliationSummary{UnknownEffectCount: 1}, nil
	})
	var result commandResult
	if code != 1 || json.Unmarshal(output.Bytes(), &result) != nil || result.Status != "blocked" || result.Summary == nil || result.Summary.UnknownEffectCount != 1 {
		t.Fatalf("blocked code=%d output=%s", code, output.String())
	}
	output.Reset()
	code = run(context.Background(), nil, reconciliationEnvironment, &output, func(context.Context, commandConfig) (account.ApprovalReconciliationSummary, error) {
		return account.ApprovalReconciliationSummary{}, account.ErrApprovalReconciliationControlChanged
	})
	if code != 1 || json.Unmarshal(output.Bytes(), &result) != nil || result.Reason != "CONTROL_CHANGED" {
		t.Fatalf("control code=%d output=%s", code, output.String())
	}
}
