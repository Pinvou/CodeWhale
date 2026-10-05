package token

import (
	"context"
	"testing"
)

func TestPostgresDefaultRoutePreservesUserAccountWithSameDisplayName(t *testing.T) {
	ctx := context.Background()
	s, repo, _ := proxyFixture(t, "http://127.0.0.1:1")
	resolved, err := s.contexts.Resolve(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := repo.ListCandidates(ctx, "model-test")
	if err != nil || len(accounts) != 1 {
		t.Fatal("missing owned fixture route")
	}
	existing := accounts[0]
	if _, err := repo.pool.Exec(ctx, `UPDATE token_accounts SET name=$2 WHERE id=$1`, existing.TokenAccountID, platformDefaultAccountName); err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewDefaultRouteProvisioner(repo, s.providerURLs, DefaultRouteConfig{Provider: "test-provider", BaseURL: "http://127.0.0.1:1", Secret: []byte("fixture-default-key"), Models: []string{"default-model"}, Priority: 10})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := provisioner.Ensure(ctx, resolved, existing.WalletID, "default-model"); err != nil {
			t.Fatal("user display-name collision blocked the platform route")
		}
	}
	var mode, name string
	if err := repo.pool.QueryRow(ctx, `SELECT management_mode,name FROM token_accounts WHERE id=$1`, existing.TokenAccountID).Scan(&mode, &name); err != nil || mode != "USER" || name != platformDefaultAccountName {
		t.Fatal("platform provisioning modified the user account")
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM token_accounts WHERE owner_id=$1 AND management_mode='PLATFORM'`, resolved.UserID).Scan(&count); err != nil || count != 1 {
		t.Fatal("repeated provisioning did not retain one platform account")
	}
	candidates, err := repo.ListCandidates(ctx, "default-model")
	if err != nil || len(candidates) != 1 || candidates[0].TokenAccountID == existing.TokenAccountID {
		t.Fatal("default route was not attached to its own platform account")
	}
}
