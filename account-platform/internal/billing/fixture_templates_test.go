package billing

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/cifixture"
)

var retainedBillingTemplates = cifixture.NewTemplatePool(os.Getenv("PINVOU_CI_RETAINED_BILLING_TEMPLATES") == "1", "billing_schema_owner", "billing_migrator", "billing_service")

var ownerTemplateScopes = struct {
	sync.RWMutex
	pools map[string]*cifixture.TemplatePool
}{pools: make(map[string]*cifixture.TemplatePool)}

// A root owns its originally absent actors. Child databases retire before this
// root's snapshot, and the snapshot retires before f06WalletActors drops roles.
// No snapshot or actor is borrowed from a preceding/following root.
func registerOwnerWalletTemplates(t *testing.T) {
	t.Helper()
	if os.Getenv("PINVOU_CI_OWNER_WALLET_TEMPLATES") != "1" {
		return
	}
	pool := cifixture.NewTemplatePool(true, "billing_schema_owner", "billing_migrator", "billing_service")
	name := t.Name()
	ownerTemplateScopes.Lock()
	_, exists := ownerTemplateScopes.pools[name]
	if !exists {
		ownerTemplateScopes.pools[name] = pool
	}
	ownerTemplateScopes.Unlock()
	if exists {
		t.Fatal("owner template scope already registered")
	}
	t.Cleanup(func() {
		ownerTemplateScopes.Lock()
		delete(ownerTemplateScopes.pools, name)
		ownerTemplateScopes.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := pool.Close(ctx); err != nil {
			t.Error("root-owned wallet templates did not retire:", err)
		}
	})
}

func walletTemplates(t *testing.T) *cifixture.TemplatePool {
	ownerTemplateScopes.RLock()
	defer ownerTemplateScopes.RUnlock()
	name, selected := "", retainedBillingTemplates
	for root, pool := range ownerTemplateScopes.pools {
		if (t.Name() == root || strings.HasPrefix(t.Name(), root+"/")) && len(root) > len(name) {
			name, selected = root, pool
		}
	}
	return selected
}

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

func TestMain(m *testing.M) {
	code := m.Run()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := retainedBillingTemplates.Close(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "owned fixture templates did not retire:", err)
		if code == 0 {
			code = 86
		}
	}
	os.Exit(code)
}
