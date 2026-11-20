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
