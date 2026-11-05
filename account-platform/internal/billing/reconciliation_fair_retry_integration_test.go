package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/billingschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func reconciliationFairRetryActualHTTP(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedFinanceRepository(t)
	now := r.now()
	orders := make([]PaymentOrder, 3)
	facts := map[string]ProviderOrder{}
	rows := []ProviderBillEntry{}
	for i := range orders {
		_, o, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		// Consistent historical simulator-origin pending LEGACY purchases, retained after the old catalog retired.
		o.ProviderID = "development_simulator"
		o.ProviderVersion = "development-simulator-v1"
		o.ProviderSKU = "historical-simulator-sku"
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id=$2,provider_version=$3,provider_sku=$4 WHERE id=$1`, o.ID, o.ProviderID, o.ProviderVersion, o.ProviderSKU); err != nil {
			t.Fatal(err)
		}
		orders[i] = o
		ref := uuid.NewString()
		rows = append(rows, ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "SUCCESS", PaidAt: now})
		facts[o.MerchantOrderNo] = ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Status: "SUCCESS", Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, ChannelTransactionID: &ref, PaidAt: &now, ExternalCustomerKey: o.RequesterID.String(), ExternalSKU: o.ProviderSKU}
	}
	var mu sync.Mutex
	calls := map[string]int{}
	var slow atomic.Bool
	slow.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer controlled-reviewer-provider" {
			http.Error(w, "credential missing", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q.URL.Path, "/internal/v1/bills/") {
			json.NewEncoder(w).Encode(map[string]any{"items": rows})
			return
		}
		name := strings.TrimPrefix(q.URL.Path, "/internal/v1/orders/")
		fact, ok := facts[name]
		if !ok {
			http.NotFound(w, q)
			return
		}
		mu.Lock()
		calls[name]++
		mu.Unlock()
		if slow.Load() && name != orders[2].MerchantOrderNo {
			<-q.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(fact)
	}))
	defer server.Close()
	provider, err := paymentsimulator.New(server.URL, "controlled-reviewer-provider")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	// Model an interrupted original caller; the installed recovery budget itself is unchanged.
	firstCtx, cancel := context.WithTimeout(ctx, reconciliationRecoverySlice)
	_, err = c.RunAdminReconciliation(firstCtx, uuid.New(), orders[0].Channel, reconciliationFixtureDateForPaidAt(now))
	cancel()
	var pending *reconciliationPendingError
	if !errors.As(err, &pending) {
		t.Fatal("slow provider did not leave recoverable durable run", err)
	}
	runID := pending.RunID
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := c.recoverReconciliationRuns(ctx); err == nil {
		t.Fatal("two slow requests unexpectedly completed the bounded recovery")
	}
	mu.Lock()
	counts := []int{calls[orders[0].MerchantOrderNo], calls[orders[1].MerchantOrderNo], calls[orders[2].MerchantOrderNo]}
	mu.Unlock()
	var processed int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_row_progress WHERE run_id=$1 AND processed_at IS NOT NULL`, runID).Scan(&processed); err != nil {
		t.Fatal(err)
	}
	if counts[0] != 2 || counts[1] != 2 || counts[2] != 1 || processed != 1 {
		t.Fatalf("next bounded recovery starved the never-attempted row: calls=%v processed=%d", counts, processed)
	}
	t.Logf("actual installed 5s HTTP client, real 10s recovery slice, original live breaker: calls=%v processed=%d", counts, processed)
	// Definite authority becomes available: the same immutable batch must then credit each order once.
	slow.Store(false)
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_runs SET next_attempt_at=clock_timestamp()-interval '1 second' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	if err := c.recoverReconciliationRuns(ctx); err != nil {
		t.Fatal("same original facts did not recover when upstream responded", err)
	}
	for _, o := range orders {
		wallet, err := r.GetWallet(ctx, o.WalletID)
		if err != nil || wallet.PostedMicrocredits != 1000000 {
			t.Fatalf("definite recovery mismatch: credit=%d err=%v", wallet.PostedMicrocredits, err)
		}
	}
	var journalCount int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&journalCount); err != nil || journalCount != 3 {
		t.Fatal("same-fact positive control failed conservation", journalCount, err)
	}
	t.Log("positive control: all three exact historical orders credited once after upstream recovery; not a live collection acceptance")
}

func TestReconciliationFairRetryRuntimePostgres(t *testing.T) {
	t.Run("circuit-refusals-retain-priority-and-local-hook-failure-returns-probes", reconciliationFairRetryCircuitAdmission)
	t.Run("actual-http-timeouts-do-not-starve-unattempted-row", reconciliationFairRetryActualHTTP)
	t.Run("cancelled-and-completed-replay-preserve-progress-and-financial-facts", reconciliationFairRetryCancellation)
	t.Run("forward-upgrade-preserves-immutable-input-ledger-and-old-migration-identities", reconciliationFairRetryUpgrade)
}

func reconciliationFairRetrySeed(t *testing.T, r *PostgresRepository, ctx context.Context) (*PaymentCoordinator, *recoveryBillProvider, uuid.UUID, PaymentOrder) {
	t.Helper()
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	now := r.now()
	ref := uuid.NewString()
	const sku = "controlled-fair-retry-sku"
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku=$2 WHERE id=$1`, order.ID, sku); err != nil {
		t.Fatal(err)
	}
	provider := &recoveryBillProvider{orders: map[string]ProviderOrder{order.MerchantOrderNo: {MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &ref, PaidAt: &now, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: sku}}, failing: map[string]bool{order.MerchantOrderNo: true}}
	row := ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ImportProviderBill(ctx, uuid.New(), order.Channel, reconciliationFixtureDateForPaidAt(now), []ProviderBillEntry{row})
	var pending *reconciliationPendingError
	if !errors.As(err, &pending) {
		t.Fatal("pending seed missing", err)
	}
	return c, provider, pending.RunID, order
}

func reconciliationFairRetrySnapshot(t *testing.T, r *PostgresRepository, ctx context.Context, id uuid.UUID, progress bool) string {
	t.Helper()
	var snapshot string
	progressSQL := "'[]'::jsonb"
	if progress {
		progressSQL = "(SELECT jsonb_agg(to_jsonb(p) ORDER BY row_number) FROM reconciliation_row_progress p WHERE run_id=$1)"
	}
	sql := `SELECT jsonb_build_object('input',(SELECT jsonb_agg(to_jsonb(b) ORDER BY row_number) FROM reconciliation_provider_rows b WHERE reconciliation_run_id=$1),'wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'journals',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j),'progress',` + progressSQL + `)::text`
	if err := r.pool.QueryRow(ctx, sql, id).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func reconciliationFairRetryCancellation(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	c, provider, id, order := reconciliationFairRetrySeed(t, r, ctx)
	before := reconciliationFairRetrySnapshot(t, r, ctx, id, true)
	callsBefore := provider.calls[order.MerchantOrderNo]
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.resumeReconciliationRun(cancelled, id); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled recovery did not return cancellation", err)
	}
	if after := reconciliationFairRetrySnapshot(t, r, ctx, id, true); before != after || provider.calls[order.MerchantOrderNo] != callsBefore {
		t.Fatal("cancelled-before-attempt changed progress, authority, input or money")
	}
	provider.failing[order.MerchantOrderNo] = false
	run, err := c.resumeReconciliationRun(ctx, id)
	if err != nil || run.Status != "COMPLETED" {
		t.Fatal("pending exact row did not finish", run.Status, err)
	}
	before = reconciliationFairRetrySnapshot(t, r, ctx, id, true)
	callsBefore = provider.calls[order.MerchantOrderNo]
	if _, err := c.resumeReconciliationRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if after := reconciliationFairRetrySnapshot(t, r, ctx, id, true); before != after || provider.calls[order.MerchantOrderNo] != callsBefore {
		t.Fatal("completed replay altered scheduling, input or financial facts")
	}
	var attempts int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_row_progress WHERE run_id=$1 AND last_attempted_at IS NOT NULL AND processed_at IS NOT NULL`, id).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatal("completed row lacks original durable attempt", attempts, err)
	}
}

func reconciliationFairRetryUpgrade(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, _, id, _ := reconciliationFairRetrySeed(t, r, ctx)
	before := reconciliationFairRetrySnapshot(t, r, ctx, id, false)
	var oldHistory string
	const history = `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY version),'[]'::jsonb)::text FROM schema_migrations m WHERE version<>900026`
	if err := r.pool.QueryRow(ctx, history).Scan(&oldHistory); err != nil {
		t.Fatal(err)
	}
	// Exact pre-900026 schema in this disposable UUID database; no published SQL is edited.
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `DROP INDEX reconciliation_row_progress_fair_retry_idx; ALTER TABLE reconciliation_row_progress DROP COLUMN last_attempted_at; DELETE FROM schema_migrations WHERE version=900026`); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + r.pool.Config().ConnConfig.Database
	u.User = url.User("billing_migrator")
	connection, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	// Match the installed migration entry: the NOINHERIT login explicitly
	// assumes the existing runtime role before the wrapper reads old history.
	if _, err = connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	if err = billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal("public old-schema upgrade", err)
	}
	migrations.Unlock(ctx, connection)
	var newHistory string
	if err := r.pool.QueryRow(ctx, history).Scan(&newHistory); err != nil || oldHistory != newHistory {
		t.Fatal("old migration identity changed", err)
	}
	if before != reconciliationFairRetrySnapshot(t, r, ctx, id, false) {
		t.Fatal("upgrade modified input, wallet or ledger bytes")
	}
	var attempted *string
	if err := r.pool.QueryRow(ctx, `SELECT last_attempted_at::text FROM reconciliation_row_progress WHERE run_id=$1`, id).Scan(&attempted); err != nil || attempted != nil {
		t.Fatal("old rows received invented attempt time", attempted, err)
	}
	ordered, err := billingschema.Ordered(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, connection, ordered); err != nil {
		t.Fatal("post-upgrade identity verification", err)
	}
	if err = billingschema.Up(ctx, connection, loaded); err != nil {
		t.Fatal("exact migration replay", err)
	}
	migrations.Unlock(ctx, connection)
	var down string
	for _, m := range loaded {
		if m.Version == 900026 {
			down = m.DownSQL
		}
	}
	if down == "" {
		t.Fatal("missing registered migration")
	}
	if _, err = retainedFinanceOwnerExec(t, r, ctx, down); err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatal("progress evidence rollback not refused", err)
	}
	if before != reconciliationFairRetrySnapshot(t, r, ctx, id, false) {
		t.Fatal("refused rollback changed financial or original input facts")
	}
}

// Existing pending facts and a reachable OPEN breaker are produced through the
// installed HTTP adapter; no clock shortening or transport timeout override.
func reconciliationFairRetryCircuitAdmission(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedFinanceRepository(t)
	now := r.now()
	orders := make([]PaymentOrder, 2)
	facts := map[string]ProviderOrder{}
	rows := []ProviderBillEntry{}
	for i := range orders {
		_, o, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
		o.ProviderID = "development_simulator"
		o.ProviderVersion = "development-simulator-v1"
		o.ProviderSKU = "historical-circuit-fair-sku"
		if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_id=$2,provider_version=$3,provider_sku=$4 WHERE id=$1`, o.ID, o.ProviderID, o.ProviderVersion, o.ProviderSKU); err != nil {
			t.Fatal(err)
		}
		orders[i] = o
		ref := uuid.NewString()
		rows = append(rows, ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: o.MerchantOrderNo, Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, Status: "SUCCESS", PaidAt: now})
		facts[o.MerchantOrderNo] = ProviderOrder{MerchantOrderNo: o.MerchantOrderNo, Status: "SUCCESS", Channel: o.Channel, AmountFen: o.AmountFen, Currency: o.Currency, ChannelTransactionID: &ref, PaidAt: &now, ExternalCustomerKey: o.RequesterID.String(), ExternalSKU: o.ProviderSKU}
	}
	var phase atomic.Int32
	var mu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Header.Get("Authorization") != "Bearer controlled-circuit-fair-provider" {
			http.Error(w, "missing credential", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(q.URL.Path, "/internal/v1/bills/") {
			json.NewEncoder(w).Encode(map[string]any{"items": rows})
			return
		}
		name := strings.TrimPrefix(q.URL.Path, "/internal/v1/orders/")
		fact, ok := facts[name]
		if !ok {
			http.NotFound(w, q)
			return
		}
		mu.Lock()
		calls[name]++
		mu.Unlock()
		if phase.Load() == 0 {
			http.Error(w, "controlled upstream unavailable", 503)
			return
		}
		if phase.Load() == 1 && name == orders[0].MerchantOrderNo {
			<-q.Context().Done()
			return
		}
		json.NewEncoder(w).Encode(fact)
	}))
	defer server.Close()
	provider, err := paymentsimulator.New(server.URL, "controlled-circuit-fair-provider")
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.RunAdminReconciliation(ctx, uuid.New(), orders[0].Channel, reconciliationFixtureDateForPaidAt(now))
	var pending *reconciliationPendingError
	if !errors.As(err, &pending) {
		t.Fatal("missing original pending batch", err)
	}
	id := pending.RunID
	key := resilience.Key(c.Capabilities().ProviderID+"@"+c.Capabilities().Version, "ORDER_QUERY", orders[0].Channel)
	// Two run queries plus three real polling failures reach the original threshold.
	for i := 0; i < 3; i++ {
		if _, err := providerCall(ctx, c, "ORDER_QUERY", orders[0].Channel, false, providerCallScope(orders[0]), func() (ProviderOrder, error) { return provider.Get(ctx, orders[0].MerchantOrderNo) }); err == nil {
			t.Fatal("controlled failure missing")
		}
	}
	if _, err := c.breaker.BeforeReadOnly(key); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatal("real five failures did not open the circuit", err)
	}
	phase.Store(1)
	before := reconciliationFairRetrySnapshot(t, r, ctx, id, true)
	if _, err := c.resumeReconciliationRun(ctx, id); err == nil {
		t.Fatal("OPEN circuit accepted authority")
	}
	if before != reconciliationFairRetrySnapshot(t, r, ctx, id, true) {
		t.Fatal("OPEN admission rewrote unattempted-row progress or money")
	}
	timer := time.NewTimer(31 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// A failed local scheduler write must release each half-open permit, without
	// creating provider attempts/health failures or entering the network callback.
	var attemptsBefore, healthBefore int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_provider_attempts),(SELECT count(*) FROM payment_provider_health_observations)`).Scan(&attemptsBefore, &healthBefore); err != nil {
		t.Fatal(err)
	}
	localErr := errors.New("controlled local scheduling persistence failure")
	for i := 0; i < 5; i++ {
		_, err := providerCallWithAdmissionHook(ctx, c, "ORDER_QUERY", orders[0].Channel, false, providerCallScope(orders[0]), func() error { return localErr }, func() (ProviderOrder, error) { t.Error("local failure entered provider"); return ProviderOrder{}, nil })
		if !errors.Is(err, localErr) {
			t.Fatal("failed hook leaked half-open permit or rewrote error", i, err)
		}
	}
	var attemptsAfter, healthAfter int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM payment_provider_attempts),(SELECT count(*) FROM payment_provider_health_observations)`).Scan(&attemptsAfter, &healthAfter); err != nil || attemptsAfter != attemptsBefore || healthAfter != healthBefore {
		t.Fatal("local scheduler failure invented provider failure", attemptsBefore, attemptsAfter, healthBefore, healthAfter, err)
	}
	mu.Lock()
	slowBefore, fastBefore := calls[orders[0].MerchantOrderNo], calls[orders[1].MerchantOrderNo]
	mu.Unlock()
	if _, err := c.resumeReconciliationRun(ctx, id); err == nil {
		t.Fatal("slow half-open query incorrectly completed")
	}
	mu.Lock()
	slowAfter, fastAfter := calls[orders[0].MerchantOrderNo], calls[orders[1].MerchantOrderNo]
	mu.Unlock()
	var next int
	if err := r.pool.QueryRow(ctx, `SELECT row_number FROM reconciliation_row_progress WHERE run_id=$1 AND processed_at IS NULL ORDER BY last_attempted_at NULLS FIRST,row_number LIMIT 1`, id).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if slowAfter != slowBefore+1 || fastAfter != fastBefore || next != 2 {
		t.Fatalf("half-open refusal lost fast-row priority: slowDelta=%d fastDelta=%d next=%d", slowAfter-slowBefore, fastAfter-fastBefore, next)
	}
	t.Logf("real 30s half-open + 5s HTTP failure: slowDelta=%d fastDelta=%d next=%d; OPEN did not authorize fast dispatch", slowAfter-slowBefore, fastAfter-fastBefore, next)
	// A real process restart gets a fresh breaker; same original batch/facts
	// remains query-only and posts each source once when the producer recovers.
	phase.Store(2)
	restarted, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.resumeReconciliationRun(ctx, id); err != nil {
		t.Fatal("same durable batch could not resume after restart", err)
	}
	for _, o := range orders {
		w, err := r.GetWallet(ctx, o.WalletID)
		if err != nil || w.PostedMicrocredits != 1000000 {
			t.Fatal("restart financial control", w.PostedMicrocredits, err)
		}
	}
	var journals int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&journals); err != nil || journals != 2 {
		t.Fatal("duplicate or missing financial effect", journals, err)
	}
}
