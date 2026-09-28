package billing

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestListPackagesProjectsCurrentLocalizedCatalogNamesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)

	ownerID, walletID, lotID, packageID, catalogItemID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	productID := "PACKAGE_NAME_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	now := time.Now().UTC()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',100,$3,$3)`, []any{walletID, ownerID, now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,TRUE,$4,$5)`, []any{lotID, walletID, "localized-package:" + packageID.String(), now.Add(24 * time.Hour), now}},
		{`INSERT INTO recharge_products(product_id,amount_fen,credit_microcredits,display_points,currency,channels,catalog_version,enabled,created_at,plan_name,sku_code,base_points,bonus_points,validity_days,price_status,invoice_product_name) VALUES($1,100,100,100,'CNY',ARRAY['ALIPAY'],1,FALSE,$2,'Purchase snapshot','',100,0,30,'FROZEN','Test invoice')`, []any{productID, now}},
		{`INSERT INTO recharge_product_catalog_items(catalog_item_id,product_id,display_name_zh_cn,display_name_en,display_name_ja,display_order,lifecycle_status,version,created_at,updated_at) VALUES($1,$2,'当前中文名','Current English name','現在の日本語名',1,'OFF_SALE',1,$3,$3)`, []any{catalogItemID, productID, now}},
		{`INSERT INTO entitlement_packages(id,user_id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,$4,$5,'Purchase-time snapshot','',100,100,100,$6,$6,$7,'ACTIVE',$6,$6,'SYSTEM',$8)`, []any{packageID, ownerID, walletID, lotID, productID, now, now.Add(24 * time.Hour), "system-package:" + packageID.String()}},
	}
	tx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for index, statement := range statements {
		if _, err := tx.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	packages, _, err := repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 {
		t.Fatalf("packages=%+v err=%v", packages, err)
	}
	item := packages[0]
	if item.PlanName != "Purchase-time snapshot" || item.CurrentPlanNameZhCN != "当前中文名" || item.CurrentPlanNameEN != "Current English name" || item.CurrentPlanNameJA != "現在の日本語名" {
		t.Fatalf("localized package projection=%+v", item)
	}

	if _, err := repository.pool.Exec(ctx, `UPDATE recharge_product_catalog_items SET display_name_zh_cn='更新中文名',display_name_en='Updated English name',display_name_ja='更新した日本語名',updated_at=$2 WHERE catalog_item_id=$1`, catalogItemID, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	packages, _, err = repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 || packages[0].PlanName != "Purchase-time snapshot" || packages[0].CurrentPlanNameEN != "Updated English name" {
		t.Fatalf("updated localized package projection=%+v err=%v", packages, err)
	}
}
