package billing

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"
)

type recoveryBillProvider struct {
	reconciliationFixtureProvider
	orders  map[string]ProviderOrder
	failing map[string]bool
	calls   map[string]int
}

func (p *recoveryBillProvider) Get(_ context.Context, merchant string) (ProviderOrder, error) {
	if p.calls == nil {
		p.calls = map[string]int{}
	}
	p.calls[merchant]++
	if p.failing[merchant] {
		return ProviderOrder{}, errors.New("controlled unavailable provider")
	}
	return p.orders[merchant], nil
}

type trackedBillProvider struct {
	reconciliationFixtureProvider
	bills         int
	failBill      bool
	sourceVersion string
}

func (p *trackedBillProvider) Bill(context.Context, string, time.Time) ([]ProviderBillEntry, error) {
	p.bills++
	if p.failBill {
		return nil, errors.New("controlled bill unavailable")
	}
	return p.rows, nil
}
func (p *trackedBillProvider) Capabilities() PaymentCapabilities {
	caps := p.reconciliationFixtureProvider.Capabilities()
	if p.sourceVersion != "" {
		caps.Version = p.sourceVersion
	}
	return caps
}

func pipelineRefundRow(order PaymentOrder, now time.Time) ProviderBillEntry {
	ref := uuid.NewString()
	return ProviderBillEntry{EntryType: "REFUND", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now}
}
func TestF03PipelineStableCasesAndVerifiedResolution(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	actor := uuid.New()
	row := pipelineRefundRow(order, r.now())
	provider := &trackedBillProvider{reconciliationFixtureProvider: reconciliationFixtureProvider{rows: []ProviderBillEntry{row}}}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET updated_at=updated_at+interval '1 second' WHERE id=$1`, order.ID); err != nil {
		t.Fatal(err)
	}
	second, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now())
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.AdminReconciliationCases(ctx, first.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.AdminReconciliationCases(ctx, second.ID, 10)
	if err != nil || len(a) != 1 || len(b) != 1 || a[0].ID != b[0].ID {
		t.Fatalf("stable case lost batch association: %v/%v %v", a, b, err)
	}
	var count int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_cases`).Scan(&count); err != nil || count != 1 {
		t.Fatal("unrelated timestamp created duplicate review", count, err)
	}
	id := a[0].ID
	if _, err := r.UpdateReconciliationCase(ctx, id, ReconciliationCaseCommand{Action: "CLAIM", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled owner claim reason"}); err != nil {
		t.Fatal(err)
	}
	command := ReconciliationCaseCommand{Action: "RESOLVE", ActorID: actor, IdempotencyKey: uuid.NewString(), Reason: "controlled verified resolution", ReviewResult: "text alone proves nothing"}
	beforeCalls := provider.bills
	wrongOwner := command
	wrongOwner.ActorID = uuid.New()
	if err := c.prepareReconciliationResolution(ctx, id, &wrongOwner); !errors.Is(err, ErrConflict) || provider.bills != beforeCalls {
		t.Fatal("nonowner resolution reached provider", err)
	}
	invalid := command
	invalid.Reason = "short"
	if err := c.prepareReconciliationResolution(ctx, id, &invalid); !errors.Is(err, ErrConflict) || provider.bills != beforeCalls {
		t.Fatal("invalid resolution reached provider", err)
	}
	if _, err := r.UpdateReconciliationCase(ctx, id, command); !errors.Is(err, ErrConflict) {
		t.Fatal("text-only resolution accepted", err)
	}
	if err := c.prepareReconciliationResolution(ctx, id, &command); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateReconciliationCase(ctx, id, command); !errors.Is(err, ErrConflict) {
		t.Fatal("fresh but unresolved supplier fact closed", err)
	}
	provider.rows = nil
	if err := c.prepareReconciliationResolution(ctx, id, &command); err != nil {
		t.Fatal(err)
	}
	// A newer provider observation invalidates an earlier clean proof.
	provider.rows = []ProviderBillEntry{row}
	if _, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now()); err != nil {
		t.Fatal(err)
	}
	if _, err := r.UpdateReconciliationCase(ctx, id, command); !errors.Is(err, ErrConflict) {
		t.Fatal("stale clean proof closed current difference", err)
	}
	provider.rows = nil
	if err := c.prepareReconciliationResolution(ctx, id, &command); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.UpdateReconciliationCase(ctx, id, command)
	if err != nil || resolved.Status != "RESOLVED" {
		t.Fatal("verified absent difference cannot close", err)
	}
	// Repeated terminal command does not need another external proof even after
	// the supplier fails and a different adapter version is configured.
	provider.failBill = true
	provider.sourceVersion = "different-version"
	beforeCalls = provider.bills
	if err := c.prepareReconciliationResolution(ctx, id, &command); err != nil {
		t.Fatal(err)
	}
	replay, err := r.UpdateReconciliationCase(ctx, id, command)
	if err != nil || replay.Version != resolved.Version {
		t.Fatal("terminal receipt changed", err)
	}
	if provider.bills != beforeCalls {
		t.Fatal("saved terminal receipt queried changed supplier")
	}
	provider.failBill = false
	provider.sourceVersion = ""
	provider.rows = []ProviderBillEntry{row}
	third, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now())
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := r.AdminReconciliationCases(ctx, third.ID, 10)
	if err != nil || len(reopened) != 1 || reopened[0].ID != id || reopened[0].Status != "OPEN" {
		t.Fatal("recurrent difference hidden by resolved case", err)
	}
	changed := row
	changed.AmountFen++
	provider.rows = []ProviderBillEntry{changed}
	fourth, err := c.RunAdminReconciliation(ctx, actor, order.Channel, r.now())
	if err != nil {
		t.Fatal(err)
	}
	newCases, err := r.AdminReconciliationCases(ctx, fourth.ID, 10)
	if err != nil || len(newCases) != 1 || newCases[0].ID == id {
		t.Fatal("changed monetary fact collapsed into old acceptance", err)
	}
}

func TestF03PipelineFailedAuthorityDoesNotStarveOtherRows(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	actor := uuid.New()
	now := r.now()
	_, first, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	_, second, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	if _, err := r.pool.Exec(ctx, `UPDATE payment_orders SET provider_sku='controlled-pipeline-sku' WHERE id=ANY($1::uuid[])`, []uuid.UUID{first.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	provider := &recoveryBillProvider{orders: map[string]ProviderOrder{}, failing: map[string]bool{first.MerchantOrderNo: true}}
	for _, order := range []PaymentOrder{first, second} {
		ref := uuid.NewString()
		provider.rows = append(provider.rows, ProviderBillEntry{EntryType: "PAYMENT", ProviderReferenceID: ref, ChannelTransactionID: ref, MerchantOrderNo: order.MerchantOrderNo, Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, Status: "SUCCESS", PaidAt: now})
		provider.orders[order.MerchantOrderNo] = ProviderOrder{MerchantOrderNo: order.MerchantOrderNo, Status: "SUCCESS", Channel: order.Channel, AmountFen: order.AmountFen, Currency: order.Currency, ChannelTransactionID: &ref, PaidAt: &now, ExternalCustomerKey: order.RequesterID.String(), ExternalSKU: "controlled-pipeline-sku"}
	}
	c, err := NewPaymentCoordinatorWithProvider(r, provider, "test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.ImportProviderBill(ctx, actor, first.Channel, now, provider.rows)
	var pending *reconciliationPendingError
	if !errors.As(err, &pending) {
		t.Fatal("failed provider incorrectly completed", err)
	}
	var status string
	var completed *time.Time
	if err := r.pool.QueryRow(ctx, `SELECT status,completed_at FROM reconciliation_runs WHERE id=$1`, pending.RunID).Scan(&status, &completed); err != nil || status != "FAILED" || completed != nil {
		t.Fatal("failure completion facts incorrect", status, completed, err)
	}
	a, _ := r.GetWallet(ctx, first.WalletID)
	b, _ := r.GetWallet(ctx, second.WalletID)
	if a.PostedMicrocredits != 0 || b.PostedMicrocredits != 1000000 {
		t.Fatal("failed first row starved second", a.PostedMicrocredits, b.PostedMicrocredits)
	}
	secondCalls := provider.calls[second.MerchantOrderNo]
	provider.failing[first.MerchantOrderNo] = false
	recovered, err := c.resumeReconciliationRun(ctx, pending.RunID)
	if err != nil || recovered.Status != "COMPLETED" || recovered.CompletedAt == nil {
		t.Fatal("durable run did not recover", err)
	}
	a, _ = r.GetWallet(ctx, first.WalletID)
	b, _ = r.GetWallet(ctx, second.WalletID)
	var journals int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='RECHARGE'`).Scan(&journals); err != nil || journals != 2 || a.PostedMicrocredits != 1000000 || b.PostedMicrocredits != 1000000 || provider.calls[second.MerchantOrderNo] != secondCalls {
		t.Fatal("recovery repeated completed monetary work", journals, err)
	}
}

func TestF03PipelineHundredThousandRowsWithoutFinancialLock(t *testing.T) {
	r, _ := retainedFinanceRepository(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	row := pipelineRefundRow(order, r.now())
	input := make([]ProviderBillEntry, 100000)
	for i := range input {
		input[i] = row
	}
	c, err := NewPaymentCoordinatorWithProvider(r, &reconciliationFixtureProvider{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	writerDone := make(chan struct{})
	var mu sync.Mutex
	writes := 0
	var writerErr error
	writerConnection, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writerConnection.Release()
	pid := writerConnection.Conn().PgConn().PID()
	samplerDone := make(chan struct{})
	samples, blockedSamples := 0, 0
	maxLatency := time.Duration(0)
	lastWait := ""
	go func() {
		defer close(samplerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			var wait string
			var blockers int
			if err := r.pool.QueryRow(ctx, `SELECT COALESCE(wait_event_type,''),cardinality(pg_blocking_pids($1)) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&wait, &blockers); err == nil {
				mu.Lock()
				samples++
				lastWait = wait
				if wait == "Lock" || blockers > 0 {
					blockedSamples++
				}
				mu.Unlock()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
			before := time.Now()
			_, err := writerConnection.Exec(bounded, `UPDATE payment_orders SET updated_at=clock_timestamp() WHERE id=$1`, order.ID)
			cancel()
			mu.Lock()
			if elapsed := time.Since(before); elapsed > maxLatency {
				maxLatency = elapsed
			}
			if err != nil {
				writerErr = err
				mu.Unlock()
				return
			}
			writes++
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()
	start := time.Now()
	run, err := c.ImportProviderBill(ctx, uuid.New(), order.Channel, r.now(), input)
	close(stop)
	<-writerDone
	<-samplerDone
	if err != nil {
		t.Fatal("actual 100000-row import failed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if writerErr != nil || writes == 0 || blockedSamples != 0 || samples == 0 {
		t.Fatal("observation batch financial writer diagnostics", "writes", writes, "samples", samples, "blocked samples", blockedSamples, "last wait", lastWait, "max latency", maxLatency, writerErr)
	}
	var stored, processed int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_provider_rows WHERE reconciliation_run_id=$1),(SELECT count(*) FROM reconciliation_row_progress WHERE run_id=$1 AND processed_at IS NOT NULL)`, run.ID).Scan(&stored, &processed); err != nil || stored != 100000 || processed != 100000 || run.Status != "COMPLETED" || run.CaseCount != 2 {
		t.Fatal("large batch lost durable facts or duplicated cases", stored, processed, run.CaseCount, err)
	}
	t.Logf("actual rows=%d processed=%d elapsed=%s concurrent order writes=%d PGsamples=%d blockedSamples=%d maxWriterLatency=%s", stored, processed, time.Since(start), writes, samples, blockedSamples, maxLatency)
}

func TestF03PipelineDistinctCasesResumeAfterCommittedProgress(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, false)
	if _, err := retainedFinanceOwnerExec(t, r, ctx, `CREATE FUNCTION controlled_review_delay() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.event_type='CREATED' THEN PERFORM pg_sleep(0.003); END IF; RETURN NEW; END $$; CREATE TRIGGER controlled_review_delay BEFORE INSERT ON manual_review_case_events FOR EACH ROW EXECUTE FUNCTION controlled_review_delay()`); err != nil {
		t.Fatal("controlled delay fixture installation failed", err)
	}
	rows := make([]ProviderBillEntry, 1000)
	for i := range rows {
		rows[i] = pipelineRefundRow(order, r.now())
	}
	c, err := NewPaymentCoordinatorWithProvider(r, &reconciliationFixtureProvider{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	operation, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := c.ImportProviderBill(operation, uuid.New(), order.Channel, r.now(), rows)
		result <- err
	}()
	progress := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_observation_progress`).Scan(&progress); err != nil {
			t.Fatal(err)
		}
		if progress >= 100 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if progress < 100 {
		t.Fatal("case stage failed to commit bounded progress", progress)
	}
	cancel()
	var pending *reconciliationPendingError
	if err := <-result; !errors.As(err, &pending) {
		t.Fatal("interruption did not preserve recovery identity", err)
	}
	var committed int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM reconciliation_observation_progress`).Scan(&committed); err != nil || committed < 100 || committed >= 1000 {
		t.Fatal("did not interrupt between short committed case batches", committed, err)
	}
	completed, err := c.resumeReconciliationRun(ctx, pending.RunID)
	if err != nil || completed.Status != "COMPLETED" || completed.CaseCount != 1000 {
		t.Fatal("distinct cases could not resume", completed.CaseCount, err)
	}
	var observations, cases, created int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM reconciliation_observations),(SELECT count(*) FROM reconciliation_cases),(SELECT count(*) FROM manual_review_case_events WHERE event_type='CREATED')`).Scan(&observations, &cases, &created); err != nil || observations != 1 || cases != 1000 || created != 1000 {
		t.Fatal("resume recreated observations or cases", observations, cases, created, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE reconciliation_observations SET facts_digest='altered'`); err == nil {
		t.Fatal("audit observation is mutable")
	}
	t.Logf("distinct cases=1000 committed before interruption=%d resumed exactly once", committed)
}

func TestF03InternalMissingReferencesHaveDistinctStableIdentity(t *testing.T) {
	run := ReconciliationRun{ProviderID: "provider", ProviderVersion: "version", Channel: "ALIPAY", BusinessDate: "2026-10-08"}
	order := uuid.New()
	first := differenceFromFact(ReconciliationInternalFact{EntryType: "REFUND", ProviderReferenceID: "refund-a", PaymentOrderID: order, MerchantOrderNo: "same-order", AmountFen: 100, Currency: "CNY"}, "INTERNAL_REFUND_PROVIDER_MISSING", "P1", nil)
	second := first
	second.InternalReference = "refund-b"
	if reconciliationDifferenceDigest(run, first) == reconciliationDifferenceDigest(run, second) {
		t.Fatal("distinct missing refund source collapsed")
	}
	third := first
	third.InternalAmountFen++
	if reconciliationDifferenceDigest(run, first) == reconciliationDifferenceDigest(run, third) {
		t.Fatal("changed internal refund amount collapsed")
	}
}
