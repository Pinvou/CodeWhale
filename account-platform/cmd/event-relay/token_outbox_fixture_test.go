package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tokenRelayFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	repository *token.PostgresRepository
	runtimeURL string
	owner      *pgx.Conn
	newDB      func() string
}

// Uses the public Token migration wrapper and the same finite deployment roles
// as its retained fixtures. Only an explicitly owned, initially empty service
// is admitted; every database has an opaque cluster/OID/owner creation lease.
func newTokenRelayFixture(t *testing.T, raw string) tokenRelayFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	t.Cleanup(cancel)
	admin, err := cifixture.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	var directory, session, actor, database string
	var super bool
	var roles, databases, clients int
	err = admin.QueryRow(ctx, `SELECT current_setting('data_directory'),session_user,current_user,current_database(),
 (SELECT rolsuper FROM pg_roles WHERE rolname=session_user),
 (SELECT count(*) FROM pg_roles WHERE rolname !~ '^pg_' AND rolname<>session_user),
 (SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')),
 (SELECT count(*) FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid())`).
		Scan(&directory, &session, &actor, &database, &super, &roles, &databases, &clients)
	ciOwned := os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID") != ""
	localOwned := os.Getenv("PINVOU_PRIVATE_OWNER_USER") == session && filepath.IsAbs(os.Getenv("PINVOU_PRIVATE_OWNER_DATA_DIRECTORY")) &&
		filepath.ToSlash(filepath.Clean(os.Getenv("PINVOU_PRIVATE_OWNER_DATA_DIRECTORY"))) == filepath.ToSlash(filepath.Clean(directory)) && clients == 0
	config := admin.Config()
	if err != nil || config.Host != "127.0.0.1" || config.Database != "postgres" || len(config.RuntimeParams) != 0 || len(config.Fallbacks) != 0 ||
		database != "postgres" || session != actor || !super || roles != 0 || databases != 0 || (!ciOwned && !localOwned) {
		t.Fatal("exclusive owned Token relay fixture rejected before writes")
	}
	created := []string{}
	leases := []*cifixture.Database{}
	roleSnapshot := func(ctx context.Context) (string, error) {
		var value string
		err := admin.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_array(oid,rolname,rolcanlogin,rolinherit,rolsuper,rolcreatedb,rolcreaterole,rolreplication,rolbypassrls) ORDER BY rolname)::text,'[]') FROM pg_roles WHERE rolname=ANY($1)`, created).Scan(&value)
		return value, err
	}
	expectedRoles := "[]"
	membershipSnapshot := func(ctx context.Context) (string, error) {
		var value string
		err := admin.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY roleid,member,grantor)::text,'[]') FROM pg_auth_members m
 WHERE roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))`, created).Scan(&value)
		return value, err
	}
	expectedMemberships := "[]"
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 25*time.Second)
		defer stop()
		actual, err := roleSnapshot(clean)
		if err != nil || actual != expectedRoles {
			t.Error("owned role identity/attributes changed; cleanup refused", err)
			return
		}
		memberships, err := membershipSnapshot(clean)
		if err != nil || memberships != expectedMemberships {
			t.Error("changed role membership prevents cleanup", err)
			return
		}
		for i := len(leases) - 1; i >= 0; i-- {
			if _, err := cifixture.DropDatabase(clean, admin, leases[i], session); err != nil {
				t.Error("owned database cleanup failed; roles retained", err)
				return
			}
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Error("owned role cleanup failed", err)
				return
			}
		}
	})
	for _, role := range []string{"token_gateway", "token_cap_policy_deployer", "token_schema_owner", "token_migrator", "token_policy_operator"} {
		login := "LOGIN"
		if role == "token_schema_owner" || role == "token_cap_policy_deployer" {
			login = "NOLOGIN"
		}
		if _, err = admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" "+login+" NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
			t.Fatal(err)
		}
		created = append(created, role)
		expectedRoles, err = roleSnapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = admin.Exec(ctx, `GRANT token_gateway,token_schema_owner,token_cap_policy_deployer TO token_migrator`); err != nil {
		t.Fatal(err)
	}
	expectedMemberships, err = membershipSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	newDB := func() string {
		t.Helper()
		name := "token_event_relay_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		lease, err := cifixture.CreateDatabase(ctx, admin, name, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
		return name
	}
	name := newDB()
	ownerConfig := admin.Config().Copy()
	ownerConfig.Database = name
	bootstrap, err := pgx.ConnectConfig(ctx, ownerConfig)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapSQL, err := os.ReadFile("../../deploy/production/token-database-bootstrap.sql")
	if err != nil {
		bootstrap.Close(context.Background())
		t.Fatal(err)
	}
	if _, err = bootstrap.Exec(ctx, string(bootstrapSQL)); err != nil {
		bootstrap.Close(context.Background())
		t.Fatal(err)
	}
	bootstrap.Close(context.Background())
	expectedMemberships, err = membershipSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ownerConfig.User, ownerConfig.Password = "token_migrator", ""
	owner, err := pgx.ConnectConfig(ctx, ownerConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close(context.Background()) })
	if _, err = owner.Exec(ctx, `SET ROLE token_gateway`); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = tokenschema.Up(ctx, owner, loaded); err != nil {
		t.Fatal(err)
	}
	if _, err = tokenschema.VerifyApplied(ctx, owner, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, owner)
	dsn := (&url.URL{Scheme: "postgres", User: url.User("token_gateway"), Host: net.JoinHostPort(config.Host, fmt.Sprint(config.Port)), Path: "/" + name, RawQuery: "sslmode=disable"}).String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	secrets, err := token.NewLocalAESProvider("controlled-relay-fixture", bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := token.NewPostgresRepository(ctx, dsn, secrets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	return tokenRelayFixture{ctx: ctx, pool: pool, repository: repository, runtimeURL: dsn, owner: owner, newDB: newDB}
}
