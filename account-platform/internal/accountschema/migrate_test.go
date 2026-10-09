package accountschema

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestOrderedPreservesOriginalsAndPlacesMetadataBeforeScope(t *testing.T) {
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var seed migrations.Migration
	for _, migration := range installed {
		if migration.Version == metadataVersion {
			seed = migration
		}
	}
	input := []migrations.Migration{{Version: 86}, {Version: 34}, seed, {Version: 2}, {Version: 27}}
	ordered, err := Ordered(input)
	if err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, migration := range ordered {
		versions = append(versions, migration.Version)
	}
	if !slices.Equal(versions, []int64{2, 27, metadataVersion, 34, 86}) {
		t.Fatalf("incorrect migration order: %v", versions)
	}
	if input[0].Version != 86 {
		t.Fatal("ordering changed the caller's migration list")
	}
	if _, err := Ordered(input[0:2]); err == nil {
		t.Fatal("missing metadata migration accepted")
	}
	if _, err := Ordered(append(input, migrations.Migration{Version: metadataVersion})); err == nil {
		t.Fatal("duplicate metadata migration accepted")
	}
	seed.UpSQL += "\nSELECT 1;\n"
	if _, err := Ordered([]migrations.Migration{seed}); err == nil {
		t.Fatal("modified SQL with a forged original checksum accepted")
	}
}

func TestInstalledPersonalAccountSchemaPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	connection, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(context.Background())
	schema := "account_schema_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := connection.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Errorf("cleanup own schema: %v", err)
		}
	}()
	if _, err := connection.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := Up(ctx, connection, installed); err != nil {
		t.Fatal(err)
	}
	verification, err := migrations.VerifyApplied(ctx, connection, installed)
	if err != nil || verification.AppliedCount != len(installed) || verification.LatestVersion != metadataVersion {
		t.Fatalf("installed migration verification: %+v err=%v", verification, err)
	}
	t.Run("mature-personal-and-sms-authority", func(t *testing.T) {
		var scope, functionName string
		if err := connection.QueryRow(ctx, `SELECT scope,to_regprocedure('increment_authorization_scope_version(uuid,text,uuid)')::text FROM roles WHERE code='PERSONAL_USER'`).Scan(&scope, &functionName); err != nil || scope != "PERSONAL" || functionName == "" {
			t.Fatalf("personal authority not installed: scope=%q helper=%q err=%v", scope, functionName, err)
		}
		var smsTable string
		if err := connection.QueryRow(ctx, `SELECT to_regclass('sms_configuration_versions')::text`).Scan(&smsTable); err != nil || smsTable == "" {
			t.Fatalf("versioned SMS configuration not installed: %q err=%v", smsTable, err)
		}
	})
	t.Run("repeat-preserves-ledger-and-role-edges", func(t *testing.T) {
		var before string
		if err := connection.QueryRow(ctx, `SELECT md5(string_agg(role_code||':'||permission_code,',' ORDER BY role_code,permission_code)) FROM role_permissions`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := Up(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
		var after string
		if err := connection.QueryRow(ctx, `SELECT md5(string_agg(role_code||':'||permission_code,',' ORDER BY role_code,permission_code)) FROM role_permissions`).Scan(&after); err != nil || before != after {
			t.Fatalf("repeat changed role edges: err=%v", err)
		}
		if _, err := migrations.VerifyApplied(ctx, connection, installed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("forward-only-rollback-is-atomic", func(t *testing.T) {
		if err := migrations.DownOne(ctx, connection, installed); err == nil {
			t.Fatal("first-delivery metadata rollback unexpectedly succeeded")
		}
		if _, err := migrations.VerifyApplied(ctx, connection, installed); err != nil {
			t.Fatalf("failed rollback changed the migration ledger: %v", err)
		}
	})
}
