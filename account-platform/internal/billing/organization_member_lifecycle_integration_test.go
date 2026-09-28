package billing

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestActiveMembershipVersionAdvanceKeepsGrantBalancesPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, _ := newAdminPaginationIntegrationRepository(t, ctx, databaseURL)

	organizationID, memberID, actorID := uuid.New(), uuid.New(), uuid.New()
	walletID, lotID, packageID, componentID, grantID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "ACTIVE", 1); err != nil {
		t.Fatal(err)
	}
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'ORGANIZATION',$2,'ACTIVE',100,$3,$3)`, []any{walletID, organizationID, now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,FALSE,$4,$5)`, []any{lotID, walletID, "member-version:" + lotID.String(), now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,'MEMBER_VERSION_TEST','Member version test','',0,100,100,$4,$4,$5,'ACTIVE',$4,$4,'SYSTEM',$6)`, []any{packageID, walletID, lotID, now, now.Add(24 * time.Hour), "member-version-package:" + packageID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,active_grant_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$3,100,20,80,$4,$5,'ACTIVE',$4,$4)`, []any{componentID, packageID, lotID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,membership_version,allocation_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',80,80,'ACTIVE',1,1,$6,$7,$8,$9,$10,$10)`, []any{grantID, organizationID, memberID, packageID, componentID, actorID, "member-version-grant-0001", strings.Repeat("a", 64), now.Add(24 * time.Hour), now}},
	}
	for index, statement := range statements {
		if _, err := repository.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index, err)
		}
	}

	if _, err := repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "ACTIVE", 2); err != nil {
		t.Fatal(err)
	}
	var version, allocationVersion, available, reserved, used int64
	var status string
	if err := repository.pool.QueryRow(ctx, `SELECT membership_version,allocation_version,available_microcredits,reserved_microcredits,used_microcredits,status FROM member_quota_grants WHERE id=$1`, grantID).Scan(&version, &allocationVersion, &available, &reserved, &used, &status); err != nil {
		t.Fatal(err)
	}
	if version != 2 || allocationVersion != 2 || available != 80 || reserved != 0 || used != 0 || status != "ACTIVE" {
		t.Fatalf("advanced grant version=%d allocation=%d available=%d reserved=%d used=%d status=%s", version, allocationVersion, available, reserved, used, status)
	}
	var audits, events int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='MEMBERSHIP_VERSION_ADVANCED'`, grantID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.member-quota.membership-version-advanced.v1'`, grantID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || events != 1 {
		t.Fatalf("version advance audit=%d outbox=%d", audits, events)
	}

	usageID := uuid.New()
	hold, err := repository.ReserveMemberQuota(ctx, usageID, walletID, organizationID, memberID, 2, 30, nil, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("reserve member quota: %v", err)
	}
	hold, err = repository.ExtendHold(ctx, usageID, hold.ID, now.Add(2*time.Hour), 10)
	if err != nil || hold.MaxMicrocredits != 40 {
		t.Fatalf("extend member quota hold=%+v err=%v", hold, err)
	}
	if _, err = repository.Commit(ctx, usageID, hold.ID, 25, map[string]any{"input_tokens": 10, "output_tokens": 5}); err != nil {
		t.Fatalf("commit member quota: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT available_microcredits,reserved_microcredits,used_microcredits FROM member_quota_grants WHERE id=$1`, grantID).Scan(&available, &reserved, &used); err != nil {
		t.Fatal(err)
	}
	if available != 55 || reserved != 0 || used != 25 {
		t.Fatalf("committed grant available=%d reserved=%d used=%d", available, reserved, used)
	}

	releasedUsageID := uuid.New()
	releasedHold, err := repository.ReserveMemberQuota(ctx, releasedUsageID, walletID, organizationID, memberID, 2, 20, nil, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("reserve releasable member quota: %v", err)
	}
	if _, err = repository.Release(ctx, releasedUsageID, releasedHold.ID, "integration release"); err != nil {
		t.Fatalf("release member quota: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT available_microcredits,reserved_microcredits,used_microcredits FROM member_quota_grants WHERE id=$1`, grantID).Scan(&available, &reserved, &used); err != nil {
		t.Fatal(err)
	}
	if available != 55 || reserved != 0 || used != 25 {
		t.Fatalf("released grant available=%d reserved=%d used=%d", available, reserved, used)
	}

	_, err = repository.ReserveMemberQuota(ctx, uuid.New(), walletID, organizationID, memberID, 2, 56, nil, now.Add(time.Hour))
	if !errors.Is(err, ErrMemberQuotaInsufficient) {
		t.Fatalf("member quota shortage error = %v", err)
	}
	_, err = repository.ReserveMemberQuota(ctx, uuid.New(), walletID, organizationID, memberID, 2, 76, nil, now.Add(time.Hour))
	if !errors.Is(err, ErrInsufficientBalance) {
		t.Fatalf("organization wallet shortage error = %v", err)
	}

	if _, err = repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "REVOKED", 3); err != nil {
		t.Fatalf("revoke membership: %v", err)
	}
	if _, err = repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "ACTIVE", 4); err != nil {
		t.Fatalf("restore membership: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT membership_version,available_microcredits,reserved_microcredits,used_microcredits,status FROM member_quota_grants WHERE id=$1`, grantID).Scan(&version, &available, &reserved, &used, &status); err != nil {
		t.Fatal(err)
	}
	if version != 4 || available != 55 || reserved != 0 || used != 25 || status != "ACTIVE" {
		t.Fatalf("restored grant version=%d available=%d reserved=%d used=%d status=%s", version, available, reserved, used, status)
	}

	// Simulate a pre-fix grant after the authoritative lifecycle row already
	// reached v4. Replaying the same lifecycle fact must repair it exactly once.
	if _, err = repository.pool.Exec(ctx, `UPDATE member_quota_grants SET membership_version=3 WHERE id=$1`, grantID); err != nil {
		t.Fatal(err)
	}
	if _, err = repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "ACTIVE", 4); err != nil {
		t.Fatalf("reconcile stale grant: %v", err)
	}
	if _, err = repository.ApplyOrganizationMemberLifecycle(ctx, organizationID, memberID, "ACTIVE", 4); err != nil {
		t.Fatalf("replay reconciled lifecycle: %v", err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='MEMBERSHIP_VERSION_ADVANCED'`, grantID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.member-quota.membership-version-advanced.v1'`, grantID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if audits != 2 || events != 2 {
		t.Fatalf("idempotent stale repair audit=%d outbox=%d", audits, events)
	}
}

func TestMemberQuotaVersionMigrationBackfillsStaleActiveGrantPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	repository, schema := newAdminPaginationIntegrationRepositoryToVersion(t, ctx, databaseURL, 77)

	organizationID, memberID, actorID := uuid.New(), uuid.New(), uuid.New()
	walletID, lotID, packageID, componentID, grantID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organization_member_lifecycle_states(organization_id,user_id,status,membership_version,updated_at) VALUES($1,$2,'ACTIVE',5,$3)`, []any{organizationID, memberID, now}},
		{`INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,created_at,updated_at) VALUES($1,'ORGANIZATION',$2,'ACTIVE',100,$3,$3)`, []any{walletID, organizationID, now}},
		{`INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,100,100,FALSE,$4,$5)`, []any{lotID, walletID, "migration-backfill:" + lotID.String(), now.Add(24 * time.Hour), now}},
		{`INSERT INTO entitlement_packages(id,wallet_id,credit_lot_id,product_id,plan_name,sku_code,amount_fen,points_total,points_remaining,credited_at,valid_from,valid_until,status,created_at,updated_at,source_type,source_reference) VALUES($1,$2,$3,'MEMBER_VERSION_MIGRATION','Member version migration','',0,100,100,$4,$4,$5,'ACTIVE',$4,$4,'SYSTEM',$6)`, []any{packageID, walletID, lotID, now, now.Add(24 * time.Hour), "migration-package:" + packageID.String()}},
		{`INSERT INTO entitlement_components(id,package_id,component_type,credit_lot_id,original_microcredits,available_microcredits,active_grant_microcredits,credited_at,expires_at,status,created_at,updated_at) VALUES($1,$2,'BASE',$3,100,20,80,$4,$5,'ACTIVE',$4,$4)`, []any{componentID, packageID, lotID, now, now.Add(24 * time.Hour)}},
		{`INSERT INTO member_quota_grants(id,organization_id,member_user_id,package_id,component_id,component_type,original_microcredits,available_microcredits,status,membership_version,allocation_version,created_by,idempotency_key,request_fingerprint,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'BASE',80,80,'ACTIVE',1,1,$6,$7,$8,$9,$10,$10)`, []any{grantID, organizationID, memberID, packageID, componentID, actorID, "migration-backfill-0001", strings.Repeat("b", 64), now.Add(24 * time.Hour), now}},
	}
	for index, statement := range statements {
		if _, err := repository.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index, err)
		}
	}

	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	if _, err = connection.Exec(ctx, `SET search_path TO `+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadBillingIntegrationMigrations("../../migrations/billing", schema)
	if err != nil {
		t.Fatal(err)
	}
	var migration78 []migrations.Migration
	for _, migration := range loaded {
		if migration.Version == 78 {
			migration78 = append(migration78, migration)
		}
	}
	if len(migration78) != 1 {
		t.Fatalf("migration 78 count = %d", len(migration78))
	}
	if err = migrations.Up(ctx, connection, migration78); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)

	var version, allocationVersion, available int64
	if err = repository.pool.QueryRow(ctx, `SELECT membership_version,allocation_version,available_microcredits FROM member_quota_grants WHERE id=$1`, grantID).Scan(&version, &allocationVersion, &available); err != nil {
		t.Fatal(err)
	}
	var audits, events int
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM member_quota_grant_audits WHERE grant_id=$1 AND action='MEMBERSHIP_VERSION_ADVANCED'`, grantID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='billing.member-quota.membership-version-advanced.v1'`, grantID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if version != 5 || allocationVersion != 2 || available != 80 || audits != 1 || events != 1 {
		t.Fatalf("migration repair version=%d allocation=%d available=%d audits=%d events=%d", version, allocationVersion, available, audits, events)
	}
}
