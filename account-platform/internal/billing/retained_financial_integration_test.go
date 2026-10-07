package billing

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/paymentsim"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/simulatorschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These fixtures use the installed production migration wrapper and PostgreSQL
// transactions in one new UUID database. Original SECURITY DEFINER functions
// intentionally bind public; moving their tables into another schema is invalid.
// Provider facts are controlled; this is not
// live collection, supplier, NATS, or four-way reconciliation acceptance.
func retainedFinanceRepository(t *testing.T) (*PostgresRepository, context.Context) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
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
	if err = billingschema.Up(ctx, connection, loaded); err != nil {
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
	return NewPostgresRepositoryWithPool(pool), ctx
}

type retainedFinancialProvider struct {
	payment.Provider
	result ProviderOrder
	err    error
}

func (p *retainedFinancialProvider) Capabilities() PaymentCapabilities {
	return PaymentCapabilities{ProviderID: "controlled", Version: "v1", Channels: []string{"ALIPAY"}, OrderQuerySupported: true, MinimumQueryIntervalSeconds: 1, ManualRepairSupported: true, RefundSupported: true, RefundIdempotencySupported: true, TestOnly: true}
}
func (p *retainedFinancialProvider) Get(context.Context, string) (ProviderOrder, error) {
	return p.result, p.err
}

func retainedFinanceOrder(t *testing.T, r *PostgresRepository, ctx context.Context, credit int64, paid bool) (*PaymentCoordinator, PaymentOrder, uuid.UUID, uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	now := r.now()
	order := PaymentOrder{ID: uuid.New(), MerchantOrderNo: "fixture-" + uuid.NewString(), RequesterID: owner, WalletID: wallet.ID, ProductID: "CNY_1", Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", CreditMicrocredits: credit, Status: "PENDING", FulfillmentStatus: "PENDING", CloseStatus: "OPEN", ExpiresAt: now.Add(time.Hour), ProviderID: "controlled", ProviderVersion: "v1"}
	snapshot, _ := json.Marshal(Product{ProductID: order.ProductID, PlanName: "controlled legacy credit", AmountFen: 100, CreditMicrocredits: credit, BasePoints: 1, ValidityDays: 30, Currency: "CNY"})
	_, err = r.pool.Exec(ctx, `INSERT INTO payment_orders(id,merchant_order_no,requester_id,payer_user_id,wallet_id,product_id,product_snapshot,channel,amount_fen,currency,credit_microcredits,status,fulfillment_status,expires_at,provider_payable_until,late_review_boundary_at,final_archive_eligible_at,created_at,updated_at,provider_id,provider_version) VALUES($1,$2,$3,$3,$4,$5,$6,$7,$8,$9,$10,'PENDING','PENDING',$11,$11,$11::timestamptz+interval '72 hours',$11::timestamptz+interval '8 days',$12,$12,$13,$14)`, order.ID, order.MerchantOrderNo, owner, wallet.ID, order.ProductID, snapshot, order.Channel, order.AmountFen, order.Currency, credit, order.ExpiresAt, now, order.ProviderID, order.ProviderVersion)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	provider := &retainedFinancialProvider{result: ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &id, PaidAt: &now, ExternalCustomerKey: owner.String()}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !paid {
		return c, order, uuid.Nil, uuid.Nil
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	order, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, order, "QUERY", "fixture-paid-"+id, []byte(`{"controlled":true}`), provider.result, false, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var transaction, pack uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT t.id,p.id FROM payment_transactions t JOIN entitlement_packages p ON p.source_payment_transaction_id=t.id WHERE t.payment_order_id=$1`, order.ID).Scan(&transaction, &pack); err != nil {
		t.Fatal(err)
	}
	return c, order, transaction, pack
}

func TestRetainedForcedRefundImmutableAndFinancialReplay(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	first, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced", 100, json.RawMessage(`{"confirmed":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if same, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced", 100, nil); err != nil || same.ID != first.ID {
		t.Fatal("same immutable evidence was not replayed", err)
	}
	if _, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "controlled-forced", 50, nil); !errors.Is(err, ErrConflict) {
		t.Fatal("changed amount was accepted", err)
	}
	applied, err := r.ApplyReviewedForcedRefund(ctx, first.ID)
	if err != nil || applied.RevokedBase != 1000000 || applied.ReceivableID != nil {
		t.Fatal("unused forced refund did not revoke exact source", err)
	}
	if same, err := r.ApplyReviewedForcedRefund(ctx, first.ID); err != nil || same.ID != applied.ID || same.RevokedBase != applied.RevokedBase {
		t.Fatal("financial replay changed receipt", err)
	}
	var posted, journals int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND') FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals); err != nil || posted != 0 || journals != 1 {
		t.Fatal("duplicate or unbalanced financial effect", posted, journals, err)
	}
}

func TestRetainedAnomalyIntentPrecedesEffectAndReceiptRecovery(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "intent-source", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	caseID, actor, key, reason := *forced.ManualReviewCaseID, uuid.New(), uuid.NewString(), "controlled financial resolution"
	if _, complete, err := r.PreparePaymentAnomalyResolution(ctx, caseID, actor, key, "CONFIRM_FORCED_REFUND", reason); err != nil || complete {
		t.Fatal("durable intent preparation", err)
	}
	for _, conflict := range []struct {
		actor                   uuid.UUID
		key, resolution, reason string
	}{{uuid.New(), key, "CONFIRM_FORCED_REFUND", reason}, {actor, key, "DISMISS", reason}, {actor, uuid.NewString(), "DISMISS", reason}, {actor, key, "CONFIRM_FORCED_REFUND", "changed immutable reason"}} {
		if _, _, err := r.PreparePaymentAnomalyResolution(ctx, caseID, conflict.actor, conflict.key, conflict.resolution, conflict.reason); !errors.Is(err, ErrConflict) {
			t.Fatal("intent was replaced", err)
		}
	}
	if _, err = r.ClaimPaymentAnomaly(ctx, caseID, actor, key); !errors.Is(err, ErrConflict) {
		t.Fatal("claim stole effect key", err)
	}
	if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
		t.Fatal(err)
	}
	// A committed financial effect followed by a failed receipt must retain the
	// original intent. Trigger is confined to this unique fixture schema.
	if _, err = r.pool.Exec(ctx, `CREATE FUNCTION fixture_receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'controlled receipt fault'; END$$; CREATE TRIGGER fixture_receipt_failure BEFORE INSERT ON payment_anomaly_commands FOR EACH ROW EXECUTE FUNCTION fixture_receipt_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, caseID, actor, key, "CONFIRM_FORCED_REFUND", reason, "APPLIED"); err == nil {
		t.Fatal("receipt fault was not reached")
	}
	if _, _, err = r.PreparePaymentAnomalyResolution(ctx, caseID, actor, uuid.NewString(), "DISMISS", reason); !errors.Is(err, ErrConflict) {
		t.Fatal("lost receipt permitted opposing decision", err)
	}
	if _, err = r.pool.Exec(ctx, `DROP TRIGGER fixture_receipt_failure ON payment_anomaly_commands; DROP FUNCTION fixture_receipt_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ApplyReviewedForcedRefund(ctx, forced.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, caseID, actor, key, "CONFIRM_FORCED_REFUND", reason, "APPLIED"); err != nil {
		t.Fatal(err)
	}
	if receipt, complete, err := r.PreparePaymentAnomalyResolution(ctx, caseID, actor, key, "CONFIRM_FORCED_REFUND", reason); err != nil || !complete || receipt.Status != "RESOLVED" {
		t.Fatal("completed receipt did not replay", err)
	}
	var posted, journals, intents int64
	if err = r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='FORCED_REFUND'),(SELECT count(*) FROM payment_anomaly_resolution_intents) FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals, &intents); err != nil || posted != 0 || journals != 1 || intents != 1 {
		t.Fatal("intent/effect receipt duplicated", err)
	}
}

func TestRetainedRefundUnknownKeepsReservationAndKnownTerminalReplays(t *testing.T) {
	for _, terminal := range []string{"SUCCESS", "FAILED"} {
		t.Run(terminal, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			c.AllowNormalRefundCreation(true)
			refund, created, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled whole legacy refund")
			if err != nil || !created || refund.AmountFen != 100 || refund.CreditMicrocredits != 1000000 {
				t.Fatal("whole targeted reservation failed", err)
			}
			unknown := ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: 100, Status: "PENDING"}
			if _, _, err = c.finishRefund(ctx, refund, unknown, false); !errors.Is(err, ErrPaymentProvider) {
				t.Fatal("unknown refund became terminal", err)
			}
			var state, packState string
			var posted int64
			if err = r.pool.QueryRow(ctx, `SELECT r.status,p.status,w.posted_microcredits FROM refunds r JOIN entitlement_packages p ON p.id=r.entitlement_package_id JOIN wallets w ON w.id=p.wallet_id WHERE r.id=$1`, refund.ID).Scan(&state, &packState, &posted); err != nil || state != "REQUESTED" || packState != "FROZEN" || posted != 1000000 {
				t.Fatal("unknown evidence released or spent reservation", state, packState, posted, err)
			}
			known := unknown
			known.Status = terminal
			known.ChannelRefundID = "controlled-refund"
			got, _, err := c.finishRefund(ctx, refund, known, false)
			if err != nil {
				t.Fatal(err)
			}
			if same, _, err := c.finishRefund(ctx, refund, known, false); err != nil || same.Status != got.Status || same.ID != got.ID {
				t.Fatal("terminal replay changed receipt", err)
			}
			wantState, wantPack, wantPosted := "SUCCEEDED", "REVOKED", int64(0)
			if terminal == "FAILED" {
				wantState, wantPack, wantPosted = "FAILED", "ACTIVE", 1000000
			}
			if err = r.pool.QueryRow(ctx, `SELECT r.status,p.status,w.posted_microcredits FROM refunds r JOIN entitlement_packages p ON p.id=r.entitlement_package_id JOIN wallets w ON w.id=p.wallet_id WHERE r.id=$1`, refund.ID).Scan(&state, &packState, &posted); err != nil || state != wantState || packState != wantPack || posted != wantPosted {
				t.Fatal("known terminal financial facts", state, packState, posted, err)
			}
		})
	}
}

func TestRetainedCrossReviewDismissalDoesNotThawOtherDecision(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "cross-forced", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "cross-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	reason := "controlled disproved payment review"
	if _, err = r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", reason, "NONE"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err = r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&state); err != nil || state != "FROZEN" {
		t.Fatal("other live review was thawed", state, err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, *charge.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", reason, "NONE"); err != nil {
		t.Fatal(err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&state); err != nil || state != "ACTIVE" {
		t.Fatal("last disproved fence not cleared", state, err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("dismissal changed money", err)
	}
}

func TestRetainedDismissedOverdueRightsEnterOrdinaryExpiry(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	old := r.now().Add(-time.Hour)
	for _, statement := range []string{`UPDATE entitlement_packages SET valid_from=$2::timestamptz-interval '30 days',valid_until=$2 WHERE id=$1`, `UPDATE entitlement_components SET credited_at=$2::timestamptz-interval '30 days',expires_at=$2 WHERE package_id=$1`, `UPDATE credit_lots SET expires_at=$2 WHERE id IN (SELECT credit_lot_id FROM entitlement_components WHERE package_id=$1)`} {
		if _, err := r.pool.Exec(ctx, statement, pack, old); err != nil {
			t.Fatal(err)
		}
	}
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "overdue-forced", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled disproved expired review", "NONE"); err != nil {
		t.Fatal(err)
	}
	_, summary, err := r.ListPackages(ctx, order.RequesterID, order.WalletID, 10)
	if err != nil || summary.AvailableMicrocredits != 0 {
		t.Fatal("elapsed rights became consumable", err)
	}
	if count, err := r.ExpirePackages(ctx, 10); err != nil || count != 1 {
		t.Fatal("ordinary expiry did not finish", count, err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 0 {
		t.Fatal("expired posted balance survived", err)
	}
}

func TestRetainedAlreadyTerminalRightsAreNotResurrected(t *testing.T) {
	for _, status := range []string{"EXPIRED", "REVOKED"} {
		t.Run(status, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, _, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			for _, statement := range []string{`UPDATE entitlement_packages SET status=$2 WHERE id=$1`, `UPDATE entitlement_components SET status=$2 WHERE package_id=$1`} {
				if _, err := r.pool.Exec(ctx, statement, pack, status); err != nil {
					t.Fatal(err)
				}
			}
			forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "terminal-review", 100, nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.ResolvePaymentAnomaly(ctx, *forced.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled disproved terminal review", "NONE"); err != nil {
				t.Fatal(err)
			}
			var actual string
			var changed int64
			if err = r.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM entitlement_components WHERE package_id=$1 AND status<>$2) FROM entitlement_packages WHERE id=$1`, pack, status).Scan(&actual, &changed); err != nil || actual != status || changed != 0 {
				t.Fatal("terminal rights were resurrected", actual, changed, err)
			}
		})
	}
}

func TestRetainedManualRepairAuditUsesNetPersistedBalance(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = getWallet(ctx, tx, order.WalletID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = createReceivableWithinTransaction(ctx, tx, order.WalletID, "FORCED_REFUND", "controlled-previous-loss", 400000, nil, nil, nil, nil, r.now()); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE payment_orders SET status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = c.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled verified payment repair"); err != nil {
		t.Fatal(err)
	}
	var before, after, gross, posted, outstanding int64
	if err = r.pool.QueryRow(ctx, `SELECT a.wallet_balance_before,a.wallet_balance_after,a.points_awarded,w.posted_microcredits,w.receivable_microcredits FROM payment_order_repairs a JOIN wallets w ON w.id=$2 WHERE a.payment_order_id=$1`, order.ID, order.WalletID).Scan(&before, &after, &gross, &posted, &outstanding); err != nil || before != 0 || after != 600000 || gross != 1000000 || posted != after || outstanding != 0 {
		t.Fatal("audit fabricated gross wallet balance", before, after, gross, posted, outstanding, err)
	}
}

func TestRetainedSourceLockRejectsDifferentWalletTuple(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	other, _, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err = lockPaymentSourcePackage(ctx, tx, pack, transaction, order.ID, other.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("source tuple accepted foreign wallet", err)
	}
}

func TestRetainedProviderFactsCannotOmitFrozenCustomerOrSKU(t *testing.T) {
	order := PaymentOrder{MerchantOrderNo: "controlled-fact-order", RequesterID: uuid.New(), Channel: "ALIPAY", AmountFen: 100, Currency: "CNY", ProviderSKU: "controlled-frozen-sku"}
	provider := ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU}
	if !providerMatchesOrder(provider, order) {
		t.Fatal("exact frozen facts rejected")
	}
	for _, field := range []string{"missing-customer", "wrong-customer", "missing-sku", "wrong-sku"} {
		t.Run(field, func(t *testing.T) {
			changed := provider
			switch field {
			case "missing-customer":
				changed.ExternalCustomerKey = ""
			case "wrong-customer":
				changed.ExternalCustomerKey = uuid.NewString()
			case "missing-sku":
				changed.ExternalSKU = ""
			case "wrong-sku":
				changed.ExternalSKU = "another-sku"
			}
			if providerMatchesOrder(changed, order) {
				t.Fatal("incomplete or conflicting frozen facts accepted")
			}
		})
	}
	// A genuinely empty historical SKU has no fabricated replacement or alias.
	order.ProviderSKU = ""
	provider.ExternalSKU = ""
	if !providerMatchesOrder(provider, order) {
		t.Fatal("actual empty historical SKU rejected")
	}
}

func TestRetainedFinancialMoneyWireRemainsExactBeyondJavascriptInteger(t *testing.T) {
	value := int64(9007199254740993)
	for _, item := range []any{ForcedRefundCase{AmountFen: value, RevokedBase: value, RevokedBonus: value}, ChargebackCase{RevokedBaseMicrocredits: value, RevokedBonusMicrocredits: value}, Refund{AmountFen: value, CreditMicrocredits: value, BaseMicrocredits: value, BonusMicrocredits: value}, PaymentTransaction{AmountFen: value}, PaymentEvidence{AmountFen: &value}} {
		body, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]json.RawMessage
		if err = json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		for key, raw := range decoded {
			if strings.HasSuffix(key, "amount_fen") || strings.HasSuffix(key, "microcredits") {
				var exact string
				if json.Unmarshal(raw, &exact) != nil || exact != "9007199254740993" {
					t.Fatal("money was not exact decimal text", key, string(raw))
				}
			}
		}
	}
	if got, err := proportionalCeiling(value, 99, 100); err != nil || got != 8917127262193584 {
		t.Fatal("proportional arithmetic lost precision", got, err)
	}
}

// A confirmed provider failure ends this refund, not another review's fence.
func TestRetainedFailedRefundKeepsConcurrentChargebackFence(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, transaction, pack, uuid.NewString(), "controlled whole legacy refund")
	if err != nil {
		t.Fatal(err)
	}
	charge, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "refund-concurrent-chargeback", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, _, err := c.finishRefund(ctx, refund, ProviderRefund{MerchantRefundNo: refund.MerchantRefundNo, AmountFen: 100, Status: "FAILED"}, false)
	if err != nil || result.Status != "FAILED" {
		t.Fatal("provider failure did not persist", err)
	}
	var state string
	var posted int64
	if err = r.pool.QueryRow(ctx, `SELECT p.status,w.posted_microcredits FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id WHERE p.id=$1`, pack).Scan(&state, &posted); err != nil || state != "FROZEN" || posted != 1000000 {
		t.Fatal("failed refund thawed concurrent chargeback", state, posted, err)
	}
	if _, err = r.ResolvePaymentAnomaly(ctx, *charge.ManualReviewCaseID, uuid.New(), uuid.NewString(), "DISMISS", "controlled disproved final chargeback", "NONE"); err != nil {
		t.Fatal(err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT status FROM entitlement_packages WHERE id=$1`, pack).Scan(&state); err != nil || state != "ACTIVE" {
		t.Fatal("final review receipt did not clear disproved fence", state, err)
	}
}

// An own-database trigger waits on a session advisory lock. Worker A is paused
// before mutation; worker B has read the same old SLA and waits on A's row lock.
func TestRetainedManualSLAConcurrentWorkersEscalateOnce(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, transaction, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "sla-controlled-source", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	caseID := *forced.ManualReviewCaseID
	if _, err = r.pool.Exec(ctx, `UPDATE manual_review_cases SET resolution_due_at=now()-interval '1 hour',mitigation_due_at=now()+interval '1 hour' WHERE id=$1`, caseID); err != nil {
		t.Fatal(err)
	}
	const barrier int64 = 761124085
	if _, err = r.pool.Exec(ctx, `CREATE FUNCTION fixture_sla_barrier() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.escalation_level<>OLD.escalation_level THEN PERFORM pg_advisory_xact_lock(761124085); END IF; RETURN NEW; END$$; CREATE TRIGGER fixture_sla_barrier BEFORE UPDATE ON manual_review_cases FOR EACH ROW EXECUTE FUNCTION fixture_sla_barrier()`); err != nil {
		t.Fatal(err)
	}
	blocker, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err = blocker.Exec(ctx, `SELECT pg_advisory_lock($1)`, barrier); err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			_, _ = blocker.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, barrier)
		}
	}()
	type outcome struct {
		count int
		err   error
	}
	done := make(chan outcome, 2)
	worker := func() { count, err := r.EscalateOverdueManualReviews(ctx, 1); done <- outcome{count, err} }
	go worker()
	go worker()
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var count int
		if err = blocker.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE manual_review_cases SET escalation_level=%'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 2 {
			blocked = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("controlled two-worker SQL interleaving not reached")
	}
	if _, err = blocker.Exec(ctx, `SELECT pg_advisory_unlock($1)`, barrier); err != nil {
		t.Fatal(err)
	}
	released = true
	first, second := <-done, <-done
	if first.err != nil || second.err != nil {
		t.Fatal("SLA worker error", first.err, second.err)
	}
	var level, events, outbox int
	if err = r.pool.QueryRow(ctx, `SELECT escalation_level,(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='SLA_ESCALATED'),(SELECT count(*) FROM outbox_events WHERE event_type='billing.manual-review.sla-escalated.v1' AND aggregate_id=$1) FROM manual_review_cases WHERE id=$1`, caseID).Scan(&level, &events, &outbox); err != nil {
		t.Fatal(err)
	}
	if first.count+second.count != 1 || level != 1 || events != 1 || outbox != 1 {
		t.Fatal("one overdue interval escalated twice", first.count+second.count, level, events, outbox)
	}
}

// The provider adapter and callback use real authenticated HTTP and two owned
// PostgreSQL stores. Public fixture credentials are not live supplier secrets.
func TestRetainedSimulatorCallbackCreditsFrozenMappingExactlyOnce(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	const sku = "controlled-real-mapped-sku"
	const secret = "public-fixture-hmac-key-at-least-32-bytes"
	const token = "public-fixture-simulator-service-token"
	order.ProviderID = "development_simulator"
	order.ProviderVersion = "development-simulator-v1"
	order.ProviderSKU = sku
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id=$2,provider_version=$3,provider_sku=$4,product_mapping_snapshot='{"status":"TEST_ONLY"}'::jsonb WHERE id=$1`, order.ID, order.ProviderID, order.ProviderVersion, sku); err != nil {
		t.Fatal(err)
	}
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	configuration, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("controlled connection configuration invalid")
	}
	schema := "retained_sim_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("simulator fixture unavailable")
	}
	quoted := pgx.Identifier{schema}.Sanitize()
	var exists bool
	if err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil || exists {
		admin.Close(ctx)
		t.Fatal("owned simulator schema identity guard failed")
	}
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal("owned simulator schema creation failed")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned simulator schema cleanup failed")
		}
		var remains bool
		if err := admin.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&remains); err != nil || remains {
			t.Error("owned simulator schema remains")
		}
		admin.Close(cleanup)
	})
	configuration.RuntimeParams["search_path"] = schema
	setup, err := pgx.ConnectConfig(ctx, configuration)
	if err != nil {
		t.Fatal("owned simulator schema unavailable")
	}
	loaded, err := migrations.Load("../../migrations/payment-simulator")
	if err == nil {
		err = simulatorschema.Up(ctx, setup, loaded)
	}
	setup.Close(ctx)
	if err != nil {
		t.Fatal("production simulator schema failed")
	}
	secrets := map[string]string{"UNIONPAY": secret, "ALIPAY": secret, "WECHAT": secret}
	simulatorURL, err := url.Parse(raw)
	if err != nil {
		t.Fatal("controlled simulator URL invalid")
	}
	query := simulatorURL.Query()
	query.Set("search_path", schema)
	simulatorURL.RawQuery = query.Encode()
	sim, err := paymentsim.NewPostgresRepository(ctx, simulatorURL.String(), "http://127.0.0.1", "public-fixture-pay-token-at-least-32-bytes", secrets)
	if err != nil {
		t.Fatal("production simulator store failed")
	}
	t.Cleanup(sim.Close)
	simService := paymentsim.NewService(sim)
	upstream := httptest.NewServer(simService.Handler(token))
	t.Cleanup(upstream.Close)
	provider, err := paymentsimulator.New(upstream.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	receiver := httptest.NewServer(NewService(r).WithPayments(coordinator).Handler(token))
	t.Cleanup(receiver.Close)
	callback := receiver.URL + "/internal/payment/notifications"
	if err = coordinator.ConfigureNotifications("pinvou-development", callback, secrets); err != nil {
		t.Fatal(err)
	}
	created, err := provider.Create(ctx, payment.Order{MerchantOrderNo: order.MerchantOrderNo, MerchantCustomerKey: order.RequesterID.String(), Mobile: "+8613800138000", ExternalSKU: sku, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency}, callback)
	if err != nil {
		t.Fatal(err)
	}
	paymentURL, err := url.Parse(created.PaymentURL)
	if err != nil {
		t.Fatal("simulator public fixture URL invalid")
	}
	completion, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL+paymentURL.Path+"/complete", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(completion)
	if err != nil {
		t.Fatal("public simulator completion unavailable")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("public simulator completion rejected")
	}
	actual, err := provider.Get(ctx, order.MerchantOrderNo)
	if err != nil || actual.ExternalCustomerKey != order.RequesterID.String() || actual.ExternalSKU != sku || actual.Status != "SUCCESS" {
		t.Fatal("actual authenticated provider mapping facts failed", err)
	}
	if err = simService.DeliverPending(ctx); err != nil {
		t.Fatal("actual signed callback delivery failed")
	}
	var notificationID uuid.UUID
	var body []byte
	if err = r.pool.QueryRow(ctx, `SELECT notification_id,payload FROM payment_notifications WHERE payload->>'merchant_order_no'=$1`, order.MerchantOrderNo).Scan(&notificationID, &body); err != nil {
		t.Fatal("actual Billing callback receipt absent", err)
	}
	if err = coordinator.ProcessPersistedNotification(ctx, notificationID); err != nil {
		t.Fatal("actual Billing callback processing failed", err)
	}
	wallet, err := r.GetWallet(ctx, order.WalletID)
	if err != nil || wallet.PostedMicrocredits != 1000000 {
		t.Fatal("actual callback did not credit original frozen owner/SKU", err)
	}
	// Retry with the original stable identity and facts, changing only timestamp.
	var facts map[string]json.RawMessage
	if err = json.Unmarshal(body, &facts); err != nil {
		t.Fatal(err)
	}
	facts["timestamp"], _ = json.Marshal(time.Now().UTC().Unix())
	retry, _ := json.Marshal(facts)
	signature := hmac.New(sha256.New, []byte(secret))
	signature.Write(retry)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(retry))
	request.Header.Set("X-Pinvou-Payment-Channel", order.Channel)
	request.Header.Set("X-Pinvou-Payment-Signature", base64.StdEncoding.EncodeToString(signature.Sum(nil)))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal("actual callback replay unavailable")
	}
	io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatal("actual callback replay rejected")
	}
	if err = coordinator.ProcessPersistedNotification(ctx, notificationID); err != nil {
		t.Fatal(err)
	}
	var posted, transactions, journals, receipts int64
	if err = r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM payment_transactions WHERE payment_order_id=$2),(SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'),(SELECT count(*) FROM payment_notifications) FROM wallets WHERE id=$1`, order.WalletID, order.ID).Scan(&posted, &transactions, &journals, &receipts); err != nil || posted != 1000000 || transactions != 1 || journals != 1 || receipts != 1 {
		t.Fatal("callback replay duplicated financial facts", posted, transactions, journals, receipts, err)
	}
}

func TestRetainedReviewedLossKeepsExpiredEntitlementFacts(t *testing.T) {
	for _, kind := range []string{"FORCED_REFUND", "CHARGEBACK"} {
		t.Run(kind, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, transaction, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
			expired := r.now().Add(-time.Hour)
			for _, statement := range []string{`UPDATE entitlement_packages SET valid_from=$2::timestamptz-interval '30 days',valid_until=$2 WHERE id=$1`, `UPDATE entitlement_components SET credited_at=$2::timestamptz-interval '30 days',expires_at=$2 WHERE package_id=$1`, `UPDATE credit_lots SET expires_at=$2 WHERE id IN (SELECT credit_lot_id FROM entitlement_components WHERE package_id=$1)`} {
				if _, err := r.pool.Exec(ctx, statement, pack, expired); err != nil {
					t.Fatal(err)
				}
			}
			if count, err := r.ExpirePackages(ctx, 1); err != nil || count != 1 {
				t.Fatal("actual expiry setup did not close rights", count, err)
			}
			actor, key, reason := uuid.New(), uuid.NewString(), "controlled reviewed loss of expired rights"
			var caseID uuid.UUID
			resolution := "CONFIRM_" + kind
			if kind == "FORCED_REFUND" {
				evidence, err := r.RecordAndFreezeForcedRefund(ctx, transaction, "expired-forced-loss", 100, nil)
				if err != nil {
					t.Fatal(err)
				}
				caseID = *evidence.ManualReviewCaseID
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, caseID, actor, key, resolution, reason); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyReviewedForcedRefund(ctx, evidence.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				evidence, err := r.RecordConfirmedChargebackForReview(ctx, transaction, "expired-chargeback-loss", nil)
				if err != nil {
					t.Fatal(err)
				}
				caseID = *evidence.ManualReviewCaseID
				if _, _, err = r.PreparePaymentAnomalyResolution(ctx, caseID, actor, key, resolution, reason); err != nil {
					t.Fatal(err)
				}
				if _, err = r.ApplyConfirmedChargeback(ctx, evidence.PaymentEvidenceID, evidence.ProviderChargebackID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.ResolvePaymentAnomaly(ctx, caseID, actor, key, resolution, reason, "APPLIED"); err != nil {
				t.Fatal(err)
			}
			var state string
			var changed, posted, receivable int64
			if err := r.pool.QueryRow(ctx, `SELECT p.status,(SELECT count(*) FROM entitlement_components WHERE package_id=$1 AND status<>'EXPIRED'),w.posted_microcredits,(SELECT COALESCE(sum(outstanding_microcredits),0) FROM receivables WHERE entitlement_package_id=$1) FROM entitlement_packages p JOIN wallets w ON w.id=p.wallet_id WHERE p.id=$1`, pack).Scan(&state, &changed, &posted, &receivable); err != nil || state != "EXPIRED" || changed != 0 || posted != 0 || receivable != 1000000 {
				t.Fatal("reviewed loss rewrote closed entitlement facts", state, changed, posted, receivable, err)
			}
			if wallet, err := r.GetWallet(ctx, order.WalletID); err != nil || wallet.PostedMicrocredits != 0 {
				t.Fatal("reviewed loss recreated customer credit", err)
			}
		})
	}
}

func TestRetainedLateClaimQueryAuthority(t *testing.T) {
	for _, mode := range []string{"claim_producer", "failed_query_reject", "repeat_success_keeps_new_time", "successful_query_accept"} {
		t.Run(mode, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			base := time.Now().UTC().Add(-time.Hour)
			r.now = func() time.Time { return base }
			item, err := r.EscalateLatePaymentReview(ctx, order.ID, "CLOSED")
			if err != nil {
				t.Fatal(err)
			}
			actor := uuid.New()
			if _, err = r.ClaimLatePaymentReview(ctx, item.ID, actor); err != nil {
				t.Fatal(err)
			}
			// Controlled historical provider facts are older than the new user claim,
			// but newer than the archive boundary. No payment/evidence fact is changed.
			if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET created_at=$2::timestamptz-interval '10 days',expires_at=$2::timestamptz-interval '9 days',provider_payable_until=$2::timestamptz-interval '9 days',late_review_boundary_at=$2::timestamptz-interval '8 days',final_archive_eligible_at=$2,last_successful_query_at=$3,last_authoritative_unpaid_at=$3 WHERE id=$1`, order.ID, base.Add(-time.Hour), base); err != nil {
				t.Fatal(err)
			}
			order.Status = "MANUAL_REVIEW"
			order.FulfillmentStatus = "MANUAL_REVIEW"
			if mode == "repeat_success_keeps_new_time" {
				if _, err = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, order, nil); err != nil {
					t.Fatal(err)
				}
			}
			r.now = func() time.Time { return base.Add(time.Minute) }
			if mode == "claim_producer" {
				if _, err = r.AddLatePaymentClaim(ctx, order.ID, order.RequesterID, "I have new evidence that this payment succeeded"); err != nil {
					t.Fatal(err)
				}
				return
			}
			// Persist a lawful claim fixture independently of the producer so that its
			// current SQL parameter failure cannot hide the archive evidence boundaries.
			if _, err = r.pool.Exec(ctx, `INSERT INTO late_payment_review_evidence(id,case_id,evidence_type,payload_digest,payload_snapshot,created_at) VALUES($1,$2,'USER_PAYMENT_CLAIM',repeat('a',64),'{"statement":"controlled new payment evidence"}',$3)`, uuid.New(), item.ID, base.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			r.now = func() time.Time { return base.Add(2 * time.Minute) }
			var queryErr error
			if mode == "failed_query_reject" {
				queryErr = errors.New("controlled provider query unavailable")
			}
			if mode != "failed_query_reject" {
				if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET last_successful_query_at=$2,last_authoritative_unpaid_at=$2 WHERE id=$1`, order.ID, base.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = r.RecordLatePaymentReviewQuery(ctx, item.ID, actor, order, queryErr); err != nil {
				t.Fatal(err)
			}
			_, err = r.ArchiveLatePaymentReview(ctx, item.ID, actor)
			if mode == "failed_query_reject" {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("failed query after newer claim must not authorize archive: error=%v", err)
				}
			} else if err != nil {
				t.Fatalf("fresh successful authoritative query after claim must permit archive: %v", err)
			}
		})
	}
}
