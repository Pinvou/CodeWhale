package account

import (
	"encoding/json"
	"testing"
)

// The owner precheck's real closed wire fixture is reused; no PG owner effect
// or approval activation is simulated by the typed presentation test.
func TestRetainedEntitlementTypedPresenter(t *testing.T) {
	var wire precheckWireResponse
	if err := json.Unmarshal([]byte(entitlementPrecheckOwnerFixture), &wire); err != nil {
		t.Fatal(err)
	}
	item := ApprovalRequest{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion,
		SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType,
		TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand,
		CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot}
	t.Run("installed-v2-exact-detail", func(t *testing.T) {
		if approvalV2Summary(item).DetailAvailability != "AVAILABLE" {
			t.Fatal("installed entitlement v2 summary is unavailable")
		}
		detail, err := approvalV2Detail(item, 9)
		if err != nil {
			t.Fatal(err)
		}
		command, ok := detail.Command.(approvalEntitlementCorrectionSummary)
		if !ok || command.PackageID.String() != wire.TargetResourceID || command.PaymentOrderID.String() != "00000000-0000-4000-8000-000000000002" || command.ReasonCode != "PRODUCT_SPLIT_CORRECTION" || detail.ActionControlVersion != 9 {
			t.Fatalf("typed command lost immutable precheck facts: %#v", detail.Command)
		}
	})
	t.Run("historical-v1-unavailable", func(t *testing.T) {
		old := item
		old.PolicyVersion, old.SnapshotSchemaVersion = 1, 1
		if approvalV2Summary(old).DetailAvailability != "DETAIL_PRESENTATION_UNAVAILABLE" || approvalV2Command(old) != nil {
			t.Fatal("obsolete entitlement schema became an installed presenter")
		}
	})
	t.Run("foreign-target-and-future-deny", func(t *testing.T) {
		wrong := item
		wrong.TargetResourceType = "LEDGER_POSTING"
		if approvalV2Command(wrong) != nil {
			t.Fatal("foreign target type accepted")
		}
		if approvalPresenterAvailable("RECEIVABLE_WRITE_OFF", 1, 1) {
			t.Fatal("future command presenter installed")
		}
	})
}
