package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
)

func TestCommandRejectsArgumentsBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{{"--batch=0"}, {"--batch=201"}, {"--deadline=0"}, {"--deadline=31m"}, {"postgres://secret"}, {"--password=secret"}} {
		var output bytes.Buffer
		code := run(context.Background(), args, func(string) string { t.Fatal("read credentials for invalid args"); return "" }, &output, nil)
		if code != 2 || strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("invalid argument result: %d %s", code, &output)
		}
	}
}

func TestCommandUsesOnlyDedicatedCredentialAndBounds(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), []string{"--batch=17", "--deadline=2s"}, func(key string) string {
		if key != "APPROVAL_BACKFILL_DATABASE_URL" {
			t.Fatalf("unexpected credential source %s", key)
		}
		return "private-dsn"
	}, &output, func(ctx context.Context, dsn string, batch int) (account.ApprovalAuthorizationBackfillReadiness, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second || time.Until(deadline) <= 0 || dsn != "private-dsn" || batch != 17 {
			t.Fatal("lost command bounds")
		}
		return account.ApprovalAuthorizationBackfillReadiness{AuthorizationCount: 4}, nil
	})
	if code != 0 || strings.Contains(output.String(), "private") || !strings.Contains(output.String(), `"authorization_count":4`) {
		t.Fatalf("unexpected output: %d %s", code, &output)
	}
}

func TestCommandRedactsFailureAndPreservesCancellation(t *testing.T) {
	for _, item := range []struct {
		err    error
		reason string
	}{{errors.New("postgres://user:secret@host payload private"), "APPROVAL_BACKFILL_UNAVAILABLE"}, {context.Canceled, "CANCELED"}, {context.DeadlineExceeded, "DEADLINE_EXCEEDED"}} {
		var output bytes.Buffer
		code := run(context.Background(), nil, func(string) string { return "secret" }, &output, func(context.Context, string, int) (account.ApprovalAuthorizationBackfillReadiness, error) {
			return account.ApprovalAuthorizationBackfillReadiness{}, item.err
		})
		if code != 1 || strings.Contains(output.String(), "secret") || !strings.Contains(output.String(), item.reason) {
			t.Fatalf("unsafe failure: %d %s", code, &output)
		}
	}
}

func TestCommandMissingCredentialDoesNotFallback(t *testing.T) {
	var output bytes.Buffer
	if code := run(context.Background(), nil, func(string) string { return "" }, &output, nil); code != 2 || !strings.Contains(output.String(), "BACKFILL_CREDENTIAL_REQUIRED") {
		t.Fatalf("missing credential: %d %s", code, &output)
	}
}

func TestCommandIncompleteReadinessIsNotSuccess(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), nil, func(string) string { return "private" }, &output, func(context.Context, string, int) (account.ApprovalAuthorizationBackfillReadiness, error) {
		return account.ApprovalAuthorizationBackfillReadiness{MissingSidecarCount: 1}, nil
	})
	if code != 1 || !strings.Contains(output.String(), "AUTHORIZATION_VALIDATION_REQUIRED") {
		t.Fatalf("incomplete backfill: %d %s", code, &output)
	}
}

func TestCommandDoesNotTrustForeignBlockerErrors(t *testing.T) {
	var output bytes.Buffer
	code := run(context.Background(), nil, func(string) string { return "private" }, &output, func(context.Context, string, int) (account.ApprovalAuthorizationBackfillReadiness, error) {
		return account.ApprovalAuthorizationBackfillReadiness{}, fmt.Errorf("private outer SQL: %w", account.ErrApprovalBackfillUnavailable)
	})
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil || code != 1 || decoded["blocker"] != nil || strings.Contains(output.String(), "private") {
		t.Fatalf("unsafe blocker output: %d %s", code, &output)
	}
}
