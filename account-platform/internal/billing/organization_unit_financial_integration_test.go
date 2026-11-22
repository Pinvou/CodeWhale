package billing

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestOrganizationUnitFinancialDeletionBoundaries(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	newUnit := func(t *testing.T) (uuid.UUID, uuid.UUID, Wallet) {
		t.Helper()
		org, unit := uuid.New(), uuid.New()
		if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 1); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		wallet, created, err := r.EnsureWallet(ctx, "ORG_UNIT", unit)
		if err != nil || !created || wallet.Status != "ACTIVE" {
			t.Fatal(wallet, created, err)
		}
		return org, unit, wallet
	}
	hasBlocker := func(a OrganizationUnitDeletionAssessment, code string) bool {
		for _, b := range a.Blockers {
			if b.Code == code {
				return true
			}
		}
		return false
	}
	t.Run("actual-posted-funds-and-entitlements-prevent-deletion", func(t *testing.T) {
		org, unit, wallet := newUnit(t)
		adjustment, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 100, "controlled unit balance", uuid.New(), uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.ExecuteApprovedAdjustment(ctx, adjustment.ID, uuid.New(), uuid.NewString(), uuid.New(), "controlled independent approval"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil {
			t.Fatal(err)
		}
		a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
		if err != nil || a.Ready || !hasBlocker(a, "WALLET_BALANCE_NOT_ZERO") || !hasBlocker(a, "EFFECTIVE_ENTITLEMENT_REMAINS") {
			t.Fatal(a, err)
		}
		got, err := r.GetWallet(ctx, wallet.ID)
		if err != nil || got.Status != "ACTIVE" || got.PostedMicrocredits != 100 {
			t.Fatal("funds were altered during assessment", got, err)
		}
		var status string
		var version int64
		if err := r.pool.QueryRow(ctx, `SELECT status,structure_version FROM organization_unit_lifecycle_states WHERE organization_id=$1 AND unit_id=$2`, org, unit).Scan(&status, &version); err != nil || status != "DELETION_PENDING" || version != 1 {
			t.Fatal(status, version, err)
		}
	})
	t.Run("pending-adjustment-prevents-deletion-with-zero-posted-balance", func(t *testing.T) {
		org, unit, wallet := newUnit(t)
		if _, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 100, "controlled pending unit adjustment", uuid.New(), uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil {
			t.Fatal(err)
		}
		a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
		if err != nil || a.Ready || !hasBlocker(a, "PENDING_FINANCIAL_ADJUSTMENT") {
			t.Fatal(a, err)
		}
	})
	t.Run("zero-wallet-closes-atomically-and-never-accepts-fresh-funds", func(t *testing.T) {
		org, unit, wallet := newUnit(t)
		if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil || !a.Ready {
			t.Fatal(a, err)
		}
		for n := 0; n < 2; n++ {
			if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2); err != nil || !a.Ready {
				t.Fatal(a, err)
			}
		}
		got, err := r.GetWallet(ctx, wallet.ID)
		if err != nil || got.Status != "CLOSED" || got.PostedMicrocredits != 0 || got.Version != wallet.Version+1 {
			t.Fatal(got, err)
		}
		if _, _, err := r.EnsureWallet(ctx, "ORG_UNIT", unit); !errors.Is(err, ErrWalletUnavailable) {
			t.Fatal("terminal ensure accepted", err)
		}
		if _, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 100, "must not fund terminal unit", uuid.New(), uuid.NewString()); !errors.Is(err, ErrWalletUnavailable) {
			t.Fatal(err)
		}
		if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "ACTIVE", 3); !errors.Is(err, ErrConflict) {
			t.Fatal("terminal unit reactivated", err)
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM manual_adjustments WHERE wallet_id=$1`, wallet.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal(count, err)
		}
	})
	t.Run("terminal-unit-cannot-acquire-a-new-wallet", func(t *testing.T) {
		org, unit := uuid.New(), uuid.New()
		for _, next := range []struct {
			status  string
			version int64
		}{{"ACTIVE", 1}, {"DELETION_PENDING", 1}, {"DELETED", 2}} {
			if a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, next.status, next.version); err != nil || !a.Ready {
				t.Fatal(a, err)
			}
		}
		if _, _, err := r.EnsureWallet(ctx, "ORG_UNIT", unit); !errors.Is(err, ErrWalletUnavailable) {
			t.Fatal(err)
		}
	})
	t.Run("concurrent-pending-adjustment-and-terminal-transition", func(t *testing.T) {
		for n := 0; n < 6; n++ {
			org, unit, wallet := newUnit(t)
			if _, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETION_PENDING", 1); err != nil {
				t.Fatal(err)
			}
			start := make(chan struct{})
			adjustmentResult := make(chan error, 1)
			type result struct {
				assessment OrganizationUnitDeletionAssessment
				err        error
			}
			deletionResult := make(chan result, 1)
			go func() {
				<-start
				_, err := r.RequestAdjustmentWithIdempotency(ctx, wallet.ID, 100, "concurrent controlled adjustment", uuid.New(), uuid.NewString())
				adjustmentResult <- err
			}()
			go func() {
				<-start
				a, err := r.ApplyOrganizationUnitLifecycle(ctx, org, unit, "DELETED", 2)
				deletionResult <- result{a, err}
			}()
			close(start)
			adjustmentErr, deletion := <-adjustmentResult, <-deletionResult
			if deletion.err != nil {
				t.Fatal(deletion.err)
			}
			if deletion.assessment.Ready {
				if !errors.Is(adjustmentErr, ErrWalletUnavailable) {
					t.Fatal("terminal transition accepted concurrent pending funds", adjustmentErr)
				}
			} else if adjustmentErr != nil || !hasBlocker(deletion.assessment, "PENDING_FINANCIAL_ADJUSTMENT") {
				t.Fatal(adjustmentErr, deletion.assessment)
			}
		}
	})
}
