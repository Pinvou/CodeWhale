package token

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testCircuitPeerEffectsPostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	base, ctx := retainedManagementTokenRepository(t)
	f := newCredentialWindowFixture(t, base, ctx)
	f.providerEntered, f.providerContinue = make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(f.providerContinue) })
	t.Cleanup(release)
	u, done, proceed := f.pauseOriginal(t)
	duplicate := make(chan *httptest.ResponseRecorder, 1)
	go func() { duplicate <- f.request(f.body) }()
	select {
	case <-f.providerEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("peer never entered actual provider HTTP")
	}
	before, err := base.GetUsage(ctx, u.ID)
	if err != nil || before.HoldID == nil || before.ExecutionStatus != "UPSTREAM_DISPATCHING" || before.SettlementStatus != "HOLD_ACTIVE" {
		t.Fatal("missing actual peer facts", err)
	}
	credential, secret, err := base.CredentialSecret(ctx, f.account, f.credential)
	clear(secret)
	if err != nil {
		t.Fatal("old credential must remain ACTIVE and readable", err)
	}
	key := resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL)
	// Controlled real Breaker.Record API outcomes, not five actual other requests.
	for i := 0; i < 5; i++ {
		f.service.breaker.Record(key, resilience.InfrastructureFailure)
	}
	if err := f.service.breaker.BeforeHighRisk(key); err == nil {
		t.Fatal("breaker was not actually OPEN")
	}
	close(proceed)
	first := <-done
	after, err := base.GetUsage(ctx, u.ID)
	release()
	select {
	case <-duplicate:
	case <-time.After(10 * time.Second):
		t.Fatal("peer cleanup timed out")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("current candidate circuit probe: originalHTTP=%d; before=%s/%s/v%d after=%s/%s/v%d actualProviderCalls=%d controlledReserveCalls=%d", first.Code, before.ExecutionStatus, before.SettlementStatus, before.ExecutionVersion, after.ExecutionStatus, after.SettlementStatus, after.ExecutionVersion, f.providerCalls.Load(), f.money.reserves.Load())
	if after.Status != before.Status || after.ExecutionStatus != before.ExecutionStatus || after.ExecutionVersion != before.ExecutionVersion || after.SettlementStatus != before.SettlementStatus {
		t.Error("CIRCUIT_ACTOR_OVERWROTE_REAL_PEER_FACTS")
	}
}

func TestCircuitFailureBeforeDispatchPostgres(t *testing.T) {
	t.Run("peer-effects-are-not-local-failure", testCircuitPeerEffectsPostgres)
	t.Run("untouched-acceptance-has-replayable-circuit-receipt", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
		t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
		base, ctx := retainedManagementTokenRepository(t)
		f := newCredentialWindowFixture(t, base, ctx)
		u, done, proceed := f.pauseOriginal(t)
		credential, secret, err := base.CredentialSecret(ctx, f.account, f.credential)
		clear(secret)
		if err != nil {
			t.Fatal(err)
		}
		key := resilience.Key(credential.Provider, "MODEL_DISPATCH", credential.BaseURL)
		for i := 0; i < 5; i++ {
			f.service.breaker.Record(key, resilience.InfrastructureFailure)
		}
		close(proceed)
		first := <-done
		if first.Code != 503 || !strings.Contains(first.Body.String(), "PROVIDER_CIRCUIT_OPEN") || !strings.Contains(first.Body.String(), u.ID.String()) {
			t.Fatal("missing exact circuit receipt", first.Code)
		}
		actual, err := base.GetUsage(ctx, u.ID)
		if err != nil || actual.HoldID != nil || actual.ExecutionStatus != "FAILED" || actual.SettlementStatus != "RELEASED" || actual.ErrorClass == nil || *actual.ErrorClass != "provider_circuit_open" {
			t.Fatal("not untouched predispatch failure", err)
		}
		again := f.request(f.body)
		if again.Code != 503 || again.Body.String() != first.Body.String() {
			t.Fatal("circuit retry lost original exact receipt", again.Code)
		}
		if f.money.reserves.Load() != 0 || f.money.releases.Load() != 0 || f.providerCalls.Load() != 0 {
			t.Fatal("local failure performed external effects")
		}
		if first.Header().Get("Retry-After") == "" {
			t.Fatal("original circuit cooldown missing")
		}
	})
}
