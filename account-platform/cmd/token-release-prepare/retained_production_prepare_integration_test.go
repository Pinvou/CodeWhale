package main

import (
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRetainedProductionPrepareGovernanceStats(t *testing.T) {
	dsn := os.Getenv("TOKEN_RELEASE_PREPARE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("dedicated TOKEN_RELEASE_PREPARE_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("fixture admin unavailable")
	}
	defer admin.Close(context.Background())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	database := "f04_cli_" + suffix
	candidates := []string{"tc_a_" + suffix, "tc_b_" + suffix}
	roles := []string{"token_gateway", "token_schema_owner", "token_cap_policy_deployer", "token_migrator", "token_policy_operator"}
	roles = append(roles, candidates...)
	var exists bool
	if err = admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1) OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($2::text[]))`, database, roles).Scan(&exists); err != nil || exists {
		t.Fatal("exclusive identities must be absent")
	}
	var created []string
	var dbCreated bool
	owner := "token_gateway"
	t.Log("OWNED_FIXTURE_INITIAL_ABSENT")
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if dbCreated {
			var actualOwner string
			var active int
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&actualOwner, &active); err != nil {
					t.Error("owned database identity unavailable")
					return
				}
				if actualOwner != owner {
					t.Error("owned database owner changed")
					return
				}
				if active == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Error("owned database connections remain")
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
			if _, err := admin.Exec(clean, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()); err != nil {
				t.Error("owned database no-force cleanup failed")
				return
			}
		}
		// Prepared roles are cluster-global; refuse any external ownership/grant
		// or role edge before dropping only this invocation's recorded identities.
		var external bool
		if err := admin.QueryRow(clean, `SELECT EXISTS(SELECT 1 FROM pg_shdepend d JOIN pg_roles r ON r.oid=d.refobjid WHERE d.refclassid='pg_authid'::regclass AND r.rolname=ANY($1::text[])) OR EXISTS(SELECT 1 FROM pg_auth_members m JOIN pg_roles child ON child.oid=m.member JOIN pg_roles parent ON parent.oid=m.roleid WHERE (child.rolname=ANY($1::text[]) OR parent.rolname=ANY($1::text[])) AND NOT (child.rolname=ANY($1::text[]) AND (parent.rolname=ANY($1::text[]) OR (child.rolname='token_migrator' AND parent.rolname='pg_read_all_stats'))))`, created).Scan(&external); err != nil || external {
			t.Error("owned role external dependency guard refused cleanup")
			return
		}
		for i := len(created) - 1; i >= 0; i-- {
			if _, err := admin.Exec(clean, "DROP ROLE "+pgx.Identifier{created[i]}.Sanitize()); err != nil {
				t.Error("recorded role cleanup failed")
				return
			}
		}
		var remaining int
		if err := admin.QueryRow(clean, `SELECT (SELECT count(*) FROM pg_database WHERE datname=$1)+(SELECT count(*) FROM pg_roles WHERE rolname=ANY($2::text[]))`, database, roles).Scan(&remaining); err != nil || remaining != 0 {
			t.Error("owned resource postcheck failed")
			return
		}
		t.Log("OWNED_FIXTURE_POST_ZERO")
	}()
	if _, err = admin.Exec(ctx, "CREATE ROLE token_gateway NOLOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS"); err != nil {
		t.Fatal("exclusive runtime role creation failed")
	}
	created = append(created, "token_gateway")
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER token_gateway"); err != nil {
		t.Fatal("owned database creation failed")
	}
	dbCreated = true
	config := admin.Config().Copy()
	config.Database = database
	connection, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("owned database connection failed")
	}
	defer connection.Close(context.Background())
	request := releaseRequest{Operation: "prepare", Database: database, Approval: "public-fixture-approval", Maintenance: "public-fixture-window", Candidates: candidates, Apply: true, Migrations: "../../migrations/token"}
	receipt, err := executeRelease(ctx, connection, request)
	// Transactional preparation either creates all expected absent roles or
	// none. Record exactly the committed catalog, even if a later assert fails.
	var observed []string
	if e := admin.QueryRow(ctx, `SELECT coalesce(array_agg(rolname::text ORDER BY rolname),'{}'::text[]) FROM pg_roles WHERE rolname=ANY($1::text[])`, roles).Scan(&observed); e != nil {
		t.Fatal("created role inventory unavailable")
	}
	for _, role := range observed {
		if !slices.Contains(created, role) {
			created = append(created, role)
		}
	}
	if err != nil || !receipt.Applied {
		t.Fatalf("production prepare did not apply: %v", err)
	}
	owner = "token_schema_owner"
	if receipt.State != "NOT_INSTALLED" {
		t.Fatal("empty own database fabricated an activation")
	}
	var stats bool
	if err = connection.QueryRow(ctx, `SELECT pg_has_role('token_migrator','pg_read_all_stats','MEMBER')`).Scan(&stats); err != nil {
		t.Fatal("actual migrator membership unavailable")
	}
	t.Log("ACTUAL_PRODUCTION_PREPARE_COMMITTED")
	if !stats {
		t.Error("prepared migrator cannot meet production governance activity-visibility contract")
	}
}
