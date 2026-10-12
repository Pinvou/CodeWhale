package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEntitlementCorrectionExecuteAndConsumerReadPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	repository, schema := entitlementRepositoryToVersion(t, ctx, databaseURL, 0)
	operatorConfig := repository.pool.Config().ConnConfig.Copy()
	operatorConfig.User = "billing_migrator"
	operator, err := pgx.ConnectConfig(ctx, operatorConfig)
	if err != nil {
		t.Fatal("activation operator connection failed")
	}
	defer operator.Close(ctx)
	setActivation := func(state string) {
		tx, err := operator.Begin(ctx)
		if err != nil {
			t.Fatal("activation operator transaction failed")
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
			t.Fatal("activation operator qualification failed")
		}
		if _, err := tx.Exec(ctx, `UPDATE public.billing_approval_command_activations SET state=$1,version=version+1 WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2`, state); err != nil {
			t.Fatal("activation operator update failed")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal("activation operator commit failed")
		}
	}

	walletID, ownerID, orderID, transactionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	lotID, packageID, walletAccountID, clearingAccountID, sourceJournalID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
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
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, reference, now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,payment_order_id,source_payment_transaction_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,$6,'CORRECTION_EXECUTE_TEST','Test','',100,100,100,$7,$7,$8,'ACTIVE',$7,$7,'PAYMENT',$9)`, []any{packageID, ownerID, walletID, orderID, transactionID, lotID, now, now.Add(24 * time.Hour), reference}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, []any{lotID, packageID, now, now.Add(24 * time.Hour)}},
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

	plan, err := repository.planEntitlementComponentCorrection(ctx, packageID, orderID)
	if err != nil {
		t.Fatal(err)
	}
	approvalRequestID, authorizationEventID := uuid.New(), uuid.New()
	requesterID, approverID := uuid.New(), uuid.New()
	executionKey := "approval:" + approvalRequestID.String() + ":ENTITLEMENT_COMPONENT_ADJUSTMENT"
	reasonCode := "ORIGINAL_SPLIT_MISMATCH"
	frozen := entitlementCorrectionFrozenFacts{
		WalletID: plan.WalletID, WalletVersion: plan.WalletVersion, PaymentTransactionID: plan.PaymentTransactionID,
		SourceComponentID: plan.SourceComponentID, SourceLotID: plan.SourceLotID,
		TargetComponentID: plan.TargetComponentID, TargetLotID: plan.TargetLotID,
		SourceType: plan.Amounts.SourceType, TargetType: plan.Amounts.TargetType, AmountMicrocredits: plan.Amounts.Amount,
		SourceEvidenceDigest: plan.SourceEvidenceDigest, FactsDigest: plan.FactsDigest,
	}
	if !frozen.matches(plan) {
		t.Fatalf("frozen owner facts differ plan=%+v frozen=%+v", plan, frozen)
	}
	if _, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen); !errors.Is(err, ErrApprovalExecutionUnavailable) {
		t.Fatal("inactive command accepted fresh financial execution")
	}
	var unchanged bool
	if err := repository.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM entitlement_component_corrections WHERE approval_request_id=$1) AND (SELECT available_microcredits=100 FROM entitlement_components WHERE id=$2)`, approvalRequestID, lotID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("inactive command mutated financial facts")
	}
	setActivation("ACTIVE")
	// Race an actual fresh execution against an operator's uncommitted disable.
	// Observe the database wait before committing the disable; goroutine startup
	// alone does not prove the funds transaction reached the activation fence.
	disableTx, err := operator.Begin(ctx)
	if err != nil {
		t.Fatal("disable race transaction failed")
	}
	defer disableTx.Rollback(ctx)
	if _, err := disableTx.Exec(ctx, `SET LOCAL ROLE billing_schema_owner`); err != nil {
		t.Fatal("disable race owner qualification failed")
	}
	if _, err := disableTx.Exec(ctx, `UPDATE public.billing_approval_command_activations SET state='INACTIVE',version=version+1 WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2 AND snapshot_schema_version=2`); err != nil {
		t.Fatal("disable race update failed")
	}
	var disablingPID int
	if err := disableTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&disablingPID); err != nil {
		t.Fatal("disable race PID failed")
	}
	executed := make(chan error, 1)
	go func() {
		_, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2, packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen)
		executed <- err
	}()
	blocked := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-executed:
			t.Fatal("fresh execution bypassed pending operator activation update")
		default:
		}
		if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, disablingPID).Scan(&blocked); err != nil {
			t.Fatal("disable race observation failed")
		}
		if blocked {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("fresh execution did not wait on activation update")
	}
	if err := disableTx.Commit(ctx); err != nil {
		t.Fatal("disable race commit failed")
	}
	select {
	case err := <-executed:
		if !errors.Is(err, ErrApprovalExecutionUnavailable) {
			t.Fatal("deactivated fresh execution did not fail closed")
		}
	case <-ctx.Done():
		t.Fatal("deactivated execution did not return")
	}
	if err := repository.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM entitlement_component_corrections WHERE approval_request_id=$1) AND (SELECT available_microcredits=100 FROM entitlement_components WHERE id=$2)`, approvalRequestID, lotID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatal("deactivation race mutated financial facts")
	}
	setActivation("ACTIVE")
	result, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen)
	if err != nil || result.Status != "SUCCEEDED" || result.AmountMicrocredits != 20 || result.SourceComponentID != lotID ||
		result.SourceType != "BASE" || result.TargetType != "BONUS" || result.TargetComponentID == uuid.Nil || result.TargetLotID == uuid.Nil {
		t.Fatalf("correction result=%+v err=%v", result, err)
	}

	var sourceAvailable, sourceReclassified, sourceRemaining int64
	if err := repository.pool.QueryRow(ctx, `SELECT component.available_microcredits,component.reclassified_out_microcredits,lot.remaining_microcredits
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, lotID).
		Scan(&sourceAvailable, &sourceReclassified, &sourceRemaining); err != nil || sourceAvailable != 80 || sourceReclassified != 20 || sourceRemaining != 80 {
		t.Fatalf("source after correction available=%d reclassified=%d lot=%d err=%v", sourceAvailable, sourceReclassified, sourceRemaining, err)
	}
	var targetAvailable, targetOriginal, targetRemaining int64
	var targetType, targetStatus string
	var targetRefundable bool
	var targetCreditedAt, targetExpiresAt time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT component.component_type,component.available_microcredits,component.original_microcredits,component.status,
component.credited_at,component.expires_at,lot.remaining_microcredits,lot.refundable
FROM entitlement_components component JOIN credit_lots lot ON lot.id=component.credit_lot_id WHERE component.id=$1`, result.TargetComponentID).
		Scan(&targetType, &targetAvailable, &targetOriginal, &targetStatus, &targetCreditedAt, &targetExpiresAt, &targetRemaining, &targetRefundable); err != nil ||
		targetType != "BONUS" || targetAvailable != 20 || targetOriginal != 20 || targetStatus != "ACTIVE" ||
		targetRemaining != 20 || targetRefundable || !targetCreditedAt.Equal(plan.SourceCreditedAt) || !targetExpiresAt.Equal(plan.SourceExpiresAt) {
		t.Fatalf("target after correction type=%s available=%d original=%d status=%s remaining=%d refundable=%v err=%v", targetType, targetAvailable, targetOriginal, targetStatus, targetRemaining, targetRefundable, err)
	}

	var journalType, journalReference string
	var postingCount int
	var postingSum int64
	if err := repository.pool.QueryRow(ctx, `SELECT entry_type,business_reference FROM journal_entries WHERE id=$1`, result.JournalEntryID).Scan(&journalType, &journalReference); err != nil || journalType != "ADJUSTMENT" || journalReference != "approval-execution:"+approvalRequestID.String() {
		t.Fatalf("adjustment journal type=%s reference=%s err=%v", journalType, journalReference, err)
	}
	postingRows, err := repository.pool.Query(ctx, `SELECT credit_lot_id,delta_microcredits FROM ledger_postings WHERE journal_entry_id=$1 ORDER BY delta_microcredits`, result.JournalEntryID)
	if err != nil {
		t.Fatal(err)
	}
	deltas := map[uuid.UUID]int64{}
	for postingRows.Next() {
		var postingLot uuid.UUID
		var delta int64
		if err := postingRows.Scan(&postingLot, &delta); err != nil {
			postingRows.Close()
			t.Fatal(err)
		}
		deltas[postingLot] = delta
		postingCount++
		postingSum += delta
	}
	if closeErr := postingRows.Err(); closeErr != nil {
		t.Fatal(closeErr)
	}
	postingRows.Close()
	if postingCount != 2 || postingSum != 0 || deltas[lotID] != -20 || deltas[result.TargetLotID] != 20 {
		t.Fatalf("paired postings count=%d sum=%d deltas=%v", postingCount, postingSum, deltas)
	}

	var walletPosted int64
	var packageTotal int64
	var effectiveTotal int64
	if err := repository.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, walletID).Scan(&walletPosted); err != nil || walletPosted != 100 {
		t.Fatalf("wallet liability changed: posted=%d err=%v", walletPosted, err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT points_total,COALESCE(sum(original_microcredits-reclassified_out_microcredits),0) FROM entitlement_packages p
JOIN entitlement_components c ON c.package_id=p.id WHERE p.id=$1 GROUP BY p.points_total`, packageID).Scan(&packageTotal, &effectiveTotal); err != nil || packageTotal != 100 || effectiveTotal != 100 {
		t.Fatalf("package effective total changed: total=%d effective=%d err=%v", packageTotal, effectiveTotal, err)
	}

	setActivation("INACTIVE")
	replayed, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, executionKey, "independent approval completed", reasonCode, frozen)
	if err != nil || replayed != result {
		t.Fatalf("idempotent replay result=%+v original=%+v err=%v", replayed, result, err)
	}
	if _, err := repository.ExecuteEntitlementComponentCorrection(ctx, approvalRequestID, authorizationEventID, 2,
		packageID, orderID, requesterID, approverID, "different-key", "independent approval completed", reasonCode, frozen); !errors.Is(err, ErrConflict) {
		t.Fatalf("different execution key accepted: %v", err)
	}
	var correctionCount, adjustmentCount int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM entitlement_component_corrections WHERE approval_request_id=$1),
	(SELECT count(*) FROM journal_entries WHERE business_reference=$2)`, approvalRequestID, "approval-execution:"+approvalRequestID.String()).Scan(&correctionCount, &adjustmentCount); err != nil || correctionCount != 1 || adjustmentCount != 1 {
		t.Fatalf("replay created facts corrections=%d journals=%d err=%v", correctionCount, adjustmentCount, err)
	}

	readResult, err := repository.EntitlementCorrectionForRequest(ctx, approvalRequestID)
	if err != nil || readResult != result {
		t.Fatalf("consumer result-first read=%+v expected=%+v err=%v", readResult, result, err)
	}
	packages, _, err := repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 || len(packages[0].Components) != 2 {
		t.Fatalf("package projection packages=%d components=%d err=%v", len(packages), len(packages[0].Components), err)
	}
	byType := map[string]EntitlementComponent{}
	for _, component := range packages[0].Components {
		byType[component.ComponentType] = component
	}
	if byType["BASE"].EffectiveOriginalMicrocredits != 80 || byType["BONUS"].EffectiveOriginalMicrocredits != 20 ||
		byType["BASE"].AvailableMicrocredits != 80 || byType["BONUS"].AvailableMicrocredits != 20 {
		t.Fatalf("effective projection base=%+v bonus=%+v", byType["BASE"], byType["BONUS"])
	}
	adminOrder, err := repository.AdminPaymentOrder(ctx, orderID)
	if err != nil || len(adminOrder.Packages) != 1 || len(adminOrder.Packages[0].Components) != 2 {
		t.Fatal("admin correction projection missing")
	}
	for _, component := range adminOrder.Packages[0].Components {
		expected := int64(20)
		if component.ComponentType == "BASE" {
			expected = 80
		}
		if component.EffectiveOriginalMicrocredits != expected {
			t.Fatal("admin correction projection retained pre-correction effective amount")
		}
	}

	qualifiedCorrections := pgx.Identifier{schema, "entitlement_component_corrections"}.Sanitize()
	var canRead, canInsert, canUpdate, canDelete bool
	if err := repository.pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service',$1,'SELECT'),has_table_privilege('billing_service',$1,'INSERT'),
has_table_privilege('billing_service',$1,'UPDATE'),has_table_privilege('billing_service',$1,'DELETE')`, qualifiedCorrections).Scan(&canRead, &canInsert, &canUpdate, &canDelete); err != nil || !canRead || !canInsert || canUpdate || canDelete {
		t.Fatalf("correction grants read=%v insert=%v update=%v delete=%v err=%v", canRead, canInsert, canUpdate, canDelete, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE `+qualifiedCorrections+` SET reason_code=reason_code`); err == nil {
		t.Fatal("immutable correction accepted UPDATE")
	}
	if _, err := repository.pool.Exec(ctx, `DELETE FROM `+qualifiedCorrections); err == nil {
		t.Fatal("immutable correction accepted DELETE")
	}

	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	configuration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.Database = repository.pool.Config().ConnConfig.Database
	configuration.User = "billing_migrator"
	configuration.Password = ""
	configuration.RuntimeParams["search_path"] = schema
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err := connection.Exec(ctx, "SET ROLE billing_service"); err != nil {
		t.Fatal(err)
	}
	if len(loaded) == 0 {
		t.Fatal("billing migration chain is empty")
	}
	head := loaded[len(loaded)-1].Version
	if err := migrations.DownOne(ctx, connection, loaded); err == nil || !strings.Contains(err.Error(), "rollback migration "+strconv.FormatInt(head, 10)) {
		t.Fatalf("forward-only migration down returned %v", err)
	}
	var latestVersion int64
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&latestVersion); err != nil || latestVersion != head {
		t.Fatalf("failed down changed migration version: %d err=%v", latestVersion, err)
	}
}

// Uses only this branch's installed SQL, the real versioned wrapper, and the
// inherited strict owned UUID database cleanup. Fixed roles belong to the
// isolated F09 test instance and are created/removed by the outer role harness.
func entitlementRepositoryToVersion(t *testing.T, ctx context.Context, raw string, maximum int64) (*PostgresRepository, string) {
	t.Helper()
	if raw == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is required for actual PostgreSQL")
	}
	control, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("cannot connect to fixture database")
	}
	database := "retained_finance_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{database}.Sanitize()
	var exists bool
	if err = control.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists); err != nil || exists {
		control.Close(context.Background())
		t.Fatal("new UUID database must not already exist")
	}
	if _, err = control.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER billing_schema_owner"); err != nil {
		control.Close(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		var owner string
		var active int
		// Closed client connections can remain visible until PostgreSQL finishes
		// their backend shutdown. Wait only for this fixture's own database.
		deadline := time.Now().Add(5 * time.Second)
		var queryErr error
		for {
			queryErr = control.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &active)
			if queryErr != nil || owner != "billing_schema_owner" || active == 0 || !time.Now().Before(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if queryErr != nil || owner != "billing_schema_owner" || active != 0 {
			control.Close(cleanup)
			t.Error("own-database identity/connection cleanup guard failed")
			return
		}
		if _, err := control.Exec(cleanup, "DROP DATABASE "+quoted); err != nil {
			t.Error("own-database cleanup failed")
		}
		control.Close(cleanup)
	})
	configuration, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("cannot parse fixture connection configuration")
	}
	configuration.Database = database
	configuration.User = "billing_migrator"
	configuration.Password = ""
	connection, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("cannot connect to own UUID database")
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	if _, err = connection.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal("owned deployment migration actor setup failed")
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	selected := selectEntitlementFixtureStage(loaded, maximum)
	if err = billingschema.Up(ctx, connection, selected); err != nil {
		t.Fatal(err)
	}
	// Installation is complete. Retaining its session lock until fixture cleanup
	// would prevent an independent migration connection from validating recovery.
	migrations.Unlock(ctx, connection)
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("cannot parse fixture database configuration")
	}
	config.ConnConfig.Database = database
	config.ConnConfig.User = "billing_service"
	config.ConnConfig.Password = ""
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot open fixture pool")
	}
	t.Cleanup(pool.Close)
	return NewPostgresRepositoryWithPool(pool), "public"
}

func selectEntitlementFixtureStage(loaded []migrations.Migration, maximum int64) []migrations.Migration {
	selected := make([]migrations.Migration, 0, len(loaded))
	for _, migration := range loaded {
		// High forward stages use a real installation prefix. The historical
		// 56-59 correction fixtures below keep their original selection rules.
		if maximum >= 900000 {
			if migration.Version <= maximum {
				selected = append(selected, migration)
			}
			continue
		}
		if migration.Version == 900013 && maximum != 0 && maximum < 59 {
			continue
		}
		if maximum == 0 || migration.Version < 56 || migration.Version > 59 || migration.Version <= maximum {
			selected = append(selected, migration)
		}
	}
	return selected
}
