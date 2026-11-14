package account

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func testAuditExportRetryPolicy(t *testing.T, ctx context.Context, service *AuditExportService, owner *pgx.Conn, base identity.Principal) {
	// Freeze only the service clock for daily quotas. Ownership and retry
	// deadlines continue to use PostgreSQL's actual clock.
	clock := time.Now().UTC()
	worker := *service
	worker.now = func() time.Time { return clock }
	actor := func(t *testing.T) identity.Principal {
		t.Helper()
		principal := base
		principal.Subject = uuid.NewString()
		if _, _, err := service.repository.Bootstrap(ctx, principal); err != nil {
			t.Fatal(err)
		}
		if err := NewWalletProvisioningWorker(service.repository, auditExportBoundaryWallet{}, time.Second).RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		return principal
	}
	create := func(t *testing.T, principal identity.Principal) (AuditExportJob, AuditExportRequest) {
		t.Helper()
		request := AuditExportRequest{Reason: "controlled retry policy", IdempotencyKey: uuid.NewString(), Filter: AuditExportFilter{From: clock.Add(-time.Minute), To: clock}}
		job, err := worker.Create(ctx, principal, request)
		if err != nil {
			t.Fatal(err)
		}
		return job, request
	}
	t.Run("transient_database_errors_stop_at_three", func(t *testing.T) {
		principal := actor(t)
		job, original := create(t, principal)
		var stop func()
		defer func() {
			if stop != nil {
				stop()
			}
		}()
		for attempt := 1; attempt <= auditExportMaximumAttempts; attempt++ {
			lock, err := owner.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Rollback(ctx)
			if _, err := lock.Exec(ctx, `LOCK TABLE audit_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
				t.Fatal(err)
			}
			if attempt == 1 {
				stop = startAuditExportPolicyWorker(t, ctx, &worker)
			}
			pid := waitAuditExportBlockedQueryWithin(t, ctx, &worker, "%ORDER BY log.occurred_at,log.id LIMIT%", 15*time.Second)
			var terminated bool
			if err := service.repository.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
				t.Fatal("controlled database transport failure failed", err)
			}
			if err := lock.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			status := "PENDING"
			if attempt == auditExportMaximumAttempts {
				status = "FAILED"
			}
			if actual := waitAuditExportPolicyState(t, ctx, &worker, job.ID, status); actual != attempt {
				t.Fatal("worker used a different attempt budget", actual, attempt)
			}
			if status == "PENDING" {
				var retained bool
				if err := service.repository.pool.QueryRow(ctx, `SELECT octet_length(wrapped_file_key)>0 AND octet_length(wrapped_file_key_nonce)>0 FROM audit_export_jobs WHERE id=$1`, job.ID).Scan(&retained); err != nil || !retained {
					t.Fatal("retry lost the original file key", err)
				}
			}
		}
		assertAuditExportPolicyFailure(t, ctx, &worker, job.ID, 3, "RETRY_EXHAUSTED")
		var retryAudits int
		if err := service.repository.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='admin.audit.export.retry.scheduled'`, job.ID.String()).Scan(&retryAudits); err != nil || retryAudits != 2 {
			t.Fatal("retry audit count differs", retryAudits, err)
		}
		healthy, _ := create(t, principal)
		waitAuditExportPolicyState(t, ctx, &worker, healthy.ID, "READY")
		stop()
		// Finish the worker before driving the remaining daily quota directly.
		// Every failed logical job still counts once, independent of attempts.
		for index := 0; index < 8; index++ {
			created, _ := create(t, principal)
			claimed, found, err := worker.claim(ctx)
			if err != nil || !found || claimed.ID != created.ID {
				t.Fatal("controlled quota claim failed", err)
			}
			if err := worker.fail(ctx, claimed, "GENERATION_FAILED"); err != nil {
				t.Fatal(err)
			}
		}
		extra := original
		extra.IdempotencyKey = uuid.NewString()
		if _, err := worker.Create(ctx, principal, extra); !errors.Is(err, ErrAuditExportQuota) {
			t.Fatal("failed jobs no longer consume daily quota", err)
		}
		replay, err := worker.Create(ctx, principal, original)
		if err != nil || replay.ID != job.ID || replay.Status != "FAILED" || !replay.Replayed {
			t.Fatal("exhausted original request did not replay", err)
		}
	})
	t.Run("expired_third_attempt_is_terminal", func(t *testing.T) {
		principal := actor(t)
		job, _ := create(t, principal)
		var first, last claimedAuditExport
		for attempt := 1; attempt <= auditExportMaximumAttempts; attempt++ {
			claimed, found, err := worker.claim(ctx)
			if err != nil || !found || claimed.ID != job.ID || claimed.Attempt != attempt {
				t.Fatal("crash recovery did not reuse the original budget", err)
			}
			last = claimed
			if attempt == 1 {
				first = claimed
			}
			if attempt == 3 {
				if err := worker.retryGeneration(ctx, first); !errors.Is(err, ErrConflict) {
					t.Fatal("stale attempt rescheduled a newer generation", err)
				}
			}
			if err := worker.exhaustAttempts(ctx); err != nil {
				t.Fatal(err)
			}
			if actual := waitAuditExportPolicyState(t, ctx, &worker, job.ID, "GENERATING"); actual != attempt {
				t.Fatal("live attempt was terminated", actual)
			}
			if _, err := service.repository.pool.Exec(ctx, `UPDATE audit_export_jobs SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, job.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, found, err := worker.claim(ctx); err != nil || found {
			t.Fatal("fourth automatic attempt was granted before exhaustion cleanup", err)
		}
		done := make(chan error, 2)
		for index := 0; index < 2; index++ {
			go func() { done <- worker.exhaustAttempts(ctx) }()
		}
		for index := 0; index < 2; index++ {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		assertAuditExportPolicyFailure(t, ctx, &worker, job.ID, 3, "RETRY_EXHAUSTED")
		if _, found, err := worker.claim(ctx); err != nil || found {
			t.Fatal("fourth automatic attempt was granted", err)
		}
		if err := worker.fail(ctx, last, "GENERATION_FAILED"); !errors.Is(err, ErrConflict) {
			t.Fatal("expired worker overwrote exhaustion", err)
		}
		if err := worker.generate(ctx, last); !errors.Is(err, ErrConflict) {
			t.Fatal("expired worker published after exhaustion", err)
		}
	})
	t.Run("permanent_generation_error_is_not_retried", func(t *testing.T) {
		principal := actor(t)
		job, _ := create(t, principal)
		invalidKey, err := NewAuditExportService(service.repository, []byte(strings.Repeat("x", 32)), "test")
		if err != nil {
			t.Fatal(err)
		}
		stop := startAuditExportPolicyWorker(t, ctx, invalidKey)
		defer stop()
		waitAuditExportPolicyState(t, ctx, &worker, job.ID, "FAILED")
		stop()
		assertAuditExportPolicyFailure(t, ctx, &worker, job.ID, 1, "GENERATION_FAILED")
	})
	t.Run("pending_without_worker_retains_quota", func(t *testing.T) {
		principal := actor(t)
		first, original := create(t, principal)
		second, _ := create(t, principal)
		future := worker
		future.now = func() time.Time { return clock.Add(48 * time.Hour) }
		if err := future.expire(ctx); err != nil {
			t.Fatal(err)
		}
		if err := worker.exhaustAttempts(ctx); err != nil {
			t.Fatal(err)
		}
		for _, job := range []AuditExportJob{first, second} {
			if attempts := waitAuditExportPolicyState(t, ctx, &worker, job.ID, "PENDING"); attempts != 0 {
				t.Fatal("unclaimed pending job changed", attempts)
			}
		}
		extra := original
		extra.IdempotencyKey = uuid.NewString()
		if _, err := worker.Create(ctx, principal, extra); !errors.Is(err, ErrAuditExportQuota) {
			t.Fatal("unclaimed pending jobs released their slots", err)
		}
		for index := 0; index < 2; index++ {
			claimed, found, err := worker.claim(ctx)
			if err != nil || !found {
				t.Fatal("pending recovery did not resume", err)
			}
			if err := worker.fail(ctx, claimed, "GENERATION_FAILED"); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func startAuditExportPolicyWorker(t *testing.T, ctx context.Context, service *AuditExportService) func() {
	t.Helper()
	workerContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); service.Run(workerContext) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("audit export supervisor did not stop")
		}
	}
}

func waitAuditExportPolicyState(t *testing.T, ctx context.Context, service *AuditExportService, id uuid.UUID, status string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var actual string
		var attempts int
		if err := service.repository.pool.QueryRow(ctx, `SELECT status,attempts FROM audit_export_jobs WHERE id=$1`, id).Scan(&actual, &attempts); err != nil {
			t.Fatal(err)
		}
		if actual == status {
			return attempts
		}
		if time.Now().After(deadline) {
			t.Fatal("audit export did not reach expected status", actual, status, attempts)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertAuditExportPolicyFailure(t *testing.T, ctx context.Context, service *AuditExportService, id uuid.UUID, attempts int, code string) {
	t.Helper()
	var actualCode string
	var actualAttempts, failures int
	var finalized bool
	if err := service.repository.pool.QueryRow(ctx, `SELECT error_code,attempts,status='FAILED' AND wrapped_file_key IS NULL AND wrapped_file_key_nonce IS NULL AND lease_until IS NULL AND file_payload IS NULL AND completed_at IS NOT NULL,(SELECT count(*) FROM audit_export_events WHERE export_id=job.id AND event_type='FAILED') FROM audit_export_jobs job WHERE id=$1`, id).Scan(&actualCode, &actualAttempts, &finalized, &failures); err != nil || actualCode != code || actualAttempts != attempts || !finalized || failures != 1 {
		t.Fatal("terminal attempt or retained failure evidence differs", actualCode, actualAttempts, finalized, failures, err)
	}
}
