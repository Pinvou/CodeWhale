package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"sync/atomic"
	"testing"
	"time"
)

func TestF09ExpiryAfterComponentLockPostgres(t *testing.T) {
	repository, ctx := retainedFinanceRepository(t)
	t.Logf("owned expiry fixture database=%s owner=billing_schema_owner", repository.pool.Config().ConnConfig.Database)
	// Use the original deployment-qualified operator before starting the short
	// expiry clock; runtime credentials cannot manufacture command activation.
	operatorConfig := repository.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "billing_migrator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal("activation operator connection failed")
	}
	defer operator.Close(ctx)
	activationTx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal("activation operator transaction failed")
	}
	defer activationTx.Rollback(ctx)
	if _, err := activationTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal("activation operator qualification failed")
	}
	activated, err := activationTx.Exec(ctx, `UPDATE public.billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2`)
	if err != nil || activated.RowsAffected() != 1 {
		t.Fatal("activation operator update failed")
	}
	if err := activationTx.Commit(ctx); err != nil {
		t.Fatal("activation operator commit failed")
	}
	walletID, ownerID, orderID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	lotID, packageID, walletAccountID, clearingAccountID, sourceJournalID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	var now, expires time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT clock_timestamp(),clock_timestamp()+interval '2 seconds'`).Scan(&now, &expires); err != nil {
		t.Fatal(err)
	}
	reference := "payment-transaction:" + transactionID.String()
	product, err := json.Marshal(Product{ProductID: "CORRECTION_EXECUTE_TEST", AmountFen: 100, CreditMicrocredits: 100, BasePoints: 80, BonusPoints: 20, Currency: "CNY"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,$3,$3)`, []any{walletID, ownerID, now}},
		{`INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, []any{walletAccountID, walletID, "correction-execute-wallet:" + walletID.String(), now}},
		{`INSERT INTO ledger_accounts(id,code,account_type,created_at) VALUES($1,$2,'PAYMENT_CLEARING',$3)`, []any{clearingAccountID, "correction-execute-clearing:" + walletID.String(), now}},
		{`INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,'CORRECTION_EXECUTE_TEST',$5,'ALIPAY',100,'CNY',100,'PAID','CREDITED',$6,$6,$7,$8,$9,$9,'test-provider','v1')`, []any{orderID, "correction-execute-" + orderID.String(), ownerID, walletID, product, now.Add(time.Hour), now.Add(72 * time.Hour), now.Add(8 * 24 * time.Hour), now}},
		{`INSERT INTO payment_transactions(id,payment_order_id,channel,provider_transaction_id,status,amount_fen,currency,source,provider_paid_at,verified_at,created_at,updated_at) VALUES($1,$2,'ALIPAY',$3,'PAID',100,'CNY','QUERY',$4,$4,$4,$4)`, []any{transactionID, orderID, "correction-execute-txn-" + transactionID.String(), now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, reference, expires, now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'CORRECTION_EXECUTE_TEST','Test','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{packageID, ownerID, walletID, orderID, transactionID, lotID, now, expires, reference}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{lotID, packageID, now, expires}},
		{`INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'RECHARGE',$2,'fixture',$3)`, []any{sourceJournalID, reference, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at) VALUES($1,$2,$3,100,$4,$5)`, []any{uuid.New(), sourceJournalID, walletAccountID, lotID, now}},
		{`INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) VALUES($1,$2,$3,-100,$4)`, []any{uuid.New(), sourceJournalID, clearingAccountID, now}},
	}
	for index, item := range statements {
		if _, err := tx.Exec(ctx, item.sql, item.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	var clock atomic.Int64
	clock.Store(now.UnixNano())
	repository.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	plan, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil {
		t.Fatal(err)
	}
	frozen := entitlementCorrectionFrozenFacts{WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PaymentTransactionID: plan.PaymentTransactionID, SourceComponentID: plan.SourceComponentID, SourceLotID: plan.SourceLotID, TargetComponentID: plan.TargetComponentID, TargetLotID: plan.TargetLotID, SourceType: plan.Amounts.SourceType, TargetType: plan.Amounts.TargetType, AmountMicrocredits: plan.Amounts.Amount, SourceEvidenceDigest: plan.SourceEvidenceDigest, FactsDigest: plan.FactsDigest}
	facts := func() string {
		var s string
		err := repository.pool.QueryRow(ctx, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM wallets x),'packages',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_packages x),'components',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_components x),'lots',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM credit_lots x),'journal',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM journal_entries x),'postings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM ledger_postings x),'results',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM entitlement_component_corrections x))::text`).Scan(&s)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := facts()
	locker, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	var locked uuid.UUID
	var blocker int
	if err = locker.QueryRow(ctx, `SELECT id,pg_backend_pid() FROM entitlement_components WHERE id=$1 FOR UPDATE`, lotID).Scan(&locked, &blocker); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	requestID := uuid.New()
	go func() {
		_, e := repository.ExecuteEntitlementComponentCorrection(ctx, requestID, uuid.New(), 2, packageID, orderID, uuid.New(), uuid.New(), "approval:"+requestID.String()+":ENTITLEMENT_COMPONENT_ADJUSTMENT", "independent approval completed", "ORIGINAL_SPLIT_MISMATCH", frozen)
		done <- e
	}()
	deadline := time.Now().Add(15 * time.Second)
	observed := false
	var serverNow time.Time
	for time.Now().Before(deadline) {
		var waiting bool
		if err = repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%entitlement_components%'),clock_timestamp()`, blocker).Scan(&waiting, &serverNow); err != nil {
			t.Fatal(err)
		}
		if waiting {
			observed = true
			break
		}
		select {
		case e := <-done:
			t.Fatalf("owner finished before actual component lock wait: %v", e)
		default:
		}
	}
	if !observed || !serverNow.Before(expires) {
		t.Fatalf("expiry fixture did not observe actual lock wait before expiry: waiting=%v db=%s expiry=%s", observed, serverNow, expires)
	}
	for serverNow.Before(expires) && time.Now().Before(deadline) {
		if err = repository.pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&serverNow); err != nil {
			t.Fatal(err)
		}
	}
	if serverNow.Before(expires) {
		t.Fatal("database clock did not cross common component/lot/package expiry")
	}
	clock.Store(serverNow.UnixNano())
	t.Logf("real component wait blocker=%d observed before common expiry; DBclock=%s expiry=%s; controlled repository clock advanced to exact observed DBtime", blocker, serverNow, expires)
	if err = locker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		if !errors.Is(e, ErrApprovalSnapshotMismatch) {
			t.Fatalf("expired after lock must reject with zero effect: err=%v unchanged=%v", e, facts() == before)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if facts() != before {
		t.Fatal("expired correction changed wallet/component/lot/journal/posting or immutable result")
	}
}
