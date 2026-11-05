package account

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// These faults affect only an owned fixture database. Business calls retain
// the real non-superuser account_service, migrations and transaction boundaries.
func TestF01SMSStorageErrorsRemainInfrastructureFailures(t *testing.T) {
	f := retainedWalletFenceActors(t)
	for _, scenario := range []struct {
		name, path, fault string
		expired           bool
	}{
		{"registration-read", "registration", "read", false},
		{"registration-attempt-write", "registration", "write", false},
		{"registration-attempt-commit", "registration", "commit", false},
		{"login-attempt-write", "login", "write", false},
		{"login-attempt-commit", "login", "commit", false},
		{"login-expiry-write", "login", "write", true},
		{"login-expiry-commit", "login", "commit", true},
		{"ready-expiry-write", "ready", "write", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			r := f.repository(t)
			challenge, operation := f01SMSStorageSeed(t, r, scenario.path == "registration", scenario.expired)
			before := f01SMSStorageSnapshot(t, r, challenge, operation)
			cfg := f.config.Copy()
			cfg.Database = r.pool.Config().ConnConfig.Database
			admin, err := pgx.ConnectConfig(f.ctx, cfg)
			if err != nil {
				t.Fatal("owned fault setup connection failed")
			}
			defer admin.Close(f.ctx)
			code := "P0001"
			if scenario.fault == "read" {
				code = "42P01"
				if _, err = admin.Exec(f.ctx, `ALTER TABLE sms_challenges RENAME TO f01_unavailable_sms_challenges`); err != nil {
					t.Fatal(err)
				}
			} else {
				trigger := `CREATE TRIGGER f01_sms_fault BEFORE UPDATE ON sms_challenges FOR EACH ROW EXECUTE FUNCTION f01_sms_fault()`
				if scenario.fault == "commit" {
					// UPDATE succeeds, but the real transaction COMMIT is rejected.
					trigger = `CREATE CONSTRAINT TRIGGER f01_sms_fault AFTER UPDATE ON sms_challenges DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION f01_sms_fault()`
				}
				if _, err = admin.Exec(f.ctx, `CREATE FUNCTION f01_sms_fault() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'controlled SMS storage fault' USING ERRCODE='P0001'; END $$; `+trigger); err != nil {
					t.Fatal(err)
				}
			}
			var result error
			switch scenario.path {
			case "registration":
				result = r.ConsumeRegistrationSMS(f.ctx, operation, challenge, "+8613800138000", false)
			case "login":
				result = r.ConsumeSMSChallenge(f.ctx, challenge, "+8613800138000", SMSPurposeLogin, false)
			case "ready":
				_, result = r.SMSChallengeReady(f.ctx, challenge, "+8613800138000", SMSPurposeLogin)
			}
			if scenario.fault == "read" {
				if _, err = admin.Exec(f.ctx, `ALTER TABLE f01_unavailable_sms_challenges RENAME TO sms_challenges`); err != nil {
					t.Fatal("owned read fault restoration failed", err)
				}
			} else if _, err = admin.Exec(f.ctx, `DROP TRIGGER f01_sms_fault ON sms_challenges; DROP FUNCTION f01_sms_fault()`); err != nil {
				t.Fatal("owned write fault cleanup failed", err)
			}
			var databaseError *pgconn.PgError
			if errors.Is(result, ErrSMSInvalid) || !errors.As(result, &databaseError) || databaseError.Code != code {
				t.Errorf("actual storage failure was masked as a verification result: %v", result)
			}
			if after := f01SMSStorageSnapshot(t, r, challenge, operation); after != before {
				t.Fatal("failed verification changed challenge, registration or audit facts")
			}
		})
	}
}

func TestF01SMSStorageNormalResultsRemainStable(t *testing.T) {
	f := retainedWalletFenceActors(t)
	for _, name := range []string{"registration-invalid", "registration-valid", "login-invalid", "login-valid", "login-expired", "ready-expired", "missing-registration"} {
		t.Run(name, func(t *testing.T) {
			r := f.repository(t)
			registration := strings.HasPrefix(name, "registration") || name == "missing-registration"
			id, op := f01SMSStorageSeed(t, r, registration, strings.HasSuffix(name, "expired"))
			valid := name == "registration-valid" || name == "login-valid"
			var err error
			switch {
			case name == "missing-registration":
				err = r.ConsumeRegistrationSMS(f.ctx, uuid.New(), id, "+8613800138000", false)
			case registration:
				err = r.ConsumeRegistrationSMS(f.ctx, op, id, "+8613800138000", valid)
			case name == "ready-expired":
				_, err = r.SMSChallengeReady(f.ctx, id, "+8613800138000", SMSPurposeLogin)
			default:
				err = r.ConsumeSMSChallenge(f.ctx, id, "+8613800138000", SMSPurposeLogin, valid)
			}
			if valid && err != nil || !valid && !errors.Is(err, ErrSMSInvalid) {
				t.Fatal("normal verification result changed", err)
			}
			var status string
			var attempts int
			if err := r.pool.QueryRow(f.ctx, `SELECT status,attempts FROM sms_challenges WHERE id=$1`, id).Scan(&status, &attempts); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantAttempts := "SENT", 1
			if valid {
				wantStatus, wantAttempts = "CONSUMED", 0
			} else if strings.HasSuffix(name, "expired") {
				wantStatus, wantAttempts = "EXPIRED", 0
			} else if name == "missing-registration" {
				wantAttempts = 0
			}
			if status != wantStatus || attempts != wantAttempts {
				t.Fatal("verification mutation changed", status, attempts)
			}
			if registration {
				var version int64
				if err := r.pool.QueryRow(f.ctx, `SELECT status,version FROM registration_operations WHERE id=$1`, op).Scan(&status, &version); err != nil {
					t.Fatal(err)
				}
				want, wantVersion := "SMS_PENDING", int64(1)
				if valid {
					want, wantVersion = "REGISTRATION_IN_PROGRESS", 2
				}
				if status != want || version != wantVersion {
					t.Fatal("registration transition changed", status, version)
				}
			}
		})
	}
}

func f01SMSStorageSeed(t *testing.T, r *PostgresRepository, registration, expired bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	id, operation := uuid.New(), uuid.New()
	purpose := SMSPurposeLogin
	if registration {
		purpose = SMSPurposeRegister
	}
	expiry := r.now().Add(time.Minute)
	if expired {
		expiry = r.now().Add(-time.Minute)
	}
	if _, err := r.pool.Exec(t.Context(), `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version) VALUES($1,'+8613800138000',$2,$3,'SENT',$4,now(),now(),'controlled','v1')`, id, purpose, []byte{1}, expiry); err != nil {
		t.Fatal("controlled challenge seed failed", err)
	}
	if registration {
		if _, err := r.pool.Exec(t.Context(), `INSERT INTO registration_operations(id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,password_policy_version,status,next_retry_at,created_at,updated_at) VALUES($1,$2,$3,$3,$4,'+8613800138000',$5,'controlled','SMS_PENDING',now(),now(),now())`, operation, uuid.New(), make([]byte, 32), "controlled"+id.String(), id); err != nil {
			t.Fatal("controlled registration seed failed", err)
		}
	}
	return id, operation
}

func f01SMSStorageSnapshot(t *testing.T, r *PostgresRepository, id, operation uuid.UUID) string {
	t.Helper()
	var facts string
	if err := r.pool.QueryRow(t.Context(), `SELECT jsonb_build_object('challenge',(SELECT to_jsonb(c) FROM sms_challenges c WHERE c.id=$1),'operation',(SELECT to_jsonb(o) FROM registration_operations o WHERE o.id=$2),'audits',(SELECT COALESCE(jsonb_agg(a ORDER BY a.id),'[]'::jsonb) FROM audit_logs a),'events',(SELECT COALESCE(jsonb_agg(e ORDER BY e.event_id),'[]'::jsonb) FROM outbox_events e))::text`, id, operation).Scan(&facts); err != nil {
		t.Fatal("owned verification snapshot failed", err)
	}
	return facts
}
