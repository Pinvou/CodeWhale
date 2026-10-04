package migrations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var migrationName = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.(up|down)\.sql$`)

type Migration struct {
	Version  int64
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum string
}

func Load(directory string) ([]Migration, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	byVersion := make(map[int64]*Migration)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		match := migrationName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("invalid migration version in %q", entry.Name())
		}
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		migration := byVersion[version]
		if migration == nil {
			migration = &Migration{Version: version, Name: match[2]}
			byVersion[version] = migration
		} else if migration.Name != match[2] {
			return nil, fmt.Errorf("migration version %d has conflicting names", version)
		}
		if match[3] == "up" {
			if migration.UpSQL != "" {
				return nil, fmt.Errorf("migration version %d has duplicate up script", version)
			}
			migration.UpSQL = string(body)
		} else {
			if migration.DownSQL != "" {
				return nil, fmt.Errorf("migration version %d has duplicate down script", version)
			}
			migration.DownSQL = string(body)
		}
	}

	migrations := make([]Migration, 0, len(byVersion))
	for _, migration := range byVersion {
		if strings.TrimSpace(migration.UpSQL) == "" {
			return nil, fmt.Errorf("migration version %d is missing an up script", migration.Version)
		}
		migration.Checksum = checksum(normalizeLineEndings(migration.UpSQL))
		migrations = append(migrations, *migration)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

func Up(ctx context.Context, connection *pgx.Conn, migrations []Migration) error {
	if err := prepare(ctx, connection); err != nil {
		return err
	}
	for _, migration := range migrations {
		var existingChecksum string
		err := connection.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, migration.Version,
		).Scan(&existingChecksum)
		switch {
		case err == nil:
			if !matchesChecksum(existingChecksum, migration.UpSQL) {
				return fmt.Errorf("migration %d checksum changed after application", migration.Version)
			}
			continue
		case !errors.Is(err, pgx.ErrNoRows):
			return fmt.Errorf("query migration %d: %w", migration.Version, err)
		}

		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.Version, err)
		}
		if _, err = tx.Exec(ctx, migration.UpSQL); err == nil {
			_, err = tx.Exec(ctx,
				`INSERT INTO schema_migrations(version, name, checksum) VALUES ($1, $2, $3)`,
				migration.Version, migration.Name, checksum(normalizeLineEndings(migration.UpSQL)),
			)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %d: %w", migration.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.Version, err)
		}
	}
	return nil
}

func DownOne(ctx context.Context, connection *pgx.Conn, migrations []Migration) error {
	if err := prepare(ctx, connection); err != nil {
		return err
	}
	var version int64
	err := connection.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("select latest migration: %w", err)
	}
	for _, migration := range migrations {
		if migration.Version != version {
			continue
		}
		if strings.TrimSpace(migration.DownSQL) == "" {
			return fmt.Errorf("migration %d is irreversible", version)
		}
		tx, err := connection.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin rollback %d: %w", version, err)
		}
		if _, err = tx.Exec(ctx, migration.DownSQL); err == nil {
			_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("rollback migration %d: %w", version, err)
		}
		return tx.Commit(ctx)
	}
	return fmt.Errorf("applied migration %d is absent from filesystem", version)
}

func prepare(ctx context.Context, connection *pgx.Conn) error {
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := connection.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY,
    name TEXT NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("prepare schema migrations: %w", err)
	}
	return nil
}

func Unlock(ctx context.Context, connection *pgx.Conn) {
	_, _ = connection.Exec(ctx, `SELECT pg_advisory_unlock(hashtext('pinvou_account_platform_migrations'))`)
}

func normalizeLineEndings(sql string) string {
	return strings.ReplaceAll(sql, "\r\n", "\n")
}

func checksum(sql string) string {
	digest := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(digest[:])
}

// Previously applied migrations hashed checkout bytes. Accept the two normal
// checkout representations of the same script, without rewriting stored hashes.
// SQL edits still change both candidate hashes and fail closed.
func matchesChecksum(existing, sql string) bool {
	lf := normalizeLineEndings(sql)
	return existing == checksum(lf) || existing == checksum(strings.ReplaceAll(lf, "\n", "\r\n"))
}
