package billing

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/usagewaiver"
	"github.com/google/uuid"
)

func waiverFixtureActor() UsageWaiverActor {
	return UsageWaiverActor{ID: uuid.New(), Issuer: "https://controlled-waiver.invalid", Subject: "operator", AuthorizationVersion: 19}
}
func waiverFixtureHold(t *testing.T, r *PostgresRepository, ctx context.Context, max int64) (Hold, uuid.UUID) {
	t.Helper()
	w, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Credit(ctx, w.ID, "waiver-funding:"+w.ID.String(), 100, false); err != nil {
		t.Fatal(err)
	}
	h, err := r.Reserve(ctx, uuid.New(), w.ID, max, nil, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return h, w.ID
}
func waiverFixtureSnapshot(t *testing.T, r *PostgresRepository, ctx context.Context) string {
	t.Helper()
	var raw string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),
 'holds',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_holds x),
 'lots',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),
 'components',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),
 'grants',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM member_quota_grants x),
 'allocations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY hold_id,component_id) FROM usage_hold_allocations x),
 'member_allocations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY hold_id,grant_id) FROM member_quota_hold_allocations x),
 'journals',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),
 'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),
 'commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY command_reference) FROM usage_waiver_commands x),
 'results',(SELECT jsonb_agg(to_jsonb(x) ORDER BY command_reference) FROM usage_waiver_results x),
 'actions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_hold_admin_actions x),
 'claims',(SELECT jsonb_agg(to_jsonb(x) ORDER BY hold_id) FROM usage_waiver_hold_claims x),
 'rejections',(SELECT jsonb_agg(to_jsonb(x) ORDER BY command_reference) FROM usage_waiver_rejections x))::text`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func waiverFixtureCommand(t *testing.T, r *PostgresRepository, ctx context.Context, h Hold) (usagewaiver.Command, string, UsageWaiverActor) {
	t.Helper()
	actor, key := waiverFixtureActor(), uuid.NewString()
	c, err := r.PrepareUsageWaiver(ctx, h.ID, "controlled administrator decision", key, actor)
	if err != nil {
		t.Fatal(err)
	}
	return c, key, actor
}

// waiverHoldReceiptEqual compares every receipt fact. A PostgreSQL timestamp and
// its JSON replay may use different locations for the same instant.
func waiverHoldReceiptEqual(got, want Hold) bool {
	if !got.ExpiresAt.Equal(want.ExpiresAt) {
		return false
	}
	got.ExpiresAt, want.ExpiresAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(got, want)
}

func TestUsageWaiverHoldReceiptEquality(t *testing.T) {
	price := uuid.New()
	want := Hold{ID: uuid.New(), UsageRequestID: uuid.New(), WalletID: uuid.New(), PriceVersionID: &price, Status: "ACTIVE", MaxMicrocredits: 50, ExpiresAt: time.Unix(1792684800, 123456000).UTC()}
	for _, offset := range []int{0, 8 * 60 * 60, -5 * 60 * 60} {
		t.Run(fmt.Sprintf("same-instant-offset-%d", offset), func(t *testing.T) {
			got := want
			got.ExpiresAt = want.ExpiresAt.In(time.FixedZone("controlled distinct location", offset))
			if got.ExpiresAt.Location() == want.ExpiresAt.Location() || !got.ExpiresAt.Equal(want.ExpiresAt) || reflect.DeepEqual(got, want) {
				t.Fatal("fixture must differ only in time representation")
			}
			if !waiverHoldReceiptEqual(got, want) {
				t.Fatal("equal receipt instant rejected")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*Hold)
	}{
		{"expiry", func(h *Hold) { h.ExpiresAt = h.ExpiresAt.Add(time.Microsecond) }},
		{"hold-id", func(h *Hold) { h.ID = uuid.New() }},
		{"usage-id", func(h *Hold) { h.UsageRequestID = uuid.New() }},
		{"wallet-id", func(h *Hold) { h.WalletID = uuid.New() }},
		{"price-version", func(h *Hold) { v := uuid.New(); h.PriceVersionID = &v }},
		{"price-version-absent", func(h *Hold) { h.PriceVersionID = nil }},
		{"status", func(h *Hold) { h.Status = "REVIEW" }},
		{"maximum", func(h *Hold) { h.MaxMicrocredits++ }},
		{"actual-presence", func(h *Hold) { v := int64(0); h.ActualMicrocredits = &v }},
	} {
		t.Run("changed-"+tc.name, func(t *testing.T) {
			got := want
			tc.change(&got)
			if waiverHoldReceiptEqual(got, want) {
				t.Fatal("changed receipt fact accepted")
			}
		})
	}
	withActual := want
	actual := int64(7)
	withActual.ActualMicrocredits = &actual
	changedActual := withActual
	otherActual := int64(8)
	changedActual.ActualMicrocredits = &otherActual
	if waiverHoldReceiptEqual(changedActual, withActual) {
		t.Fatal("changed actual amount accepted")
	}
}

func TestUsageWaiverFinancialConservationPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, max := range []int64{0, 40, 100} {
		for _, status := range []string{"ACTIVE", "REVIEW"} {
			t.Run(fmt.Sprintf("personal-%s-%d", status, max), func(t *testing.T) {
				h, walletID := waiverFixtureHold(t, r, ctx, max)
				if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET status=$2 WHERE id=$1`, h.ID, status); err != nil {
					t.Fatal(err)
				}
				c, key, actor := waiverFixtureCommand(t, r, ctx, h)
				before := waiverFixtureSnapshot(t, r, ctx)
				if _, err := r.PrepareUsageWaiver(ctx, h.ID, "changed reason", key, actor); !errors.Is(err, ErrConflict) {
					t.Fatal("changed reason accepted", err)
				}
				other := actor
				other.ID = uuid.New()
				if _, err := r.PrepareUsageWaiver(ctx, h.ID, c.Reason, key, other); !errors.Is(err, ErrConflict) {
					t.Fatal("changed actor accepted", err)
				}
				if _, err := r.PrepareUsageWaiver(ctx, h.ID, c.Reason, uuid.NewString(), actor); !errors.Is(err, ErrConflict) {
					t.Fatal("unknown original decision allowed replacement key", err)
				}
				if before != waiverFixtureSnapshot(t, r, ctx) {
					t.Fatal("rejected request mutated facts")
				}
				result, err := r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: c.Reference, Digest: c.Digest})
				if err != nil || result.Outcome != usagewaiver.Waived || !result.Matches(c) {
					t.Fatal("waive", result, err)
				}
				wallet, err := r.GetWallet(ctx, walletID)
				if err != nil || wallet.PostedMicrocredits != 100 || wallet.ReservedMicrocredits != 0 || wallet.ReceivableMicrocredits != 0 {
					t.Fatal("money moved beyond reservation", wallet, err)
				}
				var available, reserved, used, usageJournals, actions int64
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT sum(available_microcredits) FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=$1),(SELECT sum(reserved_microcredits) FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=$1),(SELECT sum(used_microcredits) FROM entitlement_components c JOIN entitlement_packages p ON p.id=c.package_id WHERE p.wallet_id=$1),(SELECT count(*) FROM journal_entries WHERE business_reference=$2),(SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$3)`, walletID, "usage:"+h.UsageRequestID.String(), h.ID).Scan(&available, &reserved, &used, &usageJournals, &actions); err != nil || available != 100 || reserved != 0 || used != 0 || usageJournals != 0 || actions != 1 {
					t.Fatal("source/journal/audit conservation", available, reserved, used, usageJournals, actions, err)
				}
				frozen := waiverFixtureSnapshot(t, r, ctx)
				replay, err := r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: c.Reference, Digest: c.Digest})
				if err != nil || !reflect.DeepEqual(replay, result) {
					t.Fatal("result replay drift", err)
				}
				original, err := r.PrepareUsageWaiver(ctx, h.ID, c.Reason, key, actor)
				if err != nil || original != c {
					t.Fatal("original command replay drift", err)
				}
				if _, err := r.Commit(ctx, h.UsageRequestID, h.ID, 25, map[string]any{"late": true}); !errors.Is(err, ErrUsageAlreadyTerminal) {
					t.Fatal("waiver permitted late charge", err)
				}
				if frozen != waiverFixtureSnapshot(t, r, ctx) {
					t.Fatal("replay/late commit changed frozen financial facts")
				}
			})
		}
	}
	t.Run("pending-freezes-extension-source-but-result-first-and-rejection-recovery-survive", func(t *testing.T) {
		h, walletID := waiverFixtureHold(t, r, ctx, 40)
		first := HoldExtension{UsageRequestID: h.UsageRequestID, HoldID: h.ID, OperationID: uuid.New(), LeaseSeconds: 900, AdditionalMicrocredits: 10}
		saved, err := r.ExtendHold(ctx, first)
		if err != nil || saved.MaxMicrocredits != 50 {
			t.Fatal("initial real extension", saved, err)
		}
		c, _, _ := waiverFixtureCommand(t, r, ctx, saved)
		next := first
		next.OperationID = uuid.New()
		before := waiverFixtureSnapshot(t, r, ctx)
		if _, err := r.ExtendHold(ctx, next); !errors.Is(err, ErrConflict) {
			t.Fatal("pending decision source extended", err)
		}
		replay, err := r.ExtendHold(ctx, first)
		if err != nil || !waiverHoldReceiptEqual(replay, saved) {
			t.Fatal("existing successful extension receipt blocked", err)
		}
		if before != waiverFixtureSnapshot(t, r, ctx) {
			t.Fatal("blocked extension/replay mutated frozen input")
		}
		rejected := usagewaiver.Decision{CommandReference: c.Reference, RequestDigest: c.Digest, Admission: usagewaiver.Rejected, ReasonCode: "EXECUTION_ACTIVE"}
		if err := r.RecordUsageWaiverRejection(ctx, c, rejected); err != nil {
			t.Fatal(err)
		}
		resumed, err := r.ExtendHold(ctx, next)
		if err != nil || resumed.MaxMicrocredits != 60 {
			t.Fatal("persisted rejection poisoned legitimate extension", resumed, err)
		}
		wallet, err := r.GetWallet(ctx, walletID)
		if err != nil || wallet.PostedMicrocredits != 100 || wallet.ReservedMicrocredits != 60 {
			t.Fatal("extension recovery conservation", wallet, err)
		}
	})
	t.Run("immutable-result-failure-rolls-back-every-financial-write", func(t *testing.T) {
		h, _ := waiverFixtureHold(t, r, ctx, 40)
		c, _, _ := waiverFixtureCommand(t, r, ctx, h)
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION waiver_fixture_fail() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled result insert unavailable'; END $$; CREATE TRIGGER waiver_fixture_fail BEFORE INSERT ON usage_waiver_results FOR EACH ROW EXECUTE FUNCTION waiver_fixture_fail()`); err != nil {
			t.Fatal(err)
		}
		before := waiverFixtureSnapshot(t, r, ctx)
		if _, err := r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: c.Reference, Digest: c.Digest}); err == nil {
			t.Fatal("expected result persistence failure")
		}
		if before != waiverFixtureSnapshot(t, r, ctx) {
			t.Fatal("failed result persistence left money/audit effects")
		}
		if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER waiver_fixture_fail ON usage_waiver_results; DROP FUNCTION waiver_fixture_fail()`); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUsageWaiverCommitWinnerPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	for _, order := range []string{"commit-first", "waive-first", "concurrent"} {
		for _, actual := range []int64{0, 25} {
			t.Run(fmt.Sprintf("%s-actual-%d", order, actual), func(t *testing.T) {
				h, walletID := waiverFixtureHold(t, r, ctx, 40)
				c, _, _ := waiverFixtureCommand(t, r, ctx, h)
				var result usagewaiver.Result
				var commitErr, waiveErr error
				commit := func() {
					_, commitErr = r.Commit(ctx, h.UsageRequestID, h.ID, actual, map[string]any{"usage": "controlled winner", "actual_microcredits": actual})
				}
				waive := func() {
					result, waiveErr = r.ExecuteUsageWaiver(ctx, usagewaiver.ExecuteRequest{Reference: c.Reference, Digest: c.Digest})
				}
				switch order {
				case "commit-first":
					commit()
					waive()
				case "waive-first":
					waive()
					commit()
				default:
					var group sync.WaitGroup
					group.Add(2)
					go func() { defer group.Done(); commit() }()
					go func() { defer group.Done(); waive() }()
					group.Wait()
				}
				if waiveErr != nil || !result.Matches(c) {
					t.Fatal("no authoritative result", result, waiveErr)
				}
				winnerCommit := result.Outcome == usagewaiver.Committed
				if order == "commit-first" && !winnerCommit || order == "waive-first" && winnerCommit {
					t.Fatal("winner changed", result)
				}
				expectedPosted, expectedActions := int64(100), int64(1)
				if winnerCommit {
					expectedPosted -= actual
					expectedActions = 0
					if commitErr != nil || result.Actual == nil || *result.Actual != actual {
						t.Fatal("committed winner lost", commitErr, result)
					}
				} else if !errors.Is(commitErr, ErrUsageAlreadyTerminal) {
					t.Fatal("waiver winner did not fence commit", commitErr)
				}
				wallet, err := r.GetWallet(ctx, walletID)
				if err != nil || wallet.PostedMicrocredits != expectedPosted || wallet.ReservedMicrocredits != 0 || wallet.ReceivableMicrocredits != 0 {
					t.Fatal("winner conservation", wallet, err)
				}
				var actions, journals int64
				if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_hold_admin_actions WHERE hold_id=$1),(SELECT count(*) FROM journal_entries WHERE business_reference=$2)`, h.ID, "usage:"+h.UsageRequestID.String()).Scan(&actions, &journals); err != nil || actions != expectedActions {
					t.Fatal("false waiver audit", actions, err)
				}
				expectedJournals := int64(0)
				if winnerCommit && actual > 0 {
					expectedJournals = 1
				}
				if journals != expectedJournals {
					t.Fatal("zero or duplicate usage journal", journals)
				}
				if journals == 1 {
					var exact bool
					if err := r.pool.QueryRow(ctx, `SELECT count(*)=2 AND sum(posting.delta_microcredits)=0 AND count(*) FILTER(WHERE account.wallet_id=$1 AND account.account_type='WALLET_LIABILITY' AND posting.delta_microcredits=-$3::bigint)=1 AND count(*) FILTER(WHERE account.id=$4 AND posting.delta_microcredits=$3)=1 FROM ledger_postings posting JOIN ledger_accounts account ON account.id=posting.ledger_account_id JOIN journal_entries journal ON journal.id=posting.journal_entry_id WHERE journal.business_reference=$2`, walletID, "usage:"+h.UsageRequestID.String(), actual, usageRevenueAccount).Scan(&exact); err != nil || !exact {
						t.Fatal("wrong posting owner/direction/account", err)
					}
				}
			})
		}
	}
}
