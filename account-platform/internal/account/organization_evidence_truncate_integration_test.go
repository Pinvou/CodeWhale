package account

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func organizationEvidenceFacts(t *testing.T, ctx context.Context, r *PostgresRepository) string {
	t.Helper()
	var facts string
	err := r.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'creation',(SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM organization_creation_requests x),
 'unit_commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM organization_unit_commands x),
 'member_commands',(SELECT jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text) FROM membership_commands x),
 'organizations',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM organizations x),
 'units',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM organization_units x),
 'memberships',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM memberships x),
 'bindings',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM role_bindings x),
 'audit',(SELECT jsonb_agg(to_jsonb(x) ORDER BY id) FROM audit_logs x),
 'outbox',(SELECT jsonb_agg(to_jsonb(x) ORDER BY event_id) FROM outbox_events x),
 'original_ledger',(SELECT jsonb_agg(to_jsonb(x) ORDER BY version) FROM schema_migrations x WHERE version<>900015)
)::text`).Scan(&facts)
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// Evidence rows are produced by the real public command writers, not inserts
// into command tables. User activation is the original fixture's controlled
// setup boundary; identity/wallet provisioning is not under test here.
func seedOrganizationEvidence(t *testing.T, ctx context.Context, r *PostgresRepository) func(*PostgresRepository) {
	t.Helper()
	newUser := func() (identity.Principal, uuid.UUID) {
		p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "e" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
		me, _, err := r.Bootstrap(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.pool.Exec(ctx, `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
			t.Fatal(err)
		}
		return p, me.UserID
	}
	owner, ownerID := newUser()
	_, memberID := newUser()
	creationKey, unitKey, memberKey := uuid.NewString(), uuid.NewString(), uuid.NewString()
	org, err := r.AdminCreateOrganization(ctx, owner, ownerID, "Retained evidence", creationKey)
	if err != nil {
		t.Fatal(err)
	}
	unit, err := r.AdminCreateUnit(ctx, owner, org.ID, nil, "Retained unit", unitKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AdminAddMember(ctx, owner, org.ID, memberID, []string{"MEMBER"}, nil, memberKey); err != nil {
		t.Fatal(err)
	}
	return func(next *PostgresRepository) {
		t.Helper()
		if replay, err := next.AdminCreateOrganization(ctx, owner, ownerID, "Retained evidence", creationKey); err != nil || replay.ID != org.ID {
			t.Fatal("organization receipt did not replay", err)
		}
		if replay, err := next.AdminCreateUnit(ctx, owner, org.ID, nil, "Retained unit", unitKey); err != nil || replay.Unit.ID != unit.Unit.ID {
			t.Fatal("unit receipt did not replay", err)
		}
		if replay, err := next.AdminAddMember(ctx, owner, org.ID, memberID, []string{"MEMBER"}, nil, memberKey); err != nil || !replay.Replayed {
			t.Fatal("membership receipt did not replay", err)
		}
	}
}

func TestOrganizationEvidenceTruncatePublicPostgres(t *testing.T) {
	f := retainedWalletFenceActors(t)
	for _, scenario := range []string{"fresh-install", "retained-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := f.ctx
			var before string
			var replay func(*PostgresRepository)
			var initial func(context.Context, *pgx.Conn, []migrations.Migration) error
			if scenario == "retained-upgrade" {
				initial = func(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) error {
					old := make([]migrations.Migration, 0, len(loaded)-1)
					for _, m := range loaded {
						if m.Version != 900015 {
							old = append(old, m)
						}
					}
					if err := accountschema.Up(ctx, connection, old); err != nil {
						return err
					}
					migrations.Unlock(ctx, connection)
					endpoint, err := url.Parse(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
					if err != nil {
						return err
					}
					endpoint.User = url.User("account_service")
					endpoint.Path = "/" + connection.Config().Database
					oldRuntime, err := NewPostgresRepository(ctx, endpoint.String())
					if err != nil {
						return err
					}
					defer oldRuntime.Close()
					replay = seedOrganizationEvidence(t, ctx, oldRuntime)
					before = organizationEvidenceFacts(t, ctx, oldRuntime)
					return nil
				}
			}
			through := int64(0) // Fresh installation covers the complete current chain.
			if scenario == "retained-upgrade" {
				through = 900015 // Retain this release's actual public rollback contract.
			}
			r := f.repositoryThroughVersion(t, through, initial)
			if scenario == "fresh-install" {
				replay = seedOrganizationEvidence(t, ctx, r)
				before = organizationEvidenceFacts(t, ctx, r)
			}
			if organizationEvidenceFacts(t, ctx, r) != before {
				t.Fatal("forward installation changed retained evidence or original ledger")
			}
			for _, table := range []string{"organization_creation_requests", "organization_unit_commands", "membership_commands"} {
				t.Run(table, func(t *testing.T) {
					qualified := pgx.Identifier{"public", table}.Sanitize()
					var actor, owner string
					var super bool
					var count int
					if err := r.pool.QueryRow(ctx, `SELECT current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user),pg_get_userbyid((SELECT relowner FROM pg_class WHERE oid=to_regclass($1)))`, qualified).Scan(&actor, &super, &owner); err != nil || actor != "account_service" || super || owner != "account_service" {
						t.Fatal("original runtime/owner boundary changed", err)
					}
					if err := r.pool.QueryRow(ctx, "SELECT count(*) FROM "+qualified).Scan(&count); err != nil || count == 0 {
						t.Fatal("real original command evidence is required", err)
					}
					for _, query := range []string{"UPDATE " + qualified + " SET idempotency_key=idempotency_key", "DELETE FROM " + qualified, "TRUNCATE " + qualified, "TRUNCATE " + qualified + " CASCADE"} {
						tx, err := r.pool.Begin(ctx)
						if err != nil {
							t.Fatal(err)
						}
						_, guardErr := tx.Exec(ctx, query)
						rollbackErr := tx.Rollback(ctx)
						var databaseError *pgconn.PgError
						if rollbackErr != nil || !errors.As(guardErr, &databaseError) || databaseError.Code != "P0001" {
							t.Fatal("runtime mutation bypassed the original immutable-evidence contract", guardErr, rollbackErr)
						}
						if organizationEvidenceFacts(t, ctx, r) != before {
							t.Fatal("rejected command mutated original evidence or ledger")
						}
					}
				})
			}
			t.Run("legal-replay-after-rejection", func(t *testing.T) {
				replay(r)
				if organizationEvidenceFacts(t, ctx, r) != before {
					t.Fatal("legal receipt replay changed retained facts")
				}
			})
			t.Run("forward-only-public-down", func(t *testing.T) {
				loaded, err := migrations.Load("../../migrations/account")
				if err != nil {
					t.Fatal(err)
				}
				if through != 0 {
					var prefix []migrations.Migration
					for _, migration := range loaded {
						if migration.Version <= through {
							prefix = append(prefix, migration)
						}
					}
					loaded = prefix
				}
				configuration := r.pool.Config().ConnConfig.Copy()
				configuration.User, configuration.Password = "account_migrator", ""
				connection, err := pgx.ConnectConfig(ctx, configuration)
				if err != nil {
					t.Fatal(err)
				}
				defer connection.Close(context.Background())
				defer migrations.Unlock(context.Background(), connection)
				if _, err := connection.Exec(ctx, "SET ROLE account_service"); err != nil {
					t.Fatal(err)
				}
				var actor, session string
				var super bool
				if err := connection.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor, &session, &super); err != nil || actor != "account_service" || session != "account_migrator" || super {
					t.Fatal("public Down requires the genuine non-superuser migration identity", err)
				}
				assertHistoricalForwardOnlyMigration(t, r, loaded, 900015, "organization evidence truncate protection is forward-only")
				wanted := "organization evidence truncate protection is forward-only"
				if through == 0 {
					wanted = "payment credit closure fence is forward-only; preserve pending and resolved outcomes"
				}
				err = accountschema.DownOne(ctx, connection, loaded)
				var databaseError *pgconn.PgError
				if !errors.As(err, &databaseError) || databaseError.Code != "55000" || databaseError.Message != wanted {
					t.Fatal("public Down removed the statement protection", err)
				}
				migrations.Unlock(ctx, connection)
				if _, err := accountschema.VerifyApplied(ctx, connection, loaded); err != nil || organizationEvidenceFacts(t, ctx, r) != before {
					t.Fatal("refused Down changed original facts or history", err)
				}
				replay(r)
				if organizationEvidenceFacts(t, ctx, r) != before {
					t.Fatal("next receipt replay failed after rejected Down")
				}
			})
		})
	}
}
