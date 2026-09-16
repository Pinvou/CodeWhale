package account

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestApprovalPresenterClosedSixCommands(t *testing.T) {
	id := uuid.New()
	for _, test := range []struct {
		command, targetType, targetID, key string
		policy                             int64
		snapshot                           int
	}{
		{"WALLET_ADJUSTMENT", "MANUAL_ADJUSTMENT", id.String(), "adjustment_id", 1, 1},
		{"PAYMENT_PROVIDER_RESUME", "PAYMENT_PROVIDER_CONTROL", "provider-one", "provider_id", 2, 1},
		{"PAYMENT_PRODUCT_RESUME", "PAYMENT_PRODUCT_CONTROL", "product-one", "product_id", 1, 1},
		{"TOKEN_MODEL_LIFECYCLE_CHANGE", "MODEL", id.String(), "model_id", 2, 2},
		{"TOKEN_MODEL_CATALOG_ROLLBACK", "MODEL_CATALOG", "123", "source_catalog_version", 2, 2},
		{"TOKEN_CREDENTIAL_REVOKE", "PROVIDER_CREDENTIAL", id.String(), "credential_id", 1, 1},
	} {
		t.Run(test.command, func(t *testing.T) {
			item := ApprovalRequest{ID: uuid.New(), CommandType: test.command, PolicyVersion: test.policy,
				SnapshotSchemaVersion: test.snapshot, TargetResourceType: test.targetType,
				TargetResourceID: test.targetID, RequesterUserID: uuid.New(), ExpiresAt: time.Now(), UpdatedAt: time.Now(),
				NormalizedCommand:        json.RawMessage(`{"secret":"leak-marker"}`),
				CriticalResourceVersions: json.RawMessage(`{"secret":"leak-marker"}`),
				FinancialSnapshot:        json.RawMessage(`{"secret":"leak-marker"}`),
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
			if bytes.Contains(encoded, []byte("leak-marker")) || !bytes.Contains(encoded, []byte(`"`+test.key+`"`)) || !bytes.Contains(encoded, []byte(`"action_control_version":5`)) {
				t.Fatalf("unexpected projection: %s", encoded)
			}
			listed, err := json.Marshal(approvalV2Summary(item))
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(listed, []byte("allowed_actions")) || bytes.Contains(listed, []byte("leak-marker")) || bytes.Contains(listed, []byte("critical_resource_versions")) {
				t.Fatalf("list leaked detail fields: %s", listed)
			}
			item.PolicyVersion++
			if approvalV2Summary(item).DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || !errors.Is(func() error { _, err := approvalV2Detail(item, 5); return err }(), ErrApprovalDetailPresentationUnavailable) {
				t.Fatal("unknown historical version must not fall back to raw JSON")
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
	if approvalV2Summary(item).DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" {
		t.Fatal("out of range version must not be presented")
	}
}
