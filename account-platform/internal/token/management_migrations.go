package token

import (
	"context"
	"fmt"

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
	ordered, err := tokenschema.Ordered(loaded)
	if err != nil {
		return err
	}
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err = migrations.VerifyApplied(ctx, connection.Conn(), ordered); err != nil {
		return err
	}
	// The public stage also pins names and canonical ledger identities. This is
	// a read-only startup check; it does not acquire migration authority.
	rows, err := connection.Query(ctx, `SELECT version,name,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var version int64
		var name, checksum string
		if err := rows.Scan(&version, &name, &checksum); err != nil {
			return err
		}
		if index >= len(ordered) || version != ordered[index].Version || name != ordered[index].Name || checksum != ordered[index].Checksum {
			return fmt.Errorf("Token database is outside the installed management stage")
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(ordered) {
		return fmt.Errorf("Token database management history is incomplete")
	}
	return nil
}
