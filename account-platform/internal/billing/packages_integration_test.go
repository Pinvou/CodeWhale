package billing

import (
	"context"
	"errors"
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

	command := func(version int64, zh, en, ja, key string) RechargeProductDisplayNamesCommand {
		return RechargeProductDisplayNamesCommand{CatalogItemID: catalogItemID, ExpectedVersion: version, DisplayNameZhCN: zh, DisplayNameEN: en, DisplayNameJA: ja, IdempotencyKey: key, RequestHash: key, Actor: ownerID.String(), Reason: "integration rename"}
	}
	for _, failure := range []struct{ table, key string }{{"recharge_product_command_records", "package-record-fail-0001"}, {"recharge_product_audits", "package-audit-fail-0001"}} {
		functionName := "fail_" + strings.TrimPrefix(failure.table, "recharge_product_")
		if _, err = repository.pool.Exec(ctx, `CREATE OR REPLACE FUNCTION `+functionName+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.command_type='BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE' THEN RAISE EXCEPTION 'injected write failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER `+functionName+` BEFORE INSERT ON `+failure.table+` FOR EACH ROW EXECUTE FUNCTION `+functionName+`()`); err != nil {
			t.Fatal(err)
		}
		if _, err = repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(1, "不应提交", "Must rollback", "ロールバック", failure.key)); err == nil {
			t.Fatalf("%s failure did not abort rename", failure.table)
		}
		var name string
		var version int64
		if err = repository.pool.QueryRow(ctx, `SELECT display_name_en,version FROM recharge_product_catalog_items WHERE catalog_item_id=$1`, catalogItemID).Scan(&name, &version); err != nil || name != "Current English name" || version != 1 {
			t.Fatalf("%s did not roll back catalog: name=%q version=%d err=%v", failure.table, name, version, err)
		}
		var commands int
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key=$1`, failure.key).Scan(&commands); err != nil || commands != 0 {
			t.Fatalf("%s left command record: count=%d err=%v", failure.table, commands, err)
		}
		if _, err = repository.pool.Exec(ctx, `DROP TRIGGER `+functionName+` ON `+failure.table+`; DROP FUNCTION `+functionName+`()`); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(1, "更新中文名", "Updated English name", "更新した日本語名", "package-rename-0001"))
	if err != nil || updated.Version != 2 || updated.LifecycleStatus != "OFF_SALE" {
		t.Fatalf("updated=%+v err=%v", updated, err)
	}
	replayed, err := repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(1, "更新中文名", "Updated English name", "更新した日本語名", "package-rename-0001"))
	if err != nil || replayed.Version != updated.Version {
		t.Fatalf("idempotent replay=%+v err=%v", replayed, err)
	}
	crossProduct := command(1, "更新中文名", "Updated English name", "更新した日本語名", "package-rename-0001")
	crossProduct.CatalogItemID = uuid.New()
	crossProduct.RequestHash = "different-resource-hash"
	if _, err = repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, crossProduct); !errors.Is(err, ErrConflict) {
		t.Fatalf("same key across resources must conflict: %v", err)
	}
	var commandCount, auditCount int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM recharge_product_command_records WHERE idempotency_key='package-rename-0001'`).Scan(&commandCount); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM recharge_product_audits WHERE idempotency_key='package-rename-0001'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if commandCount != 1 || auditCount != 1 {
		t.Fatalf("command=%d audit=%d", commandCount, auditCount)
	}
	packages, _, err = repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || len(packages) != 1 || packages[0].PlanName != "Purchase-time snapshot" || packages[0].CurrentPlanNameEN != "Updated English name" {
		t.Fatalf("updated localized package projection=%+v err=%v", packages, err)
	}
	if _, err = repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(1, "stale", "stale", "stale", "package-rename-0002")); err == nil {
		t.Fatal("stale expected_version must fail")
	}
	if _, err = repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(2, " ", "English", "日本語", "package-rename-0003")); err == nil {
		t.Fatal("blank localized name must fail")
	}
	if _, err = repository.TransitionRechargeProduct(ctx, catalogItemID, 2, "DELETED", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ExecuteRechargeProductDisplayNamesUpdate(ctx, command(3, "删除后中文", "Deleted English", "削除後日本語", "package-rename-0004")); err != nil {
		t.Fatal(err)
	}
	packages, _, err = repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || packages[0].CurrentPlanNameEN != "Deleted English" || packages[0].PlanName != "Purchase-time snapshot" {
		t.Fatalf("deleted catalog projection=%+v err=%v", packages, err)
	}
	if _, err = repository.pool.Exec(ctx, `DELETE FROM recharge_product_catalog_items WHERE catalog_item_id=$1`, catalogItemID); err != nil {
		t.Fatal(err)
	}
	packages, _, err = repository.ListPackages(ctx, ownerID, walletID, 10)
	if err != nil || packages[0].CurrentPlanNameEN != "" || packages[0].PlanName != "Purchase-time snapshot" {
		t.Fatalf("missing catalog fallback projection=%+v err=%v", packages, err)
	}
}
