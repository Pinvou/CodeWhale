package account

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestProviderQuotaConcurrentBoundary(t *testing.T) {
	f := retainedWalletFenceActors(t)
	t.Run("migration-round-trip", func(t *testing.T) {
		ctx := f.ctx
		repo := f.repositoryThroughVersion(t, 900015, nil)
		migrator := registrationBonusCheckpointMigrator(t, repo)
		for _, table := range []string{"sms_send_attempts", "sms_verification_attempts"} {
			if _, err := repo.pool.Exec(ctx, `INSERT INTO `+table+`(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at)
SELECT gen_random_uuid(),decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),
'REGISTER','fixture-provider',result,now()-interval '1 hour' FROM (VALUES ('ACCEPTED'),('RATE_LIMITED')) seed(result)`); err != nil {
				t.Fatal("retained quota evidence seed failed", err)
			}
		}
		snapshot := func() string {
			t.Helper()
			var facts string
			err := repo.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'send',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM sms_send_attempts s),
 'verify',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id) FROM sms_verification_attempts v),
 'previous_ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m WHERE version<>900017))::text`).Scan(&facts)
			if err != nil {
				t.Fatal("retained quota evidence read failed", err)
			}
			return facts
		}
		before := snapshot()
		loaded, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		var throughUpgrade []migrations.Migration
		for _, migration := range loaded {
			if migration.Version <= 900015 || migration.Version == 900017 {
				throughUpgrade = append(throughUpgrade, migration)
			}
		}
		for _, phase := range []struct {
			name  string
			apply func(context.Context, *pgx.Conn, []migrations.Migration) error
			want  int
		}{{"install", accountschema.Up, 2}, {"rollback", accountschema.DownOne, 0}, {"reinstall", accountschema.Up, 2}} {
			if err := phase.apply(ctx, migrator, throughUpgrade); err != nil {
				t.Fatalf("%s quota index migration: %v", phase.name, err)
			}
			var actual int
			var applied bool
			err := repo.pool.QueryRow(ctx, `SELECT count(*),EXISTS(SELECT 1 FROM schema_migrations WHERE version=900017)
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
WHERE c.relname IN ('sms_send_attempts_accepted_provider_window_idx','sms_verification_attempts_accepted_provider_window_idx')
  AND i.indisvalid AND i.indisready AND pg_get_indexdef(i.indexrelid) LIKE '%(provider_id, occurred_at DESC) WHERE (result = ''ACCEPTED''::text)'`).Scan(&actual, &applied)
			if err != nil || actual != phase.want || applied != (phase.want != 0) || snapshot() != before {
				t.Fatalf("%s did not preserve facts and the expected valid indexes: indexes=%d applied=%t error=%v", phase.name, actual, applied, err)
			}
		}
		for _, migration := range loaded {
			if migration.Version == 900018 {
				throughUpgrade = append(throughUpgrade, migration)
			}
		}
		if err := accountschema.Up(ctx, migrator, throughUpgrade); err != nil {
			t.Fatal("install snapshot fence on the upgraded historical ledger", err)
		}
		err = accountschema.DownOne(ctx, migrator, throughUpgrade)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "55000" || rejected.Message != "SMS send provider snapshot fence is forward-only" {
			t.Fatal("public rollback unexpectedly removed the provider snapshot fence", err)
		}
		if _, err := accountschema.VerifyApplied(ctx, migrator, throughUpgrade); err != nil {
			t.Fatal("forward-only rejection altered the migration ledger", err)
		}
	})
	for _, kind := range []string{"send", "verify"} {
		t.Run(kind, func(t *testing.T) {
			ctx := f.ctx
			repo := f.repository(t)
			// Only the fixture coordinator installs the fault gate. Admission
			// uses the restricted account_service pool from the public fixture.
			config := f.config.Copy()
			config.Database = repo.pool.Config().ConnConfig.Database
			c, err := pgx.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal("owned database coordinator connection failed")
			}
			defer c.Close(context.Background())
			now := time.Now().UTC().Truncate(time.Second)
			repo.now = func() time.Time { return now }
			provider := "independent-fixture-provider"
			table := "sms_send_attempts"
			limit := 100
			if kind == "verify" {
				table = "sms_verification_attempts"
				limit = 300
			}
			seed := `INSERT INTO ` + table + `(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at)
SELECT gen_random_uuid(),decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),
    CASE WHEN i%2=0 THEN 'REGISTER' ELSE 'PROFILE_MOBILE' END,
    CASE WHEN i=$1+1 THEN 'other-provider' ELSE $2 END,
    CASE WHEN i=$1+2 THEN 'RATE_LIMITED' ELSE 'ACCEPTED' END,
    CASE WHEN i=$1 THEN $3::timestamptz-interval '25 hours' ELSE $3::timestamptz END
FROM generate_series(1,$1+2) i`
			if _, err = c.Exec(ctx, seed, limit, provider, now); err != nil {
				t.Fatal("owned boundary seed failed", err)
			}
			var random [8]byte
			if _, err = rand.Read(random[:]); err != nil {
				t.Fatal(err)
			}
			gate := int64(binary.BigEndian.Uint64(random[:]) & 0x7fffffffffffffff)
			if _, err = c.Exec(ctx, `SELECT pg_advisory_lock($1)`, gate); err != nil {
				t.Fatal("owned gate acquire failed")
			}
			unlocked := false
			defer func() {
				if !unlocked {
					_, _ = c.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, gate)
				}
			}()
			function := fmt.Sprintf(`CREATE FUNCTION independent_insert_gate() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.result='ACCEPTED' THEN PERFORM pg_advisory_xact_lock_shared(%d::bigint); END IF; RETURN NEW; END $$`, gate)
			if _, err = c.Exec(ctx, function); err != nil {
				t.Fatal("owned gate function failed")
			}
			if _, err = c.Exec(ctx, `CREATE TRIGGER independent_insert_gate BEFORE INSERT ON `+table+` FOR EACH ROW EXECUTE FUNCTION independent_insert_gate()`); err != nil {
				t.Fatal("owned gate trigger failed")
			}
			result := make(chan error, 2)
			for i := 0; i < 2; i++ {
				id := uuid.New()
				mobile := fmt.Sprintf("+86138001380%02d", i)
				source := fmt.Sprintf("ind-source-%d", i)
				device := fmt.Sprintf("ind-device-%d", i)
				if kind == "verify" {
					if _, err = c.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,expires_at,provider_id,adapter_version,created_at,updated_at) VALUES($1,$2,'LOGIN','SENT',$3,$4,'v1',$5,$5)`, id, mobile, now.Add(time.Minute), provider, now); err != nil {
						t.Fatal("owned verification challenge failed")
					}
				}
				go func(id uuid.UUID, mobile, source, device string) {
					if kind == "send" {
						result <- repo.CreateSMSChallenge(ctx, SMSChallenge{ID: id, Mobile: mobile, Purpose: SMSPurposeLogin, Source: source, Device: device, ExpiresAt: now.Add(time.Minute), Provider: SMSProviderDescriptor{ID: provider, AdapterVersion: "v1"}})
					} else {
						result <- repo.AdmitSMSVerification(ctx, id, mobile, SMSPurposeLogin, source, device)
					}
				}(id, mobile, source, device)
			}
			deadline := time.Now().Add(10 * time.Second)
			waiting := 0
			for time.Now().Before(deadline) {
				if err = c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event='advisory'`, config.Database).Scan(&waiting); err != nil {
					t.Fatal("owned wait inspection failed")
				}
				if waiting == 2 {
					break
				}
				time.Sleep(15 * time.Millisecond)
			}
			if waiting != 2 {
				t.Fatal("both actual repository calls did not reach controlled concurrent boundary")
			}
			if _, err = c.Exec(ctx, `SELECT pg_advisory_unlock($1)`, gate); err != nil {
				t.Fatal("owned gate release failed")
			}
			unlocked = true
			accepted, limited := 0, 0
			for i := 0; i < 2; i++ {
				e := <-result
				var limitError *SMSRateLimitError
				if e == nil {
					accepted++
				} else if errors.As(e, &limitError) && limitError.RetryAfter == time.Minute {
					limited++
				} else {
					t.Fatal("unexpected actual repository outcome")
				}
			}
			var total int
			if err = c.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE provider_id=$1 AND result='ACCEPTED' AND occurred_at>$2::timestamptz-interval '1 minute'`, provider, now).Scan(&total); err != nil {
				t.Fatal("read actual admission facts failed")
			}
			if accepted != 1 || limited != 1 || total != limit {
				t.Fatalf("PROVIDER_QUOTA_OVERSHOOT accepted=%d limited=%d total=%d limit=%d", accepted, limited, total, limit)
			}
		})
	}
	for _, kind := range []string{"registration", "profile"} {
		t.Run("login-"+kind, func(t *testing.T) { testMixedSMSSendAdmission(t, f, kind) })
	}
}

// LOGIN holds the provider gate in a Read Committed transaction. The other
// entry has already fixed a Serializable snapshot when it reaches that gate.
func testMixedSMSSendAdmission(t *testing.T, f *retainedWalletFenceFixture, kind string) {
	t.Helper()
	repo := f.repository(t)
	ctx, cancel := context.WithTimeout(f.ctx, 25*time.Second)
	defer cancel()
	config := f.config.Copy()
	config.Database = repo.pool.Config().ConnConfig.Database
	control, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close(context.Background())
	now := time.Now().UTC().Truncate(time.Second)
	repo.now = func() time.Time { return now }
	provider := SMSProviderDescriptor{ID: "fixture-mixed-provider", AdapterVersion: "v1"}
	if _, err := repo.pool.Exec(ctx, `INSERT INTO sms_send_attempts(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at)
SELECT gen_random_uuid(),decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),
'LOGIN',$1,'ACCEPTED',$2 FROM generate_series(1,99)`, provider.ID, now); err != nil {
		t.Fatal(err)
	}
	login := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138010", Purpose: SMSPurposeLogin, ExpiresAt: now.Add(5 * time.Minute), Source: "fixture-login-source", Device: "fixture-login-device", Provider: provider}
	other := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138011", Purpose: SMSPurposeRegister, ExpiresAt: login.ExpiresAt, Source: "fixture-other-source", Device: "fixture-other-device", Provider: provider}
	var runOther func() error
	var unchanged func() bool
	if kind == "registration" {
		op := RegistrationOperation{ID: uuid.New(), CandidateUserID: uuid.New(), QueryTokenHash: bytes.Repeat([]byte{1}, 32), RequestFingerprint: bytes.Repeat([]byte{2}, 32), Username: "mixedfixture", Mobile: other.Mobile, ChallengeID: other.ID, PasswordPolicyVersion: "user-v1"}
		runOther = func() error { return repo.CreateRegistrationOperation(ctx, op, other) }
		unchanged = func() bool {
			var absent bool
			err := repo.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM registration_operations WHERE id=$1)`, op.ID).Scan(&absent)
			return err == nil && absent
		}
	} else {
		other.Purpose = SMSPurposeProfileMobile
		principal := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "mixedprofile"}
		if _, _, err := repo.Bootstrap(ctx, principal); err != nil {
			t.Fatal(err)
		}
		change, err := repo.CreateProfileChange(ctx, principal, "MOBILE", other.Mobile)
		if err != nil {
			t.Fatal(err)
		}
		captcha := SliderCaptcha{ID: uuid.New(), Mobile: other.Mobile, Purpose: other.Purpose, TargetX: 100, ExpiresAt: other.ExpiresAt}
		if err := repo.CreateSliderCaptcha(ctx, captcha); err != nil {
			t.Fatal(err)
		}
		runOther = func() error {
			_, err := repo.ReserveProfileSMSChallenge(ctx, change.ID, change.UserID, captcha.ID, captcha.TargetX, other)
			return err
		}
		unchanged = func() bool {
			var unbound bool
			err := repo.pool.QueryRow(ctx, `SELECT r.sms_challenge_id IS NULL AND c.status='PASSED' AND c.attempts=0
FROM profile_change_requests r CROSS JOIN slider_captcha_challenges c WHERE r.id=$1 AND c.id=$2`, change.ID, captcha.ID).Scan(&unbound)
			return err == nil && unbound
		}
	}
	if _, err := control.Exec(ctx, `SELECT pg_advisory_lock(671152406)`); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION fixture_wait_mixed_login() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF NEW.id='%s'::uuid THEN PERFORM pg_advisory_xact_lock(671152406); END IF; RETURN NEW; END $$;
CREATE TRIGGER fixture_wait_mixed_login BEFORE INSERT ON sms_challenges FOR EACH ROW EXECUTE FUNCTION fixture_wait_mixed_login();`, login.ID)); err != nil {
		t.Fatal(err)
	}
	wait := func(want int) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			var waiting int
			err := control.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1 AND usename='account_service' AND wait_event='advisory'`, config.Database).Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting == want {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
		t.Fatalf("could not observe %d SMS gate waiters", want)
	}
	loginResult := make(chan error, 1)
	otherResult := make(chan error, 1)
	go func() { loginResult <- repo.CreateSMSChallenge(ctx, login) }()
	wait(1)
	go func() { otherResult <- runOther() }()
	wait(2)
	if _, err := control.Exec(ctx, `SELECT pg_advisory_unlock(671152406)`); err != nil {
		t.Fatal(err)
	}
	loginErr, otherErr := <-loginResult, <-otherResult
	var limited *SMSRateLimitError
	if loginErr != nil || !errors.As(otherErr, &limited) || limited.RetryAfter != time.Minute {
		t.Fatalf("fresh snapshot did not return the correct quota decision: login=%v %s=%v", loginErr, kind, otherErr)
	}
	var accepted, blocked int
	var otherChallenge bool
	err = repo.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE result='ACCEPTED'),count(*) FILTER(WHERE result='RATE_LIMITED'),
EXISTS(SELECT 1 FROM sms_challenges WHERE id=$2) FROM sms_send_attempts WHERE provider_id=$1`, provider.ID, other.ID).Scan(&accepted, &blocked, &otherChallenge)
	if err != nil || accepted != 100 || blocked != 1 || otherChallenge || !unchanged() {
		t.Fatalf("mixed isolation overshot quota or left reservation facts: accepted=%d blocked=%d challenge=%t err=%v", accepted, blocked, otherChallenge, err)
	}
}
