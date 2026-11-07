package billing

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"reflect"
	"sync"
	"testing"
	"time"
)

type f04ExpiryClient struct {
	repository         *PostgresRepository
	unknown            bool
	loseACK            bool
	calls              int
	mu                 sync.Mutex
	cancelAfterRelease context.CancelFunc
}

type slowExpiryProofClient struct {
	*f04ExpiryClient
	slowUsage uuid.UUID
	identity  ExpiredHoldIdentity
	exhausted bool
}

func (c *slowExpiryProofClient) FenceExpiredHold(ctx context.Context, identity ExpiredHoldIdentity) (ExpiredHoldProof, error) {
	if identity.UsageRequestID != c.slowUsage {
		return c.f04ExpiryClient.FenceExpiredHold(ctx, identity)
	}
	c.identity = identity
	<-ctx.Done()
	c.exhausted = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return ExpiredHoldProof{}, ctx.Err()
}

func (c *f04ExpiryClient) FenceExpiredHold(ctx context.Context, i ExpiredHoldIdentity) (ExpiredHoldProof, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.unknown {
		return ExpiredHoldProof{}, errors.New("dispatch unproven")
	}
	if _, err := c.repository.Release(ctx, i.UsageRequestID, i.HoldID, "controlled pre-dispatch fence"); err != nil {
		return ExpiredHoldProof{}, err
	}
	if c.cancelAfterRelease != nil {
		c.cancelAfterRelease()
		c.cancelAfterRelease = nil
		return ExpiredHoldProof{}, context.Canceled
	}
	if c.loseACK {
		c.loseACK = false
		return ExpiredHoldProof{}, errors.New("lost release ACK")
	}
	return ExpiredHoldProof{Identity: i, ProofID: i.HoldID, Status: "LOCAL_CONFIRMED"}, nil
}
func TestF04ExpiredHoldRecoveryPreservesUnknownAndReplaysRelease(t *testing.T) {
	r, ctx := retainedFinanceRepository(t)
	_, order, _, _ := retainedFinanceOrder(t, r, ctx, 1000000, true)
	usage := uuid.New()
	hold, err := r.Reserve(ctx, usage, order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, hold.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()+interval '1 hour' WHERE id=$1`, hold.ID); err == nil {
		t.Fatal("expired lease can race recovery by renewing")
	}
	client := &f04ExpiryClient{repository: r, unknown: true}
	worker := NewExpiredHoldRecoveryWorker(r, client, time.Minute)
	if err = worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status, state string
	var reserved int64
	if err = r.pool.QueryRow(ctx, `SELECT h.status,w.reserved_microcredits,x.state FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id WHERE h.id=$1`, hold.ID).Scan(&status, &reserved, &state); err != nil || status != "ACTIVE" || reserved != 100 || state != "MANUAL_REVIEW" {
		t.Fatal("unknown released money", status, reserved, state, err)
	}
	client.unknown = false
	crashed, cancel := context.WithCancel(ctx)
	defer cancel()
	client.cancelAfterRelease = cancel
	if err = worker.recoverOne(crashed, usage); err == nil {
		t.Fatal("post-release cancellation did not interrupt receipt persistence")
	}
	if err = r.pool.QueryRow(ctx, `SELECT h.status,w.reserved_microcredits,x.state FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id WHERE h.id=$1`, hold.ID).Scan(&status, &reserved, &state); err != nil || status != "RELEASED" || reserved != 0 || state != "MANUAL_REVIEW" {
		t.Fatal("lost ACK receipt", status, reserved, state, err)
	}
	if _, err = r.pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, hold.ID); err != nil {
		t.Fatal(err)
	}
	if err = worker.RunOnce(ctx); err != nil {
		t.Fatal("released hold lost recovery candidate", err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := worker.recoverOne(ctx, usage); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if err = r.pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, hold.ID).Scan(&state); err != nil || state != "COMPLETED" {
		t.Fatal("recovery did not complete", state, err)
	}
	var releasedEvidence int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM usage_hold_expiry_evidence WHERE hold_id=$1 AND decision='CONFIRMED'`, hold.ID).Scan(&releasedEvidence); err != nil || releasedEvidence != 1 {
		t.Fatal("duplicate financial recovery receipt", releasedEvidence, err)
	}
	if _, err = r.pool.Exec(ctx, `TRUNCATE usage_hold_expiry_evidence`); err == nil {
		t.Fatal("runtime can truncate recovery evidence")
	}
	if _, err = r.pool.Exec(ctx, `DELETE FROM usage_hold_expiry_evidence`); err == nil {
		t.Fatal("runtime can delete recovery evidence")
	}
	t.Run("first-RPC-release-crash-keeps-durable-intent", func(t *testing.T) {
		freshUsage := uuid.New()
		fresh, err := r.Reserve(ctx, freshUsage, order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, fresh.ID); err != nil {
			t.Fatal(err)
		}
		interrupted, cancel := context.WithCancel(ctx)
		defer cancel()
		client.cancelAfterRelease = cancel
		if err = worker.recoverOne(interrupted, freshUsage); err == nil {
			t.Fatal("first RPC did not interrupt receipt")
		}
		var pending string
		var retryScheduled bool
		if err = r.pool.QueryRow(ctx, `SELECT state,next_attempt_at>updated_at FROM usage_hold_expiry_recovery WHERE hold_id=$1`, fresh.ID).Scan(&pending, &retryScheduled); err != nil || pending != "MANUAL_REVIEW" || !retryScheduled {
			t.Fatal("first RPC lost durable intent or retry schedule", pending, retryScheduled, err)
		}
		// Cancellation still persists the bounded retry. Advance only this
		// original hold to its next eligible attempt before replaying it.
		advanced, advanceErr := r.pool.Exec(ctx, `UPDATE usage_hold_expiry_recovery SET next_attempt_at=clock_timestamp() WHERE hold_id=$1`, fresh.ID)
		if advanceErr != nil || advanced.RowsAffected() != 1 {
			t.Fatal("first RPC retry advance", advanced.RowsAffected(), advanceErr)
		}
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err = r.pool.QueryRow(ctx, `SELECT h.status,x.state,w.reserved_microcredits FROM usage_holds h JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, fresh.ID).Scan(&status, &state, &reserved); err != nil || status != "RELEASED" || state != "COMPLETED" || reserved != 0 {
			t.Fatal("first RPC crash did not resume", status, state, reserved, err)
		}
	})
	t.Run("slow-proof-exhaustion-persists-backoff-and-next-healthy-progress", func(t *testing.T) {
		slowUsage, healthyUsage := uuid.New(), uuid.New()
		slow, err := r.Reserve(ctx, slowUsage, order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		healthy, err := r.Reserve(ctx, healthyUsage, order.WalletID, 100, nil, time.Now().UTC().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE usage_holds SET expires_at=clock_timestamp()-CASE WHEN id=$1 THEN interval '2 minutes' ELSE interval '1 minute' END WHERE id=ANY($2::uuid[])`, slow.ID, []uuid.UUID{slow.ID, healthy.ID}); err != nil {
			t.Fatal(err)
		}
		before, err := r.GetWallet(ctx, order.WalletID)
		if err != nil {
			t.Fatal(err)
		}
		proof := &slowExpiryProofClient{f04ExpiryClient: &f04ExpiryClient{repository: r}, slowUsage: slowUsage}
		bounded := NewExpiredHoldRecoveryWorker(r, proof, time.Minute)
		started := time.Now()
		if err := bounded.RunOnceLimit(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || !proof.exhausted {
			t.Fatal("controlled evidence did not exhaust the original item budget", err)
		}
		t.Logf("original five-second item budget exhausted in %s", time.Since(started))
		after, err := r.GetWallet(ctx, order.WalletID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("unproven slow evidence changed the wallet", err)
		}
		var pending string
		var noProof, backoff bool
		var raw []byte
		if err := r.pool.QueryRow(ctx, `SELECT state,proof IS NULL,next_attempt_at>clock_timestamp()+interval '50 seconds',identity FROM usage_hold_expiry_recovery WHERE hold_id=$1`, slow.ID).Scan(&pending, &noProof, &backoff, &raw); err != nil || pending != "MANUAL_REVIEW" || !noProof || !backoff {
			t.Fatal("exhausted RPC lost bounded persistent backoff", pending, noProof, backoff, err)
		}
		var persisted ExpiredHoldIdentity
		if json.Unmarshal(raw, &persisted) != nil || !persisted.Matches(proof.identity) || persisted.UsageRequestID != slowUsage || persisted.HoldID != slow.ID || persisted.WalletID != order.WalletID || persisted.MaxMicrocredits != 100 {
			t.Fatal("persistent retry changed the original hold identity")
		}
		if err := bounded.RunOnceLimit(ctx, 1); err != nil {
			t.Fatal("later healthy candidate did not advance", err)
		}
		if proof.calls != 1 {
			t.Fatal("slow candidate consumed the next finite batch")
		}
		if err := r.pool.QueryRow(ctx, `SELECT h.status,x.state FROM usage_holds h JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id WHERE h.id=$1`, healthy.ID).Scan(&status, &state); err != nil || status != "RELEASED" || state != "COMPLETED" {
			t.Fatal("healthy original did not complete with proof", status, state, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT status FROM usage_holds WHERE id=$1`, slow.ID).Scan(&status); err != nil || status != "ACTIVE" {
			t.Fatal("slow unproven hold was released", status, err)
		}
		final, err := r.GetWallet(ctx, order.WalletID)
		if err != nil || final.ReservedMicrocredits != before.ReservedMicrocredits-100 || final.AvailableMicrocredits != before.AvailableMicrocredits+100 {
			t.Fatal("healthy release did not preserve remaining reservation", final, err)
		}
	})
}
