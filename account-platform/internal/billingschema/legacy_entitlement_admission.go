package billingschema

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// Preserve the published 016 bytes. This resource identifies the supported
// installed history; it is never used as an alternative executable migration.
//
//go:embed retained/000016_entitlement_components.up.sql
var originalComponentMigrationSQL string

const originalComponentMigrationChecksum = "bfcba8858a2c6e1312dbcd3d96eb163263f62220ad816c41f339fda198944335"
const alternateComponentMigrationChecksum = "d7ae07456345f7b112d045c23a82c30cb978bca4eae8177fe8833cbcbab6114a"
const alternateComponentMigrationCRLFChecksum = "b0e7f8fcdb611ab8e8c5d44d29be779f82992cc91c4202954492a62757526b98"

var ErrLegacyEntitlementUpgradeUnsupported = errors.New("billing databases predating entitlement migration 016 are temporarily unsupported; preserve the original database and use an explicitly supported upgrade path")
var ErrAlternateEntitlementHistoryUnsupported = errors.New("billing alternate entitlement migration 016 history is temporarily unsupported; preserve the original database")
var ErrPublishedEntitlementMigrationRequired = errors.New("billing deliveries after the pre-016 foundation require the exact published migration 016")

// Admission precedes generic preparation and every migration transaction. The
// same session lock spans classification and execution. Its extra acquisition is
// paired here; the generic migrator's acquisition retains its existing caller
// Unlock contract. Only the exact <=015 foundation can omit016; a later
// delivery must not evade admission by accidentally omitting the016 resource.
func withLegacyEntitlementAdmission(ctx context.Context, connection *pgx.Conn, input []migrations.Migration, apply func() error) (err error) {
	contains016, laterDelivery := false, false
	for _, m := range input {
		if m.Version == 16 {
			contains016 = true
			if m.Name != "entitlement_components" || m.Checksum != originalComponentMigrationChecksum || sqlChecksum(m.UpSQL) != originalComponentMigrationChecksum || sqlChecksum(originalComponentMigrationSQL) != originalComponentMigrationChecksum {
				return errors.New("billing entitlement admission requires the exact published migration 016")
			}
			continue
		}
		if m.Version >= 1 && m.Version <= 15 {
			continue
		}
		// This one forward-numbered fence is part of the original <=015
		// foundation, installed immediately after006. A large version alone
		// does not identify either this exception or an installed016 history.
		if m.Version == generationVersion && m.Name == "outbox_publication_generation" && m.Checksum == generationChecksum && sqlChecksum(m.UpSQL) == generationChecksum && sqlChecksum(m.DownSQL) == generationDownChecksum {
			continue
		}
		laterDelivery = true
	}
	if !contains016 {
		if laterDelivery {
			return ErrPublishedEntitlementMigrationRequired
		}
		return apply()
	}
	if _, err = connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); err != nil {
		return err
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		releaseErr := connection.QueryRow(clean, `SELECT pg_advisory_unlock(hashtext('pinvou_account_platform_migrations'))`).Scan(&released)
		if releaseErr == nil && !released {
			releaseErr = errors.New("billing entitlement admission lock was not held")
		}
		if releaseErr != nil {
			err = errors.Join(err, releaseErr)
		}
	}()
	if err = checkLegacyEntitlementAdmission(ctx, connection); err != nil {
		return err
	}
	return apply()
}

func checkLegacyEntitlementAdmission(ctx context.Context, source *pgx.Conn) error {
	var ledger bool
	if err := source.QueryRow(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&ledger); err != nil {
		return err
	}
	if ledger {
		var name, checksum string
		err := source.QueryRow(ctx, `SELECT name,checksum FROM public.schema_migrations WHERE version=16`).Scan(&name, &checksum)
		if err == nil {
			if name != "entitlement_components" {
				return errors.New("billing installed migration 016 identity is unsupported")
			}
			if checksum == alternateComponentMigrationChecksum || checksum == alternateComponentMigrationCRLFChecksum {
				return ErrAlternateEntitlementHistoryUnsupported
			}
			digest := sha256.Sum256([]byte(strings.ReplaceAll(strings.ReplaceAll(originalComponentMigrationSQL, "\r\n", "\n"), "\n", "\r\n")))
			if checksum != originalComponentMigrationChecksum && checksum != hex.EncodeToString(digest[:]) {
				return errors.New("billing installed migration 016 checksum is unsupported")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var recorded bool
		if err := source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.schema_migrations)`).Scan(&recorded); err != nil {
			return err
		}
		if recorded {
			return ErrLegacyEntitlementUpgradeUnsupported
		}
	}
	// Dedicated Billing bootstrap may already have extension-owned objects or an
	// empty migration ledger. Other existing public relations/functions/types are
	// not a fresh database; do not repair untracked schema or infer from balances.
	var existing bool
	if err := source.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
 WHERE n.nspname='public' AND c.relname<>'schema_migrations' AND c.relkind IN ('r','p','v','m','S','f')
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_class'::regclass AND d.objid=c.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname='public'
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_proc'::regclass AND d.objid=p.oid AND d.deptype='e')
 UNION ALL SELECT 1 FROM pg_type t JOIN pg_namespace n ON n.oid=t.typnamespace WHERE n.nspname='public' AND t.typtype IN ('e','d')
 AND NOT EXISTS(SELECT 1 FROM pg_depend d WHERE d.classid='pg_type'::regclass AND d.objid=t.oid AND d.deptype='e')
 )`).Scan(&existing); err != nil {
		return err
	}
	if existing {
		return ErrLegacyEntitlementUpgradeUnsupported
	}
	return nil
}
