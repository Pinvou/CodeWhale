package account

import (
	"context"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

func TestApprovalRelayOrdinaryReplayRemainsAuditedAndIdempotentIntegration(t *testing.T) {
	db, _ := newApprovalRelayIntegrationDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tx, err := db.runtimePool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if err := events.AppendOutbox(ctx, tx, "account.fixture.v1", uuid.New(), 1, map[string]any{"ordinary": true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	generic := newApprovalGenericPartition(t, ctx, db)
	claimed, err := generic.Claim(ctx, "ordinary-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatal("ordinary claim failed")
	}
	event := claimed[0]
	if err := generic.MarkDeadLettered(ctx, "ordinary-worker", event, "fixture failure"); err != nil {
		t.Fatal(err)
	}
	inspected, err := generic.InspectDeadLetter(ctx, event.ID)
	if err != nil || !inspected.Replayable || inspected.LastError != "fixture failure" || inspected.Attempts != 1 {
		t.Fatal("ordinary inspect contract regressed")
	}
	request := events.ReplayRequest{EventID: event.ID, ExpectedDigest: inspected.EventDigest, IdempotencyKey: "ordinary-stable-replay", Reason: "approved ordinary recovery after fixture failure"}
	if _, err := generic.ReplayDeadLetter(ctx, request); err == nil {
		t.Fatal("replay without workload identity succeeded")
	}
	job, err := serviceauth.ContextForJob(ctx, "event-replay", "EVENT_REPLAY", "ordinary-run")
	if err != nil {
		t.Fatal(err)
	}
	first, err := generic.ReplayDeadLetter(job, request)
	if err != nil || !first.Replayed || first.ReplayID == uuid.Nil || first.EventID != event.ID {
		t.Fatal("ordinary audited replay failed")
	}
	second, err := generic.ReplayDeadLetter(job, request)
	if err != nil || second.Replayed || second.ReplayID != first.ReplayID {
		t.Fatal("ordinary replay retry lost idempotency")
	}
	var attempts, auditCount int
	var dead, locked any
	if err := db.adminPool.QueryRow(ctx, `SELECT attempts,dead_lettered_at,locked_by FROM public.outbox_events WHERE event_id=$1`, event.ID).Scan(&attempts, &dead, &locked); err != nil || attempts != 0 || dead != nil || locked != nil {
		t.Fatal("ordinary replay did not reset delivery state")
	}
	if err := db.adminPool.QueryRow(ctx, `SELECT count(*) FROM public.event_replay_requests WHERE event_id=$1`, event.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatal("ordinary replay did not retain one audit record")
	}
	claimed, err = generic.Claim(ctx, "ordinary-retry-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != event.ID {
		t.Fatal("ordinary replay was not claimable with original event ID")
	}
	if err := generic.MarkPublished(ctx, "ordinary-retry-worker", claimed[0]); err != nil {
		t.Fatal(err)
	}
}
