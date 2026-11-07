package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/google/uuid"
)

func TestDispatchCircuitReadOnlyRecoveryPublicPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, r, ctx)
	var reads, posts atomic.Int32
	var failReads atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if q.Header.Get("Authorization") != "Bearer controlled-window-old-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case q.Method == http.MethodGet && q.URL.Path == "/v1/models":
			reads.Add(1)
			if failReads.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, f.r.model.UpstreamModelRef)
		case q.Method == http.MethodPost && q.URL.Path == "/v1/chat/completions":
			posts.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"controlled result"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	f.providerHandler.Store(&handler)
	// Use the actual production installation method with the same concrete
	// repository and adapter as the worker. The request fixture retains its
	// controlled authorization/catalog/Billing boundaries and shares this breaker.
	wired := &Service{}
	if _, err := wired.WithPersonalModelExecution(r, f.money, f.service.executionUpstream, 1000, 10, 100); err != nil {
		t.Fatal(err)
	}
	f.service.breaker = wired.breaker
	worker, err := NewRouteHealthWorker(r, f.service.executionUpstream.(RouteHealthProbeAdapter), f.service.providerURLs, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = wired.WithRouteHealthCircuitRecovery(worker); err != nil {
		t.Fatal(err)
	}
	credential, secret, err := r.CredentialSecret(ctx, f.account, f.credential)
	clear(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL)
	// These are five controlled observations, not five paid model requests.
	for range 5 {
		wired.breaker.Record(key, resilience.InfrastructureFailure)
	}
	first := f.request(f.body)
	if first.Code != http.StatusServiceUnavailable || !strings.Contains(first.Body.String(), "PROVIDER_CIRCUIT_OPEN") {
		t.Fatal("fresh dispatch was not blocked", first.Code)
	}
	if f.money.reserves.Load() != 0 || f.money.commits.Load() != 0 || posts.Load() != 0 {
		t.Fatal("open circuit performed financial/model effects")
	}
	probe := func(expected string) {
		t.Helper()
		// Only advance this fixture's probe due time; claim/version/lease,
		// credential reads, HTTP discovery and completion use real writers.
		if _, err := r.pool.Exec(ctx, `UPDATE routing_policies SET next_probe_at=clock_timestamp() WHERE token_account_id=$1`, f.account); err != nil {
			t.Fatal(err)
		}
		target, ok := worker.claim(ctx)
		if !ok {
			t.Fatal("actual route-health worker did not claim fixture route")
		}
		worker.execute(ctx, target)
		var status string
		if err := r.pool.QueryRow(ctx, `SELECT health_status FROM routing_policies WHERE id=$1`, target.RouteID).Scan(&status); err != nil || status != expected {
			t.Fatal("actual health completion failed", status, err)
		}
	}
	probe("HEALTHY")
	if reads.Load() != 1 || posts.Load() != 0 {
		t.Fatal("health recovery did not use GET-only provider path", reads.Load(), posts.Load())
	}
	var open *resilience.CircuitOpenError
	if err = wired.breaker.BeforeHighRisk(key); !errors.As(err, &open) {
		t.Fatal("routine health bypassed cooldown", err)
	}
	timer := time.NewTimer(open.RetryAfter + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for i := 0; i < 3; i++ {
		probe("HEALTHY")
		err = wired.breaker.BeforeHighRisk(key)
		if i < 2 && !errors.Is(err, resilience.ErrCircuitOpen) {
			t.Fatal("fewer than three current probes recovered dispatch", i, err)
		}
		if i == 2 && err != nil {
			t.Fatal("read-only same-key recovery did not close circuit", err)
		}
	}
	if f.money.reserves.Load() != 0 || f.money.commits.Load() != 0 || posts.Load() != 0 || reads.Load() != 4 {
		t.Fatal("read-only recovery acquired financial/model effects")
	}
	again := f.request(f.body)
	if again.Code != first.Code || again.Body.String() != first.Body.String() || posts.Load() != 0 {
		t.Fatal("recovery replayed original terminal failure")
	}
	f.key = "recovered-new-turn-" + uuid.NewString()
	fresh := f.request(f.body)
	if fresh.Code != http.StatusOK || posts.Load() != 1 || f.money.reserves.Load() != 1 || f.money.commits.Load() != 1 {
		t.Fatal("new authorized dispatch did not proceed after recovery", fresh.Code, posts.Load(), f.money.reserves.Load(), f.money.commits.Load())
	}
	if f.money.releases.Load() != 0 {
		t.Fatal("read-only recovery invented a release")
	}
	// A later real read-only failure must reopen with the original exponential
	// backoff, rather than letting a healthy catalogue observation force-close.
	for range 5 {
		wired.breaker.Record(key, resilience.InfrastructureFailure)
	}
	if err = wired.breaker.BeforeHighRisk(key); !errors.As(err, &open) {
		t.Fatal("second episode was not open", err)
	}
	timer.Reset(open.RetryAfter + 20*time.Millisecond)
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	failReads.Store(true)
	probe("DEGRADED")
	if err = wired.breaker.BeforeHighRisk(key); !errors.As(err, &open) || open.RetryAfter < 55*time.Second {
		t.Fatal("real failed recovery did not retain/reopen cooldown", err)
	}
	if posts.Load() != 1 || f.money.reserves.Load() != 1 || f.money.commits.Load() != 1 || f.money.releases.Load() != 0 {
		t.Fatal("failed recovery performed paid effects")
	}
}

type circuitAcquireHookRepository struct {
	*credentialWindowRepository
	onAcquire func()
}

func (r *circuitAcquireHookRepository) AcquireDispatch(ctx context.Context, id uuid.UUID, worker string, lease time.Duration) (DispatchEnvelope, bool, error) {
	envelope, acquired, err := r.PostgresRepository.AcquireDispatch(ctx, id, worker, lease)
	if err == nil && acquired {
		r.onAcquire()
	}
	return envelope, acquired, err
}

func TestDispatchCircuitOpenAfterAdmissionNeverBecomesProbePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, r, ctx)
	credential, secret, err := r.CredentialSecret(ctx, f.account, f.credential)
	clear(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL)
	f.service.repository = &circuitAcquireHookRepository{credentialWindowRepository: f.r, onAcquire: func() {
		for range 5 {
			f.service.breaker.Record(key, resilience.InfrastructureFailure)
		}
	}}
	first := f.request(f.body)
	if first.Code != http.StatusOK {
		t.Fatal("already acquired one-shot work lost its original authorization", first.Code)
	}
	var id uuid.UUID
	if err = r.pool.QueryRow(ctx, `SELECT id FROM usage_requests WHERE user_id=$1`, f.user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	before, err := r.GetUsage(ctx, id)
	if err != nil || before.HoldID == nil || before.ExecutionStatus != "SUCCEEDED" || before.Status != "COMMITTED" {
		t.Fatal("original accepted outcome was not retained", before.ExecutionStatus, err)
	}
	if err = f.service.breaker.BeforeHighRisk(key); !errors.Is(err, resilience.ErrCircuitOpen) {
		t.Fatal("paid success became a recovery probe", err)
	}
	again := f.request(f.body)
	after, err := r.GetUsage(ctx, id)
	if err != nil || again.Code != http.StatusOK || again.Body.String() != first.Body.String() || after.HoldID == nil || *after.HoldID != *before.HoldID || after.Status != before.Status || after.ExecutionStatus != before.ExecutionStatus || after.ExecutionVersion != before.ExecutionVersion {
		t.Fatal("replay circuit gate lost original accepted identity/state", again.Code, err)
	}
	if f.providerCalls.Load() != 1 || f.money.reserves.Load() != 1 || f.money.commits.Load() != 1 || f.money.releases.Load() != 0 {
		t.Fatal("replay repeated original model/funds effects", f.providerCalls.Load(), f.money.reserves.Load(), f.money.commits.Load(), f.money.releases.Load())
	}
}

func TestDispatchCircuitRecoveryDoesNotRequireSharedHealthClaimPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	r, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, r, ctx)
	f.r.accepted = nil // This fixture observes more than two independent acceptances.
	var reads, posts atomic.Int32
	var prematureRead atomic.Bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if q.Header.Get("Authorization") != "Bearer controlled-window-old-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if q.Method == http.MethodGet && q.URL.Path == "/v1/models" {
			var unfinished int
			if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_requests WHERE user_id=$1 AND execution_status<>'FAILED'`, f.user).Scan(&unfinished); err != nil || unfinished != 0 {
				prematureRead.Store(true)
			}
			reads.Add(1)
			_, _ = fmt.Fprintf(w, `{"data":[{"id":%q}]}`, f.r.model.UpstreamModelRef)
			return
		}
		if q.Method == http.MethodPost && q.URL.Path == "/v1/chat/completions" {
			posts.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"controlled result"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	f.providerHandler.Store(&handler)
	credential, secret, err := r.CredentialSecret(ctx, f.account, f.credential)
	clear(secret)
	if err != nil {
		t.Fatal(err)
	}
	key := resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL)
	for range 5 {
		f.service.breaker.Record(key, resilience.InfrastructureFailure)
	}
	oldKey := f.key
	first := f.request(f.body)
	if first.Code != http.StatusServiceUnavailable {
		t.Fatal("local open circuit did not reject", first.Code)
	}
	// A second independently constructed instance shares the real route DB but
	// owns a separate breaker. This models process-local isolation in one test
	// process; it does not start a deployed second gateway process.
	other := &Service{}
	if _, err = other.WithPersonalModelExecution(r, f.money, f.service.executionUpstream, 1000, 10, 100); err != nil {
		t.Fatal(err)
	}
	worker, err := NewRouteHealthWorker(r, f.service.executionUpstream.(RouteHealthProbeAdapter), f.service.providerURLs, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.WithRouteHealthCircuitRecovery(worker); err != nil {
		t.Fatal(err)
	}
	target, ok := worker.claim(ctx)
	if !ok {
		t.Fatal("other instance did not claim real shared health lease")
	}
	worker.execute(ctx, target)
	if _, ok = worker.claim(ctx); ok {
		t.Fatal("completed health route was unexpectedly due again")
	}
	var open *resilience.CircuitOpenError
	if err = f.service.breaker.BeforeHighRisk(key); !errors.As(err, &open) {
		t.Fatal("other process health reset local circuit", err)
	}
	timer := time.NewTimer(open.RetryAfter + 20*time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for i := 0; i < 3; i++ {
		f.key = "local-circuit-read-" + uuid.NewString()
		response := f.request(f.body)
		if response.Code != http.StatusServiceUnavailable {
			t.Fatal("recovery observation changed rejected command to paid dispatch", i, response.Code)
		}
		if reads.Load() != int32(i+2) || posts.Load() != 0 || f.money.reserves.Load() != 0 || f.money.commits.Load() != 0 || f.money.releases.Load() != 0 {
			t.Fatal("local recovery missed readonly path or produced money/model effects", reads.Load(), posts.Load())
		}
		if i < 2 && !errors.Is(f.service.breaker.BeforeHighRisk(key), resilience.ErrCircuitOpen) {
			t.Fatal("local circuit closed before three valid observations")
		}
	}
	if err = f.service.breaker.BeforeHighRisk(key); err != nil {
		t.Fatal("local recovery depended on winning global health claim", err)
	}
	if err = other.breaker.BeforeHighRisk(key); err != nil {
		t.Fatal("first instance corrupted second circuit", err)
	}
	f.key = oldKey
	again := f.request(f.body)
	if again.Code != first.Code || again.Body.String() != first.Body.String() {
		t.Fatal("local recovery rewrote prior terminal receipt")
	}
	f.key = "local-recovered-new-turn-" + uuid.NewString()
	response := f.request(f.body)
	if response.Code != http.StatusOK || posts.Load() != 1 || f.money.reserves.Load() != 1 || f.money.commits.Load() != 1 || f.money.releases.Load() != 0 {
		t.Fatal("new admitted command did not proceed after local recovery", response.Code, posts.Load())
	}
	if prematureRead.Load() {
		t.Fatal("health read started before original failure receipt was durable")
	}
}
