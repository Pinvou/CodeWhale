package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/google/uuid"
)

type sequentialEvidenceBarrier struct {
	capturedUnavailableEvidence
	entered chan string
	resume  chan struct{}
	first   sync.Once
	mu      sync.Mutex
	queries []string
}

func (b *sequentialEvidenceBarrier) Lookup(ctx context.Context, reference string) (UsageEvidence, error) {
	b.mu.Lock()
	b.queries = append(b.queries, reference)
	b.mu.Unlock()
	b.first.Do(func() {
		b.entered <- reference
		select {
		case <-b.resume:
		case <-ctx.Done():
		}
	})
	return UsageEvidence{}, errors.New("controlled unavailable evidence")
}

func TestReconciliationSequentialClaimsPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	funds, _, pool := retainedManagementBillingRepository(t)
	var clock atomic.Int64
	r.now = func() time.Time {
		if now := clock.Load(); now != 0 {
			return time.Unix(0, now).UTC()
		}
		return time.Now().UTC()
	}
	server := httptest.NewServer(billing.NewService(funds).Handler("controlled-sequential-money"))
	defer server.Close()
	client, err := NewHTTPBillingClient(server.URL, "controlled-sequential-money")
	if err != nil {
		t.Fatal(err)
	}
	var cases []f05DeadlineCase
	references := make(map[uuid.UUID]string)
	for range 2 {
		now := time.Now().UTC()
		item := f05DeadlineSeedThroughChat(t, ctx, r, funds, pool, client, &clock, now.Add(-time.Hour), now.Add(-time.Minute), true, false)
		reference := "sequential-" + uuid.NewString()
		if err := r.RecordUpstreamRequest(ctx, item.usage.ID, UpstreamRequestEvidence{ProviderID: "controlled", AdapterVersion: NewHTTPUpstreamClient().AdapterVersion(), RequestID: reference}); err != nil {
			t.Fatal(err)
		}
		if err := r.RescheduleReconciliation(ctx, item.usage.ID, "USAGE_EVIDENCE_UNAVAILABLE", now.Add(-time.Second)); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, item)
		references[item.usage.ID] = reference
	}
	financial := f05DeadlineBillingFacts(t, ctx, pool)
	clock.Store(time.Now().UTC().UnixNano())
	defer clock.Store(0)
	barrier := &sequentialEvidenceBarrier{entered: make(chan string, 1), resume: make(chan struct{})}
	var release sync.Once
	resume := func() { release.Do(func() { close(barrier.resume) }) }
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	worker := NewUsageReconciler(r, client, barrier, time.Second)
	t.Cleanup(func() {
		cancel()
		resume()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("owned reconciliation worker did not stop")
		}
	})
	go func() { defer close(done); worker.runOnce(runCtx) }()
	var first string
	select {
	case first = <-barrier.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Advance only the fixture clock. The original 45-second lease and all
	// production request/financial budgets remain unchanged.
	clock.Add(int64(46 * time.Second))
	claimed, err := r.ClaimReconciliation(ctx, 50, 45*time.Second)
	if err != nil || len(claimed) != 2 {
		t.Fatal("second worker did not claim both actual eligible items", len(claimed), err)
	}
	schedule := func(id uuid.UUID) string {
		t.Helper()
		var value string
		if err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object('attempt',reconciliation_attempts,'lease',reconciliation_lease_until,'next',reconciliation_next_at,'error',reconciliation_last_error,'stopped',reconciliation_stopped_at,'status',status,'settlement',settlement_status)::text FROM usage_requests WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	before := make(map[uuid.UUID]string)
	for _, item := range claimed {
		before[item.ID] = schedule(item.ID)
		if references[item.ID] != first && item.Attempts != 1 {
			t.Error("queued item was leased before its worker was ready")
		}
	}
	resume()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	barrier.mu.Lock()
	queries := append([]string(nil), barrier.queries...)
	barrier.mu.Unlock()
	if len(queries) != 1 || queries[0] != first {
		t.Error("old worker processed a queued item now owned by another claim", len(queries))
	}
	for _, item := range claimed {
		if schedule(item.ID) != before[item.ID] {
			t.Error("old attempt changed the newer scheduling or terminal facts")
		}
	}
	f05DeadlineNoBillingChange(t, ctx, pool, financial)
	for _, item := range cases {
		if item.providerCalls.Load() != 1 {
			t.Error("recovery re-dispatched the original model request")
		}
	}
	// The new owner can consume trusted numeric evidence along the unchanged
	// commit path; replay must not create a second financial effect.
	current := NewUsageReconciler(r, client, capturedNumericEvidence{}, time.Second)
	for _, item := range claimed {
		if err := current.reconcile(ctx, item); err != nil {
			t.Fatal("current owner did not progress with trusted evidence", err)
		}
		committed, err := r.GetUsage(ctx, item.ID)
		if err != nil || committed.Status != "COMMITTED" || committed.ActualMicrocredits == nil || *committed.ActualMicrocredits != 0 {
			t.Fatal("current owner lost the original zero-usage terminal", err)
		}
		settled := f05DeadlineBillingFacts(t, ctx, pool)
		if err := current.reconcile(ctx, item); err != nil {
			t.Fatal("original current-claim result did not replay", err)
		}
		f05DeadlineNoBillingChange(t, ctx, pool, settled)
	}
}
