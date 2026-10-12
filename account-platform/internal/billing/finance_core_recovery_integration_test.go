package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type coreRecoveryProvider struct {
	*retainedFinancialProvider
	creates atomic.Int32
}

type wrongFrozenProvider struct {
	*coreRecoveryProvider
	gets atomic.Int32
}

func (p *wrongFrozenProvider) Capabilities() PaymentCapabilities {
	caps := p.coreRecoveryProvider.Capabilities()
	caps.ProviderID = "another-provider"
	return caps
}
func (p *wrongFrozenProvider) Get(ctx context.Context, merchant string) (ProviderOrder, error) {
	p.gets.Add(1)
	return p.coreRecoveryProvider.Get(ctx, merchant)
}
func TestFinanceCreationRecoveryRejectsChangedProvider(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	provider := &wrongFrozenProvider{coreRecoveryProvider: &coreRecoveryProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider)}}
	c.provider = provider
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='NOT_STARTED' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.dispatchCreatedOrder(ctx, order, uuid.NewString(), "fixture", false); !errors.Is(err, ErrConflict) || provider.creates.Load() != 0 {
		t.Fatal("historical binding used another adapter", err)
	}
	var state string
	if err := r.pool.QueryRow(ctx, `SELECT provider_call_status FROM payment_orders WHERE id=$1`, order.ID).Scan(&state); err != nil || state != "NOT_STARTED" {
		t.Fatal("provider mismatch mutated dispatch state", state, err)
	}
	if _, err := c.SyncOrder(ctx, order.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("sync accepted another provider", err)
	}
	if _, err := c.CloseOrder(ctx, order.ID, order.RequesterID); !errors.Is(err, ErrConflict) {
		t.Fatal("close accepted another provider", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RepairReviewedOrder(ctx, order.ID, uuid.New(), "controlled provider mismatch"); !errors.Is(err, ErrConflict) {
		t.Fatal("repair accepted another provider", err)
	}
	if provider.gets.Load() != 0 || provider.creates.Load() != 0 {
		t.Fatal("provider mismatch made external calls")
	}
}

func (p *coreRecoveryProvider) Create(_ context.Context, input payment.Order, _ string) (ProviderOrder, error) {
	p.creates.Add(1)
	return ProviderOrder{MerchantOrderNo: input.MerchantOrderNo, Channel: input.Channel, AmountFen: input.AmountFen, Currency: input.Currency, ExternalCustomerKey: input.MerchantCustomerKey, ExternalSKU: input.ExternalSKU, Status: "PENDING", PaymentURL: "https://provider.invalid/pay/" + input.MerchantOrderNo}, nil
}

type wrongCreateFactsProvider struct{ *coreRecoveryProvider }

func (p *wrongCreateFactsProvider) Create(ctx context.Context, input payment.Order, callback string) (ProviderOrder, error) {
	result, err := p.coreRecoveryProvider.Create(ctx, input, callback)
	result.AmountFen++
	return result, err
}

type callbackBoundProvider struct {
	*retainedFinancialProvider
	id, version string
}

func (p *callbackBoundProvider) Capabilities() PaymentCapabilities {
	caps := p.retainedFinancialProvider.Capabilities()
	caps.ProviderID, caps.Version, caps.NotificationSupported = p.id, p.version, true
	return caps
}

func signedCoreCallback(t *testing.T, c *PaymentCoordinator, order PaymentOrder, id uuid.UUID) ([]byte, string) {
	t.Helper()
	const secret = "public-fixture-callback-key-at-least-32-bytes"
	if err := c.ConfigureNotifications("fixture-merchant", "https://provider.invalid/callback", map[string]string{"ALIPAY": secret}); err != nil {
		t.Fatal(err)
	}
	now := c.now()
	body, err := json.Marshal(paymentNotification{NotificationID: id, MerchantID: "fixture-merchant", MerchantOrderNo: order.MerchantOrderNo, ChannelTransaction: uuid.NewString(), Channel: order.Channel, Status: "SUCCESS", AmountFen: order.AmountFen, Currency: order.Currency, PaidAt: now, Timestamp: now.Unix(), ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: order.ProviderSKU, ProviderPayload: json.RawMessage(`{"provider_id":"controlled","provider_version":"v1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	return body, paymentSignature([]byte(secret), body)
}

func TestFinanceSignedCallbackProvenanceCannotCrossProviderSwitch(t *testing.T) {
	for _, scenario := range []string{"another-provider", "another-version", "switch-back-to-original"} {
		t.Run(scenario, func(t *testing.T) {
			r, ctx := retainedFinanceRepository(t)
			c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
			base := c.provider.(*retainedFinancialProvider)
			sourceID, sourceVersion := "another-provider", "v1"
			if scenario == "another-version" {
				sourceID, sourceVersion = "controlled", "v2"
			}
			c.provider = &callbackBoundProvider{base, sourceID, sourceVersion}
			id := uuid.New()
			body, signature := signedCoreCallback(t, c, order, id)
			if _, created, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil || !created {
				t.Fatal("valid current-adapter HMAC was not persisted", err)
			}
			var before string
			if err := r.pool.QueryRow(ctx, `SELECT request_hash||':'||payload::text FROM payment_notifications WHERE notification_id=$1`, id).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if scenario == "switch-back-to-original" {
				c.provider = &callbackBoundProvider{base, "controlled", "v1"}
				if _, _, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); !errors.Is(err, ErrForbiddenPayment) {
					t.Fatal("another adapter reused original notification ID", err)
				}
			}
			if err := c.ProcessPersistedNotification(ctx, id); err != nil {
				t.Fatal("untrusted provenance did not enter durable human review", err)
			}
			var posted, journals, cases int64
			var status, persistedID, persistedVersion, after string
			if err := r.pool.QueryRow(ctx, `SELECT notification.result,notification.provider_id,notification.provider_version,notification.request_hash||':'||notification.payload::text,wallet.posted_microcredits,(SELECT count(*) FROM journal_entries),(SELECT count(*) FROM payment_anomaly_cases WHERE payment_order_id=$2 AND status='OPEN') FROM payment_notifications notification CROSS JOIN wallets wallet WHERE notification.notification_id=$1 AND wallet.id=$3`, id, order.ID, order.WalletID).Scan(&status, &persistedID, &persistedVersion, &after, &posted, &journals, &cases); err != nil || status != "MANUAL_REVIEW" || persistedID != sourceID || persistedVersion != sourceVersion || after != before || posted != 0 || journals != 0 || cases != 1 {
				t.Fatal("callback provider provenance changed funds or lost evidence", status, posted, journals, cases, err)
			}
			if err := c.ProcessPersistedNotification(ctx, id); err != nil {
				t.Fatal("review receipt was not idempotent", err)
			}
		})
	}
}

func TestFinanceLegacyCallbackProvenanceIsNeverInvented(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &callbackBoundProvider{c.provider.(*retainedFinancialProvider), "controlled", "v1"}
	id := uuid.New()
	body, signature := signedCoreCallback(t, c, order, id)
	// This is the original pre-900005 row shape. No verifier identity is guessed.
	if _, err := r.pool.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES($1,$2,'preserved-legacy-hash',$3,now(),'RECEIVED')`, id, order.Channel, body); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); !errors.Is(err, ErrForbiddenPayment) {
		t.Fatal("legacy pending source was filled from a replay", err)
	}
	if err := c.ProcessPersistedNotification(ctx, id); err != nil {
		t.Fatal(err)
	}
	var status string
	var sourceID, sourceVersion *string
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT notification.result,notification.provider_id,notification.provider_version,wallet.posted_microcredits FROM payment_notifications notification CROSS JOIN wallets wallet WHERE notification.notification_id=$1 AND wallet.id=$2`, id, order.WalletID).Scan(&status, &sourceID, &sourceVersion, &posted); err != nil || status != "MANUAL_REVIEW" || sourceID != nil || sourceVersion != nil || posted != 0 {
		t.Fatal("legacy facts were attributed or credited", status, posted, err)
	}
	for _, terminal := range []string{"PROCESSED", "REJECTED", "MANUAL_REVIEW"} {
		terminalID := uuid.New()
		terminalBody, terminalSignature := signedCoreCallback(t, c, order, terminalID)
		if _, err := r.pool.Exec(ctx, `INSERT INTO payment_notifications(notification_id,channel,request_hash,payload,received_at,result) VALUES($1,$2,'preserved-terminal-hash',$3,now(),$4)`, terminalID, order.Channel, terminalBody, terminal); err != nil {
			t.Fatal(err)
		}
		if _, created, err := c.PersistNotification(ctx, order.Channel, terminalSignature, terminalBody, "fixture-source"); err != nil || created {
			t.Fatal("legacy terminal read-only receipt regressed", terminal, err)
		}
		if err := c.ProcessPersistedNotification(ctx, terminalID); err != nil {
			t.Fatal(err)
		}
		var unchanged bool
		if err := r.pool.QueryRow(ctx, `SELECT result=$2 AND provider_id IS NULL AND provider_version IS NULL AND request_hash='preserved-terminal-hash' AND payload=$3::jsonb FROM payment_notifications WHERE notification_id=$1`, terminalID, terminal, terminalBody).Scan(&unchanged); err != nil || !unchanged {
			t.Fatal("legacy terminal receipt was rewritten", terminal, err)
		}
	}
}

func TestFinanceSignedCallbackMatchingProvenanceCreditsOnce(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &callbackBoundProvider{c.provider.(*retainedFinancialProvider), "controlled", "v1"}
	id := uuid.New()
	body, signature := signedCoreCallback(t, c, order, id)
	for attempt := 0; attempt < 2; attempt++ {
		if _, created, err := c.PersistNotification(ctx, order.Channel, signature, body, "fixture-source"); err != nil || created != (attempt == 0) {
			t.Fatal("trusted matching callback receipt", attempt, created, err)
		}
		if err := c.ProcessPersistedNotification(ctx, id); err != nil {
			t.Fatal("trusted matching callback credit", err)
		}
	}
	var posted, journals int64
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT wallet.posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'),notification.result FROM wallets wallet CROSS JOIN payment_notifications notification WHERE wallet.id=$1 AND notification.notification_id=$2`, order.WalletID, id).Scan(&posted, &journals, &status); err != nil || posted != 1000000 || journals != 1 || status != "PROCESSED" {
		t.Fatal("trusted matching callback did not credit exactly once", posted, journals, status, err)
	}
}
func TestFinanceCreateRejectsConflictingProviderInstructions(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	c.provider = &wrongCreateFactsProvider{&coreRecoveryProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider)}}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='NOT_STARTED' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	if _, _, err := c.dispatchCreatedOrder(ctx, order, key, "controlled-conflict", true); !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting payment link accepted", err)
	}
	var receipt bool
	var url string
	if err := r.pool.QueryRow(ctx, `SELECT COALESCE(payment_url,''),EXISTS(SELECT 1 FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$2) FROM payment_orders WHERE id=$1`, order.ID, key).Scan(&url, &receipt); err != nil || url != "" || receipt {
		t.Fatal("conflicting instructions persisted", url, receipt, err)
	}
}

func TestFinanceCreateBindingReplayAfterPreDispatchCrash(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	owner := uuid.New()
	wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
	if err != nil {
		t.Fatal(err)
	}
	product := retainedSameKeyProduct(t, r, ctx, owner)
	provider := &coreRecoveryProvider{retainedFinancialProvider: &retainedFinancialProvider{}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	c.AllowTentativePrices(true)
	key := uuid.NewString()
	original, _, err := c.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct precisely the committed binding/order at the boundary before
	// dispatch. The controlled provider has no external money effects.
	if _, err = r.pool.Exec(ctx, `DELETE FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='NOT_STARTED',payment_url=NULL,qr_data=NULL WHERE id=$1`, original.ID); err != nil {
		t.Fatal(err)
	}
	recovered, created, err := c.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
	if err != nil || created || recovered.ID != original.ID || recovered.PaymentURL == "" || provider.creates.Load() != 2 {
		t.Fatal("actual same-key binding did not resume dispatch", recovered, created, err)
	}
	replay, _, err := c.CreateOrder(ctx, owner, wallet.ID, product, "ALIPAY", key)
	if err != nil || replay.ID != recovered.ID || replay.PaymentURL != recovered.PaymentURL || provider.creates.Load() != 2 {
		t.Fatal("completed receipt changed or redispatched", replay, err)
	}
}

func TestFinancePaymentRepairReceiptSurvivesCaseCompletionFailure(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET status='MANUAL_REVIEW',fulfillment_status='MANUAL_REVIEW' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	item, err := createPaymentAnomalyWithinTransaction(ctx, tx, "LATE_PAYMENT_CONFLICT", "P1", uuid.NewString(), paymentAnomalyReference{PaymentOrderID: &order.ID}, map[string]any{"controlled": true}, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	actor, key, reason := uuid.New(), uuid.NewString(), "controlled immutable payment repair"
	retainedClaimAnomaly(t, r, ctx, item.ID, actor)
	if _, _, err := r.PreparePaymentAnomalyResolution(ctx, item.ID, actor, key, "CONFIRM_PAYMENT", reason); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repairReviewedOrder(ctx, order.ID, actor, reason, item.ID, key); err != nil {
		t.Fatal("initial financial repair", err)
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION fixture_payment_receipt_failure() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'controlled completion fault'; END$$; CREATE TRIGGER fixture_payment_receipt_failure BEFORE INSERT ON payment_anomaly_commands FOR EACH ROW EXECUTE FUNCTION fixture_payment_receipt_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolvePaymentAnomaly(ctx, item.ID, actor, key, "CONFIRM_PAYMENT", reason, "APPLIED"); err == nil {
		t.Fatal("completion fault not reached")
	}
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP TRIGGER fixture_payment_receipt_failure ON payment_anomaly_commands; DROP FUNCTION fixture_payment_receipt_failure()`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.repairReviewedOrder(ctx, order.ID, actor, reason, item.ID, key); err != nil {
		t.Fatal("durable receipt did not bypass completed-order rejection", err)
	}
	if _, err := r.ResolvePaymentAnomaly(ctx, item.ID, actor, key, "CONFIRM_PAYMENT", reason, "APPLIED"); err != nil {
		t.Fatal("same command could not finish", err)
	}
	var posted, journals int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE') FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals); err != nil || posted != 1000000 || journals != 1 {
		t.Fatal("recovery duplicated financial effect", posted, journals, err)
	}
}

type cancelGateRefundProvider struct {
	*retainedFinancialProvider
	entered chan struct{}
}

func (p *cancelGateRefundProvider) Refund(ctx context.Context, _, _ string, _ int64) (ProviderRefund, error) {
	close(p.entered)
	<-ctx.Done()
	return ProviderRefund{}, ctx.Err()
}
func TestFinanceRefundCancelledCallReleasesFinancialGate(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, money, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	c.AllowNormalRefundCreation(true)
	refund, _, err := c.CreateRefund(ctx, order.ID, order.RequesterID, money, pack, uuid.NewString(), "controlled gate cancellation")
	if err != nil {
		t.Fatal(err)
	}
	provider := &cancelGateRefundProvider{retainedFinancialProvider: c.provider.(*retainedFinancialProvider), entered: make(chan struct{})}
	c.provider = provider
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- c.ProcessRefund(callCtx, refund.ID) }()
	select {
	case <-provider.entered:
	case <-ctx.Done():
		t.Fatal("provider gate not reached")
	}
	freezeCtx, stop := context.WithTimeout(ctx, 150*time.Millisecond)
	defer stop()
	if _, err := r.RecordAndFreezeForcedRefund(freezeCtx, money, "concurrent-before-cancel", 100, nil); err == nil {
		t.Fatal("financial writer passed active refund gate")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled provider looked successful")
		}
	case <-ctx.Done():
		t.Fatal("refund cancellation did not release gate")
	}
	if _, err := r.RecordAndFreezeForcedRefund(ctx, money, "concurrent-after-cancel", 100, nil); err != nil {
		t.Fatal("cancelled call left financial gate locked", err)
	}
	var posted int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted); err != nil || posted != 1000000 {
		t.Fatal("cancelled call changed money", posted, err)
	}
}

func TestFinanceCallbackWaitsForWalletBeforeLockingOrder(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	walletGate, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer walletGate.Rollback(context.Background())
	if _, err := getWallet(ctx, walletGate, order.WalletID, true); err != nil {
		t.Fatal(err)
	}
	callbackCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		tx, e := r.pool.Begin(callbackCtx)
		if e != nil {
			finished <- e
			return
		}
		defer tx.Rollback(context.Background())
		_, e = getPaymentOrderForUpdate(callbackCtx, tx, order.MerchantOrderNo)
		finished <- e
	}()
	// Wait for the real PostgreSQL callback connection to block on a lock, not
	// an arbitrary scheduling sleep, then prove it has not acquired the order.
	deadline := time.Now().Add(5 * time.Second)
	waiting := false
	for !waiting && time.Now().Before(deadline) {
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if !waiting {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !waiting {
		t.Fatal("callback did not reach wallet lock")
	}
	var id uuid.UUID
	if err := walletGate.QueryRow(ctx, `SELECT id FROM payment_orders WHERE id=$1 FOR UPDATE NOWAIT`, order.ID).Scan(&id); err != nil {
		t.Fatal("callback took order before wallet", err)
	}
	if err := walletGate.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("wallet/order callback deadlocked")
	}
}

func TestFinanceCreateDurableDispatchRecovery(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	base := c.provider.(*retainedFinancialProvider)
	base.result.Status = "PENDING"
	base.result.ChannelTransactionID, base.result.PaidAt = nil, nil
	provider := &coreRecoveryProvider{retainedFinancialProvider: base}
	c.provider = provider
	key := uuid.NewString()
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='NOT_STARTED' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	got, _, err := c.dispatchCreatedOrder(ctx, order, key, "fixture-hash", false)
	if err != nil || got.ID != order.ID || provider.creates.Load() != 1 {
		t.Fatal("pre-dispatch crash was not recovered exactly once", got, err)
	}
	if _, _, err = c.dispatchCreatedOrder(ctx, order, key, "fixture-hash", false); err != nil {
		t.Fatal(err)
	}
	if provider.creates.Load() != 1 {
		t.Fatal("completed dispatch issued Create again")
	}
	if _, err = r.pool.Exec(ctx, `DELETE FROM idempotency_records WHERE scope='payment.create' AND idempotency_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE payment_orders SET provider_call_status='IN_PROGRESS' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	base.result.Status = "PENDING"
	base.result.ChannelTransactionID, base.result.PaidAt = nil, nil
	base.result.PaymentURL = "https://provider.invalid/recovered"
	got, _, err = c.dispatchCreatedOrder(ctx, order, key, "fixture-hash", false)
	if err != nil || got.PaymentURL != base.result.PaymentURL || provider.creates.Load() != 1 {
		t.Fatal("ambiguous dispatch did not query stable merchant identity", got, err)
	}
}

func TestFinanceAnomalyOwnerAndProviderFailureAdmission(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, money, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	forced, err := r.RecordAndFreezeForcedRefund(ctx, money, "owner-fence", 100, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	owner, other := uuid.New(), uuid.New()
	caseID := *forced.ManualReviewCaseID
	retainedClaimAnomaly(t, r, ctx, caseID, owner)
	if _, _, err := r.PreparePaymentAnomalyResolution(ctx, caseID, other, uuid.NewString(), "CONFIRM_FORCED_REFUND", "controlled owner admission"); !errors.Is(err, ErrConflict) {
		t.Fatal("unclaimed actor bound immutable intent", err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM payment_anomaly_resolution_intents WHERE payment_anomaly_case_id=$1`, caseID).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected actor mutated intent", count, err)
	}
	if _, _, err := r.PreparePaymentAnomalyResolution(ctx, caseID, owner, uuid.NewString(), "CONFIRM_FORCED_REFUND", "controlled owner admission"); err != nil {
		t.Fatal("owner could not prepare", err)
	}
	if !validPaymentAnomalyResolution("ILLEGAL_TRANSITION", "CONFIRM_PROVIDER_FAILED", "NONE") || validPaymentAnomalyResolution("ILLEGAL_TRANSITION", "CONFIRM_PROVIDER_FAILED", "APPLIED") {
		t.Fatal("provider failure resolution admission contract")
	}
}

func TestFinanceConflictingSuccessPreservesCompletedMoney(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	provider := c.provider.(*retainedFinancialProvider)
	provider.result.AmountFen++
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	locked, err := getPaymentOrderByID(ctx, tx, order.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = c.mergeSuccessfulPaymentWithinTransaction(ctx, tx, locked, "QUERY", "controlled-conflict", nil, provider.result, false, r.now())
	if !errors.Is(err, ErrConflict) {
		t.Fatal("conflicting money accepted", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := getPaymentOrderByID(ctx, r.pool, order.ID, false)
	if err != nil || got.Status != "PAID" || got.FulfillmentStatus != "CREDITED" {
		t.Fatal("conflict downgraded completed payment", got, err)
	}
	var posted, journals int64
	if err := r.pool.QueryRow(ctx, `SELECT posted_microcredits,(SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE') FROM wallets WHERE id=$1`, order.WalletID).Scan(&posted, &journals); err != nil || posted != 1000000 || journals != 1 {
		t.Fatal("conflict changed credited money", posted, journals, err)
	}
}

func TestFinanceDismissRestoresOnlyFinancialGrantSuspension(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, money, pack := retainedFinanceOrder(t, r, ctx, 1000000, true)
	var component uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM entitlement_components WHERE package_id=$1 AND component_type='BASE'`, pack).Scan(&component); err != nil {
		t.Fatal(err)
	}
	financial, administrative := uuid.New(), uuid.New()
	for _, g := range []struct {
		id             uuid.UUID
		status, reason string
	}{{financial, "ACTIVE", ""}, {administrative, "SUSPENDED", "MEMBER_REMOVED"}} {
		if _, err := r.pool.Exec(ctx, `INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,suspension_reason,membership_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',100000,100000,$6,NULLIF($7,''),1,$3,$1::uuid::text,repeat('a',64),now()+interval '1 day',now(),now())`, g.id, uuid.New(), uuid.New(), pack, component, g.status, g.reason); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.pool.Exec(ctx, `UPDATE entitlement_components SET available_microcredits=available_microcredits-200000,active_grant_microcredits=100000,suspended_grant_microcredits=100000 WHERE id=$1`, component); err != nil {
		t.Fatal(err)
	}
	forced, err := r.RecordAndFreezeForcedRefund(ctx, money, "grant-financial-review", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	if _, err = retainedResolveClaimedAnomaly(t, r, ctx, *forced.ManualReviewCaseID, actor, uuid.NewString(), "DISMISS", "controlled disproved grant fence", "NONE"); err != nil {
		t.Fatal(err)
	}
	var financialStatus, adminStatus, reason string
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT status FROM member_quota_grants WHERE id=$1),(SELECT status FROM member_quota_grants WHERE id=$2),(SELECT suspension_reason FROM member_quota_grants WHERE id=$2)`, financial, administrative).Scan(&financialStatus, &adminStatus, &reason); err != nil || financialStatus != "ACTIVE" || adminStatus != "SUSPENDED" || reason != "MEMBER_REMOVED" {
		t.Fatal("suspension ownership not preserved", financialStatus, adminStatus, reason, err)
	}
	var active, suspended int64
	if err := r.pool.QueryRow(ctx, `SELECT active_grant_microcredits,suspended_grant_microcredits FROM entitlement_components WHERE id=$1`, component).Scan(&active, &suspended); err != nil || active != 100000 || suspended != 100000 {
		t.Fatal("grant conservation changed", active, suspended, err)
	}
}
