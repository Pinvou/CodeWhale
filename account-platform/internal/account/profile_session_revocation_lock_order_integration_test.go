package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
	"testing"
	"time"
)

// Port of the independent review's two real 40P01 counterexamples. The actual
// production writer must wait on USER before owning profile, not on audit's FK.
func historyProfileAuditLockOrder(t *testing.T, f *retainedWalletFenceFixture) {
	for _, mode := range []string{"first-dispatch-audit", "repair-required-audit"} {
		t.Run("audit-lock-order-"+mode, func(t *testing.T) {
			r := f.repository(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			c := historyProfile(t, r)
			if err := r.MarkProfileIdentityUpdated(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			if err := r.CommitProfileChange(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
			owner, err := r.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Rollback(context.Background())
			var user uuid.UUID
			if err = owner.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, c.UserID).Scan(&user); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if mode == "first-dispatch-audit" {
					_, err := r.StartProfileSessionRevocation(ctx, c.ID)
					done <- err
				} else {
					done <- r.ScheduleProfileChangeRetry(ctx, c.ID, "SESSION_REVOCATION_UNCONFIRMED")
				}
			}()
			var waiting string
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err = f.admin.QueryRow(ctx, `SELECT COALESCE((SELECT query FROM pg_stat_activity WHERE datname=$1 AND wait_event_type='Lock' AND pid<>pg_backend_pid() LIMIT 1),'')`, r.pool.Config().ConnConfig.Database).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting != "" {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if waiting == "" {
				t.Fatal("audited production writer never reached the real PostgreSQL lock barrier")
			}
			t.Logf("blocked production query=%s", strings.TrimSpace(waiting))
			// Same order used by closure/CommitProfileChange. A reversed audited writer
			// already holds profile, so this forms the original real deadlock cycle.
			var profile uuid.UUID
			userErr := owner.QueryRow(ctx, `SELECT id FROM profile_change_requests WHERE id=$1 FOR UPDATE`, c.ID).Scan(&profile)
			_ = owner.Rollback(context.Background())
			writerErr := <-done
			for _, e := range []error{userErr, writerErr} {
				var p *pgconn.PgError
				if errors.As(e, &p) && p.Code == "40P01" {
					t.Fatalf("audited writer reversed USER -> profile: user=%v writer=%v", userErr, writerErr)
				}
			}
			if userErr != nil || writerErr != nil {
				t.Fatalf("USER owner=%v audited writer=%v", userErr, writerErr)
			}
			if !strings.Contains(waiting, "SELECT id FROM users WHERE") {
				t.Fatal("audited writer waited after owning profile instead of USER first")
			}
		})
	}
}
