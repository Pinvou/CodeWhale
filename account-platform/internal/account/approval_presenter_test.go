package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalPresenterClosedSixCommands(t *testing.T) {
	data, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ProofVectors []struct {
			CommandType           string `json:"command_type"`
			PolicyVersion         int64  `json:"policy_version"`
			SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
			Legacy                struct {
				TargetResourceType string `json:"target_resource_type"`
				TargetResourceID   string `json:"target_resource_id"`
				NormalizedJSON     string `json:"normalized_json"`
				CriticalJSON       string `json:"critical_json"`
				FinancialJSON      string `json:"financial_json"`
			} `json:"legacy"`
		} `json:"proof_vectors"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.ProofVectors) != 6 {
		t.Fatalf("expected six golden commands, got %d", len(fixture.ProofVectors))
	}
	for _, vector := range fixture.ProofVectors {
		t.Run(vector.CommandType, func(t *testing.T) {
			item := ApprovalRequest{ID: uuid.New(), CommandType: vector.CommandType, PolicyVersion: vector.PolicyVersion,
				SnapshotSchemaVersion: vector.SnapshotSchemaVersion, TargetResourceType: vector.Legacy.TargetResourceType,
				TargetResourceID: vector.Legacy.TargetResourceID, RequesterUserID: uuid.New(), ExpiresAt: time.Now(), UpdatedAt: time.Now(),
				NormalizedCommand:        json.RawMessage(vector.Legacy.NormalizedJSON),
				CriticalResourceVersions: json.RawMessage(vector.Legacy.CriticalJSON),
				FinancialSnapshot:        json.RawMessage(vector.Legacy.FinancialJSON),
				AllowedActions:           []string{"APPROVE"},
			}
			projection, err := approvalV2Detail(item, 5)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(projection)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(encoded, []byte("critical_resource_versions")) || bytes.Contains(encoded, []byte("normalized_command")) || !bytes.Contains(encoded, []byte(`"action_control_version":5`)) {
				t.Fatalf("unexpected projection: %s", encoded)
			}
			listed, err := json.Marshal(approvalV2Summary(item))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(listed, []byte("allowed_actions")) || bytes.Contains(listed, []byte("critical_resource_versions")) {
				t.Fatalf("list leaked detail fields: %s", listed)
			}
			item.PolicyVersion++
			if approvalV2Summary(item).DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || !errors.Is(func() error { _, err := approvalV2Detail(item, 5); return err }(), ErrApprovalDetailPresentationUnavailable) {
				t.Fatal("unknown historical version must not fall back to raw JSON")
			}
			item.PolicyVersion--
			item.NormalizedCommand = json.RawMessage(`{"secret":"leak-marker"}`)
			if _, err := approvalV2Detail(item, 5); !errors.Is(err, ErrApprovalDetailPresentationUnavailable) {
				t.Fatalf("malformed history accepted: %v", err)
			}
		})
	}
}

func TestApprovalPresenterRejectsUnsafeHistoricalTargets(t *testing.T) {
	for _, target := range []string{strings.Repeat("x", 161), "https://internal.example/secret", "person@example.com", "white space"} {
		item := ApprovalRequest{CommandType: "PAYMENT_PROVIDER_RESUME", PolicyVersion: 2, SnapshotSchemaVersion: 1,
			TargetResourceType: "PAYMENT_PROVIDER_CONTROL", TargetResourceID: target}
		if summary := approvalV2Summary(item); summary.Target.ResourceID != "" || summary.DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" {
			t.Fatalf("unsafe target should be unavailable: %+v", summary)
		}
		if _, err := approvalV2Detail(item, 1); !errors.Is(err, ErrApprovalDetailPresentationUnavailable) {
			t.Fatal("unsafe target detail must fail closed")
		}
	}
	item := ApprovalRequest{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, SnapshotSchemaVersion: 2,
		TargetResourceType: "MODEL_CATALOG", TargetResourceID: "9223372036854775808"}
	if summary := approvalV2Summary(item); summary.DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || summary.Target.ResourceID != "" {
		t.Fatal("out of range version must not be presented")
	}
	item = ApprovalRequest{CommandType: "FUTURE_COMMAND", PolicyVersion: 9, SnapshotSchemaVersion: 9,
		TargetResourceType: "PRIVATE", TargetResourceID: "private-resource"}
	if summary := approvalV2Summary(item); summary.Target != (ApprovalV2Target{}) {
		t.Fatalf("unknown historical command leaked target: %+v", summary)
	}
}

func TestApprovalDetailActionsFollowControl(t *testing.T) {
	control := AdminApprovalControl{ControlID: adminApprovalControlID, SchemaVersion: 1,
		Mode: AdminApprovalModeLegacy, ApprovalWireVersion: 1, AuthorizationRelayState: AdminApprovalRelayRunning,
		ApprovalEpoch: 1, AuthorizationRelayEpoch: 1, Version: 1, UpdatedAt: time.Now()}
	all := []string{"EDIT", "SUBMIT", "APPROVE", "REJECT", "WITHDRAW"}
	actions, err := approvalActionsForControl(all, control)
	if err != nil || len(actions) != len(all) {
		t.Fatalf("legacy actions=%v err=%v", actions, err)
	}
	control.AuthorizationRelayState = AdminApprovalRelayFenced
	actions, err = approvalActionsForControl(all, control)
	if err != nil || slices.Contains(actions, "APPROVE") {
		t.Fatalf("fenced actions=%v err=%v", actions, err)
	}
	control.Mode = AdminApprovalModeSafeRollback
	control.ProofRequired = true
	actions, err = approvalActionsForControl(all, control)
	if err != nil || len(actions) != 2 || actions[0] != "REJECT" || actions[1] != "WITHDRAW" {
		t.Fatalf("safe actions=%v err=%v", actions, err)
	}
}
