package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reconciliationRepairCrossDomainFacts names the seeded provider bill row that
// never matched an internal order, together with its provider-side refund
// evidence.
type reconciliationRepairCrossDomainFacts struct {
	CaseID               uuid.UUID
	RunID                uuid.UUID
	EvidenceID           uuid.UUID
	MerchantOrderNo      string
	ChannelTransactionID string
	AmountFen            int64
	Currency             string
	BusinessDate         string
}

// newReconciliationRepairCrossDomainBillingFixture provisions a
// production-shaped Billing database for the cross-domain reconciliation
// closure gate. Activation stays exactly as deployed: the command remains the
// original INACTIVE v1/s1 row.
func newReconciliationRepairCrossDomainBillingFixture(t *testing.T, ctx context.Context, databaseURL string) (*billingdomain.PostgresRepository, *pgxpool.Pool, *pgx.Conn, reconciliationRepairCrossDomainFacts) {
	t.Helper()
	repository, pool, connection := newEntitlementCrossDomainBillingFixture(t, ctx, databaseURL)
	facts := seedReconciliationRepairCrossDomainCase(t, ctx, pool)
	return repository, pool, connection, facts
}

// seedReconciliationRepairCrossDomainCase writes the production-shaped
// difference the closed plan accepts: a completed reconciliation run with one
// OPEN PROVIDER_PAID_INTERNAL_MISSING case whose merchant order has no internal
// order, plus the provider bill refund row that disposed of the amount.
func seedReconciliationRepairCrossDomainCase(t *testing.T, ctx context.Context, pool *pgxpool.Pool) reconciliationRepairCrossDomainFacts {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	facts := reconciliationRepairCrossDomainFacts{
		CaseID: uuid.New(), RunID: uuid.New(), EvidenceID: uuid.New(),
		MerchantOrderNo:      "unmatched-" + uuid.NewString(),
		ChannelTransactionID: "channel-tx-" + uuid.NewString(),
		AmountFen:            1500, Currency: "CNY",
		BusinessDate: now.Format("2006-01-02"),
	}
	snapshot, err := json.Marshal(map[string]any{
		"entry_type": "PAYMENT", "merchant_order_no": facts.MerchantOrderNo,
		"channel_transaction_id": facts.ChannelTransactionID, "channel": "ALIPAY",
		"amount_fen": facts.AmountFen, "currency": facts.Currency, "status": "SUCCESS",
		"paid_at": now.Format(time.RFC3339Nano),
	})
	if err != nil {
		t.Fatal(err)
	}
	refundReference := "refund-" + uuid.NewString()
	refundRaw, err := json.Marshal(billingdomain.ProviderBillEntry{
		EntryType: "REFUND", ProviderReferenceID: refundReference,
		MerchantOrderNo: facts.MerchantOrderNo, ChannelTransactionID: facts.ChannelTransactionID,
		Channel: "ALIPAY", AmountFen: facts.AmountFen, Currency: facts.Currency,
		Status: "SUCCESS", PaidAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	refundDigest := sha256.Sum256(refundRaw)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO reconciliation_runs(id,channel,business_date,status,provider_row_count,case_count,started_at,completed_at,
provider_id,provider_version,source_mode)
VALUES($1,'ALIPAY',$2::date,'COMPLETED',1,1,$3,$3,'test-provider','v1','MANUAL_API')`,
			[]any{facts.RunID, facts.BusinessDate, now}},
		{`INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,
claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at)
VALUES($1,'BILLING','RECONCILIATION_DIFFERENCE','RECONCILIATION_CASE',$1,'P1','OPEN',$2,$3,$3,$3,$3,$2)`,
			[]any{facts.CaseID, now, now.Add(time.Hour)}},
		{`INSERT INTO reconciliation_cases(id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,
severity,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,created_at,updated_at,manual_review_case_id)
VALUES($1,$2,NULL,$3,'PROVIDER_PAID_INTERNAL_MISSING','OPEN','P1',$4,NULL,NULL,NULL,$5,$5,$1)`,
			[]any{facts.CaseID, facts.RunID, facts.MerchantOrderNo, snapshot, now}},
		{`INSERT INTO payment_evidence(id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,
evidence_key,merchant_order_no,provider_transaction_id,provider_refund_id,amount_fen,currency,provider_status,
provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,NULL,NULL,'BILL','test-provider','v1',$2,$3,$4,$5,$6,'CNY','SUCCESS',$7,$7,$8,$9,'REVIEW_REQUIRED')`,
			[]any{facts.EvidenceID, "REFUND|" + refundReference + "|" + facts.MerchantOrderNo + "|" + now.Format(time.RFC3339Nano),
				facts.MerchantOrderNo, facts.ChannelTransactionID, refundReference,
				facts.AmountFen, now, hex.EncodeToString(refundDigest[:]), refundRaw}},
	} {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("reconciliation repair fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return facts
}

func seedReconciliationAdvancementRequest(t *testing.T, ctx context.Context, repo *PostgresRepository, user uuid.UUID, policy approvalpolicy.Policy, status string, frozen approvalencoding.LegacyProofInput) uuid.UUID {
	t.Helper()
	id := uuid.New()
	binding, authorizationVersion, _, err := strongPlatformAuthorization(ctx, repo.pool, user, policy.RequesterPermission)
	if err != nil {
		t.Fatal(err)
	}
	_, err = repo.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,expires_at,submitted_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$9,$6,$10,decode(repeat('00',32),'hex'),'RECONCILIATION_CASE',$7,'controlled historical draft','[]','{}',$8,now()+interval '1 hour',CASE WHEN $8='SUBMITTED' THEN now() ELSE NULL END,now(),now())`, id, policy.CommandType, policy.PolicyVersion, user, binding, "historical-"+id.String(), frozen.TargetResourceID, status, authorizationVersion, frozen.NormalizedCommand)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,'{}','{}',now())`, id, policy.SnapshotSchemaVersion, frozen.CriticalResourceVersions, frozen.FinancialSnapshot); err != nil {
		t.Fatal(err)
	}
	return id
}

// Capture complete financial rows after legitimate fixture seeding. Closure may
// resolve its case, but cannot mutate any of these financial facts.
func reconciliationCrossDomainFinancialSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var snapshot string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_array(
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM receivables x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_orders x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_transactions x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM payment_evidence x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM refunds x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM chargeback_cases x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM forced_refund_cases x),
(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM usage_holds x))::text`).Scan(&snapshot); err != nil {
		t.Fatal("complete financial snapshot query failed")
	}
	return snapshot
}
