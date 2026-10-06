package billing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
)

func TestPostgresOrganizationGovernanceRegistrationSharesDeletionFence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := &PostgresRepository{pool: testdb.Open(t, "billing"), now: func() time.Time { return time.Now().UTC() }}
	org := uuid.New()
	if _, err := r.ApplyOrganizationLifecycle(ctx, org, "DISABLED", 1, false); err != nil {
		t.Fatal(err)
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "organization-lifecycle:"+org.String()); err != nil {
		t.Fatal(err)
	}
	// Hold the production deletion fence after its blocker assessment, then let
	// registration race the terminal transition. No timing-only winner is assumed.
	if _, err = tx.Exec(ctx, `UPDATE organization_lifecycle_states SET status='DELETED',account_version=2 WHERE organization_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- r.RegisterOrganizationGovernanceBlocker(ctx, org, "OWNED", "fixture", "one", true, nil, r.now())
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		select {
		case got := <-finished:
			t.Fatalf("registration bypassed held deletion fence: %v", got)
		default:
		}
		var waiting bool
		if err = r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event='advisory' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registration did not reach shared fence")
		}
		time.Sleep(time.Millisecond)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-finished; !errors.Is(err, ErrConflict) {
		t.Fatal("deleted organization accepted active blocker", err)
	}
	var count int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM organization_governance_blockers WHERE organization_id=$1 AND active`, org).Scan(&count); err != nil || count != 0 {
		t.Fatal("terminal organization retained new blocker", err)
	}
	if err = r.RegisterOrganizationGovernanceBlocker(ctx, org, "OWNED", "fixture", "one", false, nil, r.now()); err != nil {
		t.Fatal("terminal cleanup must remain allowed", err)
	}
	other := uuid.New()
	if _, err = r.ApplyOrganizationLifecycle(ctx, other, "DISABLED", 1, false); err != nil {
		t.Fatal(err)
	}
	if err = r.RegisterOrganizationGovernanceBlocker(ctx, other, "OWNED", "fixture", "two", true, nil, r.now()); err != nil {
		t.Fatal("disabled organization must allow blocker registration", err)
	}
	assessment, err := r.ApplyOrganizationLifecycle(ctx, other, "DELETED", 2, false)
	if err != nil || assessment.Ready || len(assessment.Blockers) != 1 {
		t.Fatal("registered blocker did not prevent deletion", err)
	}
	if err = r.RegisterOrganizationGovernanceBlocker(ctx, other, "OWNED", "fixture", "two", false, nil, r.now()); err != nil {
		t.Fatal(err)
	}
	assessment, err = r.ApplyOrganizationLifecycle(ctx, other, "DELETED", 2, false)
	if err != nil || !assessment.Ready {
		t.Fatal("cleared blocker did not permit deletion", err)
	}
}
