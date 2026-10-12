package billing

import (
	"context"
	"errors"
	"github.com/google/uuid"
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
		if err = r.pool.QueryRow(ctx, `SELECT state FROM usage_hold_expiry_recovery WHERE hold_id=$1`, fresh.ID).Scan(&pending); err != nil || pending != "MANUAL_REVIEW" {
			t.Fatal("first RPC lost durable intent", pending, err)
		}
		if err = worker.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		if err = r.pool.QueryRow(ctx, `SELECT h.status,x.state,w.reserved_microcredits FROM usage_holds h JOIN usage_hold_expiry_recovery x ON x.hold_id=h.id JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, fresh.ID).Scan(&status, &state, &reserved); err != nil || status != "RELEASED" || state != "COMPLETED" || reserved != 0 {
			t.Fatal("first RPC crash did not resume", status, state, reserved, err)
		}
	})
}
