package account

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func TestLifecycleForceCancelPresenterWithPersistedOffset(t *testing.T) {
	model, plan, replacement := uuid.New(), uuid.New(), uuid.New()
	// pgx's default binary timestamptz decoder uses time.Unix, which returns
	// time.Local. A persisted instant therefore need not be encoded with Z.
	at := time.Date(2026, 11, 1, 20, 0, 0, 123456000, time.FixedZone("persisted-local", 8*60*60))
	encode := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, force := range []bool{false, true} {
		item := ApprovalRequest{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 3, SnapshotSchemaVersion: 3, TargetResourceType: "MODEL", TargetResourceID: model.String(),
			NormalizedCommand:        encode(map[string]any{"model_id": model, "lifecycle_plan_id": plan, "lifecycle_plan_version": 1, "target_lifecycle": "REMOVED", "effective_at": at, "existing_session_cutoff_at": at, "replacement_model_id": replacement, "force_cancel_inflight": force}),
			CriticalResourceVersions: encode(map[string]any{"model_id": model, "catalog_version": 1, "current_lifecycle": "PUBLISHED", "target_lifecycle": "REMOVED", "lifecycle_plan_version": 1, "effective_at": at.UTC(), "existing_session_cutoff_at": at, "replacement_model_id": replacement, "billing_price_version": 1, "route_set_fingerprint": strings.Repeat("a", 64), "provisioning_set_fingerprint": strings.Repeat("b", 64), "inflight_request_set_fingerprint": strings.Repeat("c", 64), "policy_version": 3, "force_cancel_inflight": force}),
			FinancialSnapshot:        encode(map[string]any{"billing_price_revision": 1, "price_set_fingerprint": strings.Repeat("d", 64), "models_checked": 1, "models_covered": 1, "fully_covered": true, "billing_as_of": at.Add(-time.Minute)})}
		detail, err := approvalV2Detail(item, 1)
		if err != nil {
			t.Fatalf("persisted offset force=%t: %v", force, err)
		}
		command, ok := detail.Command.(approvalLifecycleSummary)
		if !ok || command.ForceCancelInflight == nil || *command.ForceCancelInflight != force || command.EffectiveAt == nil || *command.EffectiveAt != at.UTC().Format(time.RFC3339Nano) {
			t.Fatal("frozen force decision or canonical instant omitted")
		}
		// The wire is still strict: offset strings are accepted only by the
		// legacy domain adapter, never as canonical proof input.
		artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: item.CommandType, PolicyVersion: 3, SnapshotSchemaVersion: 3, TargetResourceType: item.TargetResourceType, TargetResourceID: item.TargetResourceID, NormalizedCommand: item.NormalizedCommand, CriticalResourceVersions: item.CriticalResourceVersions, FinancialSnapshot: item.FinancialSnapshot})
		if err != nil {
			t.Fatal(err)
		}
		bad := strings.Replace(string(artifacts.ProofInputJSON), at.UTC().Format(time.RFC3339Nano), at.Format(time.RFC3339Nano), 1)
		if _, err := approvalencoding.DecodeProofInput(item.CommandType, 3, []byte(bad)); err == nil {
			t.Fatal("noncanonical proof wire accepted")
		}
		draft := ApprovalDraft{NormalizedCommand: item.NormalizedCommand, TargetResourceType: "MODEL", TargetResourceID: model.String(), Reason: "owned exact lifecycle intent", RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`)}
		before, err := approvalDraftOriginalFingerprint(item.CommandType, draft)
		if err != nil {
			t.Fatal(err)
		}
		draft.NormalizedCommand, err = canonicalApprovalCommand(item.CommandType, item.NormalizedCommand)
		if err != nil {
			t.Fatal(err)
		}
		after, err := approvalDraftOriginalFingerprint(item.CommandType, draft)
		if err != nil || before != after {
			t.Fatal("same v3 intent changed idempotency identity after persistence", err)
		}
		if strings.Contains(string(draft.NormalizedCommand), "+08:00") {
			t.Fatal("stored v3 command retains noncanonical time")
		}
	}
}
