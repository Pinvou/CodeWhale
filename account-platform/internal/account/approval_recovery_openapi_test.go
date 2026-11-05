package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
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

func TestApprovalRecoveryOpenAPIActualDTO(t *testing.T) {
	now := time.Now().UTC()
	descriptor := approvalconsumer.BillingCommands()[0]
	c := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalKey: "original-only-key", OriginalEventID: uuid.New(), Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType, CommandFingerprint: "a100000000000000000000000000000000000000000000000000000000000000", PolicyVersion: descriptor.PolicyVersion, ApprovalEpoch: 1, RequestVersion: 1, RequestStatus: "EXECUTION_FAILED", ExecutionStatus: "FAILED", ExecutionDeadline: now, WindowElapsed: true, Delivery: ApprovalDeliveryState{DomainAttemptCount: 1, State: "FAILED", Version: 1}, Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
	lease := RecoveryQueryLease{RequestID: c.RequestID, Token: uuid.New(), Until: now.Add(time.Second), QueryCount: 1, Version: 1}
	confirmation := approvalRecoveryConfirmation{Candidate: c, Lease: lease}
	t.Run("closed-original-candidate-and-lease", func(t *testing.T) { recoveryContractValidate(t, "account", "Confirmation", confirmation, true) })
	t.Run("actual-plan-and-control", func(t *testing.T) {
		control := approvalRecoveryControlView{ControlID: "ADMIN_APPROVAL", SchemaVersion: 1, Mode: AdminApprovalModeProofRequired, ApprovalWireVersion: 2, ProofRequired: true, AuthorizationRelayState: AdminApprovalRelayRunning, ApprovalEpoch: 1, AuthorizationRelayEpoch: 1, Version: 1, UpdatedAt: now}
		recoveryContractValidate(t, "account", "Plan", approvalRecoveryPlanView{SchemaVersion: 1, ObservedAt: now, Control: control, Candidates: []ApprovalRecoveryCandidate{c}}, true)
		control.ApprovalWireVersion = 1
		recoveryContractValidate(t, "account", "Control", control, false)
	})
	t.Run("worker-verdict-and-foreign-fields-refused", func(t *testing.T) {
		raw, _ := json.Marshal(confirmation)
		var changed map[string]any
		json.Unmarshal(raw, &changed)
		changed["zero_effect_proven"] = true
		recoveryContractValidate(t, "account", "Confirmation", changed, false)
		delete(changed, "zero_effect_proven")
		changed["candidate"].(map[string]any)["verdict"] = "ABSENT"
		recoveryContractValidate(t, "account", "Confirmation", changed, false)
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
