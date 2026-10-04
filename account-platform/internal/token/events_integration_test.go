package token

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func settlementFixture(t *testing.T) (*PostgresRepository, UsageRequest) {
	t.Helper()
	ctx := context.Background()
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	resolved, err := s.contexts.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := repo.ListCandidates(ctx, "model-test")
	if err != nil || len(candidates) != 1 {
		t.Fatal("fixture candidate missing")
	}
	c := candidates[0]
	u, created, err := repo.BeginUsage(ctx, UsageRequest{ID: uuid.New(), IdempotencyKey: uuid.NewString(), UserID: resolved.UserID, SessionID: resolved.SessionID, TokenAccountID: c.TokenAccountID, CredentialID: c.CredentialID, WalletID: c.WalletID, Model: "model-test", MaximumMicrocredits: 100}, map[string]any{})
	if err != nil || !created {
		t.Fatalf("begin usage: %v", err)
	}
	if err := repo.MarkReserved(ctx, u.ID, uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkUpstream(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	return repo, u
}

func TestPostgresSettlementOutboxAtomicAndConcurrentIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	// Only this test's random schema is altered: force the outbox insert to fail
	// after the terminal UPDATE, proving that both roll back together.
	if _, err := repo.pool.Exec(ctx, `ALTER TABLE outbox_events ADD CONSTRAINT fixture_reject_settled CHECK(event_type <> 'token.usage.settled.v1')`); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkCommitted(ctx, usage.ID, 60, map[string]any{"input_tokens": 60}); err == nil {
		t.Fatal("outbox failure committed usage")
	}
	stored, err := repo.GetUsage(ctx, usage.ID)
	if err != nil || stored.Status != "UPSTREAM_ACTIVE" || stored.ActualMicrocredits != nil {
		t.Fatal("failed outbox did not roll back terminal")
	}
	if _, err := repo.pool.Exec(ctx, `ALTER TABLE outbox_events DROP CONSTRAINT fixture_reject_settled`); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkReview(ctx, usage.ID, "local_commit_record_pending", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			failures <- repo.MarkCommitted(ctx, usage.ID, 60, map[string]any{"input_tokens": 60})
		}()
	}
	close(start)
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	stored, err = repo.GetUsage(ctx, usage.ID)
	if err != nil || stored.Status != "COMMITTED" || stored.ErrorClass != nil || stored.ActualMicrocredits == nil || *stored.ActualMicrocredits != 60 {
		t.Fatal("pending settlement did not converge")
	}
	var count int
	var payload []byte
	if err := repo.pool.QueryRow(ctx, `SELECT count(*),min(payload::text)::jsonb FROM outbox_events WHERE aggregate_id=$1 AND event_type='token.usage.settled.v1'`, usage.ID).Scan(&count, &payload); err != nil {
		t.Fatal(err)
	}
	var facts struct {
		UsageID   uuid.UUID `json:"usage_request_id"`
		WalletID  uuid.UUID `json:"wallet_id"`
		AccountID uuid.UUID `json:"token_account_id"`
		Actual    int64     `json:"actual_microcredits"`
	}
	if err := json.Unmarshal(payload, &facts); err != nil || count != 1 || facts.UsageID != usage.ID || facts.WalletID != usage.WalletID || facts.AccountID != usage.TokenAccountID || facts.Actual != 60 {
		t.Fatal("settlement event absent, duplicated, or changed facts")
	}
	if err := repo.MarkCommitted(ctx, usage.ID, 61, map[string]any{}); !errors.Is(err, ErrForbidden) {
		t.Fatal("terminal settlement accepted changed cost")
	}
}
