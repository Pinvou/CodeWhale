package account

import (
	"errors"
	"testing"
	"time"
)

func validAdminApprovalControl(mode AdminApprovalMode, relay AdminApprovalRelayState) AdminApprovalControl {
	control := AdminApprovalControl{
		ControlID: adminApprovalControlID, SchemaVersion: 1, Mode: mode,
		AuthorizationRelayState: relay, ApprovalEpoch: 1,
		AuthorizationRelayEpoch: 1, Version: 1, UpdatedAt: time.Unix(1, 0).UTC(),
	}
	switch mode {
	case AdminApprovalModeProofRequired:
		control.ApprovalWireVersion, control.ProofRequired = 2, true
	case AdminApprovalModeSafeRollback:
		control.ApprovalWireVersion, control.ProofRequired = 1, true
	default:
		control.ApprovalWireVersion = 1
	}
	return control
}

func TestAdminApprovalActionsForFrozenMatrix(t *testing.T) {
	tests := []struct {
		name  string
		mode  AdminApprovalMode
		relay AdminApprovalRelayState
		want  AdminApprovalActions
	}{
		{"legacy running", AdminApprovalModeLegacy, AdminApprovalRelayRunning, AdminApprovalActions{true, true, AdminApprovalBeginLegacyPublished, true, true, AdminApprovalRelayNormal}},
		{"legacy fenced", AdminApprovalModeLegacy, AdminApprovalRelayFenced, AdminApprovalActions{true, false, AdminApprovalBeginLegacyPublished, true, true, AdminApprovalRelayDisabled}},
		{"quiescing running", AdminApprovalModeQuiescing, AdminApprovalRelayRunning, AdminApprovalActions{true, false, AdminApprovalBeginLegacyPreFence, true, true, AdminApprovalRelayLegacyDrain}},
		{"quiescing fenced", AdminApprovalModeQuiescing, AdminApprovalRelayFenced, AdminApprovalActions{true, false, AdminApprovalBeginExistingOnly, true, true, AdminApprovalRelayDisabled}},
		{"proof required running", AdminApprovalModeProofRequired, AdminApprovalRelayRunning, AdminApprovalActions{true, true, AdminApprovalBeginProofBound, true, true, AdminApprovalRelayNormal}},
		{"proof required fenced", AdminApprovalModeProofRequired, AdminApprovalRelayFenced, AdminApprovalActions{true, false, AdminApprovalBeginProofBound, true, true, AdminApprovalRelayDisabled}},
		{"safe rollback running", AdminApprovalModeSafeRollback, AdminApprovalRelayRunning, AdminApprovalActions{false, false, AdminApprovalBeginDisabled, true, true, AdminApprovalRelaySafeDrain}},
		{"safe rollback fenced", AdminApprovalModeSafeRollback, AdminApprovalRelayFenced, AdminApprovalActions{false, false, AdminApprovalBeginDisabled, true, true, AdminApprovalRelayDisabled}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := AdminApprovalActionsFor(validAdminApprovalControl(test.mode, test.relay))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("actions = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestAdminApprovalControlValidationFailsClosed(t *testing.T) {
	valid := validAdminApprovalControl(AdminApprovalModeLegacy, AdminApprovalRelayRunning)
	tests := []struct {
		name   string
		mutate func(*AdminApprovalControl)
	}{
		{"identity", func(value *AdminApprovalControl) { value.ControlID = "OTHER" }},
		{"schema", func(value *AdminApprovalControl) { value.SchemaVersion = 2 }},
		{"mode", func(value *AdminApprovalControl) { value.Mode = "UNKNOWN" }},
		{"wire", func(value *AdminApprovalControl) { value.ApprovalWireVersion = 2 }},
		{"proof", func(value *AdminApprovalControl) { value.ProofRequired = true }},
		{"relay", func(value *AdminApprovalControl) { value.AuthorizationRelayState = "UNKNOWN" }},
		{"approval epoch", func(value *AdminApprovalControl) { value.ApprovalEpoch = 0 }},
		{"relay epoch", func(value *AdminApprovalControl) { value.AuthorizationRelayEpoch = 0 }},
		{"version", func(value *AdminApprovalControl) { value.Version = 0 }},
		{"timestamp", func(value *AdminApprovalControl) { value.UpdatedAt = time.Time{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if _, err := AdminApprovalActionsFor(candidate); !errors.Is(err, ErrAdminApprovalControlInvalid) {
				t.Fatalf("error = %v, want ErrAdminApprovalControlInvalid", err)
			}
		})
	}
}
