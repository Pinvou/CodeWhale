package token

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Reviewer-only probes: external billing is controlled, provider HTTP is real.
type preparationFaultRepository struct {
	*credentialWindowRepository
	prepareEntered, prepareContinue chan struct{}
	prepares                        atomic.Int64
	mode                            string
	failRead                        bool
}

func (r *preparationFaultRepository) PrepareDispatch(ctx context.Context, id uuid.UUID, e DispatchEnvelope) error {
	if r.prepares.Add(1) != 1 {
		return r.PostgresRepository.PrepareDispatch(ctx, id, e)
	}
	if r.mode == "before-commit" {
		c, cancel := context.WithCancel(ctx)
		cancel()
		return r.PostgresRepository.PrepareDispatch(c, id, e)
	}
	if err := r.PostgresRepository.PrepareDispatch(ctx, id, e); err != nil {
		return err
	}
	if r.prepareEntered != nil {
		close(r.prepareEntered)
		select {
		case <-r.prepareContinue:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return errors.New("controlled committed preparation lost ACK")
}
func (r *preparationFaultRepository) LoadReplay(ctx context.Context, id uuid.UUID, after int64) (ReplaySnapshot, error) {
	if r.failRead {
		return ReplaySnapshot{}, errors.New("controlled authoritative read unavailable")
	}
	return r.PostgresRepository.LoadReplay(ctx, id, after)
}
func testPreparationCommittedACKPeerPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, base, ctx)
	f.providerEntered, f.providerContinue = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(f.providerContinue) })
	t.Cleanup(release)
	wrapper := &preparationFaultRepository{credentialWindowRepository: f.r, prepareEntered: make(chan struct{}), prepareContinue: make(chan struct{})}
	resume := sync.OnceFunc(func() { close(wrapper.prepareContinue) })
	t.Cleanup(resume)
	f.service.repository = wrapper
	original := make(chan *httptest.ResponseRecorder, 1)
	go func() { original <- f.request(f.body) }()
	select {
	case <-wrapper.prepareEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("preparation commit barrier not reached")
	}
	var u UsageRequest
	select {
	case u = <-f.r.accepted:
	case <-time.After(time.Second):
		t.Fatal("acceptance missing")
	}
	var preparations int
	if err := base.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_preparations WHERE usage_request_id=$1`, u.ID).Scan(&preparations); err != nil || preparations != 1 {
		t.Fatal("actual committed prep missing", err)
	}
	peer := make(chan *httptest.ResponseRecorder, 1)
	go func() { peer <- f.request(f.body) }()
	select {
	case <-f.providerEntered:
	case w := <-peer:
		t.Fatal("peer did not dispatch", w.Code, w.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("peer provider barrier not reached")
	}
	before, err := base.GetUsage(ctx, u.ID)
	if err != nil || before.ExecutionStatus != "UPSTREAM_DISPATCHING" || before.SettlementStatus != "HOLD_ACTIVE" || before.HoldID == nil {
		t.Fatal("actual peer effects missing", err)
	}
	resume()
	first := <-original
	after, err := base.GetUsage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("actual committed prep=1; originalHTTP=%d; before=%s/%s/v%d after=%s/%s/v%d; actualProviderCalls=%d controlledReserveCalls=%d", first.Code, before.ExecutionStatus, before.SettlementStatus, before.ExecutionVersion, after.ExecutionStatus, after.SettlementStatus, after.ExecutionVersion, f.providerCalls.Load(), f.money.reserves.Load())
	release()
	select {
	case <-peer:
	case <-time.After(10 * time.Second):
		t.Fatal("peer cleanup timeout")
	}
	if after.ExecutionStatus != before.ExecutionStatus || after.SettlementStatus != before.SettlementStatus || after.ExecutionVersion != before.ExecutionVersion || after.HoldID == nil || *after.HoldID != *before.HoldID {
		t.Error("PREPARATION_ACK_ACTOR_OVERWROTE_REAL_PEER_FACTS")
	}
	if first.Code != 202 || !strings.Contains(first.Body.String(), u.ID.String()) {
		t.Error("preparation failure did not project original progressed usage", first.Code)
	}
}

func TestPreparationFailurePreservesAcceptedUsagePostgres(t *testing.T) {
	t.Run("peer-effects-survive-lost-preparation-ack", testPreparationCommittedACKPeerPostgres)
	for _, mode := range []string{"committed-no-peer", "before-commit", "read-unconfirmed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("APP_ENV", "test")
			t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
			t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
			base, ctx := retainedManagementTokenRepository(t)
			f := newCredentialWindowFixture(t, base, ctx)
			repository := &preparationFaultRepository{credentialWindowRepository: f.r, mode: mode, failRead: mode == "read-unconfirmed"}
			f.service.repository = repository
			first := f.request(f.body)
			u := <-f.r.accepted
			want := 202
			if mode == "read-unconfirmed" {
				want = 503
			}
			if first.Code != want || first.Header().Get("X-Pinvou-Usage-Request-ID") != u.ID.String() || !strings.Contains(first.Body.String(), u.ID.String()) {
				t.Fatal("accepted identity unavailable", first.Code)
			}
			if mode == "read-unconfirmed" && !strings.Contains(first.Body.String(), "USAGE_STATE_UNCONFIRMED") {
				t.Fatal("unknown read became terminal")
			}
			actual, err := base.GetUsage(ctx, u.ID)
			if err != nil || actual.ExecutionStatus != "RECEIVED" || actual.HoldID != nil || actual.ExecutionVersion != 1 {
				t.Fatal("failed prepare inferred terminal or changed original", err)
			}
			if f.providerCalls.Load() != 0 || f.money.reserves.Load() != 0 || f.money.releases.Load() != 0 {
				t.Fatal("failure handler performed external effect")
			}
			var count int
			if err := base.pool.QueryRow(ctx, `SELECT count(*) FROM usage_dispatch_preparations WHERE usage_request_id=$1`, u.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			expected := 1
			if mode == "before-commit" {
				expected = 0
			}
			if count != expected {
				t.Fatal("fixture did not reach expected commit boundary", count)
			}
			repository.failRead = false
			again := f.request(f.body)
			if again.Code != 200 || again.Header().Get("X-Pinvou-Usage-Request-ID") != u.ID.String() {
				t.Fatal("original explicit retry did not recover", again.Code)
			}
			if f.providerCalls.Load() != 1 || f.money.reserves.Load() != 1 || f.money.commits.Load() != 1 || f.money.releases.Load() != 0 {
				t.Fatal("retry duplicated or lost external side effect")
			}
			if err := base.pool.QueryRow(ctx, `SELECT count(*) FROM usage_requests WHERE user_id=$1`, f.user).Scan(&count); err != nil || count != 1 {
				t.Fatal("retry replaced original usage", err, count)
			}
		})
	}
}
