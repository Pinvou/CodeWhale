package billing

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPersonalZeroBudgetHoldReleasePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	var actor string
	var superuser bool
	if err := r.pool.QueryRow(ctx, `SELECT current_user,rolsuper FROM pg_roles WHERE rolname=current_user`).Scan(&actor, &superuser); err != nil || actor != "billing_service" || superuser {
		t.Fatal("restricted runtime", actor, superuser, err)
	}
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Credit(ctx, wallet.ID, "zero-hold-control", 1000, false); err != nil {
		t.Fatal(err)
	}
	snapshot := func(hold uuid.UUID) string {
		t.Helper()
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$1),
'hold',(SELECT to_jsonb(h) FROM usage_holds h WHERE id=$2),
'allocations',(SELECT jsonb_agg(to_jsonb(a) ORDER BY component_id) FROM usage_hold_allocations a WHERE hold_id=$2),
'components',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c),
'ledger',(SELECT count(*) FROM journal_entries))::text`, wallet.ID, hold).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, maximum := range []int64{0, 10} {
		name := "zero-budget"
		if maximum > 0 {
			name = "positive-budget-control"
		}
		t.Run(name, func(t *testing.T) {
			usage := uuid.New()
			expires := time.Now().UTC().Add(time.Hour)
			hold, err := r.Reserve(ctx, usage, wallet.ID, maximum, nil, expires)
			if err != nil || hold.MaxMicrocredits != maximum || hold.Status != "ACTIVE" {
				t.Fatal("reserve", hold, err)
			}
			var allocations int
			if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_hold_allocations WHERE hold_id=$1`, hold.ID).Scan(&allocations); err != nil || (maximum == 0 && allocations != 0) || (maximum > 0 && allocations == 0) {
				t.Fatal("allocation facts", allocations, err)
			}
			replay, err := r.Reserve(ctx, usage, wallet.ID, maximum, nil, expires)
			if err != nil || replay.ID != hold.ID {
				t.Fatal("reserve replay", replay, err)
			}
			released, err := r.Release(ctx, usage, hold.ID, "trusted pre-dispatch cancellation")
			if err != nil || released.Status != "RELEASED" {
				t.Fatal("legal reservation must release", released, err)
			}
			before := snapshot(hold.ID)
			for n := 0; n < 2; n++ {
				replayed, e := r.Release(ctx, usage, hold.ID, "repeat original release")
				if e != nil || replayed.ID != hold.ID || replayed.Status != "RELEASED" || snapshot(hold.ID) != before {
					t.Fatal("release replay changed financial facts", replayed, e)
				}
			}
			got, err := r.GetWallet(ctx, wallet.ID)
			if err != nil || got.ReservedMicrocredits != 0 || got.AvailableMicrocredits != 1000 {
				t.Fatal("conservation", got, err)
			}
		})
	}
	t.Run("zero-on-empty-wallet-needs-no-entitlement", func(t *testing.T) {
		empty, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		usage := uuid.New()
		h, err := r.Reserve(ctx, usage, empty.ID, 0, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if h, err = r.Release(ctx, usage, h.ID, "free cancellation"); err != nil || h.Status != "RELEASED" {
			t.Fatal(h, err)
		}
	})
	t.Run("positive-budget-missing-allocation-still-conflicts", func(t *testing.T) {
		usage := uuid.New()
		h, err := r.Reserve(ctx, usage, wallet.ID, 10, nil, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// Corrupt only this owned fixture; the domain call still uses billing_service.
		if _, err = r.pool.Exec(ctx, `DELETE FROM usage_hold_allocations WHERE hold_id=$1`, h.ID); err != nil {
			t.Fatal("fixture corruption", err)
		}
		before := snapshot(h.ID)
		if _, err = r.Release(ctx, usage, h.ID, "must fail closed"); !errors.Is(err, ErrConflict) || snapshot(h.ID) != before {
			t.Fatal("lost positive allocation accepted or mutated", err)
		}
	})
	for _, closed := range []bool{false, true} {
		name := "zero-with-open-allocation-rejected"
		if closed {
			name = "zero-with-closed-allocation-rejected"
		}
		t.Run(name, func(t *testing.T) {
			usage := uuid.New()
			h, err := r.Reserve(ctx, usage, wallet.ID, 0, nil, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			var packageID, componentID uuid.UUID
			if err = r.pool.QueryRow(ctx, `SELECT package_id,id FROM entitlement_components WHERE available_microcredits>0 LIMIT 1`).Scan(&packageID, &componentID); err != nil {
				t.Fatal(err)
			}
			released := int64(0)
			if closed {
				released = 1
			}
			if _, err = r.pool.Exec(ctx, `INSERT INTO usage_hold_allocations(hold_id,package_id,component_id,reserved_microcredits,released_microcredits,created_at,updated_at) VALUES($1,$2,$3,1,$4,now(),now())`, h.ID, packageID, componentID, released); err != nil {
				t.Fatal("fixture corruption", err)
			}
			before := snapshot(h.ID)
			if _, err = r.Release(ctx, usage, h.ID, "must fail closed"); !errors.Is(err, ErrConflict) || snapshot(h.ID) != before {
				t.Fatal("bad zero allocation accepted or mutated", err)
			}
		})
	}
}
