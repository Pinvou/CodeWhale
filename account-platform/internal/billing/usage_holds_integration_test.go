package billing

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

// Uses the inherited strict absent-role harness and production migration wrapper.
// Real personal wallet/ledger/hold transactions; no supplier or Token execution.
func TestPersonalUsageHoldBoundaries(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	funded := func(t *testing.T) Wallet {
		t.Helper()
		w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		w, err = r.Credit(ctx, w.ID, "hold-fixture-"+uuid.NewString(), 10000, false)
		if err != nil {
			t.Fatal(err)
		}
		return w
	}
	pricing := func() *HoldPricingContext {
		return &HoldPricingContext{ModelID: "controlled-model", ProtocolCode: "CHAT_COMPLETIONS", FormulaVersionID: uuid.New(), PricingSnapshot: json.RawMessage(`{"input":1,"output":2}`), RequestSnapshot: json.RawMessage(`{"provider":"controlled","maximum":100}`)}
	}
	t.Run("priced-replay-binds-all-five-facts", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		p := pricing()
		expiry := r.now().Truncate(time.Microsecond).Add(time.Hour)
		first, err := r.ReservePriced(ctx, id, w.ID, 100, nil, expiry, p)
		if err != nil {
			t.Fatal(err)
		}
		same := *p
		same.PricingSnapshot = json.RawMessage(`{"output":2.0,"input":1e0}`)
		replay, err := r.ReservePriced(ctx, id, w.ID, 100, nil, expiry.Add(time.Hour), &same)
		if err != nil || replay.ID != first.ID || !replay.ExpiresAt.Equal(first.ExpiresAt) {
			t.Fatal("unchanged semantic facts did not replay original hold", err)
		}
		cases := []struct {
			name   string
			change func(*HoldPricingContext)
		}{
			{"model", func(p *HoldPricingContext) { p.ModelID = "other" }},
			{"protocol", func(p *HoldPricingContext) { p.ProtocolCode = "OTHER" }},
			{"formula", func(p *HoldPricingContext) { p.FormulaVersionID = uuid.New() }},
			{"pricing", func(p *HoldPricingContext) { p.PricingSnapshot = json.RawMessage(`{"input":3}`) }},
			{"request", func(p *HoldPricingContext) { p.RequestSnapshot = json.RawMessage(`{"provider":"other"}`) }},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				bad := *p
				c.change(&bad)
				if _, err := r.ReservePriced(ctx, id, w.ID, 100, nil, expiry, &bad); !errors.Is(err, ErrConflict) {
					t.Fatal("changed priced fact accepted", err)
				}
			})
		}
		if _, err := r.Reserve(ctx, id, w.ID, 100, nil, expiry); !errors.Is(err, ErrConflict) {
			t.Fatal("priced hold accepted unpriced retry", err)
		}
		var reserved, count int64
		if err := r.pool.QueryRow(ctx, `SELECT reserved_microcredits,(SELECT count(*) FROM usage_holds WHERE usage_request_id=$2) FROM wallets WHERE id=$1`, w.ID, id).Scan(&reserved, &count); err != nil || reserved != 100 || count != 1 {
			t.Fatal("retries changed financial reservation", reserved, count, err)
		}
	})
	t.Run("unpriced-hold-rejects-priced-retry", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		expiry := r.now().Truncate(time.Microsecond).Add(time.Hour)
		first, err := r.Reserve(ctx, id, w.ID, 100, nil, expiry)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.ReservePriced(ctx, id, w.ID, 100, nil, expiry, pricing()); !errors.Is(err, ErrConflict) {
			t.Fatal("unpriced hold adopted new snapshot", err)
		}
		same, err := r.Reserve(ctx, id, w.ID, 100, nil, expiry)
		if err != nil || same.ID != first.ID {
			t.Fatal("original unpriced replay failed", err)
		}
	})
	t.Run("commit-preserves-frozen-rights-and-exact-ledger", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		h, err := r.ReservePriced(ctx, id, w.ID, 100, nil, r.now().Add(time.Hour), pricing())
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{`UPDATE entitlement_components SET status='FROZEN' WHERE package_id IN (SELECT id FROM entitlement_packages WHERE wallet_id=$1)`, `UPDATE entitlement_packages SET status='FROZEN' WHERE wallet_id=$1`} {
			if _, err = r.pool.Exec(ctx, query, w.ID); err != nil {
				t.Fatal(err)
			}
		}
		usage := map[string]any{"input_tokens": 3, "output_tokens": 2, "cached_input_tokens": 0}
		committed, err := r.Commit(ctx, id, h.ID, 30, usage)
		if err != nil || committed.Status != "COMMITTED" {
			t.Fatal("original hold did not settle", err)
		}
		same, err := r.Commit(ctx, id, h.ID, 30, usage)
		if err != nil || same.ID != h.ID {
			t.Fatal("terminal replay failed", err)
		}
		if _, err = r.Commit(ctx, id, h.ID, 31, usage); !errors.Is(err, ErrUsageAlreadyTerminal) {
			t.Fatal("changed committed amount accepted", err)
		}
		var posted, reserved, used, sum, journals int64
		var component, pack string
		err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,c.used_microcredits,c.status,p.status,(SELECT sum(delta_microcredits) FROM ledger_postings),(SELECT count(*) FROM journal_entries WHERE entry_type='USAGE') FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id WHERE w.id=$1`, w.ID).Scan(&posted, &reserved, &used, &component, &pack, &sum, &journals)
		if err != nil || posted != 9970 || reserved != 0 || used != 30 || component != "FROZEN" || pack != "FROZEN" || sum != 0 || journals != 1 {
			t.Fatal("settlement thawed rights or duplicated money", err)
		}
	})
	t.Run("release-preserves-frozen-rights", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		h, err := r.Reserve(ctx, id, w.ID, 100, nil, r.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{`UPDATE entitlement_components SET status='FROZEN' WHERE package_id IN (SELECT id FROM entitlement_packages WHERE wallet_id=$1)`, `UPDATE entitlement_packages SET status='FROZEN' WHERE wallet_id=$1`} {
			if _, err = r.pool.Exec(ctx, query, w.ID); err != nil {
				t.Fatal(err)
			}
		}
		released, err := r.Release(ctx, id, h.ID, "controlled-before-generation")
		if err != nil || released.Status != "RELEASED" {
			t.Fatal("release failed", err)
		}
		same, err := r.Release(ctx, id, h.ID, "controlled-before-generation")
		if err != nil || same.ID != h.ID {
			t.Fatal("release replay failed", err)
		}
		var posted, reserved, available int64
		var c, p string
		err = r.pool.QueryRow(ctx, `SELECT w.posted_microcredits,w.reserved_microcredits,component.available_microcredits,component.status,package.status FROM wallets w JOIN entitlement_packages package ON package.wallet_id=w.id JOIN entitlement_components component ON component.package_id=package.id WHERE w.id=$1`, w.ID).Scan(&posted, &reserved, &available, &c, &p)
		if err != nil || posted != 10000 || reserved != 0 || available != 10000 || c != "FROZEN" || p != "FROZEN" {
			t.Fatal("release reopened frozen rights", err)
		}
	})
	t.Run("extend-locks-wallet-before-hold", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		h, err := r.Reserve(ctx, id, w.ID, 100, nil, r.now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		blocker, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = getWallet(ctx, blocker, w.ID, true); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := r.ExtendHold(ctx, id, h.ID, h.ExpiresAt.Add(time.Hour), 40); done <- err }()
		deadline := time.Now().Add(3 * time.Second)
		blocked := false
		for time.Now().Before(deadline) {
			var count int
			err = r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%FROM wallets WHERE id=$1%'`).Scan(&count)
			if err != nil {
				t.Fatal(err)
			}
			if count > 0 {
				blocked = true
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("extension did not reach controlled wallet lock wait")
		}
		probe, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var holdID uuid.UUID
		err = probe.QueryRow(ctx, `SELECT id FROM usage_holds WHERE id=$1 FOR UPDATE NOWAIT`, h.ID).Scan(&holdID)
		probe.Rollback(ctx)
		if err != nil || holdID != h.ID {
			t.Fatal("extension acquired hold before wallet", err)
		}
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("extension did not finish after wallet release")
		}
		var max, reserved int64
		var events int
		err = r.pool.QueryRow(ctx, `SELECT h.max_microcredits,w.reserved_microcredits,(SELECT count(*) FROM usage_hold_lease_events WHERE hold_id=h.id) FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, h.ID).Scan(&max, &reserved, &events)
		if err != nil || max != 140 || reserved != 140 || events != 2 {
			t.Fatal("extension lost exact reserve or immutable lease event", err)
		}
	})
	t.Run("expiry-releases-only-original-hold", func(t *testing.T) {
		w := funded(t)
		id := uuid.New()
		now := r.now()
		h, err := r.Reserve(ctx, id, w.ID, 100, nil, now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		originalClock := r.now
		r.now = func() time.Time { return now.Add(2 * time.Minute) }
		defer func() { r.now = originalClock }()
		count, err := r.ExpireHolds(ctx, 100)
		if err != nil || count < 1 {
			t.Fatal("expired lease did not release", err)
		}
		got, err := getHoldByUsage(ctx, r.pool, id, false)
		if err != nil || got.ID != h.ID || got.Status != "RELEASED" {
			t.Fatal("expiry changed wrong hold", err)
		}
		var snapshot string
		if err = r.pool.QueryRow(ctx, `SELECT usage_snapshot::text FROM usage_holds WHERE id=$1`, h.ID).Scan(&snapshot); err != nil || !strings.Contains(snapshot, "hold lease expired") {
			t.Fatal("expiry lacked release evidence", err)
		}
	})
}
