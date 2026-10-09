package billingschema

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"testing"
	"time"
)

func TestIndependentRoleGrantAuthorizationAfterTargetWait(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_SCHEMA_DATABASE_URL is required for actual dedicated PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	m, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, c, selectVersions(m, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, stageAccountSeed); err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	owner := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-subject", SessionID: "owned-session"}
	org, err := r.AdminCreateOrganization(ctx, owner, uuid.MustParse("11111111-1111-4111-8111-111111111111"), "expiry owned org", "role-expiry-organization")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := func(label string) (identity.Principal, uuid.UUID) {
		t.Helper()
		p := identity.Principal{Issuer: "https://owned.invalid", Subject: "expiry-" + label, SessionID: "expiry-session-" + label, Username: "expiry_" + label}
		me, _, e := r.Bootstrap(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		jobs, e := r.LeaseWalletProvisioningJobs(ctx, 100, time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, j := range jobs {
			if j.UserID == me.UserID {
				if e = r.CompleteWalletProvisioning(ctx, j); e != nil {
					t.Fatal(e)
				}
				found = true
			}
		}
		if !found {
			t.Fatal("actual wallet job absent")
		}
		if _, e = r.AdminAddMember(ctx, owner, org.ID, me.UserID, []string{"MEMBER"}, nil, "role-expiry-add-"+label); e != nil {
			t.Fatal(e)
		}
		return p, me.UserID
	}
	actor, actorID := bootstrap("actor")
	_, expiredTarget := bootstrap("expired_target")
	_, liveTarget := bootstrap("live_target")
	held, err := pgx.Connect(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close(context.Background())
	var super bool
	if err = held.QueryRow(ctx, "SELECT rolsuper FROM pg_roles WHERE rolname=current_user").Scan(&super); err != nil || super {
		t.Fatal("non-super runtime required")
	}
	snapshot := func() string {
		t.Helper()
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('users',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM users x),'memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),'assignments',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_assignments x),'versions',(SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM authorization_versions x),'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x),'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x))::text`)
	}
	for _, expired := range []bool{true, false} {
		label := "live"
		targetID := liveTarget
		if expired {
			label = "expired"
			targetID = expiredTarget
		}
		t.Run(label, func(t *testing.T) {
			ttl := 30 * time.Second
			if expired {
				ttl = 3 * time.Second
			}
			var databaseNow time.Time
			if err = c.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&databaseNow); err != nil {
				t.Fatal(err)
			}
			expiry := databaseNow.Add(ttl).UTC().Truncate(time.Microsecond)
			if _, err = r.GrantOrganizationRoleBinding(ctx, owner, account.RoleBindingGrant{UserID: actorID, RoleCode: "ORG_ADMIN", OrganizationID: &org.ID, ExpiresAt: &expiry, IdempotencyKey: "role-expiry-authority-" + label}); err != nil {
				t.Fatal(err)
			}
			tx, e := held.Begin(ctx)
			if e != nil {
				t.Fatal(e)
			}
			defer tx.Rollback(context.Background())
			var membership uuid.UUID
			if e = tx.QueryRow(ctx, "SELECT id FROM memberships WHERE organization_id=$1 AND user_id=$2 AND status='ACTIVE' FOR UPDATE", org.ID, targetID).Scan(&membership); e != nil {
				t.Fatal(e)
			}
			before := snapshot()
			request := account.RoleBindingGrant{UserID: targetID, RoleCode: "AUDITOR", OrganizationID: &org.ID, IdempotencyKey: "role-expiry-target-" + label}
			type result struct {
				binding account.RoleBinding
				err     error
			}
			done := make(chan result, 1)
			go func() { b, e := r.GrantOrganizationRoleBinding(ctx, actor, request); done <- result{b, e} }()
			deadline := time.Now().Add(2 * time.Second)
			waited := false
			for time.Now().Before(deadline) {
				var query string
				var now time.Time
				e = f.admin.QueryRow(ctx, `SELECT query,clock_timestamp() FROM pg_stat_activity WHERE datname=$1 AND state='active' AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) AND query LIKE 'SELECT id FROM memberships%'`, c.Config().Database, held.PgConn().PID()).Scan(&query, &now)
				if e == nil {
					if !now.Before(expiry) {
						t.Fatal("wait not proven before legal expiry")
					}
					waited = true
					t.Log("actual target FOR UPDATE waits on held runtime PID before expiry")
					break
				}
				if !errors.Is(e, pgx.ErrNoRows) {
					t.Fatal(e)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !waited {
				t.Fatal("actual target lock wait not observed")
			}
			if expired {
				for {
					var past bool
					if e = c.QueryRow(ctx, "SELECT clock_timestamp()>$1", expiry).Scan(&past); e != nil {
						t.Fatal(e)
					}
					if past {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			if e = tx.Commit(ctx); e != nil {
				t.Fatal(e)
			}
			got := <-done
			if expired {
				after := snapshot()
				if !errors.Is(got.err, account.ErrForbidden) || after != before {
					t.Errorf("expired target-wait grant: err=%v persistentFactsUnchanged=%v", got.err, after == before)
				}
				t.Run("unauthorized-missing-target-preserves-forbidden", func(t *testing.T) {
					old := snapshot()
					_, e := r.GrantOrganizationRoleBinding(ctx, actor, account.RoleBindingGrant{UserID: uuid.New(), RoleCode: "AUDITOR", OrganizationID: &org.ID, IdempotencyKey: "role-expiry-missing-target"})
					if !errors.Is(e, account.ErrForbidden) || snapshot() != old {
						t.Fatal("early authorization priority/zero facts changed", e)
					}
				})
				return
			}
			if got.err != nil || got.binding.CreatedBy == nil || *got.binding.CreatedBy != actorID {
				t.Fatal("live authority grant failed", got.err)
			}
			first := snapshot()
			again, e := r.GrantOrganizationRoleBinding(ctx, actor, request)
			if e != nil || again.ID != got.binding.ID || snapshot() != first {
				t.Fatal("same actor/key live replay altered facts", e)
			}
			if _, e = r.GrantOrganizationRoleBinding(ctx, owner, request); !errors.Is(e, account.ErrConflict) || snapshot() != first {
				t.Fatal("foreign actor replay bypassed binding", e)
			}
		})
	}
}
