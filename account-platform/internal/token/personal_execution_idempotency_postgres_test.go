package token

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Two genuine concurrent acceptance calls start before either row exists.
// Waiting behind the real admission lock must not turn the second same-call
// retry into a new quota admission after the first reaches the user limit.
func TestPersonalExecutionSameKeyReplaysAtInflightLimit(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	user, account, credential, wallet := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, user, wallet, account.String()); err != nil {
		t.Fatal(err)
	}
	cipher, err := r.secrets.Encrypt([]byte("controlled-idempotency-secret"), []byte(account.String()+":"+credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,now())`, credential, account, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), user); err != nil {
		t.Fatal(err)
	}
	request := UsageRequest{ID: uuid.New(), UserID: user, SessionID: "controlled-samekey", TokenAccountID: account, CredentialID: credential, WalletID: wallet, Model: "controlled-model", MaximumMicrocredits: 0, IdempotencyNamespace: "model-inference", CanonicalVersion: 1, RequestFingerprint: strings.Repeat("b", 64)}
	for i := 0; i < 4; i++ {
		item := request
		item.ID = uuid.New()
		item.IdempotencyKeyDigest = digestString([]byte(item.ID.String()))
		if _, created, err := r.BeginUsage(ctx, item, map[string]any{"context_type": "PERSONAL"}); err != nil || !created {
			t.Fatalf("legitimate admission setup: %v", err)
		}
	}
	request.IdempotencyKeyDigest = digestString([]byte("samekey-" + user.String()))
	lock, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	transaction, err := lock.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback(context.Background())
	if _, err = transaction.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('usage:global',0))`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		usage   UsageRequest
		created bool
		err     error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			item, created, err := r.BeginUsage(ctx, request, map[string]any{"context_type": "PERSONAL"})
			results <- result{item, created, err}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND query LIKE 'SELECT pg_advisory_xact_lock(%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("two real admission waiters not observed: %d", waiting)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err = transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	createdCount, replayCount := 0, 0
	for i := 0; i < 2; i++ {
		got := <-results
		if got.err != nil {
			t.Errorf("same call was treated as new quota admission: %v", got.err)
			continue
		}
		if got.usage.ID != request.ID {
			t.Errorf("same call returned another usage")
		}
		if got.created {
			createdCount++
		} else {
			replayCount++
		}
	}
	var accepted, intents int
	if err = r.pool.QueryRow(ctx, `SELECT count(*),(SELECT count(*) FROM usage_dispatch_intents WHERE usage_request_id=$2) FROM usage_requests WHERE user_id=$1 AND idempotency_key_digest=$3`, user, request.ID, request.IdempotencyKeyDigest).Scan(&accepted, &intents); err != nil {
		t.Fatal(err)
	}
	if createdCount != 1 || replayCount != 1 || accepted != 1 || intents != 0 {
		t.Fatalf("samekey acceptance: created=%d replay=%d rows=%d dispatches=%d", createdCount, replayCount, accepted, intents)
	}
	changed := request
	changed.RequestFingerprint = strings.Repeat("c", 64)
	if _, _, err := r.BeginUsage(ctx, changed, map[string]any{"context_type": "PERSONAL"}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("same key with different facts bypassed conflict: %v", err)
	}
	newCall := request
	newCall.ID = uuid.New()
	newCall.IdempotencyKeyDigest = digestString([]byte(newCall.ID.String()))
	var limited *RateLimitError
	if _, _, err := r.BeginUsage(ctx, newCall, map[string]any{"context_type": "PERSONAL"}); !errors.As(err, &limited) {
		t.Fatalf("distinct sixth invocation bypassed the original quota: %v", err)
	}
}
