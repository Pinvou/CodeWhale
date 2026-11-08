// Package cifixture binds privileged test coordinators to this job's verified
// PostgreSQL service. It is used only by test fixtures, never product connections.
package cifixture

import (
	"context"
	"errors"
	"os"
	"strconv"

	"github.com/jackc/pgx/v5"
)

func ConfigurationAllowed(c *pgx.ConnConfig, port, system, directory string) bool {
	n, err := strconv.ParseUint(port, 10, 16)
	_, systemErr := strconv.ParseUint(system, 10, 64)
	return err == nil && n >= 1024 && strconv.FormatUint(n, 10) == port && systemErr == nil && system != "0" && directory != "" && c != nil && c.Host == "127.0.0.1" && c.Port == uint16(n) && c.User == "postgres" && c.Password == "" && c.Database != "" && c.TLSConfig == nil && len(c.Fallbacks) == 0 && len(c.RuntimeParams) == 0
}

// Verify checks a privileged test connection routed through this helper.
// Each such connection is checked separately; pgx.Conn does not reconnect itself.
// Existing UUID-database setup paths retain their ownership/name fences and
// are not all routed through this helper. Cleanup retains its existing fences.
func Verify(ctx context.Context, c *pgx.Conn) error {
	if !active() {
		return nil
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || !ConfigurationAllowed(c.Config(), os.Getenv("PINVOU_ACCOUNT_CI_PG_PORT"), os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID"), os.Getenv("PINVOU_ACCOUNT_CI_PG_DIRECTORY")) {
		return errors.New("CI PostgreSQL coordinator configuration rejected")
	}
	var system, directory, session, current, database string
	var port int
	var super bool
	err := c.QueryRow(ctx, `SELECT system_identifier::text,current_setting('data_directory'),current_setting('port')::int,session_user,current_user,current_database(),(SELECT rolsuper FROM pg_roles WHERE rolname=session_user) FROM pg_control_system()`).Scan(&system, &directory, &port, &session, &current, &database, &super)
	if err != nil || system != os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID") || directory != os.Getenv("PINVOU_ACCOUNT_CI_PG_DIRECTORY") || port != 5432 || session != "postgres" || current != "postgres" || database != c.Config().Database || !super {
		return errors.New("CI PostgreSQL coordinator identity rejected")
	}
	return nil
}

func active() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("PINVOU_ACCOUNT_CI_PG_PORT") != "" || os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID") != "" || os.Getenv("PINVOU_ACCOUNT_CI_PG_DIRECTORY") != ""
}

func Connect(ctx context.Context, raw string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	return ConnectConfig(ctx, config)
}

func ConnectConfig(ctx context.Context, config *pgx.ConnConfig) (*pgx.Conn, error) {
	if active() && (os.Getenv("GITHUB_ACTIONS") != "true" || !ConfigurationAllowed(config, os.Getenv("PINVOU_ACCOUNT_CI_PG_PORT"), os.Getenv("PINVOU_ACCOUNT_CI_PG_SYSTEM_ID"), os.Getenv("PINVOU_ACCOUNT_CI_PG_DIRECTORY"))) {
		return nil, errors.New("CI PostgreSQL coordinator configuration rejected before connection")
	}
	c, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, err
	}
	if err = Verify(ctx, c); err != nil {
		_ = c.Close(ctx)
		return nil, err
	}
	return c, nil
}
