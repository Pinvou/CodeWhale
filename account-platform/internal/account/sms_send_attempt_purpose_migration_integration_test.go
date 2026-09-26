package account

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestSMSSendAttemptPurposeMigrationPostgresIntegration(t *testing.T) {
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	repository, connection, _, applied := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL, 77)
	loaded, err := loadAccountIntegrationMigrations(t)
	if err != nil {
		t.Fatal(err)
	}
	if latest := loaded[len(loaded)-1]; latest.Version != 84 {
		t.Fatalf("latest account migration=%d, want 84", latest.Version)
	}
	if latest := applied[len(applied)-1]; latest.Version != 77 {
		t.Fatalf("initial account migration=%d, want 77", latest.Version)
	}

	blockerConfiguration, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	blockerConfiguration.Database = connection.Config().Database
	blocker, err := pgx.ConnectConfig(ctx, blockerConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close(context.Background()) })

	lockTable := func() {
		t.Helper()
		if _, lockErr := blocker.Exec(ctx, `BEGIN; LOCK TABLE sms_send_attempts IN ACCESS EXCLUSIVE MODE`); lockErr != nil {
			t.Fatal(lockErr)
		}
	}
	releaseTable := func() {
		t.Helper()
		if _, rollbackErr := blocker.Exec(ctx, `ROLLBACK`); rollbackErr != nil {
			t.Fatal(rollbackErr)
		}
	}
	assertLatestVersion := func(want int64) {
		t.Helper()
		var got int64
		if queryErr := connection.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&got); queryErr != nil {
			t.Fatal(queryErr)
		}
		if got != want {
			t.Fatalf("latest applied migration=%d, want %d", got, want)
		}
	}
	constraintState := func(name string) (exists, validated bool) {
		t.Helper()
		queryErr := connection.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
WHERE conrelid='sms_send_attempts'::regclass AND conname=$1`, name).Scan(&validated)
		if queryErr == nil {
			return true, validated
		}
		if queryErr == pgx.ErrNoRows {
			return false, false
		}
		t.Fatal(queryErr)
		return false, false
	}

	lockTable()
	started := time.Now()
	err = migrations.Up(ctx, connection, loaded)
	migrations.Unlock(ctx, connection)
	if postgresSQLState(err) != "55P03" {
		t.Fatalf("migration 78 lock timeout SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("migration 78 lock wait=%s, want bounded wait", elapsed)
	}
	assertLatestVersion(77)
	if exists, _ := constraintState("sms_send_attempts_purpose_v2_check"); exists {
		t.Fatal("migration 78 lock timeout left the v2 constraint behind")
	}
	releaseTable()

	through79 := make([]migrations.Migration, 0, len(loaded))
	for _, migration := range loaded {
		if migration.Version <= 79 {
			through79 = append(through79, migration)
		}
	}
	if err = migrations.Up(ctx, connection, through79); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	assertLatestVersion(79)
	if exists, validated := constraintState("sms_send_attempts_purpose_check"); !exists || !validated {
		t.Fatalf("migration 79 old constraint exists=%t validated=%t", exists, validated)
	}
	if exists, validated := constraintState("sms_send_attempts_purpose_v2_check"); !exists || !validated {
		t.Fatalf("migration 79 v2 constraint exists=%t validated=%t", exists, validated)
	}

	lockTable()
	started = time.Now()
	err = migrations.Up(ctx, connection, loaded)
	migrations.Unlock(ctx, connection)
	if postgresSQLState(err) != "55P03" {
		t.Fatalf("migration 80 lock timeout SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("migration 80 lock wait=%s, want bounded wait", elapsed)
	}
	assertLatestVersion(79)
	if exists, validated := constraintState("sms_send_attempts_purpose_check"); !exists || !validated {
		t.Fatalf("migration 80 lock timeout changed old constraint: exists=%t validated=%t", exists, validated)
	}
	if exists, validated := constraintState("sms_send_attempts_purpose_v2_check"); !exists || !validated {
		t.Fatalf("migration 80 lock timeout changed v2 constraint: exists=%t validated=%t", exists, validated)
	}
	releaseTable()

	through80 := make([]migrations.Migration, 0, len(loaded))
	for _, migration := range loaded {
		if migration.Version <= 80 {
			through80 = append(through80, migration)
		}
	}
	if err = migrations.Up(ctx, connection, through80); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	assertLatestVersion(80)

	var validated bool
	if err := repository.pool.QueryRow(ctx, `SELECT convalidated FROM pg_constraint
WHERE conrelid='sms_send_attempts'::regclass AND conname='sms_send_attempts_purpose_check'`).Scan(&validated); err != nil || !validated {
		t.Fatalf("active SMS purpose constraint validated=%t err=%v", validated, err)
	}
	for _, purpose := range []string{"REGISTER", "LOGIN", "PROFILE_MOBILE", "PASSWORD_RESET"} {
		if _, err := repository.pool.Exec(ctx, `INSERT INTO sms_send_attempts
(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,result,occurred_at,provider_id,adapter_version)
VALUES($1,NULL,$2,$2,$2,$3,'ACCEPTED',now(),'integration','v1')`, uuid.New(), make([]byte, 32), purpose); err != nil {
			t.Fatalf("insert SMS purpose %s: %v", purpose, err)
		}
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO sms_send_attempts
(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,result,occurred_at,provider_id,adapter_version)
VALUES($1,NULL,$2,$2,$2,'UNKNOWN','ACCEPTED',now(),'integration','v1')`, uuid.New(), make([]byte, 32)); postgresSQLState(err) != "23514" {
		t.Fatalf("invalid SMS purpose SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	if err := migrations.DownOne(ctx, connection, through80); postgresSQLState(err) != "55000" {
		t.Fatalf("SMS purpose migration down SQLSTATE=%s err=%v", postgresSQLState(err), err)
	}
	migrations.Unlock(ctx, connection)
}
