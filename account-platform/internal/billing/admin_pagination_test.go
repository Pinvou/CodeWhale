package billing

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAdminPageLimitIsStrictAndBounded(t *testing.T) {
	for _, item := range []struct {
		raw  string
		want int
		err  bool
	}{{"", 50, false}, {"1", 1, false}, {"200", 200, false}, {"0", 0, true}, {"201", 0, true}, {"1.5", 0, true}} {
		got, err := parseAdminPageLimit(item.raw)
		if item.err != (err != nil) || (!item.err && got != item.want) {
			t.Fatalf("parseAdminPageLimit(%q) = %d, %v; want %d, err=%v", item.raw, got, err, item.want, item.err)
		}
	}
}

func TestAdminCursorIsDeterministicOpaqueAndRejectsTampering(t *testing.T) {
	snapshotID := uuid.New()
	secret := []byte("0123456789abcdef0123456789abcdef")
	first, err := signedAdminCursor(snapshotID, 200, secret)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := signedAdminCursor(snapshotID, 200, secret)
	if err != nil {
		t.Fatal(err)
	}
	if first != replayed || len(first) != 75 {
		t.Fatalf("cursor replay is not deterministic or has an unexpected size")
	}
	payload, err := parseAdminCursor(first)
	if err != nil {
		t.Fatal(err)
	}
	if payload.SnapshotID != snapshotID || payload.Position != 200 || !validAdminCursorSignature(payload, secret) {
		t.Fatalf("signed cursor payload was not recovered and verified")
	}
	for name, cursor := range map[string]string{
		"position": mustAdminCursor(t, snapshotID, 201, secret),
		"snapshot": mustAdminCursor(t, uuid.New(), 200, secret),
		"secret":   mustAdminCursor(t, snapshotID, 200, []byte("abcdef0123456789abcdef0123456789")),
	} {
		if cursor == first {
			t.Fatalf("%s change did not change the cursor", name)
		}
	}
	tampered, err := parseAdminCursor(first)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Signature[0] ^= 0xff
	if validAdminCursorSignature(tampered, secret) {
		t.Fatal("tampered cursor signature was accepted")
	}
	for _, invalid := range []string{"", "plain-text-position", first + "x", first[:len(first)-1]} {
		if _, err := parseAdminCursor(invalid); !errors.Is(err, ErrPaginationCursorInvalid) {
			t.Fatalf("cursor %q was not rejected: %v", invalid, err)
		}
	}
}

func mustAdminCursor(t *testing.T, snapshotID uuid.UUID, position int, secret []byte) string {
	t.Helper()
	cursor, err := signedAdminCursor(snapshotID, position, secret)
	if err != nil {
		t.Fatal(err)
	}
	return cursor
}

func TestAdminPaginationRangesAreHalfOpenAndBounded(t *testing.T) {
	filters := map[string]string{"created_from": "2026-01-01T00:00:00Z", "created_to": "2026-01-02T00:00:00Z"}
	if !validAdminTimeRange(filters, "created_from", "created_to") {
		t.Fatal("valid half-open time range was rejected")
	}
	filters["created_to"] = filters["created_from"]
	if validAdminTimeRange(filters, "created_from", "created_to") {
		t.Fatal("empty time range was accepted")
	}
	filters["created_to"] = "2027-01-03T00:00:00Z"
	if validAdminTimeRange(filters, "created_from", "created_to") {
		t.Fatal("time range longer than 366 days was accepted")
	}
	dateFilters := map[string]string{"from": "2026-01-01", "to": "2026-01-02"}
	if !validAdminDateRange(dateFilters, "from", "to") {
		t.Fatal("valid half-open date range was rejected")
	}
}

func TestAdminSnapshotQueriesFreezeEveryResourceByDeterministicKey(t *testing.T) {
	resources := []string{
		adminPageWallets, adminPageWalletLedger, adminPageReceivables, adminPagePaymentOrders,
		adminPageRefunds, adminPageAdjustments, adminPageUsageHolds, adminPageLatePaymentReviews,
		adminPagePaymentAnomalies, adminPageReconciliationRuns, adminPageReconciliationCases,
	}
	for _, resource := range resources {
		scope := ""
		if resource == adminPageWalletLedger || resource == adminPageReconciliationCases {
			scope = uuid.NewString()
		}
		query, _, err := adminSnapshotResourceQuery(AdminPaginationRequest{Resource: resource, Scope: scope, Filters: map[string]string{}})
		if err != nil {
			t.Fatalf("%s query failed: %v", resource, err)
		}
		for _, fragment := range []string{"resource_id", "sort_at", "sort_id", "ORDER BY"} {
			if !strings.Contains(query, fragment) {
				t.Fatalf("%s query does not contain %q", resource, fragment)
			}
		}
	}
}

func TestAdminPaginationFilterEnumsCoverCurrentFinancialStates(t *testing.T) {
	entryTypes := adminPaginationFilters[adminPageWalletLedger]["entry_type"].values
	for _, value := range []string{"RECHARGE", "USAGE", "REFUND", "REVERSAL", "ADJUSTMENT", "EXPIRY", "RECEIVABLE", "CHARGEBACK", "REPAIR_CREDIT", "FORCED_REFUND"} {
		if !entryTypes[value] {
			t.Fatalf("ledger entry type %s is missing", value)
		}
	}
	sourceTypes := adminPaginationFilters[adminPageReceivables]["source_type"].values
	for _, value := range []string{"USAGE_OVERAGE", "CHARGEBACK", "FORCED_REFUND"} {
		if !sourceTypes[value] {
			t.Fatalf("receivable source type %s is missing", value)
		}
	}
	caseTypes := adminPaginationFilters[adminPageReconciliationCases]["case_type"].values
	for _, value := range []string{
		"PROVIDER_PAID_INTERNAL_MISSING", "PROVIDER_REFUND_INTERNAL_MISSING", "PROVIDER_CHARGEBACK_INTERNAL_MISSING",
		"PAYMENT_LEDGER_MISSING", "REFUND_LEDGER_MISSING", "CHARGEBACK_LEDGER_MISSING", "UNSUPPORTED_PROVIDER_ENTRY",
	} {
		if !caseTypes[value] {
			t.Fatalf("reconciliation case type %s is missing", value)
		}
	}
	if !adminFilterIdentifier.MatchString("WECHAT_NATIVE") || adminFilterIdentifier.MatchString("wechat-native") {
		t.Fatal("channel identifier validation is inconsistent")
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(366 * 24 * time.Hour)
	if !validAdminTimeRange(map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)}, "from", "to") {
		t.Fatal("the documented maximum time range was rejected")
	}
}

func TestPaymentAnomalyProviderFilterUsesPaymentOrderProjection(t *testing.T) {
	query, _, err := adminSnapshotResourceQuery(AdminPaginationRequest{Resource: adminPagePaymentAnomalies, Filters: map[string]string{"provider_id": "h3c_mall"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "payment.provider_id=$7") || strings.Contains(query, "review.provider_id") {
		t.Fatalf("payment anomaly provider filter is not bound to payment_orders: %s", query)
	}
}
