package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func recoveryContractProcessAccepted(ctx context.Context, command *exec.Cmd, want bool) bool {
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		return false
	}
	if want {
		return err == nil && stdout.String() == "VALID\n" && stderr.Len() == 0
	}
	var exited *exec.ExitError
	return errors.As(err, &exited) && exited.ExitCode() == 1 && stdout.Len() == 0 && stderr.String() == "INVALID_APPROVAL_RECOVERY_WIRE\n"
}
func recoveryContractValidate(t *testing.T, service, name string, value any, want bool) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join("..", "..", "test", "fixtures", "approval-recovery-validator.mjs"), service, name)
	command.Stdin = bytes.NewReader(raw)
	if !recoveryContractProcessAccepted(ctx, command, want) {
		t.Fatal("contract validator failed exact exit/output protocol (payload suppressed)")
	}
}

func TestTokenRecoveryOpenAPIActualDTO(t *testing.T) {
	now := time.Now().UTC()
	id, reference := uuid.New(), uuid.New()
	t.Run("positive-reference-required", func(t *testing.T) {
		result := tokenRecoveryResult{SchemaVersion: 1, ApprovalRequestID: id, CommandType: "TOKEN_ACCOUNT_CREATE", CommandFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OriginalKeyDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: "SUCCEEDED", CommandReference: &reference, ObservedAt: now}
		recoveryContractValidate(t, "token", "Result", result, true)
		result.CommandReference = nil
		recoveryContractValidate(t, "token", "Result", result, false)
	})
	t.Run("unknown-does-not-carry-confirmed-reference", func(t *testing.T) {
		result := tokenRecoveryResult{SchemaVersion: 1, ApprovalRequestID: id, CommandType: "TOKEN_ACCOUNT_CREATE", CommandFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", OriginalKeyDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", State: "UNKNOWN", ObservedAt: now}
		recoveryContractValidate(t, "token", "Result", result, true)
		result.CommandReference = &reference
		recoveryContractValidate(t, "token", "Result", result, false)
	})
	t.Run("actual-effect-observation", func(t *testing.T) {
		recoveryContractValidate(t, "token", "Effect", approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityToken, ApprovalRequestID: id, CommandType: "TOKEN_ACCOUNT_CREATE", CommandFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", State: approvaleffect.StateAbsent, ObservedAt: now}, true)
	})
}

func TestRecoveryContractToolFailuresCannotPass(t *testing.T) {
	for _, tc := range []struct {
		name, script   string
		want, accepted bool
	}{
		{"valid-exact", "process.stdout.write('VALID\\n')", true, true},
		{"business-rejection-exact", "process.stderr.write('INVALID_APPROVAL_RECOVERY_WIRE\\n');process.exitCode=1", false, true},
		{"wrong-exit-code", "process.stderr.write('INVALID_APPROVAL_RECOVERY_WIRE\\n');process.exitCode=2", false, false},
		{"runtime-error", "throw new Error('controlled-validator-failure')", false, false},
		{"missing-module", "require('pinvou-absent-contract-validator')", false, false},
		{"wrong-marker", "process.stderr.write('other\\n');process.exitCode=1", false, false},
		{"unexpected-stdout", "process.stdout.write('unexpected');process.stderr.write('INVALID_APPROVAL_RECOVERY_WIRE\\n');process.exitCode=1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if got := recoveryContractProcessAccepted(ctx, exec.CommandContext(ctx, "node", "-e", tc.script), tc.want); got != tc.accepted {
				t.Fatal("tool failure became business verdict", got)
			}
		})
	}
	t.Run("start-error", func(t *testing.T) {
		ctx := context.Background()
		if recoveryContractProcessAccepted(ctx, exec.CommandContext(ctx, filepath.Join(t.TempDir(), "absent-validator")), false) {
			t.Fatal("start failure passed")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		if recoveryContractProcessAccepted(ctx, exec.CommandContext(ctx, "node", "-e", "setTimeout(()=>{},10000)"), false) {
			t.Fatal("timeout passed")
		}
	})
	t.Run("cancelled-context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if recoveryContractProcessAccepted(ctx, exec.CommandContext(ctx, "node", "-e", "process.stderr.write('INVALID_APPROVAL_RECOVERY_WIRE\\n');process.exitCode=1"), false) {
			t.Fatal("cancel passed")
		}
	})
}
