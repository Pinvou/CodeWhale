package account

import (
	"context"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
)

func TestPostgresA04AccountUpgradePreservesExistingIdentityOrganizationAndEventGeneration(t *testing.T) {
	ctx := context.Background()
	repo := accountDatabase(t)
	principal, me := accountUser(t, repo)
	organization := accountOrganization(t, repo, principal)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Release()
	defer connection.Conn().Close(ctx)
	for range len(loaded) - 3 {
		if err := migrations.DownOne(ctx, connection.Conn(), loaded); err != nil {
			t.Fatal(err)
		}
	}
	var version int64
	if err := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 3 {
		t.Fatal("fixture was not at the A03 account schema")
	}
	if err := migrations.Up(ctx, connection.Conn(), loaded); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetMe(ctx, principal)
	if err != nil || stored.UserID != me.UserID || stored.Status != "ACTIVE" {
		t.Fatal("A04 migration changed an existing identity")
	}
	if _, err := repo.SetActiveContext(ctx, principal, &organization.ID); err != nil {
		t.Fatal(err)
	}
	resolved, err := repo.ResolveAuthorizationContext(ctx, principal)
	if err != nil || resolved.OrganizationID == nil || *resolved.OrganizationID != organization.ID {
		t.Fatal("A04 upgrade changed existing organization authorization")
	}
	var generation int64
	if err := repo.pool.QueryRow(ctx, `SELECT publication_generation FROM outbox_events WHERE event_type='account.membership.changed.v1' LIMIT 1`).Scan(&generation); err != nil || generation != 0 {
		t.Fatal("A04 upgrade changed the A03 publication identity")
	}
}
