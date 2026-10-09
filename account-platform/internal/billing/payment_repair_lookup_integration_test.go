package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"os"
	"strings"
	"testing"
	"time"
)

func TestF12PaymentRepairOrderLookupPostgres(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, _ := entitlementRepositoryWithStage(t, ctx, databaseURL, func(loaded []migrations.Migration) ([]migrations.Migration, error) {
		return selectF12PaymentLookupStage(loaded, 900016)
	})
	pool := repository.pool
	var installedIndex, installedLedger string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(pg_get_indexdef(indexrelid),';' ORDER BY indexrelid),'') FROM pg_index WHERE indrelid='public.payment_order_repairs'::regclass`).Scan(&installedIndex); err != nil {
		t.Fatal("actual index catalog read failed")
	}
	if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(version::text,',' ORDER BY version),'') FROM schema_migrations WHERE version>=900013`).Scan(&installedLedger); err != nil {
		t.Fatal("actual installed stage read failed")
	}
	t.Logf("actual installed high stage=%s repair_indexes=%s", installedLedger, installedIndex)
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

	input := PaymentFinancialRepairExecutionInput{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 2, ApproverUserID: approvingUserID, RequesterUserID: requestingUserID, ExecutionKey: "lookup-seed:" + uuid.NewString(), PolicyVersion: 1, SchemaVersion: 1, ActivationVersion: activationVersion, Command: command, Frozen: PaymentFinancialRepairFrozenFacts{OrderUpdatedAt: plan.OrderUpdatedAt, AmountFen: plan.AmountFen, CreditMicrocredits: plan.CreditMicrocredits, ProviderTransactionID: plan.ProviderTransactionID, WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, AppliedToReceivables: plan.AppliedToReceivables, FactsDigest: plan.FactsDigest}}
	result, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
	if err != nil {
		t.Fatal("real source repair failed", err)
	}
	var sourcePosted int64
	if err := pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&sourcePosted); err != nil || sourcePosted != 100 {
		t.Fatal("real source payment conservation differs")
	}
	// The isolated planner dataset copies constraint-valid historical audit shapes.
	// It is not a measurement of provider traffic, new payments or monetary effects.
	const rows = 10000
	clones := []struct {
		table     string
		source    uuid.UUID
		overrides string
		rangeSQL  string
	}{
		{"payment_orders", orderID, `jsonb_build_object('id',md5('lookup-order-'||g)::uuid,'merchant_order_no','lookup-order-'||g,'primary_provider_transaction_id','lookup-primary-'||g)`, `generate_series(0,1999) g`},
		{"payment_transactions", result.PaymentTransactionID, `jsonb_build_object('id',md5('lookup-tx-'||g)::uuid,'payment_order_id',md5('lookup-order-'||((g-1)/5))::uuid,'provider_transaction_id','lookup-trade-'||g)`, `generate_series(1,10000) g`},
		{"credit_lots", uuid.Nil, `jsonb_build_object('id',md5('lookup-lot-'||g)::uuid,'source_reference','lookup-lot:'||g)`, `generate_series(1,10000) g`},
		{"entitlement_packages", result.EntitlementPackageID, `jsonb_build_object('id',md5('lookup-package-'||g)::uuid,'credit_lot_id',md5('lookup-lot-'||g)::uuid,'payment_order_id',md5('lookup-order-'||((g-1)/5))::uuid,'source_payment_transaction_id',md5('lookup-tx-'||g)::uuid)`, `generate_series(1,10000) g`},
		{"payment_order_repairs", result.RepairID, `jsonb_build_object('id',md5('lookup-repair-'||g)::uuid,'payment_order_id',md5('lookup-order-'||((g-1)/5))::uuid,'payment_transaction_id',md5('lookup-tx-'||g)::uuid,'entitlement_package_id',md5('lookup-package-'||g)::uuid,'approval_request_id',NULL,'authorization_event_id',NULL,'approval_epoch',NULL,'execution_key_digest',NULL,'facts_digest',NULL,'requester_user_id',NULL)`, `generate_series(1,10000) g`},
	}
	if err := pool.QueryRow(ctx, `SELECT credit_lot_id FROM entitlement_packages WHERE id=$1`, result.EntitlementPackageID).Scan(&clones[2].source); err != nil {
		t.Fatal(err)
	}
	for _, c := range clones {
		var unknownFields int64
		validateKeys := fmt.Sprintf(`SELECT count(*) FROM %s CROSS JOIN LATERAL jsonb_object_keys(%s) k WHERE NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=$1::regclass AND attname=k AND attnum>0 AND NOT attisdropped)`, c.rangeSQL, c.overrides)
		if err := pool.QueryRow(ctx, validateKeys, "public."+c.table).Scan(&unknownFields); err != nil || unknownFields != 0 {
			t.Fatalf("planner override table=%s unknown_fields=%d query_failed=%t", c.table, unknownFields, err != nil)
		}
		var catalog string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(definition,';' ORDER BY definition),'') FROM (SELECT pg_get_indexdef(indexrelid)||' valid='||indisvalid::text||' unique='||indisunique::text definition FROM pg_index WHERE indrelid=$1::regclass UNION ALL SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid=$1::regclass AND contype='f') facts`, "public."+c.table).Scan(&catalog); err != nil {
			t.Fatal("planner current constraints read failed")
		}
		t.Logf("planner current table=%s constraints=%s", c.table, catalog)
		if c.table == "payment_orders" {
			// Migration 024 renamed the column; its UNIQUE constraint retains
			// the old channel_transaction_id name.
			var generated, distinct int64
			preview := fmt.Sprintf(`SELECT count(*),count(DISTINCT r.primary_provider_transaction_id) FROM public.payment_orders template CROSS JOIN %s CROSS JOIN LATERAL jsonb_populate_record(NULL::public.payment_orders,to_jsonb(template)||%s) r WHERE template.id=$1`, c.rangeSQL, c.overrides)
			if err := pool.QueryRow(ctx, preview, c.source).Scan(&generated, &distinct); err != nil || generated != 2000 || distinct != 2000 {
				t.Fatal("planner order generation uniqueness differs", err)
			}
		}
		query := fmt.Sprintf(`INSERT INTO public.%s SELECT r.* FROM public.%s template CROSS JOIN %s CROSS JOIN LATERAL jsonb_populate_record(NULL::public.%s,to_jsonb(template)||%s) r WHERE template.id=$1`, c.table, c.table, c.rangeSQL, c.table, c.overrides)
		if _, err := pool.Exec(ctx, query, c.source); err != nil {
			t.Fatalf("planner dataset table=%s query_failed=true: %v", c.table, err)
		}
	}
	var count, orders int64
	if err := pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT payment_order_id) FROM payment_order_repairs`).Scan(&count, &orders); err != nil || count != rows+1 || orders != 2001 {
		t.Fatal("representative dataset cardinality differs", err)
	}
	operator := ledgerActivationFixtureOperator(t, ctx, pool)
	if _, err := operator.Exec(ctx, `SET ROLE billing_schema_owner; ANALYZE public.payment_order_repairs; SET ROLE billing_service`); err != nil {
		t.Fatal("planner statistics preparation failed")
	}
	var existing bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=i.indkey[0] WHERE i.indrelid='public.payment_order_repairs'::regclass AND i.indisvalid AND a.attname='payment_order_id')`).Scan(&existing); err != nil || existing {
		t.Fatal("pre-index history already has a leading order index", err)
	}
	target := uuid.New()
	explain := func(label string) string {
		var raw []byte
		if err := pool.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) SELECT EXISTS(SELECT 1 FROM payment_order_repairs WHERE payment_order_id=$1)`, target).Scan(&raw); err != nil {
			t.Fatal("actual owner lookup EXPLAIN failed", err)
		}
		t.Logf("lookup plan %s rows=%d distinct_orders=%d cached_uncontrolled=true %s", label, count, orders, raw)
		return string(raw)
	}
	first := explain("before-first-observed")
	explain("before-warmed")
	if !strings.Contains(first, "Seq Scan") {
		t.Fatal("before plan did not expose the confirmed scan gap")
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	historical, err := selectF12PaymentLookupStage(loaded, 900017)
	if err != nil {
		t.Fatal(err)
	}
	if err := billingschema.Up(ctx, operator, historical); err != nil {
		t.Fatal("official 900016 to 900017 upgrade failed", err)
	}
	migrations.Unlock(ctx, operator)
	if _, err := migrations.VerifyApplied(ctx, operator, historical); err != nil {
		t.Fatal("upgrade ledger verification failed", err)
	}
	after := explain("after-first-observed")
	explain("after-warmed")
	if !strings.Contains(after, "payment_order_repairs_order_lookup_idx") {
		t.Fatal("actual lookup did not use the registered index")
	}
	var valid, unique bool
	var definition string
	if err := pool.QueryRow(ctx, `SELECT i.indisvalid,i.indisunique,pg_get_indexdef(i.indexrelid) FROM pg_index i WHERE i.indexrelid='public.payment_order_repairs_order_lookup_idx'::regclass`).Scan(&valid, &unique, &definition); err != nil || !valid || unique || !strings.Contains(definition, "(payment_order_id)") {
		t.Fatal("lookup index definition differs")
	}
	replay, err := coordinator.ApplyPaymentFinancialRepair(ctx, input)
	if err != nil || replay != result {
		t.Fatal("index upgrade changed original receipt replay", err)
	}
	err = migrations.DownOne(ctx, operator, historical)
	migrations.Unlock(ctx, operator)
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != "55000" {
		t.Fatal("highest lookup migration must retain forward-only rollback boundary")
	}
	if _, err := migrations.VerifyApplied(ctx, operator, historical); err != nil {
		t.Fatal("refused Down changed ledger", err)
	}
	// Upgrade the intact historical installation to this wave's full schema.
	// The performance observations above belong only to the F12 history.
	if err := billingschema.Up(ctx, operator, loaded); err != nil {
		t.Fatal("historical lookup installation cannot upgrade to current schema", err)
	}
	migrations.Unlock(ctx, operator)
	if _, err := migrations.VerifyApplied(ctx, operator, loaded); err != nil {
		t.Fatal("current upgrade ledger verification failed", err)
	}
}
