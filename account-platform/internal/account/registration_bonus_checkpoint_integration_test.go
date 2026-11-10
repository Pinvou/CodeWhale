package account

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The first page commits through the real public schema and account_service.
// A fixture trigger interrupts the next page, without guessing a timing window.
func registrationBonusCheckpoint(t *testing.T) (*PostgresRepository, time.Time) {
	t.Helper()
	r := retainedWalletFenceActors(t).repository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	r.now = func() time.Time { return now }
	if err := r.ConfigureRegistrationBonusFingerprint("checkpoint-key", bytes.Repeat([]byte{0x51}, 32)); err != nil {
		t.Fatal(err)
	}
	_, err := r.pool.Exec(ctx, `WITH capability AS (SELECT set_config('pinvou.registration_bonus_capable','1',true))
INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at)
SELECT gen_random_uuid(),generate_unique_user_code(),'ACTIVE','backfill_'||g,'Backfill fixture',
       '+86139'||lpad(g::text,8,'0'),$1,$1,$1
FROM generate_series(1,1001) g CROSS JOIN capability`, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.pool.Exec(ctx, `CREATE FUNCTION checkpoint_test_interrupt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS(SELECT 1 FROM users WHERE id=NEW.first_user_id AND mobile_e164='+8613900001001') THEN
        RAISE EXCEPTION 'controlled checkpoint interruption';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER checkpoint_test_interrupt BEFORE INSERT ON registration_bonus_mobile_eligibility
FOR EACH ROW EXECUTE FUNCTION checkpoint_test_interrupt()`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.PrepareRegistrationBonusEligibility(ctx); err == nil || !strings.Contains(err.Error(), "controlled checkpoint interruption") {
		t.Fatalf("second page did not reach the controlled interruption: %v", err)
	}
	if _, err = r.pool.Exec(ctx, `DROP TRIGGER checkpoint_test_interrupt ON registration_bonus_mobile_eligibility; DROP FUNCTION checkpoint_test_interrupt()`); err != nil {
		t.Fatal(err)
	}
	var status, keyID, mobile string
	var count, facts int64
	var verifier []byte
	var snapshot time.Time
	if err = r.pool.QueryRow(ctx, `SELECT status,fingerprint_key_id,fingerprint_key_verifier,recoverable_registration_count,
backfill_cursor_mobile_e164,backfill_snapshot_at,(SELECT count(*) FROM registration_bonus_mobile_eligibility)
FROM registration_bonus_eligibility_state WHERE singleton`).Scan(&status, &keyID, &verifier, &count, &mobile, &snapshot, &facts); err != nil {
		t.Fatal(err)
	}
	wantVerifier, err := r.registrationBonusFingerprintKeyVerifier()
	if err != nil || status != "PENDING" || keyID != "checkpoint-key" || !bytes.Equal(verifier, wantVerifier) || count != 1000 || facts != 1000 || mobile != "+8613900001000" || !snapshot.Equal(now) {
		t.Fatalf("first committed page did not retain its exact key/cursor/snapshot: status=%s count=%d facts=%d err=%v", status, count, facts, err)
	}
	return r, now
}

func registrationBonusCheckpointSnapshot(t *testing.T, r *PostgresRepository) string {
	t.Helper()
	var snapshot string
	if err := r.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
'state',(SELECT to_jsonb(s) FROM registration_bonus_eligibility_state s WHERE singleton),
'facts',(SELECT COALESCE(jsonb_agg(to_jsonb(f) ORDER BY encode(mobile_fingerprint,'hex')),'[]'::jsonb) FROM registration_bonus_mobile_eligibility f))::text`).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestRegistrationBonusEligibilityCheckpointPostgresIntegration(t *testing.T) {
	r, snapshotAt := registrationBonusCheckpoint(t)
	ctx := context.Background()
	before := registrationBonusCheckpointSnapshot(t, r)
	for _, keyID := range []string{"different-key", "checkpoint-key"} {
		if err := r.ConfigureRegistrationBonusFingerprint(keyID, bytes.Repeat([]byte{0x52}, 32)); err != nil {
			t.Fatal(err)
		}
		if _, err := r.PrepareRegistrationBonusEligibility(ctx); err == nil || !strings.Contains(err.Error(), "fingerprint key differs") {
			t.Fatalf("PENDING resume accepted changed key identity/material: %v", err)
		}
		if registrationBonusCheckpointSnapshot(t, r) != before {
			t.Fatal("rejected resume changed the committed checkpoint or permanent facts")
		}
	}
	// The ordinary registration writer must also verify a bound PENDING key,
	// including rotation under the same ID; it must not add mixed-key facts.
	opID := registrationBonusTestOperation(t, ctx, r, "wrong_pending_key", "+8613600000001")
	if _, err := r.pool.Exec(ctx, `UPDATE registration_operations SET status='IDENTITY_CREATED',identity_subject='checkpoint-wrong-key',identity_created_at=now(),updated_at=now() WHERE id=$1`, opID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CommitRegistration(ctx, opID, "https://identity.registration-bonus.test"); !errors.Is(err, ErrRegistrationConflict) {
		t.Fatalf("registration with changed PENDING key was not rejected: %v", err)
	}
	if registrationBonusCheckpointSnapshot(t, r) != before {
		t.Fatal("rejected registration changed eligibility state/facts")
	}
	if err := r.ConfigureRegistrationBonusFingerprint("checkpoint-key", bytes.Repeat([]byte{0x51}, 32)); err != nil {
		t.Fatal(err)
	}
	// Facts appearing after the frozen snapshot must not change its historical
	// counts, including anonymized registrations which previously flipped READY.
	later := snapshotAt.Add(24 * time.Hour)
	r.now = func() time.Time { return later }
	newUser := commitRegistrationBonusTestOperation(t, ctx, r, "after_checkpoint", "+8613600000002")
	assertRegistrationBonusJobMode(t, ctx, r, newUser, "NOT_APPLICABLE")
	legacyOp := registrationBonusTestOperation(t, ctx, r, "later_anonymized", "+8613600000003")
	legacyUser := uuid.New()
	if _, err := r.pool.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,created_at,updated_at)
VALUES($1::uuid,generate_unique_user_code(),'DELETED','later_deleted','Later deleted','closed:'||$1::uuid::text,$2,$2)`, legacyUser, later); err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `UPDATE registration_operations SET status='ACTIVE',identity_subject='later-deleted',committed_user_id=$2::uuid,mobile_e164='closed:'||$2::uuid::text,account_committed_at=$3,updated_at=$3 WHERE id=$1`, legacyOp, legacyUser, later); err != nil {
		t.Fatal(err)
	}
	// Two independently configured repository instances resume the same page.
	pool, err := pgxpool.NewWithConfig(ctx, r.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	other := &PostgresRepository{pool: pool, now: r.now}
	if err := other.ConfigureRegistrationBonusFingerprint("checkpoint-key", bytes.Repeat([]byte{0x51}, 32)); err != nil {
		t.Fatal(err)
	}
	type result struct {
		readiness RegistrationBonusEligibilityReadiness
		err       error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for _, instance := range []*PostgresRepository{r, other} {
		go func(instance *PostgresRepository) {
			<-start
			state, err := instance.PrepareRegistrationBonusEligibility(ctx)
			results <- result{state, err}
		}(instance)
	}
	close(start)
	for range 2 {
		got := <-results
		if got.err != nil || got.readiness.Status != "READY" || got.readiness.RecoverableRegistrationCount != 1001 || got.readiness.UnrecoverableRegistrationCount != 0 {
			t.Errorf("concurrent resume lost the original snapshot: state=%+v err=%v", got.readiness, got.err)
		}
	}
	var historical, current int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE source='HISTORICAL_BACKFILL'),count(*) FILTER(WHERE source='REGISTRATION') FROM registration_bonus_mobile_eligibility`).Scan(&historical, &current); err != nil || historical != 1001 || current != 1 {
		t.Fatalf("resume lost/duplicated permanent facts: historical=%d registration=%d err=%v", historical, current, err)
	}
	after := registrationBonusCheckpointSnapshot(t, r)
	if _, err := r.PrepareRegistrationBonusEligibility(ctx); err != nil || registrationBonusCheckpointSnapshot(t, r) != after {
		t.Fatalf("completed replay changed permanent evidence: %v", err)
	}
}

func TestRegistrationBonusCheckpointForeignFactsPostgresIntegration(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound-existing-facts", true: "foreign-key-before-finalize"}[bound], func(t *testing.T) {
			var r *PostgresRepository
			if bound {
				r, _ = registrationBonusCheckpoint(t)
			} else {
				r = retainedWalletFenceActors(t).repository(t)
				if err := r.ConfigureRegistrationBonusFingerprint("checkpoint-key", bytes.Repeat([]byte{0x51}, 32)); err != nil {
					t.Fatal(err)
				}
			}
			ctx := context.Background()
			user := uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,created_at,updated_at) VALUES($1,generate_unique_user_code(),'ACTIVE','foreign_fact','Foreign fixture',now(),now())`, user); err != nil {
				t.Fatal(err)
			}
			// Controlled inconsistent evidence, not a claimed normal writer path.
			if _, err := r.pool.Exec(ctx, `INSERT INTO registration_bonus_mobile_eligibility(mobile_fingerprint,fingerprint_key_id,first_user_id,source,created_at) VALUES(decode(repeat('ab',32),'hex'),'unknown-key',$1,'HISTORICAL_BACKFILL',now())`, user); err != nil {
				t.Fatal(err)
			}
			before := registrationBonusCheckpointSnapshot(t, r)
			if _, err := r.PrepareRegistrationBonusEligibility(ctx); err == nil || !strings.Contains(err.Error(), "fingerprint key") {
				t.Fatalf("unknown/mixed-key facts were silently adopted: %v", err)
			}
			if registrationBonusCheckpointSnapshot(t, r) != before {
				t.Fatal("rejected inconsistent evidence changed state/facts")
			}
		})
	}
}

func TestRegistrationBonusCheckpointGuardsPostgresIntegration(t *testing.T) {
	r, _ := registrationBonusCheckpoint(t)
	ctx := context.Background()
	before := registrationBonusCheckpointSnapshot(t, r)
	for _, mutation := range []map[string]string{
		{"fingerprint_key_id": `'changed'`},
		{"fingerprint_key_verifier": `decode(repeat('00',32),'hex')`},
		{"fingerprint_key_id": `NULL`, "fingerprint_key_verifier": `NULL`},
		{"backfill_snapshot_at": `NULL`},
		{"backfill_snapshot_at": `backfill_snapshot_at+interval '1 second'`},
		{"backfill_cursor_mobile_e164": `'+8613900000001'`},
		{"backfill_cursor_user_id": `NULL`},
	} {
		// Otherwise this is a valid advancing checkpoint: an unchanged cursor
		// must not mask a missing key-identity or frozen-snapshot guard.
		assignments := map[string]string{
			"recoverable_registration_count": "recoverable_registration_count+1",
			"backfill_cursor_mobile_e164":    "'+8613900001001'",
		}
		for name, value := range mutation {
			assignments[name] = value
		}
		var updates []string
		for name, value := range assignments {
			updates = append(updates, name+"="+value)
		}
		sort.Strings(updates)
		if _, err := r.pool.Exec(ctx, `UPDATE registration_bonus_eligibility_state SET `+strings.Join(updates, ",")+` WHERE singleton`); err == nil {
			t.Fatal("checkpoint mutation was accepted: ", mutation)
		}
		if registrationBonusCheckpointSnapshot(t, r) != before {
			t.Fatal("rejected checkpoint mutation changed state/facts")
		}
	}
	connection := registrationBonusCheckpointMigrator(t, r)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	// The current chain refuses at its new forward-only head. Exercise the
	// original checkpoint guard independently so that refusal cannot mask it.
	assertHistoricalRollbackRefusal(t, r, loaded, 900014, "P0001", "registration bonus eligibility facts exist; destructive rollback is forbidden")
	if err = accountschema.DownOne(ctx, connection, loaded); err == nil || !strings.Contains(err.Error(), "organization evidence truncate protection is forward-only") {
		t.Fatalf("public down discarded a committed checkpoint: %v", err)
	}
	if registrationBonusCheckpointSnapshot(t, r) != before {
		t.Fatal("rejected rollback changed checkpoint or facts")
	}
	if _, err := r.PrepareRegistrationBonusEligibility(ctx); err != nil {
		t.Fatal("valid continuation failed after rejected mutations: ", err)
	}
}

func registrationBonusCheckpointMigrator(t *testing.T, r *PostgresRepository) *pgx.Conn {
	t.Helper()
	config := r.pool.Config().ConnConfig.Copy()
	config.User, config.Password = "account_migrator", ""
	connection, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close(context.Background()) })
	if _, err = connection.Exec(context.Background(), `SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestRegistrationBonusCheckpointMigrationPostgresIntegration(t *testing.T) {
	f := retainedWalletFenceActors(t)
	// Test the actual checkpoint release, before later forward-only heads.
	r := f.repositoryThroughVersion(t, 900014, func(ctx context.Context, connection *pgx.Conn, input []migrations.Migration) error {
		var previous []migrations.Migration
		for _, migration := range input {
			if migration.Version != 900014 {
				previous = append(previous, migration)
			}
		}
		if err := accountschema.Up(ctx, connection, previous); err != nil {
			return err
		}
		var checksum string
		if err := connection.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=87`).Scan(&checksum); err != nil {
			return err
		}
		if checksum != "1d76a6e9aa6b5fcee84e2eabe8912cbfb8fa53c79ba29ce2bb4c29852be070e1" {
			t.Fatal("published migration87 identity changed")
		}
		return nil
	})
	ctx := context.Background()
	connection := registrationBonusCheckpointMigrator(t, r)
	input, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var history []migrations.Migration
	for _, migration := range input {
		if migration.Version <= 900014 {
			history = append(history, migration)
		}
	}
	input = history
	if err := accountschema.DownOne(ctx, connection, input); err != nil {
		t.Fatal("empty unbound checkpoint rollback failed: ", err)
	}
	var progressAbsent bool
	if err := connection.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900014) AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name='registration_bonus_eligibility_state' AND column_name='backfill_snapshot_at')`).Scan(&progressAbsent); err != nil || !progressAbsent {
		t.Fatalf("empty rollback did not restore the real predecessor: %v", err)
	}
	if err := accountschema.Up(ctx, connection, input); err != nil {
		t.Fatal("public re-up after allowed rollback failed: ", err)
	}
	if err := r.ConfigureRegistrationBonusFingerprint("checkpoint-key", bytes.Repeat([]byte{0x51}, 32)); err != nil {
		t.Fatal(err)
	}
	if state, err := r.PrepareRegistrationBonusEligibility(ctx); err != nil || state.Status != "READY" {
		t.Fatalf("empty upgraded backfill failed: state=%+v err=%v", state, err)
	}
	if err := accountschema.DownOne(ctx, connection, input); err == nil || !strings.Contains(err.Error(), "destructive rollback is forbidden") {
		t.Fatalf("public down discarded a completed key binding: %v", err)
	}
}
