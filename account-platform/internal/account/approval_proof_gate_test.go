package account

import (
	"errors"
	"testing"
)

func TestApprovalProofGateEightTuples(t *testing.T) {
	for _, sample := range []struct {
		mode  AdminApprovalMode
		relay AdminApprovalRelayState
		want  error
	}{
		{AdminApprovalModeLegacy, AdminApprovalRelayRunning, nil},
		{AdminApprovalModeLegacy, AdminApprovalRelayFenced, ErrApprovalRelayFenced},
		{AdminApprovalModeQuiescing, AdminApprovalRelayRunning, ErrApprovalQuiescing},
		{AdminApprovalModeQuiescing, AdminApprovalRelayFenced, ErrApprovalQuiescing},
		{AdminApprovalModeProofRequired, AdminApprovalRelayRunning, nil},
		{AdminApprovalModeProofRequired, AdminApprovalRelayFenced, ErrApprovalRelayFenced},
		{AdminApprovalModeSafeRollback, AdminApprovalRelayRunning, ErrApprovalSafeRollback},
		{AdminApprovalModeSafeRollback, AdminApprovalRelayFenced, ErrApprovalSafeRollback},
	} {
		control := validAdminApprovalControl(sample.mode, sample.relay)
		if got := approvalApproveGateError(control); !errors.Is(got, sample.want) || (sample.want == nil && got != nil) {
			t.Fatalf("%s/%s: %v, expected %v", sample.mode, sample.relay, got, sample.want)
		}
	}
}
