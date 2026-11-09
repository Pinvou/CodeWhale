package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The caller owns the database and public migration/runtime identities. An
// exclusive audit-table lock holds the real CSV query while lease operations
// continue on the job table through a separate connection.
func testAuditExportGenerationLease(t *testing.T, ctx context.Context, service *AuditExportService, owner *pgx.Conn, create func(string) AuditExportJob) {
	for _, scenario := range []string{"renew_live_attempt", "cancel_expired_attempt_and_recover", "ordinary_error_stops_inflight_renewal"} {
		t.Run(scenario, func(t *testing.T) {
			job := create("controlled long generation")
			claim, found, err := service.claim(ctx)
			if err != nil || !found || claim.ID != job.ID {
				t.Fatal("generation claim failed", err)
			}
			lock, err := owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(ctx)
			if _, err := lock.Exec(ctx, `LOCK TABLE audit_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			generationContext, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- service.generateWithLease(generationContext, claim, 20*time.Millisecond) }()
			joined := false
			defer func() {
				cancel()
				_ = lock.Rollback(ctx)
				if !joined {
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("generator did not stop during cleanup")
					}
				}
			}()
			// Wait for the actual generator to reach the locked source table.
			generatorPID := waitAuditExportBlockedQuery(t, ctx, service, "%ORDER BY log.occurred_at,log.id LIMIT%")
			if scenario == "renew_live_attempt" {
				var shortDeadline time.Time
				if err := service.repository.pool.QueryRow(ctx, `UPDATE audit_export_jobs SET lease_until=clock_timestamp()+interval '1 second' WHERE id=$1 RETURNING lease_until`, job.ID).Scan(&shortDeadline); err != nil {
					t.Fatal(err)
				}
				time.Sleep(time.Until(shortDeadline) + 50*time.Millisecond)
				var until time.Time
				if err := service.repository.pool.QueryRow(ctx, `SELECT lease_until FROM audit_export_jobs WHERE id=$1`, job.ID).Scan(&until); err != nil || !until.After(shortDeadline.Add(time.Minute)) {
					t.Fatal("blocked generator did not renew live lease", err)
				}
				if _, found, err := service.claim(ctx); err != nil || found {
					t.Fatal("another worker reclaimed live generator", err)
				}
				if err := lock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err := <-done
				joined = true
				if err != nil {
					t.Fatal("renewed generator did not publish", err)
				}
			} else if scenario == "ordinary_error_stops_inflight_renewal" {
				jobLock, err := service.repository.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer jobLock.Rollback(ctx)
				if _, err := jobLock.Exec(ctx, `SELECT id FROM audit_export_jobs WHERE id=$1 FOR UPDATE`, job.ID); err != nil {
					t.Fatal(err)
				}
				waitAuditExportBlockedQuery(t, ctx, service, "SELECT attempts,actor_user_id,status,lease_until FROM audit_export_jobs WHERE id=$1 FOR UPDATE")
				// Cancel the real CSV query while a real renewal is in flight.
				// The resulting generation error then stops that renewal itself.
				var cancelled bool
				if err := service.repository.pool.QueryRow(ctx, `SELECT pg_cancel_backend($1)`, generatorPID).Scan(&cancelled); err != nil || !cancelled {
					t.Fatal("controlled source query cancellation failed", err)
				}
				select {
				case err = <-done:
					joined = true
				case <-time.After(5 * time.Second):
					t.Fatal("ordinary generation error did not stop in-flight renewal")
				}
				if err == nil || errors.Is(err, errAuditExportLeaseLost) {
					t.Fatal("ordinary generation error misclassified as lease loss", err)
				}
				var sourceError *pgconn.PgError
				if !errors.As(err, &sourceError) || sourceError.Code != "57014" {
					t.Fatal("expected controlled PostgreSQL source-query error", err)
				}
				if err := jobLock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := lock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := service.fail(ctx, claim, stableAuditExportError(err)); err != nil {
					t.Fatal("ordinary error could not finalize original live attempt", err)
				}
				var status string
				var failedEvents int
				if err := service.repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM audit_export_events WHERE export_id=job.id AND event_type='FAILED') FROM audit_export_jobs job WHERE id=$1`, job.ID).Scan(&status, &failedEvents); err != nil || status != "FAILED" || failedEvents != 1 {
					t.Fatal("ordinary error did not retain one terminal failure", status, failedEvents, err)
				}
				return
			} else {
				if _, err := service.repository.pool.Exec(ctx, `UPDATE audit_export_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					joined = true
					if !errors.Is(err, errAuditExportLeaseLost) {
						t.Fatal("expired generation did not report lease loss", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("failed heartbeat did not cancel blocked source query")
				}
				if err := lock.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				recovered, found, err := service.claim(ctx)
				if err != nil || !found || recovered.ID != job.ID || recovered.Attempt != claim.Attempt+1 {
					t.Fatal("expired generation did not recover", err)
				}
				if err := service.fail(ctx, claim, "GENERATION_FAILED"); !errors.Is(err, ErrConflict) {
					t.Fatal("stale generation failed recovered attempt", err)
				}
				if err := service.generate(ctx, recovered); err != nil {
					t.Fatal("recovered generator did not publish", err)
				}
			}
			var status string
			var readyEvents int
			if err := service.repository.pool.QueryRow(ctx, `SELECT status,(SELECT count(*) FROM audit_export_events WHERE export_id=job.id AND event_type='READY') FROM audit_export_jobs job WHERE id=$1`, job.ID).Scan(&status, &readyEvents); err != nil || status != "READY" || readyEvents != 1 {
				t.Fatal("expected exactly one published result", status, readyEvents, err)
			}
		})
	}
}

func waitAuditExportBlockedQuery(t *testing.T, ctx context.Context, service *AuditExportService, pattern string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var pid int
		if err := service.repository.pool.QueryRow(ctx, `SELECT COALESCE(min(pid),0) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE $1`, pattern).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatal("audit query did not reach real lock wait", pattern)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
