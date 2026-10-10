package billing

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestF16UsageFinancialActivationRacePostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned Billing fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	r, _ := newAdminPaginationIntegrationRepository(t, ctx, raw)
	set := usageRepairFixtureActivation(t, ctx, r.pool)
	v := set("ACTIVE")
	f := seedUnsettledUsageLoss(t, ctx, r, "activation-race", 50, 100, 100)
	in := usageRepairInput(t, ctx, r, f, v)
	in.ExecutionKey = "race:usage-settlement-repair"
	before := usageRepairEffectSnapshot(t, ctx, r)
	set("INACTIVE")
	if _, err := r.ApplyUsageSettlementRepair(ctx, in); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
		t.Fatal("inactive fresh execution changed facts")
	}
	v = set("ACTIVE")
	if _, err := r.ApplyUsageSettlementRepair(ctx, in); !errors.Is(err, ErrApprovalSnapshotMismatch) || usageRepairEffectSnapshot(t, ctx, r) != before {
		t.Fatal("stale activation version changed facts")
	}
	in = usageRepairInput(t, ctx, r, f, v)
	in.ExecutionKey = "race:usage-settlement-repair"
	result, err := usageFinancialWithDeactivationRace(t, ctx, r, in)
	if err != nil || result.Status != "SUCCEEDED" || result.SettledMicrocredits != 50 {
		t.Fatal("legitimate race failed")
	}
	var available, used, remaining, posted int64
	if err := r.pool.QueryRow(ctx, `SELECT c.available_microcredits,c.used_microcredits,l.remaining_microcredits,w.posted_microcredits FROM entitlement_components c JOIN credit_lots l ON l.id=c.credit_lot_id JOIN wallets w ON w.id=l.wallet_id WHERE c.id=$1`, f.ComponentID).Scan(&available, &used, &remaining, &posted); err != nil || available != 50 || used != 50 || remaining != 50 || posted != 50 {
		t.Fatal("actual Usage settlement financial projection differs")
	}
}
