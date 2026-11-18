package cifixture

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestTemplateKeyIncludesCompleteSetup(t *testing.T) {
	a, err := TemplateKey("v1", []string{"SQL", "checksum"})
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		policy string
		values []string
	}{
		{"v2", []string{"SQL", "checksum"}}, {"v1", []string{"changed SQL", "checksum"}}, {"v1", []string{"SQL", "changed checksum"}},
	} {
		b, err := TemplateKey(input.policy, input.values)
		if err != nil || a == b {
			t.Fatal("changed fixture input reused identity")
		}
	}
}

func TestRetiredTemplatePoolRefusesCreation(t *testing.T) {
	pool := NewTemplatePool(false)
	if err := pool.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Create(context.Background(), nil, "unused", "postgres", "", nil); err == nil {
		t.Fatal("retired pool admitted new resource")
	}
}

// The existing six financial Jobs run this real PostgreSQL probe before their
// selected contracts. It uses no production schema, relaxes no ownership fence,
// and leaves the service pristine for the original absent-actor checks.
func TestDatabaseTemplateIsolationPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit disposable PostgreSQL service required")
	}
	t.Setenv("PINVOU_CI_FIXTURE_TEMPLATES", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(context.Background())
	key, _ := TemplateKey("isolation-probe-v1", []string{"CREATE TABLE marker(value text)", "seed"})
	t.Run("private-data-schema-and-frozen-source", func(t *testing.T) {
		pool := NewTemplatePool(true, "postgres")
		t.Cleanup(func() {
			if err := pool.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		prepared := 0
		prepare := func(ctx context.Context, name string) error {
			prepared++
			config := c.Config().Copy()
			config.Database = name
			db, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				return err
			}
			defer db.Close(ctx)
			_, err = db.Exec(ctx, "CREATE TABLE marker(value text); INSERT INTO marker VALUES('seed')")
			return err
		}
		for i := 0; i < 2; i++ {
			name := "ci_clone_" + uuid.NewString()
			lease, cached, err := pool.Create(ctx, c, name, "postgres", key, prepare)
			if err != nil || !cached {
				t.Fatalf("clone: cached=%v err=%v", cached, err)
			}
			config := c.Config().Copy()
			config.Database = name
			db, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			var value string
			if err := db.QueryRow(ctx, "SELECT value FROM marker").Scan(&value); err != nil || value != "seed" {
				t.Fatal("clone leaked another case's mutations", err)
			}
			if _, err := db.Exec(ctx, "UPDATE marker SET value='mutated'; ALTER TABLE marker ADD COLUMN private text"); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := DropDatabase(ctx, c, lease, "postgres"); err != nil {
				t.Fatal(err)
			}
		}
		if prepared != 1 || pool.hits != 1 {
			t.Fatal("setup was not reused")
		}
		for _, entry := range pool.entries {
			config := c.Config().Copy()
			config.Database = entry.lease.key.name
			db, err := pgx.ConnectConfig(ctx, config)
			if err == nil {
				db.Close(ctx)
				t.Fatal("template accepted a writer connection")
			}
			if _, err := c.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{entry.lease.key.name}.Sanitize()+" ALLOW_CONNECTIONS true"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := pool.Create(ctx, c, "ci_rejected_"+uuid.NewString(), "postgres", key, prepare); err == nil {
				t.Fatal("unfrozen source admitted")
			}
			if _, err := c.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{entry.lease.key.name}.Sanitize()+" ALLOW_CONNECTIONS false"); err != nil {
				t.Fatal(err)
			}
		}
	})
	t.Run("failed-preparation-never-becomes-hit", func(t *testing.T) {
		pool := NewTemplatePool(true, "postgres")
		t.Cleanup(func() {
			if err := pool.Close(ctx); err != nil {
				t.Error(err)
			}
		})
		failure := errors.New("deliberate preparation failure")
		_, _, err := pool.Create(ctx, c, "ci_failed_"+uuid.NewString(), "postgres", key, func(context.Context, string) error { return failure })
		if !errors.Is(err, failure) {
			t.Fatal("preparation failure masked", err)
		}
		_, _, err = pool.Create(ctx, c, "ci_failed_"+uuid.NewString(), "postgres", key, func(context.Context, string) error { t.Fatal("failed preparation retried"); return nil })
		if err == nil {
			t.Fatal("unconfirmed template admitted")
		}
	})
	t.Run("disabled-path-keeps-empty-create", func(t *testing.T) {
		pool := NewTemplatePool(false)
		name := "ci_original_" + uuid.NewString()
		lease, cached, err := pool.Create(ctx, c, name, "postgres", key, func(context.Context, string) error { t.Fatal("disabled preparation called"); return nil })
		if err != nil || cached {
			t.Fatal("original create changed", err)
		}
		if _, err := DropDatabase(ctx, c, lease, "postgres"); err != nil {
			t.Fatal(err)
		}
		if err := pool.Close(ctx); err != nil {
			t.Fatal(err)
		}
	})
	var remaining int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_database WHERE left(datname,12)='ci_template_'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal("template leaked", remaining, err)
	}
	if strings.TrimSpace(raw) == "" {
		t.Fatal("missing explicit fixture identity")
	}
}
