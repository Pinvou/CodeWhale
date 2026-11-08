package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordResetPhasePostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	principal, user := f01ActiveFixtureUser(t, r)
	op, err := r.CreateAuthenticatedPasswordReset(ctx, principal, "controlled-handle", "controlled-policy")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.MarkPasswordWriteStarted(ctx, op.ID, uuid.New()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := r.MarkPasswordSessionsRevoked(ctx, op.ID, false); err != nil {
		t.Fatal(err)
	}
	updated, err := r.MarkPasswordUpdated(ctx, op.ID)
	if err != nil || updated.PasswordUpdatedAt == nil || updated.SessionsRevokedAt != nil {
		t.Fatal("first phase did not invalidate precaution", updated, err)
	}
	session := uuid.NewString()
	if _, err := r.pool.Exec(ctx, `INSERT INTO user_sessions(issuer,session_id,user_id,active_context_type,membership_version,last_seen_at,created_at) VALUES($1,$2,$3,'PERSONAL',1,clock_timestamp(),clock_timestamp())`, principal.Issuer, session, user); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkPasswordSessionsRevoked(ctx, op.ID, false); !errors.Is(err, errPasswordResetPhaseAdvanced) {
		t.Fatal("late pre accepted", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET attempts=9,sla_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.SchedulePasswordResetRetry(ctx, op.ID, "old-pre-error", false); err != nil {
		t.Fatal(err)
	}
	var sessions, attempts int
	var state string
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM user_sessions WHERE user_id=$1),attempts,status FROM password_reset_operations WHERE id=$2`, user, op.ID).Scan(&sessions, &attempts, &state); err != nil || sessions != 1 || attempts != 9 || state != "PASSWORD_UPDATED" {
		t.Fatal("late phase mutated sessions/retry", sessions, attempts, state, err)
	}
	if err := r.MarkPasswordSessionsRevoked(ctx, op.ID, true); err != nil {
		t.Fatal(err)
	}
	post, err := r.MarkPasswordUpdated(ctx, op.ID)
	if err != nil || post.SessionsRevokedAt == nil {
		t.Fatal("idempotent update cleared post", err)
	}
	if err := r.CompletePasswordReset(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	complete, err := r.MarkPasswordUpdated(ctx, op.ID)
	if err != nil || complete.Status != "COMPLETED" || complete.SessionsRevokedAt == nil {
		t.Fatal("completed replay regressed", complete, err)
	}
	// Controlled pre-cutover legacy shape, not a current writer or automatic migration.
	legacy, err := r.CreateAuthenticatedPasswordReset(ctx, principal, "controlled-old", "controlled-old")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.MarkPasswordWriteStarted(ctx, legacy.ID, uuid.New()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET password_updated_at=clock_timestamp(),sessions_revoked_at=clock_timestamp(),password_handle='',password_policy_version='',status='PASSWORD_UPDATED',next_attempt_at=clock_timestamp() WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	}
	// Exact runbook quarantine predicate: completed history is excluded.
	applyPasswordPhaseCutover(t, r)
	isolated, err := scanPasswordReset(r.pool.QueryRow(ctx, passwordResetSelect+` WHERE id=$1`, legacy.ID))
	if err != nil || isolated.Status != "REPAIR_REQUIRED" {
		t.Fatal("legacy pending not isolated", isolated, err)
	}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: r, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(ctx, isolated); !errors.Is(err, ErrConflict) || len(identities.calls) != 0 {
		t.Fatal("ambiguous pending dispatched", err)
	}
	if _, err := r.RetryProfileRepair(ctx, principal, legacy.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("ordinary retry revived quarantine", err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET status='RETRY' WHERE id=$1`, legacy.ID); err != nil {
		t.Fatal(err)
	} // A stale pre-cutover retry must not revive quarantine.
	if err := r.CompletePasswordReset(ctx, legacy.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("ambiguous marker completed", err)
	}
	if claimed, err := r.ClaimPasswordReset(ctx); err != nil || claimed != nil {
		t.Fatal("quarantine automatically claimed", claimed, err)
	}
	history, err := r.MarkPasswordUpdated(ctx, op.ID)
	if err != nil || history.Status != "COMPLETED" || history.SessionsRevokedAt == nil {
		t.Fatal("quarantine touched completed history", history, err)
	}

}

type passwordPhaseBarrier struct {
	hit, release chan struct{}
	armed        atomic.Bool
}

func (b *passwordPhaseBarrier) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if q.SQL == "commit" && b.armed.Swap(false) {
		close(b.hit)
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}
	return ctx
}
func (*passwordPhaseBarrier) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func TestPasswordResetPhaseOrderPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	for _, preFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "pre-then-password", false: "password-then-late-pre"}[preFirst], func(t *testing.T) {
			principal, _ := f01ActiveFixtureUser(t, r)
			op, err := r.CreateAuthenticatedPasswordReset(ctx, principal, "controlled", "controlled")
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := r.MarkPasswordWriteStarted(ctx, op.ID, uuid.New()); err != nil || !ok {
				t.Fatal(ok, err)
			}
			b := &passwordPhaseBarrier{hit: make(chan struct{}), release: make(chan struct{})}
			b.armed.Store(true)
			cfg := r.pool.Config()
			cfg.ConnConfig.Tracer = b
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			paused := *r
			paused.pool = pool
			pre := func(repo *PostgresRepository) error { return repo.MarkPasswordSessionsRevoked(ctx, op.ID, false) }
			write := func(repo *PostgresRepository) error { _, err := repo.MarkPasswordUpdated(ctx, op.ID); return err }
			first, second := make(chan error, 1), make(chan error, 1)
			go func() {
				if preFirst {
					first <- pre(&paused)
				} else {
					first <- write(&paused)
				}
			}()
			select {
			case <-b.hit:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			released := false
			defer func() {
				if !released {
					close(b.release)
				}
			}()
			go func() {
				if preFirst {
					second <- write(r)
				} else {
					second <- pre(r)
				}
			}()
			blocked := false
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND cardinality(pg_blocking_pids(pid))>0)`).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			close(b.release)
			released = true
			if !blocked {
				t.Fatal("real phase transactions did not serialize")
			}
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			err = <-second
			if preFirst && err != nil || !preFirst && !errors.Is(err, errPasswordResetPhaseAdvanced) {
				t.Fatal("wrong phase winner", err)
			}
			current, err := scanPasswordReset(r.pool.QueryRow(ctx, passwordResetSelect+` WHERE id=$1`, op.ID))
			if err != nil || current.PasswordUpdatedAt == nil || current.SessionsRevokedAt != nil {
				t.Fatal("pre became post evidence", current, err)
			}
		})
	}
}

// Execute the actual documented cutover, so a missing historical status in the
// operator predicate cannot pass by duplicating a corrected predicate in tests.
func applyPasswordPhaseCutover(t *testing.T, r *PostgresRepository) {
	t.Helper()
	body, err := os.ReadFile("../../docs/identity-recovery-profile-revocation.md")
	if err != nil {
		t.Fatal(err)
	}
	_, cutover, ok := strings.Cut(strings.ReplaceAll(string(body), "\r\n", "\n"), "## Password reset phase cutover (controlled stop required)")
	if !ok {
		t.Fatal("missing cutover section")
	}
	_, sql, ok := strings.Cut(cutover, "```sql\n")
	if !ok {
		t.Fatal("missing cutover SQL")
	}
	sql, _, ok = strings.Cut(sql, "```")
	if !ok {
		t.Fatal("unterminated cutover SQL")
	}
	ctx := context.Background()
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	// Read the actual multi-statement results, including the documented final
	// SELECT. Exec alone discards a nonzero count without returning an error.
	results, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	for _, result := range results {
		if result.Err != nil {
			t.Fatal(result.Err)
		}
		if len(result.FieldDescriptions) != 1 || result.FieldDescriptions[0].Name != "unsafe_pending" {
			continue
		}
		checks++
		if len(result.Rows) != 1 || len(result.Rows[0]) != 1 {
			t.Fatal("invalid unsafe_pending result")
		}
		count, err := strconv.ParseInt(string(result.Rows[0][0]), 10, 64)
		if err != nil || count != 0 {
			t.Fatal("cutover unsafe_pending must be zero", count, err)
		}
	}
	if checks != 1 {
		t.Fatal("expected one documented unsafe_pending check", checks)
	}
}

func TestPasswordResetLegacyRepairCutoverPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := context.Background()
	principal, _ := f01ActiveFixtureUser(t, r)
	op, err := r.CreateAuthenticatedPasswordReset(ctx, principal, "controlled-old", "controlled-old")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := r.MarkPasswordWriteStarted(ctx, op.ID, uuid.New()); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := r.MarkPasswordSessionsRevoked(ctx, op.ID, false); err != nil {
		t.Fatal(err)
	}
	// Historical diagnostic fixture: old MarkUpdated retained the pre marker;
	// an old late pre retry at attempt 9 could then set this ordinary repair code.
	// This is not a claim that the corrected current producer writes this shape.
	if _, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET password_updated_at=clock_timestamp(),password_handle='',password_policy_version='',status='REPAIR_REQUIRED',attempts=10,last_error_code='IDENTITY_RESET_UNCERTAIN' WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	applyPasswordPhaseCutover(t, r)
	if _, err := r.RetryProfileRepair(ctx, principal, op.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("old repair revived by ordinary retry", err)
	}
	isolated, err := scanPasswordReset(r.pool.QueryRow(ctx, passwordResetSelect+` WHERE id=$1`, op.ID))
	if err != nil || isolated.Status != "REPAIR_REQUIRED" || isolated.LastErrorCode != "PASSWORD_SESSION_ORDER_UNPROVEN" {
		t.Fatal("old repair not quarantined", isolated, err)
	}
	identities := &profileIdentityFake{}
	service := &ProfileChangeService{store: r, identities: identities, now: time.Now}
	if err := service.resumePasswordReset(ctx, isolated); !errors.Is(err, ErrConflict) || len(identities.calls) != 0 {
		t.Fatal("old repair dispatched", err, identities.calls)
	}
	if claimed, err := r.ClaimPasswordReset(ctx); err != nil || claimed != nil {
		t.Fatal("old repair automatically claimed", claimed, err)
	}
	if err := r.CompletePasswordReset(ctx, op.ID); !errors.Is(err, ErrConflict) {
		t.Fatal("old repair completed", err)
	}
	// PW-null repairs have no ambiguous post marker. Their existing explicit
	// human retry remains eligible after this one-time historical cutover.
	principal, _ = f01ActiveFixtureUser(t, r)
	pending, err := r.CreateAuthenticatedPasswordReset(ctx, principal, "controlled-pre", "controlled-pre")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET status='REPAIR_REQUIRED',last_error_code='PASSWORD_WRITE_UNCERTAIN' WHERE id=$1`, pending.ID); err != nil {
		t.Fatal(err)
	}
	applyPasswordPhaseCutover(t, r)
	if _, err := r.RetryProfileRepair(ctx, principal, pending.ID); err != nil {
		t.Fatal("PW-null manual recovery broadened", err)
	}
}
