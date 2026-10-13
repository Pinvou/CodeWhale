package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestF12PaymentActivationFinancialTransactionPostgres(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)
	pool := repository.pool
	setActivation := paymentFixtureActivation(t, ctx, pool)
	activationVersion := setActivation("ACTIVE")
	provider := &paymentRepairStubProvider{}
	coordinator, err := NewPaymentCoordinatorWithProvider(repository, provider)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The approving identity the approval froze; every approval-driven audit row
	// must carry it as the actor.
	approvingUserID := uuid.New()
	requestingUserID := uuid.New()

	seed := func(amountFen, credits, basePoints, bonusPoints int64, mergeResult, providerStatus string) (uuid.UUID, uuid.UUID, uuid.UUID) {
		orderID, walletID, evidenceID := uuid.New(), uuid.New(), uuid.New()
		walletAccountID := uuid.New()
		// Package issuance requires the personal wallet owner to be the order
		// requester.
		ownerID := uuid.New()
		product, err := json.Marshal(Product{ProductID: "PAYMENT_REPAIR", AmountFen: amountFen, CreditMicrocredits: credits,
			BasePoints: basePoints, BonusPoints: bonusPoints, Currency: "CNY", PlanName: "Repair"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,version,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',0,1,$3,$3)`, walletID, ownerID, now); err != nil {
			t.Fatal(err)
		}
		// Package issuance posts the wallet leg against the wallet liability
		// account, exactly as the real fulfillment path requires.
		if _, err := pool.Exec(ctx, `INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at)
VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, walletAccountID, walletID, "wallet:"+walletID.String(), now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,
amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,
created_at,updated_at,provider_sku,provider_id,provider_version)
VALUES($1,$2,$3,$3,$4,'PAYMENT_REPAIR',$5,'ALIPAY',$6,'CNY',$7,'MANUAL_REVIEW','MANUAL_REVIEW',$8,$8,$9,$10,$11,$11,'sku-repair','test-provider','v1')`,
			orderID, "repair-"+orderID.String(), ownerID, walletID, product, amountFen, credits,
			now.Add(time.Hour), now.Add(72*time.Hour), now.Add(8*24*time.Hour), now); err != nil {
			t.Fatal(err)
		}
		providerFacts, err := json.Marshal(map[string]any{"external_customer_key": ownerID.String(), "external_sku": "sku-repair"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO payment_evidence(id,payment_order_id,evidence_type,provider_id,provider_version,evidence_key,
merchant_order_no,provider_transaction_id,amount_fen,currency,provider_status,provider_event_at,received_at,payload_digest,payload_snapshot,merge_result)
VALUES($1,$2,'QUERY','test-provider','v1',$3,$4,$5,$6,'CNY',$7,$8,$8,$9,$11,$10)`,
			evidenceID, orderID, "query:"+evidenceID.String(), "repair-"+orderID.String(), "trade-"+evidenceID.String(),
			amountFen, providerStatus, now, strings.Repeat("ab", 32), mergeResult, providerFacts); err != nil {
			t.Fatal(err)
		}
		// The matching success evidence and the MANUAL_REVIEW order state are
		// produced by a review-required transaction (late or duplicate payment);
		// the approved repair reuses the existing repair branch on that row.
		if _, err := pool.Exec(ctx, `INSERT INTO payment_transactions(id,payment_order_id,provider_id,provider_version,channel,
provider_transaction_id,status,fulfillment_status,amount_fen,currency,source,provider_status,provider_paid_at,verified_at,created_at,updated_at)
VALUES($1,$2,'test-provider','v1','ALIPAY',$3,'REVIEW_REQUIRED','REVIEW_REQUIRED',$4,'CNY','QUERY','PAY_SUCCESS',$5,$5,$5,$5)`,
			uuid.New(), orderID, "trade-"+evidenceID.String(), amountFen, now); err != nil {
			t.Fatal(err)
		}
		return orderID, walletID, evidenceID
	}
	orderID, walletID, evidenceID := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	command := PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}
	plan, err := paymentFinancialRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.AmountFen != 100 || plan.CreditMicrocredits != 100 || plan.BaseAmount != 80 || plan.BonusAmount != 20 ||
		plan.AppliedToReceivables != 0 || plan.FactsDigest == "" || plan.Provider.Status != "SUCCESS" ||
		plan.Provider.ChannelTransactionID == nil || plan.Provider.PaidAt == nil {
		t.Fatalf("unexpected plan %+v", plan)
	}

	frozen := func(plan PaymentFinancialRepairPlan) PaymentFinancialRepairFrozenFacts {
		return PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}
	}
	input := PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: approvingUserID, RequesterUserID: requestingUserID, ExecutionKey: "payment-race:legacy-compat", PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: frozen(plan)}
	result, err := paymentRepairWithDeactivationRace(t, ctx, coordinator, input)
	if err != nil || result.Status != "SUCCEEDED" || result.CreditMicrocredits != 100 || provider.calls != 0 {
		t.Fatal("qualified payment race failed", err)
	}
	var repairs, journals, postings, posted int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1),(SELECT count(*) FROM journal_entries WHERE id=$2),(SELECT count(*) FROM ledger_postings WHERE journal_entry_id=$2),(SELECT posted_microcredits FROM wallets WHERE id=$3)`, orderID, result.JournalEntryID, walletID).Scan(&repairs, &journals, &postings, &posted); err != nil || repairs != 1 || journals != 1 || postings != 3 || posted != 100 {
		t.Fatalf("actual committed facts repairs=%d journals=%d postings=%d posted=%d", repairs, journals, postings, posted)
	}
	freshOrder, _, freshEvidence := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
	freshPlan, err := paymentFinancialRepairPlanFrom(ctx, pool, PaymentFinancialRepairCommandInput{PaymentOrderID: freshOrder, PaymentEvidenceID: freshEvidence}, 1, 1, activationVersion)
	if err != nil {
		t.Fatal(err)
	}
	fresh := input
	fresh.ApprovalRequestID = uuid.New()
	fresh.AuthorizationEventID = uuid.New()
	fresh.ExecutionKey = "payment-fresh:" + uuid.NewString()
	fresh.Command = PaymentFinancialRepairCommandInput{PaymentOrderID: freshOrder, PaymentEvidenceID: freshEvidence}
	fresh.Frozen = frozen(freshPlan)
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, fresh); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatal("inactive fresh execution must reject", err)
	}
	setActivation("ACTIVE")
	if _, err := coordinator.ApplyPaymentFinancialRepair(ctx, fresh); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatal("old activation version must reject", err)
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE payment_order_id=$1`, freshOrder).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected fresh effect wrote a receipt")
	}
	if replay, err := coordinator.ApplyPaymentFinancialRepair(ctx, input); err != nil || replay != result {
		t.Fatal("old receipt no longer replays after operator transitions")
	}
	t.Log("operator waited until real financial commit; inactive/version mismatch wrote zero receipts; old receipt remains read-only")

	t.Run("operator-first-lock-rejects-stale-serializable-apply", func(t *testing.T) {
		activationVersion = setActivation("ACTIVE")
		orderID, _, evidenceID := seed(100, 100, 80, 20, "RECORDED", "PAY_SUCCESS")
		command := PaymentFinancialRepairCommandInput{PaymentOrderID: orderID, PaymentEvidenceID: evidenceID}
		plan, err := paymentFinancialRepairPlanFrom(ctx, pool, command, 1, 1, activationVersion)
		if err != nil {
			t.Fatal("reverse race requires a valid current frozen plan", err)
		}
		reverse := input
		reverse.ApprovalRequestID = uuid.New()
		reverse.AuthorizationEventID = uuid.New()
		reverse.ExecutionKey = "payment-operator-first:" + uuid.NewString()
		reverse.ActivationVersion = activationVersion
		reverse.Command = command
		reverse.Frozen = frozen(plan)
		snapshot := func() map[string]string {
			facts := entitlementFinancialFactsSnapshot(t, ctx, repository)
			for _, table := range []string{"payment_evidence", "payment_order_repairs", "ledger_accounts"} {
				var rows string
				if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(row ORDER BY row::text),'[]'::jsonb)::text FROM (SELECT to_jsonb(f) AS row FROM `+table+` f) facts`).Scan(&rows); err != nil {
					t.Fatal("reverse race complete financial snapshot failed", err)
				}
				facts[table] = rows
			}
			return facts
		}
		before := snapshot()
		raceCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		defer stop()
		operator := ledgerActivationFixtureOperator(t, raceCtx, pool)
		observer := ledgerActivationFixtureOperator(t, raceCtx, pool)
		// The migrator has SET membership but deliberately does not inherit the
		// service role. Observe that role's query text under its existing authority.
		if _, err := observer.Exec(raceCtx, `SET ROLE billing_service`); err != nil {
			t.Fatal("observer cannot assume its existing Billing service role", err)
		}
		var sessionActor, currentActor string
		var superuser bool
		if err := observer.QueryRow(raceCtx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&sessionActor, &currentActor, &superuser); err != nil || sessionActor != "billing_migrator" || currentActor != "billing_service" || superuser {
			t.Fatal("reverse observer identity must remain the non-super fixture migrator acting as service", err)
		}
		operatorTx, err := operator.Begin(raceCtx)
		if err != nil {
			t.Fatal(err)
		}
		defer operatorTx.Rollback(context.Background())
		if _, err := operatorTx.Exec(raceCtx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
			t.Fatal(err)
		}
		var nextVersion int64
		if err := operatorTx.QueryRow(raceCtx, `UPDATE public.billing_approval_command_activations SET state='INACTIVE',version=version+1 WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1 RETURNING version`).Scan(&nextVersion); err != nil || nextVersion != activationVersion+1 {
			t.Fatal("operator must hold the exact installed activation transition", err)
		}
		var operatorPID int
		if err := operatorTx.QueryRow(raceCtx, `SELECT pg_backend_pid()`).Scan(&operatorPID); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			result PaymentFinancialRepairResult
			err    error
		}
		applied := make(chan outcome, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			result, err := coordinator.ApplyPaymentFinancialRepair(raceCtx, reverse)
			applied <- outcome{result, err}
		}()
		defer func() {
			_ = operatorTx.Rollback(context.Background())
			stop()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("reverse Apply did not retire before fixture cleanup")
			}
		}()
		blocked := false
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-applied:
				t.Fatal("Apply returned before the operator activation lock was released")
			default:
			}
			if _, err := observer.Exec(raceCtx, `SELECT pg_stat_clear_snapshot()`); err != nil {
				t.Fatal(err)
			}
			if err := observer.QueryRow(raceCtx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename='billing_service' AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%billing_approval_execution.lock_payment_activation%')`, operatorPID).Scan(&blocked); err != nil {
				t.Fatal("reverse runtime activation wait observation failed", err)
			}
			if blocked {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !blocked {
			t.Fatal("real Apply did not wait on the operator's installed activation row")
		}
		if err := operatorTx.Commit(raceCtx); err != nil {
			t.Fatal("operator transition failed to commit", err)
		}
		var got outcome
		select {
		case got = <-applied:
		case <-raceCtx.Done():
			t.Fatal("reverse Apply did not complete within the bounded fixture budget")
		}
		var pgErr *pgconn.PgError
		if !errors.As(got.err, &pgErr) || pgErr.Code != "40001" || got.result != (PaymentFinancialRepairResult{}) {
			t.Fatalf("stale serializable Apply must preserve its typed rejection and no receipt: %v", got.err)
		}
		if !reflect.DeepEqual(snapshot(), before) {
			t.Fatal("operator-first rejection changed a financial/effect/repair row")
		}
		var repairs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM payment_order_repairs WHERE approval_request_id=$1`, reverse.ApprovalRequestID).Scan(&repairs); err != nil || repairs != 0 {
			t.Fatal("operator-first rejection wrote an execution receipt", err)
		}
		var state string
		var version int64
		if err := pool.QueryRow(ctx, `SELECT state,version FROM billing_approval_command_activations WHERE command_type='PAYMENT_FINANCIAL_REPAIR' AND policy_version=1 AND snapshot_schema_version=1`).Scan(&state, &version); err != nil || state != "INACTIVE" || version != nextVersion || provider.calls != 0 {
			t.Fatal("operator transition or provider-free rejection contract changed", err)
		}
	})
}
