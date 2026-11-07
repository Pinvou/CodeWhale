package billing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/jackc/pgx/v5"
)

type f18RoleLease struct {
	name string
	oid  uint32
}

type f18Coordinator struct {
	user, directory string
	port            int
	localOwned      bool
	ciOwned         bool
}

// Database cleanup runs first through cifixture leases. Role retirement must
// then prove this same coordinator, no clients/databases and no external role
// dependencies. The recorded OIDs prevent adopting a same-name replacement.
func (owner f18Coordinator) cleanupRoles(ctx context.Context, admin *pgx.Conn, leases []f18RoleLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if owner.ciOwned {
		if err := cifixture.Verify(ctx, admin); err != nil {
			return err // Never retire roles through an unconfirmed CI backend.
		}
	}
	names, ids := make([]string, 0, len(leases)), make([]uint32, 0, len(leases))
	for _, lease := range leases {
		if lease.name == "" || lease.oid == 0 {
			return errors.New("owned role identity was not recorded")
		}
		names, ids = append(names, lease.name), append(ids, lease.oid)
	}
	var session, current, directory string
	var port, clients, databases int
	var outsideRoles, outsideMembership, dependencies bool
	err := admin.QueryRow(ctx, `SELECT session_user,current_user,current_setting('data_directory'),current_setting('port')::int,
 (SELECT count(*) FROM pg_stat_activity WHERE pid<>pg_backend_pid() AND backend_type='client backend'),
 (SELECT count(*) FROM pg_database WHERE NOT datistemplate AND datname<>'postgres'),
 EXISTS(SELECT 1 FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>$1 AND NOT(rolname=ANY($2::text[]) AND oid=ANY($3::oid[]))),
 EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid=ANY($3::oid[]) OR member=ANY($3::oid[])) AND NOT(roleid=ANY($3::oid[]) AND member=ANY($3::oid[]))),
 EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid=ANY($3::oid[]) AND dbid<>0)`, owner.user, names, ids).Scan(
		&session, &current, &directory, &port, &clients, &databases, &outsideRoles, &outsideMembership, &dependencies)
	if err != nil {
		return fmt.Errorf("verify owned coordinator before role cleanup: %w", err)
	}
	directoryMatches := strings.ReplaceAll(directory, "\\", "/") == owner.directory
	if owner.localOwned {
		directoryMatches = f18OwnedDirectoryMatches(directory, owner.directory)
	}
	if session != owner.user || current != owner.user || port != owner.port || !directoryMatches || clients != 0 || databases != 0 || outsideRoles || outsideMembership || dependencies {
		return errors.New("owned coordinator, client or dependency state changed; no role cleanup permitted")
	}
	rows, err := admin.Query(ctx, `SELECT rolname,oid FROM pg_roles WHERE rolname=ANY($1::text[]) OR oid=ANY($2::oid[])`, names, ids)
	if err != nil {
		return err
	}
	found := make(map[string]uint32)
	for rows.Next() {
		var name string
		var oid uint32
		if err := rows.Scan(&name, &oid); err != nil {
			rows.Close()
			return err
		}
		found[name] = oid
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(found) == 0 {
		return nil // Confirmed absent roles are an idempotent retirement.
	}
	if len(found) != len(leases) {
		return errors.New("owned role set changed; no role cleanup permitted")
	}
	for _, lease := range leases {
		if found[lease.name] != lease.oid {
			return errors.New("owned role identity changed; no role cleanup permitted")
		}
	}
	tx, err := admin.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	for index := len(leases) - 1; index >= 0; index-- {
		if _, err := tx.Exec(ctx, "DROP ROLE "+pgx.Identifier{leases[index].name}.Sanitize()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func TestF18OwnedRoleCleanupPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" || os.Getenv("F18_TEST_DATABASE_PROFILE") == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Second)
	defer cancel()
	config, directory, port := f18OwnedConnectionConfig(t, raw)
	admin, err := f18ConnectOwned(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	owner := f18Coordinator{user: config.User, directory: directory, port: port, localOwned: os.Getenv("F18_TEST_DATABASE_PROFILE") == "local-owned", ciOwned: os.Getenv("F18_TEST_DATABASE_PROFILE") == "ci-owned"}
	if err := owner.cleanupRoles(ctx, admin, nil); err != nil {
		t.Fatal("exclusive empty coordinator required before any DDL", err)
	}
	for _, scenario := range []string{"normal", "partial-creation", "active-client", "external-membership", "replacement-role", "wrong-directory", "cancelled-context", "expired-context"} {
		t.Run(scenario, func(t *testing.T) {
			var leases []f18RoleLease
			for _, name := range []string{"billing_schema_owner", "billing_service", "billing_migrator"} {
				if _, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{name}.Sanitize()+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
					t.Fatal(err)
				}
				lease := f18RoleLease{name: name}
				if err := admin.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname=$1`, name).Scan(&lease.oid); err != nil {
					t.Fatal(err)
				}
				leases = append(leases, lease)
				if scenario == "partial-creation" {
					break // A later creation failure still owns this recorded role.
				}
			}
			var client *pgx.Conn
			candidate, callContext := owner, ctx
			if scenario == "active-client" {
				client, err = pgx.ConnectConfig(ctx, config)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "external-membership" {
				if _, err := admin.Exec(ctx, "GRANT billing_service TO "+pgx.Identifier{owner.user}.Sanitize()); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "replacement-role" {
				if _, err := admin.Exec(ctx, "DROP ROLE billing_service; CREATE ROLE billing_service NOLOGIN"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "wrong-directory" {
				candidate.directory += "-not-the-created-instance"
			}
			if scenario == "cancelled-context" {
				var stop context.CancelFunc
				callContext, stop = context.WithCancel(ctx)
				stop()
			}
			if scenario == "expired-context" {
				var stop context.CancelFunc
				callContext, stop = context.WithDeadline(ctx, time.Unix(1, 0))
				defer stop()
			}
			before := f18RoleSnapshot(t, ctx, admin)
			cleanupErr := candidate.cleanupRoles(callContext, admin, leases)
			if scenario == "normal" || scenario == "partial-creation" {
				if cleanupErr != nil {
					t.Fatal(cleanupErr)
				}
			} else {
				if cleanupErr == nil || f18RoleSnapshot(t, ctx, admin) != before {
					t.Fatal("unconfirmed cleanup changed owned or external role facts", cleanupErr)
				}
				if client != nil {
					pid := client.PgConn().PID()
					if err := client.Close(ctx); err != nil {
						t.Fatal(err)
					}
					// Socket close is not acknowledgement that PostgreSQL has
					// retired the owned backend. Observe that exact PID first.
					for {
						var active bool
						if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1)`, pid).Scan(&active); err != nil {
							t.Fatal(err)
						}
						if !active {
							break
						}
						timer := time.NewTimer(10 * time.Millisecond)
						select {
						case <-timer.C:
						case <-ctx.Done():
							timer.Stop()
							t.Fatal("owned client backend did not exit", ctx.Err())
						}
					}
				}
				if scenario == "external-membership" {
					if _, err := admin.Exec(ctx, "REVOKE billing_service FROM "+pgx.Identifier{owner.user}.Sanitize()); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "replacement-role" {
					// Only this test created the replacement. Record its own
					// new lease after proving the stale lease could not delete it.
					if err := admin.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname='billing_service'`).Scan(&leases[1].oid); err != nil {
						t.Fatal(err)
					}
				}
				if err := owner.cleanupRoles(ctx, admin, leases); err != nil {
					t.Fatal("next cleanup with confirmed identity did not recover", err)
				}
			}
			if err := owner.cleanupRoles(ctx, admin, leases); err != nil {
				t.Fatal("repeated cleanup was not idempotent", err)
			}
		})
	}
}

func f18RoleSnapshot(t *testing.T, ctx context.Context, admin *pgx.Conn) string {
	t.Helper()
	var value string
	if err := admin.QueryRow(ctx, `SELECT json_build_object('roles',(SELECT json_agg(row(oid,rolname,rolsuper,rolcanlogin)::text ORDER BY oid) FROM pg_roles WHERE rolname !~ '^pg_'),'members',(SELECT json_agg(row(roleid,member,grantor,admin_option,inherit_option,set_option)::text ORDER BY roleid,member,grantor) FROM pg_auth_members))::text`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
