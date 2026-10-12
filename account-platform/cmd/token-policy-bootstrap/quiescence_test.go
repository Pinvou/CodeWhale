package main

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWriterQuiescencePostgres(t *testing.T) {
	dsn := os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL")
	if dsn == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	database, writer, member := "quiescence_"+suffix, "qw_"+suffix, "qm_"+suffix
	quote := func(name string) string { return pgx.Identifier{name}.Sanitize() }
	exec := func(connection interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	}, sql string) {
		if _, err := connection.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	var exists bool
	if err := admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1) OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=ANY($2::text[]))`, database, []string{writer, member}).Scan(&exists); err != nil || exists {
		t.Fatal("exclusive UUID identities required")
	}
	var createdRoles []string
	exec(admin, "CREATE DATABASE "+quote(database))
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		var owner string
		var sessions int
		if err := admin.QueryRow(cleanup, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, database).Scan(&owner, &sessions); err != nil || owner != admin.Config().User || sessions != 0 {
			t.Error("owned fixture cleanup guard failed")
			return
		}
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quote(database)); err != nil {
			t.Error("owned fixture DB cleanup failed")
			return
		}
		for i := len(createdRoles) - 1; i >= 0; i-- {
			if _, err := admin.Exec(cleanup, "DROP ROLE "+quote(createdRoles[i])); err != nil {
				t.Error("owned fixture role cleanup failed")
			}
		}
	}()
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + database
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	exec(pool, "CREATE TABLE public.token_accounts(id integer)")
	exec(pool, "CREATE TABLE public.token_account_provisioning_reservations(status text,lease_owner text)")
	exec(pool, "CREATE TABLE public.token_command_results(command_type text,status text)")
	expect := func(wantPass bool) {
		t.Helper()
		err := checkWriterQuiescence(ctx, pool)
		if (err == nil) != wantPass {
			t.Fatalf("quiescence pass=%v, error=%v", wantPass, err)
		}
	}
	expect(true)
	exec(admin, "CREATE ROLE "+quote(writer)+" LOGIN")
	createdRoles = append(createdRoles, writer)
	exec(pool, "GRANT INSERT ON public.token_accounts TO "+quote(writer))
	expect(false) // a stopped process with an unfenced login can restart
	writerURL := *parsed
	writerURL.User = url.User(writer)
	oldWriter, err := pgx.Connect(ctx, writerURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer oldWriter.Close(ctx)
	exec(admin, "ALTER ROLE "+quote(writer)+" NOLOGIN")
	expect(false) // NOLOGIN does not evict an already connected old binary
	if err = oldWriter.Close(ctx); err != nil {
		t.Fatal(err)
	}
	expect(true)
	exec(admin, "CREATE ROLE "+quote(member)+" LOGIN NOINHERIT")
	createdRoles = append(createdRoles, member)
	exec(admin, "GRANT "+quote(writer)+" TO "+quote(member))
	expect(false) // NOINHERIT still permits SET ROLE
	exec(admin, "ALTER ROLE "+quote(member)+" NOLOGIN")
	expect(true)
	exec(pool, "INSERT INTO public.token_account_provisioning_reservations VALUES ('MANUAL_REVIEW',NULL)")
	expect(false)
	exec(pool, "TRUNCATE public.token_account_provisioning_reservations")
	exec(pool, "INSERT INTO public.token_command_results VALUES ('TOKEN_ACCOUNT_CREATE','PENDING')")
	expect(false)
	exec(pool, "TRUNCATE public.token_command_results")
	other, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Release()
	tx, err := other.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, "INSERT INTO public.token_accounts VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	expect(false) // NOWAIT rejects an in-flight writer
	if err = tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	expect(true)
}
