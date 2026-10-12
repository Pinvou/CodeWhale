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

// UpHooks let a trusted domain wrapper lend and revoke narrowly scoped DDL
// privileges in the same transaction as one pending migration. Hooks never run
// for already-recorded versions, and any hook or migration failure rolls the
// complete transaction back before the ledger can advance. Both hooks start as
// the original migration role; elevated hooks must select authority explicitly.
type UpHooks struct {
	// ApplySQL may execute a checksum-pinned compatibility representation without
	// changing the immutable source or the checksum recorded in the ledger.
	ApplySQL func(context.Context, pgx.Tx, Migration) error
	// LedgerRole separates migration history writes from runtime DML authority.
	LedgerRole  string
	BeforeApply func(context.Context, pgx.Tx, Migration) error
	AfterApply  func(context.Context, pgx.Tx, Migration) error
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
		migration.Checksum = normalizedChecksum(migration.UpSQL)
		migrations = append(migrations, *migration)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

func Up(ctx context.Context, connection *pgx.Conn, migrations []Migration) error {
	return UpWithHooks(ctx, connection, migrations, UpHooks{})
}

func UpWithHooks(ctx context.Context, connection *pgx.Conn, migrations []Migration, hooks UpHooks) error {
	if err := prepare(ctx, connection); err != nil {
		return err
	}
	role, err := migrationRole(ctx, connection)
	if err != nil {
		return err
	}
	for _, migration := range migrations {
		var existingChecksum string
		err = connection.QueryRow(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, migration.Version,
		).Scan(&existingChecksum)
		switch {
		case err == nil:
			if !checksumMatches(existingChecksum, migration.UpSQL) {
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
		if err = setMigrationRole(ctx, tx, role); err == nil && hooks.BeforeApply != nil {
			err = hooks.BeforeApply(ctx, tx, migration)
		}
		if err == nil {
			err = setMigrationRole(ctx, tx, role)
		}
		if err == nil {
			if hooks.ApplySQL != nil {
				err = hooks.ApplySQL(ctx, tx, migration)
			} else {
				_, err = tx.Exec(ctx, migration.UpSQL)
			}
		}
		// Migration SQL may legitimately end under a trusted owner. Hooks are
		// always entered as the original ledger/migration role; a hook that needs
		// elevated authority must select that role explicitly and is reset again
		// before the ledger write.
		if err == nil {
			err = setMigrationRole(ctx, tx, role)
		}
		if err == nil && hooks.AfterApply != nil {
			err = hooks.AfterApply(ctx, tx, migration)
		}
		if err == nil {
			err = setMigrationRole(ctx, tx, role)
		}
		if err == nil && hooks.LedgerRole != "" {
			err = setMigrationRole(ctx, tx, hooks.LedgerRole)
		}
		if err == nil {
			_, err = tx.Exec(ctx,
				`INSERT INTO schema_migrations(version, name, checksum) VALUES ($1, $2, $3)`,
				migration.Version, migration.Name, migration.Checksum,
			)
		}
		if err == nil && hooks.LedgerRole != "" {
			err = setMigrationRole(ctx, tx, role)
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

// migrationRole is the identity the ledger belongs to. A deployment runs the
// migrator with MIGRATION_ROLE already selected, and that role owns
// schema_migrations in every domain.
func migrationRole(ctx context.Context, connection *pgx.Conn) (string, error) {
	var role string
	if err := connection.QueryRow(ctx, `SELECT current_user`).Scan(&role); err != nil {
		return "", fmt.Errorf("select migration role: %w", err)
	}
	return role, nil
}

// setMigrationRole pins the transaction back to the migration role. A
// migration may legitimately enter a trusted owner through SET ROLE, but
// neither that owner nor the role it leaves behind may decide whether the
// ledger row is recorded or readable later: the ledger is owned by the
// migration role, so an unpinned connection would fail the next ledger read
// with SQLSTATE 42501 instead of applying or skipping history.
func setMigrationRole(ctx context.Context, tx pgx.Tx, role string) error {
	if _, err := tx.Exec(ctx, `SET ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("restore migration role: %w", err)
	}
	return nil
}

type Verification struct {
	AppliedCount   int    `json:"applied_count"`
	LatestVersion  int64  `json:"latest_version"`
	LatestChecksum string `json:"latest_checksum"`
}

func normalizedChecksum(sql string) string {
	return checksum(normalizeLineEndings(sql))
}

func checksumMatches(existingChecksum, sql string) bool {
	normalized := normalizeLineEndings(sql)
	if existingChecksum == checksum(normalized) {
		return true
	}

	// Older deployments recorded bytes after checkout or packaging. Accept the
	// CRLF representation only when the normalized SQL is otherwise identical.
	return existingChecksum == checksum(strings.ReplaceAll(normalized, "\n", "\r\n"))
}

func normalizeLineEndings(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func checksum(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func DownOne(ctx context.Context, connection *pgx.Conn, migrations []Migration) error {
	return DownOneWithGuard(ctx, connection, migrations, nil)
}

// DownOneWithGuard checks domain-owned rollback invariants in the same
// transaction as the original Down SQL and ledger removal. A rejected guard
// leaves both data and migration history unchanged.
func DownOneWithGuard(ctx context.Context, connection *pgx.Conn, migrations []Migration, guard func(context.Context, pgx.Tx, Migration) error) error {
	if err := prepare(ctx, connection); err != nil {
		return err
	}
	role, err := migrationRole(ctx, connection)
	if err != nil {
		return err
	}
	var version int64
	err = connection.QueryRow(ctx, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
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
		if err = setMigrationRole(ctx, tx, role); err == nil && guard != nil {
			err = guard(ctx, tx, migration)
		}
		// A trusted guard must not choose the identity of the original SQL.
		if err == nil {
			err = setMigrationRole(ctx, tx, role)
		}
		if err == nil {
			if _, err = tx.Exec(ctx, migration.DownSQL); err == nil {
				if err = setMigrationRole(ctx, tx, role); err == nil {
					_, err = tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version)
				}
			}
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("rollback migration %d: %w", version, err)
		}
		return tx.Commit(ctx)
	}
	return fmt.Errorf("applied migration %d is absent from filesystem", version)
}

// VerifyApplied proves that the database contains exactly the checked-in
// forward migration history. It is intentionally run after Up so release
// evidence cannot be produced from a partially migrated database.
func VerifyApplied(ctx context.Context, connection *pgx.Conn, loaded []Migration) (Verification, error) {
	rows, err := connection.Query(ctx, `SELECT version, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return Verification{}, fmt.Errorf("query applied migration inventory: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]string)
	for rows.Next() {
		var version int64
		var migrationChecksum string
		if err := rows.Scan(&version, &migrationChecksum); err != nil {
			return Verification{}, fmt.Errorf("scan applied migration inventory: %w", err)
		}
		applied[version] = migrationChecksum
	}
	if err := rows.Err(); err != nil {
		return Verification{}, fmt.Errorf("read applied migration inventory: %w", err)
	}
	if len(applied) != len(loaded) {
		return Verification{}, fmt.Errorf("applied migration count %d does not match packaged count %d", len(applied), len(loaded))
	}

	verification := Verification{AppliedCount: len(applied)}
	for _, migration := range loaded {
		existing, ok := applied[migration.Version]
		if !ok {
			return Verification{}, fmt.Errorf("packaged migration %d is not applied", migration.Version)
		}
		if !checksumMatches(existing, migration.UpSQL) {
			return Verification{}, fmt.Errorf("applied migration %d checksum does not match package", migration.Version)
		}
		verification.LatestVersion = migration.Version
		verification.LatestChecksum = migration.Checksum
	}
	return verification, nil
}

func prepare(ctx context.Context, connection *pgx.Conn) error {
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	// PostgreSQL checks schema CREATE even for CREATE TABLE IF NOT EXISTS.
	// Upgrades retain only ledger DML after schema creation is deliberately
	// revoked; keep the same search_path and lock without requesting new DDL.
	var exists bool
	if err := connection.QueryRow(ctx, `SELECT pg_catalog.to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("locate schema migrations: %w", err)
	}
	if exists {
		return nil
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
