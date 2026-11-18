package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These identities belong only to clients created by the admitted, isolated
// managed-control fixture. Observing retirement never authorizes a DB drop or
// termination, nor adopts a replacement backend that happens to reuse its PID.
type managedFixtureBackend struct {
	pid     uint32
	started time.Time
}

func managedFixtureBackendIdentity(ctx context.Context, observer, client *pgx.Conn) (managedFixtureBackend, error) {
	identity := managedFixtureBackend{pid: client.PgConn().PID()}
	err := observer.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity
 WHERE pid=$1 AND datname=$2 AND usename=$3`, identity.pid, client.Config().Database, client.Config().User).Scan(&identity.started)
	if err != nil {
		return identity, fmt.Errorf("owned fixture backend identity unavailable: %w", err)
	}
	return identity, nil
}

const managedFixtureBackendWaitSQL = `/* pinvou owned fixture backend retirement */
 SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND backend_start=$2)`

func waitManagedFixtureBackendExit(ctx context.Context, observer *pgx.Conn, identity managedFixtureBackend) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var present bool
		if err := observer.QueryRow(ctx, managedFixtureBackendWaitSQL, identity.pid, identity.started).Scan(&present); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("owned fixture backend retirement unavailable: %w", err)
		}
		if !present {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func closeManagedFixtureConnection(ctx context.Context, observer, client *pgx.Conn) error {
	// Preserve ordinary client cleanup even when observing identity fails.
	defer client.Close(context.Background())
	identity, err := managedFixtureBackendIdentity(ctx, observer, client)
	if err != nil {
		return err
	}
	if err := client.Close(ctx); err != nil {
		return fmt.Errorf("owned fixture client close failed: %w", err)
	}
	// Close acknowledges the local socket, not PostgreSQL's processing of
	// Terminate. Keep the original caller budget for this explicit handoff.
	return waitManagedFixtureBackendExit(ctx, observer, identity)
}

func closeManagedFixtureSinglePool(ctx context.Context, observer *pgx.Conn, pool *pgxpool.Pool) error {
	defer pool.Close()
	// The governance fixture deliberately uses MaxConns=1. Borrow that exact
	// owned connection, not a role-wide list that could include another DBA.
	configuration := pool.Config()
	if configuration.MaxConns != 1 || configuration.MinConns != 0 || configuration.MinIdleConns != 0 {
		return errors.New("fixture pool retirement requires its single-connection contract")
	}
	connection, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("owned fixture pool connection unavailable: %w", err)
	}
	identity, err := managedFixtureBackendIdentity(ctx, observer, connection.Conn())
	connection.Release()
	pool.Close()
	if err != nil {
		return err
	}
	return waitManagedFixtureBackendExit(ctx, observer, identity)
}
