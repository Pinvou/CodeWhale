package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestBillingApprovalCommandActivationsPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_BILLING_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_BILLING_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "billing_activation_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupCtx, `RESET SESSION AUTHORIZATION; RESET ROLE; DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadBillingIntegrationMigrations("../../migrations/billing", schema)
	if err != nil {
		t.Fatal(err)
	}
	activationIndex := -1
	for index := range loaded {
		if loaded[index].Version == 54 {
			activationIndex = index
			break
		}
	}
	if activationIndex < 0 || activationIndex+1 >= len(loaded) || loaded[activationIndex+1].Version != 55 {
		t.Fatal("integration fixture must explicitly isolate migration 54 from future migrations")
	}
	if err := migrations.Up(ctx, connection, loaded[:activationIndex]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	legacyWallet := uuid.New()
	if _, err := connection.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',now(),now())`, legacyWallet, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// Prior fixture migrations ran as the isolated bootstrap identity. Mirror
	// the production ledger owner before testing the real non-superuser path.
	// Unlike production public, a fresh named schema has no runtime USAGE.
	if _, err := connection.Exec(ctx, `GRANT USAGE ON SCHEMA `+identifier+` TO billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `ALTER TABLE schema_migrations OWNER TO billing_service`); err != nil {
		t.Fatal(err)
	}
	production, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	// Only rewrite this test's schema, preserving the production SET ROLE flow.
	// The generic bootstrap fixture's RESET ROLE would instead reset a
	// NOINHERIT migrator before its ledger append and is not production SQL.
	loaded[activationIndex].UpSQL = rewriteBillingPublicSchema(production[activationIndex].UpSQL, identifier)
	digest := sha256.Sum256([]byte(strings.ReplaceAll(loaded[activationIndex].UpSQL, "\r\n", "\n")))
	loaded[activationIndex].Checksum = hex.EncodeToString(digest[:])
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION billing_migrator; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	var sessionRole, currentRole string
	var superuser bool
	if err := connection.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&sessionRole, &currentRole, &superuser); err != nil || sessionRole != "billing_migrator" || currentRole != "billing_service" || superuser {
		t.Fatal("migration did not execute as the non-superuser deployment identity")
	}
	if err := migrations.Up(ctx, connection, loaded[:activationIndex+1]); err != nil {
		t.Fatalf("non-superuser activation migration: %v", err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}

	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	configuration.ConnConfig.RuntimeParams = map[string]string{"search_path": schema}
	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}

	snapshot, err := repository.ApprovalCommandActivations(ctx)
	if err != nil || snapshot.SchemaVersion != 1 || len(snapshot.Activations) != 11 {
		t.Fatalf("unexpected activation snapshot: %#v err=%v", snapshot, err)
	}
	active := map[string]bool{}
	for _, item := range snapshot.Activations {
		active[item.CommandType] = item.State == "ACTIVE"
	}
	for _, command := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME"} {
		if !active[command] {
			t.Fatalf("existing command %s did not remain active", command)
		}
	}
	if len(active) != 11 {
		t.Fatal("migration did not install exactly eleven commands")
	}
	activeCount := 0
	for _, enabled := range active {
		if enabled {
			activeCount++
		}
	}
	if activeCount != 3 {
		t.Fatal("a policy-only command was activated")
	}
	initialHistory := readBillingActivationHistory(t, ctx, pool)
	if !reflect.DeepEqual(snapshot.Activations, initialHistory) {
		t.Fatal("initial current projection and immutable history differ")
	}
	var legacyStatus string
	if err := connection.QueryRow(ctx, `SELECT status FROM wallets WHERE id=$1`, legacyWallet).Scan(&legacyStatus); err != nil || legacyStatus != "ACTIVE" {
		t.Fatal("additive migration broke the previous wallet reader")
	}
	t.Run("runtime read only", func(t *testing.T) {
		runtime, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close(context.Background())
		if _, err := runtime.Exec(ctx, `SET search_path TO `+identifier+`; SET SESSION AUTHORIZATION billing_service`); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := runtime.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations`).Scan(&count); err != nil || count != 11 {
			t.Fatal("runtime cannot read activation projection")
		}
		for _, statement := range []string{
			`UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1 WHERE command_type='WALLET_ADJUSTMENT'`,
			`DELETE FROM billing_approval_command_activations`,
			`INSERT INTO billing_approval_command_activations VALUES('FUTURE',1,1,'ACTIVE',1,now())`,
			`TRUNCATE billing_approval_command_activations`,
			`ALTER TABLE billing_approval_command_activations DISABLE TRIGGER USER`,
			`SET ROLE billing_schema_owner`,
			`SELECT * FROM billing_approval_command_activation_history`,
			`INSERT INTO billing_approval_command_activation_history VALUES('FUTURE',1,1,'ACTIVE',1,now())`,
			`UPDATE billing_approval_command_activation_history SET state='INACTIVE'`,
			`DELETE FROM billing_approval_command_activation_history`,
			`TRUNCATE billing_approval_command_activation_history`,
		} {
			_, err := runtime.Exec(ctx, statement)
			requireActivationSQLState(t, err, "42501")
		}
		if _, err := runtime.Exec(ctx, loaded[activationIndex].UpSQL); err == nil {
			t.Fatal("runtime session executed a deployment migration")
		} else {
			requireActivationSQLState(t, err, "P0001")
		}
	})
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET state='INACTIVE' WHERE command_type='WALLET_ADJUSTMENT'`); err == nil {
		t.Fatal("activation state changed without a version increment")
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET state='INACTIVE',version=version+1,updated_at='2000-01-01' WHERE command_type='WALLET_ADJUSTMENT'`); err != nil {
		t.Fatal(err)
	}
	var walletState string
	var walletVersion int64
	var walletUpdatedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT state,version,updated_at FROM billing_approval_command_activations WHERE command_type='WALLET_ADJUSTMENT'`).Scan(&walletState, &walletVersion, &walletUpdatedAt); err != nil || walletState != "INACTIVE" || walletVersion != 2 || walletUpdatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("guard did not assign trusted state/version/time: %s/%d/%s err=%v", walletState, walletVersion, walletUpdatedAt, err)
	}
	historyAfterChange := readBillingActivationHistory(t, ctx, pool)
	if len(historyAfterChange) != 12 || !reflect.DeepEqual(initialHistory, historyAfterChange[:11]) {
		t.Fatal("activation update did not preserve every previous revision")
	}
	walletRevision := historyAfterChange[11]
	if walletRevision.CommandType != "WALLET_ADJUSTMENT" || walletRevision.Version != 2 || walletRevision.State != walletState || !walletRevision.UpdatedAt.Equal(walletUpdatedAt) {
		t.Fatal("new immutable revision does not match current projection")
	}
	for _, statement := range []string{
		`TRUNCATE billing_approval_command_activations`,
		`UPDATE billing_approval_command_activation_history SET state='INACTIVE' WHERE command_type='WALLET_ADJUSTMENT' AND version=1`,
		`DELETE FROM billing_approval_command_activation_history WHERE command_type='WALLET_ADJUSTMENT' AND version=1`,
		`TRUNCATE billing_approval_command_activation_history`,
	} {
		_, err := pool.Exec(ctx, statement)
		requireActivationSQLState(t, err, "55000")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='WALLET_ADJUSTMENT'`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	var transactionalHistory int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activation_history`).Scan(&transactionalHistory); err != nil || transactionalHistory != 13 {
		_ = tx.Rollback(ctx)
		t.Fatal("activation change and history were not written in the same transaction")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(historyAfterChange, readBillingActivationHistory(t, ctx, pool)) {
		t.Fatal("rolled-back activation change left an orphan history revision")
	}
	beforeRepeat, err := repository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Re-entry may only replay migrations this fixture already applied; later
	// forward migrations are exercised deliberately below through the
	// production deployment identities.
	if err := migrations.Up(ctx, connection, loaded[:activationIndex+1]); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	afterRepeat, err := repository.ApprovalCommandActivations(ctx)
	if err != nil || !reflect.DeepEqual(beforeRepeat, afterRepeat) {
		t.Fatal("migration reentry reset a trusted deactivation or its revision")
	}
	if !reflect.DeepEqual(historyAfterChange, readBillingActivationHistory(t, ctx, pool)) {
		t.Fatal("migration reentry changed immutable revision history")
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET version=version+1 WHERE command_type='WALLET_ADJUSTMENT'`); err == nil {
		t.Fatal("unchanged activation advanced its revision")
	} else {
		requireActivationSQLState(t, err, "55000")
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+2 WHERE command_type='WALLET_ADJUSTMENT'`); err == nil {
		t.Fatal("activation skipped a revision")
	} else {
		requireActivationSQLState(t, err, "55000")
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET snapshot_schema_version=2 WHERE command_type='WALLET_ADJUSTMENT'`); err == nil {
		t.Fatal("activation identity was mutable")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM billing_approval_command_activations WHERE command_type='WALLET_ADJUSTMENT'`); err == nil {
		t.Fatal("activation history was deletable")
	}
	for _, tc := range []struct{ name, corrupt, restore string }{
		{"missing row", `DELETE FROM billing_approval_command_activations WHERE command_type='LEDGER_REPAIR'`, `INSERT INTO billing_approval_command_activations VALUES('LEDGER_REPAIR',1,1,'INACTIVE',1,now())`},
		{"unknown row", `INSERT INTO billing_approval_command_activations VALUES('FUTURE',1,1,'INACTIVE',1,now())`, `DELETE FROM billing_approval_command_activations WHERE command_type='FUTURE'`},
		{"retained incompatible policy", `INSERT INTO billing_approval_command_activations VALUES('PAYMENT_PROVIDER_RESUME',1,1,'ACTIVE',1,now())`, `DELETE FROM billing_approval_command_activations WHERE command_type='PAYMENT_PROVIDER_RESUME' AND policy_version=1`},
		{"mixed policy", `UPDATE billing_approval_command_activations SET policy_version=3 WHERE command_type='PAYMENT_PROVIDER_RESUME'`, `UPDATE billing_approval_command_activations SET policy_version=2 WHERE command_type='PAYMENT_PROVIDER_RESUME'`},
		{"mixed schema", `UPDATE billing_approval_command_activations SET snapshot_schema_version=2 WHERE command_type='LEDGER_REPAIR'`, `UPDATE billing_approval_command_activations SET snapshot_schema_version=1 WHERE command_type='LEDGER_REPAIR'`},
		{"policy-only active", `UPDATE billing_approval_command_activations SET state='ACTIVE' WHERE command_type='LEDGER_REPAIR'`, `UPDATE billing_approval_command_activations SET state='INACTIVE' WHERE command_type='LEDGER_REPAIR'`},
		{"future timestamp", `UPDATE billing_approval_command_activations SET updated_at=now()+interval '1 day' WHERE command_type='LEDGER_REPAIR'`, `UPDATE billing_approval_command_activations SET updated_at=now() WHERE command_type='LEDGER_REPAIR'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Corruption is injected only into this UUID-named isolated schema;
			// production runtime cannot disable this trigger (verified above).
			alter := func(statement string) {
				t.Helper()
				_, err := pool.Exec(ctx, `ALTER TABLE billing_approval_command_activations DISABLE TRIGGER USER; `+statement+`; ALTER TABLE billing_approval_command_activations ENABLE TRIGGER USER`)
				if err != nil {
					t.Fatal(err)
				}
			}
			alter(tc.corrupt)
			got, err := repository.ApprovalCommandActivations(ctx)
			if !errors.Is(err, ErrApprovalActivationUnavailable) || len(got.Activations) != 0 {
				t.Fatal("incompatible owner projection did not fail closed without partial data")
			}
			alter(tc.restore)
			if _, err := repository.ApprovalCommandActivations(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	beforeDown, err := repository.ApprovalCommandActivations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = migrations.DownOne(ctx, connection, loaded)
	migrations.Unlock(ctx, connection)
	requireActivationSQLState(t, err, "55000")
	afterDown, err := repository.ApprovalCommandActivations(ctx)
	if err != nil || !reflect.DeepEqual(beforeDown, afterDown) {
		t.Fatal("rollback removed activation history")
	}
	if !reflect.DeepEqual(historyAfterChange, readBillingActivationHistory(t, ctx, pool)) {
		t.Fatal("rollback changed immutable activation history")
	}
	if _, err := migrations.VerifyApplied(ctx, connection, loaded[:activationIndex+1]); err != nil {
		t.Fatal(err)
	}
	// 000057 is the real forward migration that appends the still-INACTIVE
	// policy v2 / snapshot schema v2 owner activation for the correction
	// command. Run it through the production deployment identities instead of
	// injecting the row by hand so its guards, the immutable history append and
	// the reader projection are exercised together.
	correctionIndex := -1
	for index := range loaded {
		if loaded[index].Version == 57 {
			correctionIndex = index
			break
		}
	}
	if correctionIndex < 0 {
		t.Fatal("billing correction activation migration is missing")
	}
	// The shared fixture helper appends a role reset for migrations 55-57 so the
	// ledger write runs as the owning session user. This schema deliberately
	// keeps the ledger owned by billing_service, so apply the production SQL for
	// this migration unchanged and let it end in its own runtime role.
	loaded[correctionIndex].UpSQL = rewriteBillingPublicSchema(production[correctionIndex].UpSQL, identifier)
	correctionChecksum := sha256.Sum256([]byte(strings.ReplaceAll(loaded[correctionIndex].UpSQL, "\r\n", "\n")))
	loaded[correctionIndex].Checksum = hex.EncodeToString(correctionChecksum[:])
	if _, err := connection.Exec(ctx, `SET SESSION AUTHORIZATION billing_migrator`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `SELECT set_config('search_path',$1,false)`, schema); err != nil {
		t.Fatal(err)
	}
	applied := append(append([]migrations.Migration{}, loaded[:activationIndex+1]...), loaded[correctionIndex])
	if err := migrations.Up(ctx, connection, applied); err != nil {
		t.Fatalf("forward correction activation migration: %v", err)
	}
	migrations.Unlock(ctx, connection)
	if _, err := connection.Exec(ctx, `RESET SESSION AUTHORIZATION; RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	versioned, err := repository.ApprovalCommandActivations(ctx)
	if err != nil || len(versioned.Activations) != 11 || versioned.Activations[1].CommandType != entitlementComponentAdjustmentCommand ||
		versioned.Activations[1].PolicyVersion != 2 || versioned.Activations[1].SnapshotSchemaVersion != 2 || versioned.Activations[1].State != "INACTIVE" {
		t.Fatalf("versioned inactive entitlement projection=%+v err=%v", versioned, err)
	}
	var retainedV1 int
	if err := connection.QueryRow(ctx, `SELECT count(*) FROM billing_approval_command_activations WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=1 AND snapshot_schema_version=1 AND state='INACTIVE'`).Scan(&retainedV1); err != nil || retainedV1 != 1 {
		t.Fatalf("immutable v1 entitlement history row was replaced: count=%d err=%v", retainedV1, err)
	}
	versionedActive := 0
	for _, item := range versioned.Activations {
		if item.State == "ACTIVE" {
			versionedActive++
		}
	}
	priorActive := 0
	for _, item := range beforeDown.Activations {
		if item.State == "ACTIVE" {
			priorActive++
		}
	}
	if versionedActive != priorActive {
		t.Fatal("the forward activation migration opened a command")
	}
	downErr := migrations.DownOne(ctx, connection, loaded)
	migrations.Unlock(ctx, connection)
	requireActivationSQLState(t, downErr, "55000")
	if _, err := migrations.VerifyApplied(ctx, connection, applied); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE billing_approval_command_activations SET state='ACTIVE',version=version+1 WHERE command_type='ENTITLEMENT_COMPONENT_ADJUSTMENT' AND policy_version=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ApprovalCommandActivations(ctx); !errors.Is(err, ErrApprovalActivationUnavailable) {
		t.Fatalf("policy-only v2 activation escaped: %v", err)
	}
}

func readBillingActivationHistory(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []ApprovalCommandActivation {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT command_type,policy_version,snapshot_schema_version,state,version,updated_at FROM billing_approval_command_activation_history ORDER BY command_type,policy_version,version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var history []ApprovalCommandActivation
	for rows.Next() {
		var item ApprovalCommandActivation
		if err := rows.Scan(&item.CommandType, &item.PolicyVersion, &item.SnapshotSchemaVersion, &item.State, &item.Version, &item.UpdatedAt); err != nil {
			t.Fatal(err)
		}
		history = append(history, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return history
}

func requireActivationSQLState(t *testing.T, err error, expected string) {
	t.Helper()
	var pgError *pgconn.PgError
	if !errors.As(err, &pgError) || pgError.Code != expected {
		t.Fatalf("expected SQLSTATE %s, got %v", expected, err)
	}
}
