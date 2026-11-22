package billing

import (
	"testing"
	"time"
)

func TestApprovalActivationRetainsMandatoryEntitlementHistory(t *testing.T) {
	now := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	legacy := ApprovalCommandActivationSnapshot{SchemaVersion: approvalActivationSchemaVersion}
	entitlementIndex := -1
	for _, command := range billingApprovalActivationOrder {
		spec := billingApprovalActivationSpecs[command]
		item := ApprovalCommandActivation{CommandType: command, PolicyVersion: spec.policyVersion,
			SnapshotSchemaVersion: spec.snapshotSchemaVersion, State: "INACTIVE", Version: 1, UpdatedAt: now}
		if command == "LEDGER_REPAIR" {
			item.State = "ACTIVE"
		}
		if command == entitlementComponentAdjustmentCommand {
			entitlementIndex = len(legacy.Activations)
		}
		legacy.Activations = append(legacy.Activations, item)
	}
	if entitlementIndex < 0 {
		t.Fatal("entitlement history is missing from the installed command set")
	}
	v2 := ApprovalCommandActivation{CommandType: entitlementComponentAdjustmentCommand,
		PolicyVersion: 2, SnapshotSchemaVersion: 2, State: "ACTIVE", Version: 1, UpdatedAt: now}
	appended := legacy
	appended.Activations = append([]ApprovalCommandActivation(nil), legacy.Activations[:entitlementIndex+1]...)
	appended.Activations = append(appended.Activations, v2)
	appended.Activations = append(appended.Activations, legacy.Activations[entitlementIndex+1:]...)
	for name, snapshot := range map[string]ApprovalCommandActivationSnapshot{"legacy-with-active-ledger": legacy, "v1-plus-v2": appended} {
		t.Run(name, func(t *testing.T) {
			if err := validateApprovalCommandActivationSnapshot(snapshot, now); err != nil {
				t.Fatal("supported history was rejected", err)
			}
		})
	}
	for _, name := range []string{"v2-substitutes-v1", "duplicate-v2", "active-v1"} {
		t.Run(name, func(t *testing.T) {
			invalid := legacy
			if name == "duplicate-v2" {
				invalid = appended
			}
			invalid.Activations = append([]ApprovalCommandActivation(nil), invalid.Activations...)
			if name == "active-v1" {
				invalid.Activations[entitlementIndex].State = "ACTIVE"
			} else {
				invalid.Activations[entitlementIndex] = v2
			}
			if err := validateApprovalCommandActivationSnapshot(invalid, now); err == nil {
				t.Fatal("mandatory inactive v1 history was not enforced")
			}
		})
	}
}
