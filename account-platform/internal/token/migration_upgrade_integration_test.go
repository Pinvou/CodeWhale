package token

import (
	"context"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPostgresA04TokenUpgradePreservesExistingUsageAndRoutes(t *testing.T) {
	ctx := context.Background()
	repo, usage := settlementFixture(t)
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	defer connection.Conn().Close(ctx)
	// Return only this owned schema to its A03 version with existing data, then
	// execute both original A04 migrations through the production migration API.
	for range 2 {
		if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
			t.Fatal(err)
		}
	}
	var version int64
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 4 {
		t.Fatal("fixture was not at the A03 token schema")
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetUsage(ctx, usage.ID)
	if err != nil || stored.Status != "UPSTREAM_ACTIVE" || stored.IdempotencyKey != usage.IdempotencyKey {
		t.Fatal("A04 migrations changed existing usage state")
	}
	account, err := repo.GetAccount(ctx, usage.TokenAccountID)
	if err != nil || account.ManagementMode != "USER" {
		t.Fatal("A04 upgrade did not preserve a user-owned route")
	}
	candidates, err := repo.ListCandidates(ctx, usage.Model)
	if err != nil || len(candidates) != 1 || candidates[0].TokenAccountID != usage.TokenAccountID || candidates[0].CredentialID != usage.CredentialID {
		t.Fatal("A04 upgrade changed route or credential ownership")
	}
}
