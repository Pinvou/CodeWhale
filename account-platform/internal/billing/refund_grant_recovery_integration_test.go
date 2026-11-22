package billing

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func refundRecoveryGrant(t *testing.T, r *PostgresRepository, ctx context.Context, order PaymentOrder, packageID uuid.UUID, reason string) MemberQuotaGrant {
	t.Helper()
	organization, member := *order.BeneficiaryOrganizationID, uuid.New()
	if _, err := r.ApplyOrganizationMemberLifecycle(ctx, organization, member, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	var componentID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE' AND available_microcredits>0 ORDER BY id LIMIT 1`, packageID).Scan(&componentID); err != nil {
		t.Fatal(err)
	}
	grant, created, err := r.AllocateMemberQuota(ctx, MemberQuotaAllocation{OrganizationID: organization, MemberUserID: member,
		PackageID: packageID, ComponentID: componentID, ComponentType: "BASE", AmountMicrocredits: 10, MembershipVersion: 1,
		CreatedBy: uuid.New(), IdempotencyKey: uuid.NewString()})
	if err != nil || !created {
		t.Fatal("actual member allocation", err)
	}
	if reason != "" {
		if _, err := r.ApplyOrganizationMemberLifecycle(ctx, organization, member, "REVOKED", 2); err != nil {
			t.Fatal(err)
		}
		if reason == "ACCOUNT_CLOSED" {
			if _, err := r.IsolateAccountClosedGrants(ctx, uuid.New(), member, 2); err != nil {
				t.Fatal("actual account-closed isolation", err)
			}
		}
	}
	return grant
}

func refundRecoveryMoneySnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, table := range []string{"wallets", "credit_lots", "entitlement_packages", "entitlement_components", "member_quota_grants",
		"organization_member_lifecycle_states", "account_closure_grant_isolations", "member_quota_grant_audits", "journal_entries", "ledger_postings", "receivables"} {
		var raw string
		if err := r.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM `+table+` x`).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		result[table] = raw
	}
	return result
}

// The current writer deliberately cannot make this old reservation. These SQL
// statements preserve the original reserveRefund snapshot/freeze shape over
// real payment/grant/lifecycle producers. They model already-persisted requests
// from the reviewed predecessor, not newly admissible product behavior.
func refundRecoveryLegacyReservation(t *testing.T, ctx context.Context, r *PostgresRepository, order PaymentOrder, transactionID, packageID uuid.UUID) Refund {
	t.Helper()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	now, refundID := r.now(), uuid.New()
	var base, bonus int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits) FILTER(WHERE component_type='BASE'),0),COALESCE(sum(available_microcredits+active_grant_microcredits+suspended_grant_microcredits) FILTER(WHERE component_type='BONUS'),0) FROM entitlement_components WHERE package_id=$1`, packageID).Scan(&base, &bonus); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO refunds(id,payment_order_id,payment_transaction_id,entitlement_package_id,requester_id,merchant_refund_no,amount_fen,credit_microcredits,base_microcredits,bonus_microcredits,status,idempotency_key,request_hash,reason,entitlement_snapshot,created_at,updated_at)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'REQUESTED',$11,repeat('a',64),'retained original grant reservation','{}',$12,$12)`,
		refundID, order.ID, transactionID, packageID, order.RequesterID, "retained-refund-"+refundID.String(), order.AmountFen, base+bonus, base, bonus, uuid.NewString(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO refund_component_reservations(refund_id,component_id,package_id,component_type,credit_lot_id,available_microcredits,active_grant_microcredits,suspended_grant_microcredits,prior_status,created_at)
 SELECT $1,id,package_id,component_type,credit_lot_id,available_microcredits,active_grant_microcredits,suspended_grant_microcredits,status,$3 FROM entitlement_components WHERE package_id=$2`, refundID, packageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO refund_grant_reservations(refund_id,grant_id,component_id,available_microcredits,prior_status,prior_suspension_reason,created_at)
 SELECT $1,id,component_id,available_microcredits,status,suspension_reason,$3 FROM member_quota_grants WHERE package_id=$2`, refundID, packageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_packages SET status='FROZEN',updated_at=$2 WHERE id=$1`, packageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE entitlement_components SET status='FROZEN',updated_at=$2 WHERE package_id=$1`, packageID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE member_quota_grants SET status='SUSPENDED',suspension_reason='REFUND_PENDING',allocation_version=allocation_version+1,updated_at=$2 WHERE package_id=$1 AND status='ACTIVE'`, packageID, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	refund, err := getRefundByID(ctx, r.pool, refundID, false)
	if err != nil {
		t.Fatal(err)
	}
	return refund
}

func TestRefundRejectsExistingSuspendedGrantsBeforeReservationPostgres(t *testing.T) {
	for _, reason := range []string{"MEMBER_REMOVED", "ACCOUNT_CLOSED"} {
		t.Run(reason, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
			refundRecoveryGrant(t, r, ctx, order, pack, reason)
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "must-not-dispatch"}}
			c.provider = provider
			c.AllowNormalRefundCreation(true)
			money, effects := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
			_, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "new refund must reject prior suspension")
			if !errors.Is(err, ErrConflict) || created || provider.calls != 0 || effects != refundEffectSnapshot(t, ctx, r) || !reflect.DeepEqual(money, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("unsupported grant reservation was accepted or changed financial/command facts", err)
			}
		})
	}
}

func TestRefundLegacySuspendedDispatchNeverCallsProviderPostgres(t *testing.T) {
	for _, dispatch := range []string{"NOT_STARTED", "RESULT_UNKNOWN", "IN_PROGRESS"} {
		t.Run(dispatch, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
			refundRecoveryGrant(t, r, ctx, order, pack, "MEMBER_REMOVED")
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state=$2,dispatch_lease_until=$3 WHERE id=$1`, refund.ID, dispatch, r.now().Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "must-not-repeat"}}
			c.provider = provider
			money := refundRecoveryMoneySnapshot(t, ctx, r)
			if err := c.ProcessRefund(ctx, refund.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) {
				t.Fatal("unknown/manual recovery must not authorize a provider request", err)
			}
			var status, retainedDispatch string
			if err := r.pool.QueryRow(ctx, `SELECT status,dispatch_state FROM refunds WHERE id=$1`, refund.ID).Scan(&status, &retainedDispatch); err != nil || status != "MANUAL_REVIEW" || retainedDispatch != dispatch || provider.calls != 0 || !reflect.DeepEqual(money, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("retained unknown request called provider or changed money/grants", err)
			}
			if _, err := r.RefundExceptionRepairProofInput(ctx, RefundExceptionRepairCommandInput{RefundID: refund.ID, PaymentEvidenceID: uuid.New(), AnomalyCaseID: uuid.New()}); err == nil {
				t.Fatal("an unsupported unknown request acquired SUCCESS authority")
			}
		})
	}
}

func refundRecoveryEscalate(t *testing.T, ctx context.Context, r *PostgresRepository, refund Refund, order PaymentOrder) RefundExceptionRepairExecutionInput {
	t.Helper()
	var evidence, anomaly uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT e.id,a.id FROM payment_evidence e JOIN payment_anomaly_cases a ON a.refund_id=$1 AND a.dedupe_key='refund:'||$1::text||':local-conflict'
 WHERE e.payment_order_id=$2 AND e.payment_transaction_id=$3 AND e.provider_refund_id=$4 AND e.provider_status='SUCCESS'`, refund.ID, order.ID, refund.PaymentTransactionID, *refund.ChannelRefundID).Scan(&evidence, &anomaly); err != nil {
		t.Fatal("actual successful-provider conflict evidence", err)
	}
	if _, err := retainedResolveClaimedAnomaly(t, r, ctx, anomaly, uuid.New(), uuid.NewString(), "ESCALATE_FINANCIAL_REPAIR", "verified original provider success needs controlled reversal", "PENDING"); err != nil {
		t.Fatal(err)
	}
	activation := refundFixtureActivation(t, ctx, r.pool)("ACTIVE")
	plan, err := refundExceptionRepairPlanFrom(ctx, r.pool, RefundExceptionRepairCommandInput{RefundID: refund.ID, PaymentEvidenceID: evidence, AnomalyCaseID: anomaly}, 1, 1, activation)
	if err != nil {
		t.Fatal("actual owner precheck", err)
	}
	input := refundFrozenInput(plan)
	input.ActivationVersion = activation
	return input
}

func refundRecoveryExactPostings(t *testing.T, ctx context.Context, r *PostgresRepository, result RefundExceptionRepairResult, expected int64) {
	t.Helper()
	var valid bool
	if err := r.pool.QueryRow(ctx, `SELECT
 (SELECT count(*)=2 FROM ledger_postings WHERE journal_entry_id=$1)
 AND (SELECT count(*)=1 FROM ledger_postings p JOIN ledger_accounts a ON a.id=p.ledger_account_id WHERE p.journal_entry_id=$1 AND a.wallet_id=$2 AND a.account_type='WALLET_LIABILITY' AND p.delta_microcredits=-$3::bigint AND p.credit_lot_id IS NOT NULL)
 AND (SELECT count(*)=1 FROM ledger_postings p JOIN ledger_accounts a ON a.id=p.ledger_account_id WHERE p.journal_entry_id=$1 AND a.wallet_id IS NULL AND a.account_type='REFUND_CLEARING' AND p.delta_microcredits=$3 AND p.credit_lot_id IS NULL)
 AND NOT EXISTS(SELECT 1 FROM entitlement_components WHERE package_id=$4 AND status<>'REVOKED')`, result.JournalEntryID, result.WalletID, expected, result.PackageID).Scan(&valid); err != nil || !valid {
		t.Fatal("repair owner/account/sign/lot/amount or component terminal state differs", err)
	}
}

func TestRefundVerifiedSuccessRepairsOriginalSuspendedGrantPostgres(t *testing.T) {
	for _, reason := range []string{"MEMBER_REMOVED", "ACCOUNT_CLOSED"} {
		t.Run(reason, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
			grant := refundRecoveryGrant(t, r, ctx, order, pack, reason)
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "verified-legacy-success"}}
			// Models the predecessor's already-returned provider SUCCESS. The
			// current dispatch preflight refuses to issue that old request again.
			confirmed, err := provider.Refund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen)
			if err != nil {
				t.Fatal(err)
			}
			refund, _, err = c.finishRefund(ctx, refund, confirmed, false)
			if err != nil || refund.Status != "MANUAL_REVIEW" || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("ordinary SUCCESS must preserve evidence without revoking prior suspension", err)
			}
			input := refundRecoveryEscalate(t, ctx, r, refund, order)
			result, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || result.Status != "SUCCEEDED" || result.RevokedMicrocredits != 100 || result.ShortfallMicrocredits != 0 {
				t.Fatal("governed original SUCCESS repair", err)
			}
			refundRecoveryExactPostings(t, ctx, r, result, 100)
			var revoked, retainedReason bool
			if err := r.pool.QueryRow(ctx, `SELECT g.status='REVOKED' AND g.available_microcredits=0 AND g.revoked_microcredits=10,
 reservation.prior_status='SUSPENDED' AND reservation.prior_suspension_reason=$3
 FROM member_quota_grants g JOIN refund_grant_reservations reservation ON reservation.grant_id=g.id AND reservation.refund_id=$2 WHERE g.id=$1`, grant.ID, refund.ID, reason).Scan(&revoked, &retainedReason); err != nil || !revoked || !retainedReason {
				t.Fatal("historical suspension or precise grant recovery lost", err)
			}
			after := refundRecoveryMoneySnapshot(t, ctx, r)
			for _, table := range []string{"organization_member_lifecycle_states", "account_closure_grant_isolations"} {
				if after[table] != before[table] {
					t.Fatal("financial repair restored member or account-closed identity", table)
				}
			}
			// Discarding the first ACK cannot authorize a second financial effect.
			receipt := refundEffectSnapshot(t, ctx, r)
			replayed, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || replayed != result || receipt != refundEffectSnapshot(t, ctx, r) || !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || provider.calls != 1 {
				t.Fatal("lost response/replayed original approval changed financial facts", err)
			}
		})
	}
}

func TestRefundRepairRetainsZeroHistoricalCorrectionSourcePostgres(t *testing.T) {
	r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 0, 100)
	c.AllowNormalRefundCreation(true)
	refund, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "corrected source original refund")
	if err != nil || !created {
		t.Fatal(err)
	}
	// Same explicit historical balance fault as the existing F13 authority test.
	if _, err := r.pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits-1,version=version+1 WHERE id=$1`, order.WalletID); err != nil {
		t.Fatal(err)
	}
	refund, _, err = c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "verified-corrected-refund", AmountFen: refund.AmountFen, Status: "SUCCESS"}, false)
	if err != nil || refund.Status != "MANUAL_REVIEW" {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE wallets SET posted_microcredits=posted_microcredits+1,version=version+1 WHERE id=$1`, order.WalletID); err != nil {
		t.Fatal(err)
	}
	input := refundRecoveryEscalate(t, ctx, r, refund, order)
	var retainedZero int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM refund_component_reservations WHERE refund_id=$1 AND available_microcredits+active_grant_microcredits+suspended_grant_microcredits=0`, refund.ID).Scan(&retainedZero); err != nil || retainedZero != 1 {
		t.Fatal("real correction lost its historical zero source", err)
	}
	result, err := r.ApplyRefundExceptionRepair(ctx, input)
	if err != nil || result.Status != "SUCCEEDED" || result.RevokedMicrocredits != 100 || result.ShortfallMicrocredits != 0 {
		t.Fatal("zero historical source failed controlled reversal", err)
	}
	refundRecoveryExactPostings(t, ctx, r, result, 100)
	before := refundEffectSnapshot(t, ctx, r)
	replayed, err := r.ApplyRefundExceptionRepair(ctx, input)
	if err != nil || replayed != result || before != refundEffectSnapshot(t, ctx, r) {
		t.Fatal("original-key zero-source receipt replay changed effects", err)
	}
}

// Controlled provider result only; the correction, lifecycle, original SQL
// reservation, coordinator, governed repair, ledger and replay are real.
type refundRecoveryQueryProvider struct {
	*reviewRefundProvider
	result   ProviderRefund
	queries  int
	queryErr error
}

func (p *refundRecoveryQueryProvider) QueryRefund(context.Context, string, string) (ProviderRefund, error) {
	p.queries++
	return p.result, p.queryErr
}

func TestRefundCorrectedZeroSourceAndPriorSuspensionRecoverTogetherPostgres(t *testing.T) {
	for _, reason := range []string{"MEMBER_REMOVED", "ACCOUNT_CLOSED"} {
		t.Run(reason, func(t *testing.T) {
			r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 0, 100, true)
			refundRecoveryGrant(t, r, ctx, order, pack, reason)
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			provider := &refundRecoveryQueryProvider{reviewRefundProvider: &reviewRefundProvider{}, result: ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "controlled-combined-success", AmountFen: refund.AmountFen, Status: "SUCCESS"}}
			c.provider = provider
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			refund, err := c.processRefund(ctx, refund.ID, true)
			if !errors.Is(err, ErrConflict) || refund.Status != "MANUAL_REVIEW" || provider.queries != 1 || provider.calls != 0 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("combined legacy source failed read-only recovery", err)
			}
			input := refundRecoveryEscalate(t, ctx, r, refund, order)
			result, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || result.RevokedMicrocredits != 100 || result.ShortfallMicrocredits != 0 {
				t.Fatal("combined original facts failed governed repair", err)
			}
			refundRecoveryExactPostings(t, ctx, r, result, 100)
			after := refundRecoveryMoneySnapshot(t, ctx, r)
			if replay, err := r.ApplyRefundExceptionRepair(ctx, input); err != nil || replay != result || !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || provider.calls != 0 || provider.queries != 1 {
				t.Fatal("combined lost ACK duplicated effects", err)
			}
		})
	}
}

func TestRefundPriorSuspensionRepairFrozenGrantChangesFailWithoutEffectsPostgres(t *testing.T) {
	// PostgreSQL scans use time.Local, while receipt JSON Z timestamps use UTC.
	// Exercise the same-instant representation boundary on every host.
	local := time.Local
	time.Local = time.FixedZone("refund-receipt-test-UTC", 0)
	t.Cleanup(func() { time.Local = local })
	for _, mutation := range []string{"suspension_reason='ACCOUNT_CLOSED'", "allocation_version=allocation_version+1", "available_microcredits=available_microcredits-1,used_microcredits=used_microcredits+1"} {
		t.Run(mutation, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
			grant := refundRecoveryGrant(t, r, ctx, order, pack, "MEMBER_REMOVED")
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			refund, _, err := c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "controlled-original-success", AmountFen: refund.AmountFen, Status: "SUCCESS"}, false)
			if err != nil {
				t.Fatal(err)
			}
			input := refundRecoveryEscalate(t, ctx, r, refund, order)
			if _, err := r.pool.Exec(ctx, `UPDATE member_quota_grants SET `+mutation+` WHERE id=$1`, grant.ID); err != nil {
				t.Fatal(err)
			}
			money, effects := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
			if _, err := r.ApplyRefundExceptionRepair(ctx, input); !errors.Is(err, ErrApprovalSnapshotMismatch) {
				t.Fatal("changed original grant matched frozen authority", err)
			}
			if effects != refundEffectSnapshot(t, ctx, r) || !reflect.DeepEqual(money, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("rejected repair changed financial facts")
			}
		})
	}
	t.Run("ordinary-revoke-preserves-financial-freeze", refundFrozenQuotaOrdinaryRevokeRegression)
}

func TestRefundUnknownQueryCannotInventSuccessOrDispatchPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "unknown requires exact original authority")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	good := ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "controlled-original", AmountFen: refund.AmountFen, Status: "SUCCESS"}
	for _, kind := range []string{"UNKNOWN", "wrong-key", "wrong-amount", "missing-reference", "provider-unavailable"} {
		t.Run(kind, func(t *testing.T) {
			result := good
			provider := &refundRecoveryQueryProvider{reviewRefundProvider: &reviewRefundProvider{}, result: result}
			switch kind {
			case "UNKNOWN":
				provider.result.Status = "UNKNOWN"
			case "wrong-key":
				provider.result.MerchantRefundNo = "other-original"
			case "wrong-amount":
				provider.result.AmountFen++
			case "missing-reference":
				provider.result.ChannelRefundID = ""
			case "provider-unavailable":
				provider.queryErr = ErrPaymentProvider
			}
			c.provider = provider
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			got, err := c.processRefund(ctx, refund.ID, true)
			if !errors.Is(err, ErrConflict) || got.Status != "MANUAL_REVIEW" || provider.calls != 0 || provider.queries != 1 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("unproven query invented provider authority or financial effects", err)
			}
			var successes int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND provider_refund_id IS NOT NULL AND provider_status='SUCCESS'`, order.ID).Scan(&successes); err != nil || successes != 0 {
				t.Fatal("invalid query became SUCCESS evidence", err)
			}
		})
	}
}

func TestRefundOriginalSuccessQueryRetainsCompetingFinancialFreezePostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 100, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "retain competing financial source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-competing-refund", 10, nil); err != nil {
		t.Fatal(err)
	}
	provider := &refundRecoveryQueryProvider{reviewRefundProvider: &reviewRefundProvider{}, result: ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "controlled-original-success", AmountFen: refund.AmountFen, Status: "SUCCESS"}}
	c.provider = provider
	before := refundRecoveryMoneySnapshot(t, ctx, r)
	got, err := c.processRefund(ctx, refund.ID, true)
	if !errors.Is(err, ErrConflict) || got.Status != "MANUAL_REVIEW" || got.ChannelRefundID == nil || provider.calls != 0 || provider.queries != 1 || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
		t.Fatal("read-only recovery bypassed competing source", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_evidence WHERE payment_order_id=$1 AND provider_refund_id=$2 AND provider_status='SUCCESS'`, order.ID, *got.ChannelRefundID).Scan(&count); err != nil || count != 1 {
		t.Fatal("competing source lost actual SUCCESS evidence", err)
	}
}

func TestRefundNeverDispatchedReviewCannotBecomeNewProviderRequestPostgres(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 100, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "never-dispatched manual review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE refunds SET status='MANUAL_REVIEW' WHERE id=$1`, refund.ID); err != nil {
		t.Fatal(err)
	}
	provider := &refundRecoveryQueryProvider{reviewRefundProvider: &reviewRefundProvider{}, result: ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: "must-not-query", AmountFen: refund.AmountFen, Status: "SUCCESS"}}
	c.provider = provider
	money, effects := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
	if _, err := c.processRefund(ctx, refund.ID, true); !errors.Is(err, ErrConflict) || provider.calls != 0 || provider.queries != 0 || effects != refundEffectSnapshot(t, ctx, r) || !reflect.DeepEqual(money, refundRecoveryMoneySnapshot(t, ctx, r)) {
		t.Fatal("never-dispatched review created external operation", err)
	}
}

func TestRefundPersistedSuccessReferenceUsesPublishedCanonicalIdentityPostgres(t *testing.T) {
	const reference = "published-original-refund-reference"
	for _, tc := range []struct{ name, raw string }{
		{"spaces", " " + reference + " "},
		{"tabs", "\t" + reference + "\t"},
		{"NBSP", "\u00a0" + reference + "\u00a0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Actual F09 complete correction also retains a legal zero source, and
			// the original request retains a real MEMBER_REMOVED grant snapshot.
			r, ctx, c, order, transaction, pack := correctedRefundFixture(t, 0, 100, true)
			refundRecoveryGrant(t, r, ctx, order, pack, "MEMBER_REMOVED")
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			if _, err := r.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='RESULT_UNKNOWN' WHERE id=$1`, refund.ID); err != nil {
				t.Fatal(err)
			}
			original := ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, ChannelRefundID: tc.raw, AmountFen: refund.AmountFen, Status: "SUCCESS"}
			provider := &refundRecoveryQueryProvider{reviewRefundProvider: &reviewRefundProvider{}, result: original}
			c.provider = provider
			money := refundRecoveryMoneySnapshot(t, ctx, r)
			got, err := c.processRefund(ctx, refund.ID, true)
			if !errors.Is(err, ErrConflict) || got.Status != "MANUAL_REVIEW" || got.ChannelRefundID == nil || *got.ChannelRefundID != tc.raw || provider.queries != 1 || provider.calls != 0 || !reflect.DeepEqual(money, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("accepted original raw SUCCESS did not preserve financial facts", err)
			}
			encoded, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			digest := digestHex(encoded)
			evidenceFacts := func() string {
				var facts string
				if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_array(to_jsonb(refund),to_jsonb(evidence))::text
 FROM refunds refund JOIN payment_evidence evidence ON evidence.payment_order_id=refund.payment_order_id
 AND evidence.payment_transaction_id=refund.payment_transaction_id
 WHERE refund.id=$1 AND evidence.provider_refund_id=$2 AND evidence.provider_status='SUCCESS'
 AND refund.channel_refund_id=$3 AND evidence.payload_snapshot->>'channel_refund_id'=$3 AND evidence.payload_digest=$4`, refund.ID, reference, tc.raw, digest).Scan(&facts); err != nil {
					t.Fatal("raw refund/payload or published evidence identity changed", err)
				}
				return facts
			}
			// Only this in-memory evidence selector uses its published key. The
			// real owner plan rereads the original raw refund and payload from DB.
			selector := got
			canonicalReference := strings.TrimSpace(*got.ChannelRefundID)
			selector.ChannelRefundID = &canonicalReference
			input := refundRecoveryEscalate(t, ctx, r, selector, order)
			beforeEffects, beforeEvidence := refundEffectSnapshot(t, ctx, r), evidenceFacts()
			retried, retryErr := c.processRefund(ctx, refund.ID, true)
			afterEffects, afterEvidence := refundEffectSnapshot(t, ctx, r), evidenceFacts()
			result, applyErr := r.ApplyRefundExceptionRepair(ctx, input)
			if !errors.Is(retryErr, ErrConflict) || retried.ChannelRefundID == nil || *retried.ChannelRefundID != tc.raw || provider.queries != 1 || provider.calls != 0 || beforeEffects != afterEffects || beforeEvidence != afterEvidence || applyErr != nil || result.Status != "SUCCEEDED" {
				t.Fatal("persisted SUCCESS requery changed frozen facts or invalidated original approval", provider.queries, "facts_changed", beforeEffects != afterEffects || beforeEvidence != afterEvidence, "apply_error", applyErr, "retry_error", retryErr)
			}
			refundRecoveryExactPostings(t, ctx, r, result, 100)
			terminalEffects, terminalMoney, terminalEvidence := refundEffectSnapshot(t, ctx, r), refundRecoveryMoneySnapshot(t, ctx, r), evidenceFacts()
			replayed, err := r.ApplyRefundExceptionRepair(ctx, input)
			if err != nil || replayed != result || terminalEffects != refundEffectSnapshot(t, ctx, r) || !reflect.DeepEqual(terminalMoney, refundRecoveryMoneySnapshot(t, ctx, r)) || terminalEvidence != evidenceFacts() {
				t.Fatal("original approval key replay changed raw evidence or money", err)
			}
			if _, err := c.processRefund(ctx, refund.ID, true); err != nil || provider.queries != 1 || provider.calls != 0 || terminalEffects != refundEffectSnapshot(t, ctx, r) {
				t.Fatal("terminal receipt repeated original provider query", err)
			}
		})
	}
}

// Exercise the ordinary revoke producer at the supported retained-refund
// boundary, including a mixed component that defeats the old bucket-only gate.
func refundFrozenQuotaOrdinaryRevokeRegression(t *testing.T) {
	for _, scenario := range []string{"current-active-freeze", "legacy-removed-freeze", "legacy-mixed-active-target", "unfrozen-active", "unfrozen-removed"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedOrganizationFinanceOrder(t, r, ctx, 100, true)
			reason := ""
			if scenario == "legacy-removed-freeze" || scenario == "legacy-mixed-active-target" || scenario == "unfrozen-removed" {
				reason = "MEMBER_REMOVED"
			}
			grant := refundRecoveryGrant(t, r, ctx, order, pack, reason)
			if scenario == "legacy-mixed-active-target" {
				grant = refundRecoveryGrant(t, r, ctx, order, pack, "")
			}
			actor, key := uuid.New(), uuid.NewString()
			commandReason := "ordinary quota revocation regression"
			if strings.HasPrefix(scenario, "unfrozen-") {
				result, err := r.RevokeMemberQuota(ctx, *order.BeneficiaryOrganizationID, grant.ID, actor, commandReason, key)
				if err != nil || result.Status != "REVOKED" || result.AvailableMicrocredits != 0 || result.RevokedMicrocredits != 10 {
					t.Fatal("valid ordinary revocation regressed", err)
				}
				after := refundRecoveryMoneySnapshot(t, ctx, r)
				receipt := refundRecoveryRevokeReceiptSnapshot(t, ctx, r)
				replayed, err := r.RevokeMemberQuota(ctx, *order.BeneficiaryOrganizationID, grant.ID, actor, commandReason, key)
				if err != nil || !sameRefundRecoveryGrantReceipt(result, replayed) || !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || !reflect.DeepEqual(receipt, refundRecoveryRevokeReceiptSnapshot(t, ctx, r)) {
					t.Fatal("original ordinary revoke receipt changed on retry", err)
				}
				if _, err := r.RevokeMemberQuota(ctx, *order.BeneficiaryOrganizationID, grant.ID, actor, commandReason+" changed", key); !errors.Is(err, ErrIdempotencyConflict) {
					t.Fatal("changed ordinary revoke facts reused original key", err)
				}
				if !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || !reflect.DeepEqual(receipt, refundRecoveryRevokeReceiptSnapshot(t, ctx, r)) {
					t.Fatal("rejected key conflict changed funds")
				}
				return
			}
			refund := refundRecoveryLegacyReservation(t, ctx, r, order, transaction, pack)
			before := refundRecoveryMoneySnapshot(t, ctx, r)
			if _, err := r.RevokeMemberQuota(ctx, *order.BeneficiaryOrganizationID, grant.ID, actor, commandReason, key); !errors.Is(err, ErrConflict) {
				t.Fatal("ordinary revoke accepted a financial freeze", err)
			}
			if !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
				t.Fatal("rejected ordinary revoke changed frozen funds or grants")
			}
			var commands int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_revoke_commands WHERE organization_id=$1 AND idempotency_key=$2`, *order.BeneficiaryOrganizationID, key).Scan(&commands); err != nil || commands != 0 {
				t.Fatal("rejected ordinary revoke wrote a success receipt", err)
			}
			provider := &reviewRefundProvider{refund: ProviderRefund{Status: "SUCCESS", ChannelRefundID: "original-frozen-quota-success"}}
			confirmed, err := provider.Refund(ctx, order.MerchantOrderNo, refund.MerchantRefundNo, refund.AmountFen)
			if err != nil {
				t.Fatal(err)
			}
			refund, _, err = c.finishRefund(ctx, refund, confirmed, false)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "current-active-freeze" {
				if refund.Status != "SUCCEEDED" {
					t.Fatal("original active-grant refund no longer settles", refund.Status)
				}
			} else {
				if refund.Status != "MANUAL_REVIEW" || !reflect.DeepEqual(before, refundRecoveryMoneySnapshot(t, ctx, r)) {
					t.Fatal("retained prior suspension must preserve funds before governed repair")
				}
				input := refundRecoveryEscalate(t, ctx, r, refund, order)
				result, err := r.ApplyRefundExceptionRepair(ctx, input)
				if err != nil || result.Status != "SUCCEEDED" || result.RevokedMicrocredits != 100 || result.ShortfallMicrocredits != 0 {
					t.Fatal("ordinary refusal failed to preserve full original refund repair", err)
				}
				refundRecoveryExactPostings(t, ctx, r, result, 100)
				after, receipt := refundRecoveryMoneySnapshot(t, ctx, r), refundEffectSnapshot(t, ctx, r)
				replayed, err := r.ApplyRefundExceptionRepair(ctx, input)
				if err != nil || replayed != result || !reflect.DeepEqual(after, refundRecoveryMoneySnapshot(t, ctx, r)) || receipt != refundEffectSnapshot(t, ctx, r) {
					t.Fatal("same original repair no longer replays without a second effect", err)
				}
			}
			var posted int64
			if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted); err != nil || posted != 0 {
				t.Fatal("original refund left customer balance after full reclaim", err)
			}
		})
	}
}

// Retain complete persisted command/outbox facts as well as the original
// financial and audit table snapshots across replay and rejected-key reuse.
func refundRecoveryRevokeReceiptSnapshot(t *testing.T, ctx context.Context, r *PostgresRepository) map[string]string {
	t.Helper()
	facts := map[string]string{}
	for _, table := range []string{"member_quota_revoke_commands", "outbox_events"} {
		var raw string
		if err := r.pool.QueryRow(ctx, "SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM "+table+" x").Scan(&raw); err != nil {
			t.Fatal(err)
		}
		facts[table] = raw
	}
	return facts
}
