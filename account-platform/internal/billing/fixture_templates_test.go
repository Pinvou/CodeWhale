package billing

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
)

func TestOwnerWalletTemplateScopePostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit disposable PostgreSQL service required")
	}
	t.Setenv("PINVOU_CI_FIXTURE_TEMPLATES", "1")
	t.Setenv("PINVOU_CI_OWNER_WALLET_TEMPLATES", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	admin, err := cifixture.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var previous uint32
	for _, root := range []string{"first", "second"} {
		t.Run(root, func(t *testing.T) {
			f06WalletActors(t)
			var oid uint32
			if err := admin.QueryRow(ctx, "SELECT oid FROM pg_roles WHERE rolname='billing_service'").Scan(&oid); err != nil {
				t.Fatal(err)
			}
			if oid == previous {
				t.Fatal("root reused the previous root's actors")
			}
			previous = oid
			for _, name := range []string{"case-a", "case-b"} {
				t.Run(name, func(t *testing.T) {
					r, ctx := retainedFinanceRepository(t)
					var absent bool
					if err := r.pool.QueryRow(ctx, "SELECT to_regclass('public.owner_scope_probe') IS NULL").Scan(&absent); err != nil || !absent {
						t.Fatal("private schema leaked between clones", err)
					}
					if _, err := retainedFinanceOwnerExec(t, r, ctx, "CREATE TABLE owner_scope_probe(value integer)"); err != nil {
						t.Fatal(err)
					}
					var templates int
					if err := admin.QueryRow(ctx, "SELECT count(*) FROM pg_database WHERE left(datname,12)='ci_template_'").Scan(&templates); err != nil || templates != 1 {
						t.Fatal("root did not reuse one baseline", templates, err)
					}
				})
			}
		})
		var roles, databases int
		if err := admin.QueryRow(ctx, "SELECT (SELECT count(*) FROM pg_roles WHERE rolname IN('billing_service','billing_schema_owner','billing_migrator')),(SELECT count(*) FROM pg_database WHERE datname NOT IN('postgres','template0','template1'))").Scan(&roles, &databases); err != nil || roles != 0 || databases != 0 {
			t.Fatal("root templates or actors survived retirement", roles, databases, err)
		}
	}
}
