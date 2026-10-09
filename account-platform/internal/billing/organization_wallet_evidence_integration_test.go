package billing

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Own only absent deployment actors. Existing actors are never modified.
func f06WalletActors(t *testing.T) {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL is required")
	}
	c, err := pgx.Connect(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	roles := []string{"billing_service", "billing_schema_owner", "billing_migrator"}
	var count int
	if err = c.QueryRow(context.Background(), `SELECT count(*) FROM pg_roles WHERE rolname=ANY($1)`, roles).Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture requires absent billing actors; no existing actor writes", count, err)
	}
	tx, err := c.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = tx.Exec(context.Background(), `CREATE ROLE billing_service LOGIN INHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE billing_schema_owner NOLOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; CREATE ROLE billing_migrator LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS; GRANT billing_service,billing_schema_owner TO billing_migrator; GRANT billing_service TO billing_schema_owner WITH INHERIT TRUE,SET TRUE`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var external bool
		err := c.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_shdepend WHERE refclassid='pg_authid'::regclass AND refobjid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND dbid<>0) OR EXISTS(SELECT 1 FROM pg_auth_members WHERE (roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) OR member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))) AND NOT(roleid IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1)) AND member IN(SELECT oid FROM pg_roles WHERE rolname=ANY($1))))`, roles).Scan(&external)
		if err != nil || external {
			t.Error("external dependencies: owned actors retained", err)
			return
		}
		for i := len(roles) - 1; i >= 0; i-- {
			if _, err = c.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{roles[i]}.Sanitize()); err != nil {
				t.Error(err)
			}
		}
	})
}

func TestF06OrganizationWalletRequiresLifecycleEvidence(t *testing.T) {
	f06WalletActors(t)
	r, ctx := retainedFinanceRepository(t) // Genuine complete billing migration chain.
	t.Run("unknown organization creates no wallet or liability", func(t *testing.T) {
		owner := uuid.New()
		var before int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if _, created, err := r.EnsureWallet(ctx, "ORGANIZATION", owner); !errors.Is(err, ErrConflict) || created {
			t.Fatal("unknown lifecycle accepted", created, err)
		}
		var count int
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM wallets WHERE owner_type='ORGANIZATION' AND owner_id=$1`, owner).Scan(&count); err != nil || count != 0 {
			t.Fatal("unknown wallet persisted", count, err)
		}
		if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts`).Scan(&count); err != nil || count != before {
			t.Fatal("unknown lifecycle wrote liability", count, before, err)
		}
	})
	for _, state := range []struct{ projection, wallet string }{{"ACTIVE", "ACTIVE"}, {"DISABLED", "FROZEN"}, {"DELETED", "CLOSED"}} {
		t.Run("projection first "+state.projection, func(t *testing.T) {
			owner := uuid.New()
			if state.projection == "DELETED" {
				if _, err := r.ApplyOrganizationLifecycle(ctx, owner, "DISABLED", 1, false); err != nil {
					t.Fatal(err)
				}
			}
			version := int64(1)
			if state.projection == "DELETED" {
				version = 2
			}
			if _, err := r.ApplyOrganizationLifecycle(ctx, owner, state.projection, version, false); err != nil {
				t.Fatal(err)
			}
			wallet, created, err := r.EnsureWallet(ctx, "ORGANIZATION", owner)
			if err != nil || !created || wallet.Status != state.wallet {
				t.Fatal(wallet, created, err)
			}
			replay, recreated, err := r.EnsureWallet(ctx, "ORGANIZATION", owner)
			if err != nil || recreated || replay.ID != wallet.ID || replay.Version != wallet.Version {
				t.Fatal("wallet replay changed", replay, err)
			}
		})
	}
	t.Run("legacy existing organization and personal stay unchanged", func(t *testing.T) {
		owner := uuid.New()
		wallet, _, err := r.EnsureWallet(ctx, "PERSONAL", owner)
		if err != nil {
			t.Fatal(err)
		}
		// Represent a genuine pre-projection wallet using its authorized owner in
		// this UUID database. Do not fabricate a lifecycle record or bypass guards.
		if _, err = retainedFinanceOwnerExec(t, r, ctx, `UPDATE wallets SET owner_type='ORGANIZATION',status='FROZEN' WHERE id='`+wallet.ID.String()+`'`); err != nil {
			t.Fatal(err)
		}
		old, created, err := r.EnsureWallet(ctx, "ORGANIZATION", owner)
		if err != nil || created || old.ID != wallet.ID || old.Status != "FROZEN" {
			t.Fatal(old, created, err)
		}
		personal, created, err := r.EnsureWallet(ctx, "PERSONAL", uuid.New())
		if err != nil || !created || personal.Status != "ACTIVE" {
			t.Fatal(personal, created, err)
		}
	})
	t.Run("projection races wallet creation under the shared lock", func(t *testing.T) {
		owner := uuid.New()
		if _, err := r.ApplyOrganizationLifecycle(ctx, owner, "ACTIVE", 1, false); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		errs := make(chan error, 2)
		go func() { defer wg.Done(); _, _, err := r.EnsureWallet(ctx, "ORGANIZATION", owner); errs <- err }()
		go func() {
			defer wg.Done()
			_, err := r.ApplyOrganizationLifecycle(ctx, owner, "DISABLED", 2, false)
			errs <- err
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		wallet, created, err := r.EnsureWallet(ctx, "ORGANIZATION", owner)
		if err != nil || created || wallet.Status != "FROZEN" {
			t.Fatal("projection missed new wallet", wallet, created, err)
		}
	})
}
