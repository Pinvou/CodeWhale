package billing

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func preparedWaiverFixture(t *testing.T) (*PostgresRepository, Hold, string, string, string) {
	t.Helper()
	ctx := context.Background()
	repo := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	wallet, _, err := repo.EnsureWallet(ctx, "PERSONAL", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Credit(ctx, wallet.ID, "controlled-waiver:"+uuid.NewString(), 1000, false); err != nil {
		t.Fatal(err)
	}
	hold, err := repo.Reserve(ctx, uuid.New(), wallet.ID, 100, nil, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return repo, hold, "controlled final waiver", uuid.NewString(), uuid.NewString()
}

func TestPostgresPreparedWaiverUnknownReplyDoesNotReleaseOrChangeFacts(t *testing.T) {
	repo, hold, reason, actor, key := preparedWaiverFixture(t)
	ctx := context.Background()
	if _, err := repo.PrepareAdminUsageWaiver(ctx, hold.ID, reason, actor, key); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordAdminUsageWaiver(ctx, hold.ID, reason, actor, key); !errors.Is(err, ErrUsageAlreadyTerminal) {
		t.Fatalf("prepared command was treated as terminal proof: %v", err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE usage_holds SET status='REVIEW',expires_at=now()-interval '1 hour' WHERE id=$1`, hold.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordAdminUsageWaiver(ctx, hold.ID, reason, actor, key); !errors.Is(err, ErrUsageAlreadyTerminal) {
		t.Fatalf("expired REVIEW was treated as terminal proof: %v", err)
	}
	for _, changed := range []struct{ reason, actor string }{{reason + " changed", actor}, {reason, uuid.NewString()}} {
		if _, err := repo.PrepareAdminUsageWaiver(ctx, hold.ID, changed.reason, changed.actor, key); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("unknown reply key was rebound: %v", err)
		}
	}
	var posted, reserved int64
	var actions, commands int
	if err := repo.pool.QueryRow(ctx, `SELECT posted_microcredits,reserved_microcredits,(SELECT count(*) FROM usage_hold_admin_actions),(SELECT count(*) FROM usage_hold_admin_commands) FROM wallets WHERE id=$1`, hold.WalletID).Scan(&posted, &reserved, &actions, &commands); err != nil || posted != 1000 || reserved != 100 || actions != 0 || commands != 1 {
		t.Fatalf("unknown terminal outcome changed financial facts %d/%d/%d/%d: %v", posted, reserved, actions, commands, err)
	}
}

func TestPostgresWaiverConcurrentLostReplyHasOneImmutableAction(t *testing.T) {
	repo, hold, reason, actor, key := preparedWaiverFixture(t)
	ctx := context.Background()
	if _, err := repo.PrepareAdminUsageWaiver(ctx, hold.ID, reason, actor, key); err != nil {
		t.Fatal(err)
	}
	// The existing internal authority is invoked only to model a confirmed
	// upstream terminal decision. This test does not prove upstream inactivity.
	if authority, err := repo.ResolveUsageWaiver(ctx, hold.ID, hold.UsageRequestID, reason, actor); err != nil || authority.Hold.Status != "RELEASED" {
		t.Fatalf("terminal authority failed: %v", err)
	}
	var wg sync.WaitGroup
	errorsSeen := make(chan error, 8)
	start := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.PrepareAdminUsageWaiver(ctx, hold.ID, reason, actor, key)
			if err == nil {
				_, err = repo.RecordAdminUsageWaiver(ctx, hold.ID, reason, actor, key)
			}
			errorsSeen <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatal(err)
		}
	}
	var actions, commands int
	var before, after string
	var snapshot json.RawMessage
	if err := repo.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_hold_admin_actions),(SELECT count(*) FROM usage_hold_admin_commands),before_status,after_status,allocation_snapshot FROM usage_hold_admin_actions WHERE idempotency_key=$1`, key).Scan(&actions, &commands, &before, &after, &snapshot); err != nil || actions != 1 || commands != 1 || before != "ACTIVE" || after != "RELEASED" {
		t.Fatalf("logical waiver duplicated or changed original facts: %v", err)
	}
	var evidence struct {
		Components []struct {
			Reserved string `json:"reserved_microcredits"`
			Settled  string `json:"settled_microcredits"`
			Released string `json:"released_microcredits"`
		} `json:"component_allocations"`
	}
	if err := json.Unmarshal(snapshot, &evidence); err != nil || len(evidence.Components) != 1 || evidence.Components[0].Reserved != "100" || evidence.Components[0].Settled != "0" || evidence.Components[0].Released != "100" {
		t.Fatalf("immutable allocation evidence lost exact money facts: %v", err)
	}
	var posted, reserved, available int64
	if err := repo.pool.QueryRow(ctx, `SELECT posted_microcredits,reserved_microcredits,(SELECT sum(component.available_microcredits) FROM entitlement_components component JOIN entitlement_packages package ON package.id=component.package_id WHERE package.wallet_id=$1) FROM wallets WHERE id=$1`, hold.WalletID).Scan(&posted, &reserved, &available); err != nil || posted != 1000 || reserved != 0 || available != 1000 {
		t.Fatalf("waiver failed bucket conservation %d/%d/%d: %v", posted, reserved, available, err)
	}
	if _, err := repo.Commit(ctx, hold.UsageRequestID, hold.ID, 90, map[string]any{"late_actual": true}); !errors.Is(err, ErrUsageAlreadyTerminal) {
		t.Fatalf("later commit charged a final waiver: %v", err)
	}
}

func TestPostgresPreparedWaiverAndCommitShareOneFinancialWinner(t *testing.T) {
	repo, hold, reason, actor, key := preparedWaiverFixture(t)
	ctx := context.Background()
	if _, err := repo.PrepareAdminUsageWaiver(ctx, hold.ID, reason, actor, key); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	commitReply := make(chan error, 1)
	waiverReply := make(chan error, 1)
	go func() {
		<-start
		_, err := repo.Commit(ctx, hold.UsageRequestID, hold.ID, 90, map[string]any{"controlled_actual": 90})
		commitReply <- err
	}()
	go func() {
		<-start
		_, err := repo.ResolveUsageWaiver(ctx, hold.ID, hold.UsageRequestID, reason, actor)
		waiverReply <- err
	}()
	close(start)
	commitErr, waiverErr := <-commitReply, <-waiverReply
	if waiverErr != nil || commitErr != nil && !errors.Is(commitErr, ErrUsageAlreadyTerminal) {
		t.Fatalf("financial race error: commit=%v waiver=%v", commitErr, waiverErr)
	}
	var status string
	var actual, posted, reserved int64
	if err := repo.pool.QueryRow(ctx, `SELECT h.status,h.actual_microcredits,w.posted_microcredits,w.reserved_microcredits FROM usage_holds h JOIN wallets w ON w.id=h.wallet_id WHERE h.id=$1`, hold.ID).Scan(&status, &actual, &posted, &reserved); err != nil {
		t.Fatal(err)
	}
	_, recordErr := repo.RecordAdminUsageWaiver(ctx, hold.ID, reason, actor, key)
	switch status {
	case "RELEASED":
		if actual != 0 || posted != 1000 || reserved != 0 || recordErr != nil || !errors.Is(commitErr, ErrUsageAlreadyTerminal) {
			t.Fatalf("waiver winner was charged: %v", recordErr)
		}
	case "COMMITTED":
		if actual != 90 || posted != 910 || reserved != 0 || !errors.Is(recordErr, ErrUsageAlreadyTerminal) || commitErr != nil {
			t.Fatalf("commit winner was waived: %v", recordErr)
		}
	default:
		t.Fatalf("no terminal financial winner: %s", status)
	}
}
