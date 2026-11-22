package token

import (
	"context"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/tokenschema"
)

// VerifyManagementMigrations rejects incomplete, later-stage and altered
// databases before exposing this stage's management routes or workers.
func (r *PostgresRepository) VerifyManagementMigrations(ctx context.Context, directory string) error {
	loaded, err := migrations.Load(directory)
	if err != nil {
		return err
	}
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	_, err = tokenschema.VerifyApplied(ctx, connection.Conn(), loaded)
	return err
}
