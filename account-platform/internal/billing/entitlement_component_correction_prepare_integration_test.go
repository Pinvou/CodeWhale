package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestEntitlementComponentCorrectionPreparePostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, schema := newAdminPaginationIntegrationRepositoryToVersion(t, ctx, databaseURL, 57)
	var defaultValue, nullable string
	err := repository.pool.QueryRow(ctx, `SELECT column_default,is_nullable FROM information_schema.columns
WHERE table_schema=$1 AND table_name='entitlement_components' AND column_name='reclassified_out_microcredits'`, schema).Scan(&defaultValue, &nullable)
	if err != nil || defaultValue != "0" || nullable != "NO" {
		t.Fatalf("correction column must be a safe zero default: default=%q nullable=%q err=%v", defaultValue, nullable, err)
	}
	var uniqueOriginal, oldConservation, zeroOnly bool
	err = repository.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
   WHERE n.nspname=$1 AND t.relname='entitlement_components' AND c.contype='u'
   AND pg_get_constraintdef(c.oid)='UNIQUE (package_id, component_type)'),
 EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
   WHERE n.nspname=$1 AND t.relname='entitlement_components' AND c.contype='c'
   AND pg_get_constraintdef(c.oid) LIKE '%available_microcredits%original_microcredits%'
   AND pg_get_constraintdef(c.oid) NOT LIKE '%reclassified_out_microcredits%'),
 EXISTS(SELECT 1 FROM pg_constraint c JOIN pg_class t ON t.oid=c.conrelid JOIN pg_namespace n ON n.oid=t.relnamespace
   WHERE n.nspname=$1 AND t.relname='entitlement_components'
   AND c.conname='entitlement_components_correction_inactive'
   AND pg_get_constraintdef(c.oid) LIKE '%reclassified_out_microcredits = 0%')`, schema).Scan(&uniqueOriginal, &oldConservation, &zeroOnly)
	if err != nil || !uniqueOriginal || !oldConservation || !zeroOnly {
		t.Fatalf("prepare migration must not enable corrections: unique=%v old_check=%v zero_only=%v err=%v", uniqueOriginal, oldConservation, zeroOnly, err)
	}
	walletID, ownerID, lotID, packageID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at)
VALUES($1,'PERSONAL',$2,'ACTIVE',$3,$3)`, walletID, ownerID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at)
VALUES($1,$2,'ADJUSTMENT',$3,100,100,FALSE,$4,$5)`, lotID, walletID, "test-correction:"+lotID.String(), now.Add(24*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO entitlement_packages(id,user_id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,
points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference)
VALUES($1,$2,$3,$4,'TEST','Test','',0,100,100,$5,$5,$6,'ACTIVE',$5,$5,'SYSTEM',$7)`, packageID, ownerID, walletID, lotID, now, now.Add(24*time.Hour), "test-package:"+packageID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,
credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BONUS',$1,100,100,$3,$4,'ACTIVE',$3,$3)`, lotID, packageID, now, now.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE entitlement_components SET reclassified_out_microcredits=1 WHERE id=$1`, lotID); err == nil {
		t.Fatal("prepare migration accepted a nonzero reclassification")
	}
	qualified := pgx.Identifier{schema, "entitlement_component_corrections"}.Sanitize()
	var canRead, canInsert, canUpdate, canDelete bool
	err = repository.pool.QueryRow(ctx, `SELECT has_table_privilege('billing_service',$1,'SELECT'),
 has_table_privilege('billing_service',$1,'INSERT'),has_table_privilege('billing_service',$1,'UPDATE'),
 has_table_privilege('billing_service',$1,'DELETE')`, qualified).Scan(&canRead, &canInsert, &canUpdate, &canDelete)
	if err != nil || !canRead || canInsert || canUpdate || canDelete {
		t.Fatalf("runtime correction grants are not read-only: read=%v insert=%v update=%v delete=%v err=%v", canRead, canInsert, canUpdate, canDelete, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE `+qualified+` SET reason_code=reason_code`); err == nil {
		t.Fatal("immutable correction table accepted UPDATE")
	}
	if _, err := repository.pool.Exec(ctx, `TRUNCATE TABLE `+qualified); err == nil {
		t.Fatal("immutable correction table accepted TRUNCATE")
	}
	var rows int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM `+qualified).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("prepare migration created a correction: rows=%d err=%v", rows, err)
	}
}
