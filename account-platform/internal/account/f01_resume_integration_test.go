package account

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func f01ActiveFixtureUser(t *testing.T, repo *PostgresRepository) (identity.Principal, uuid.UUID) {
	t.Helper()
	p := identity.Principal{Issuer: "https://identity.example.test", Subject: uuid.NewString(), SessionID: uuid.NewString(), Username: "f01" + strings.ReplaceAll(uuid.NewString(), "-", "")[:15]}
	me, _, err := repo.Bootstrap(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.pool.Exec(context.Background(), `UPDATE users SET status='ACTIVE' WHERE id=$1`, me.UserID); err != nil {
		t.Fatal(err)
	}
	return p, me.UserID
}

func TestF01ProfilePasswordRecovery(t *testing.T) {
	fixture := retainedWalletFenceActors(t)
	repo := fixture.repository(t)
	ctx := context.Background()
	mobileSequence := 0
	for _, method := range []string{"authenticated", "sms"} {
		for _, boundary := range []string{"expired", "live", "verified-proof", "identity-proof", "committed-proof", "revoked-proof", "verified-state", "password-already-active"} {
			t.Run(method+"/"+boundary, func(t *testing.T) {
				p, user := f01ActiveFixtureUser(t, repo)
				mobileSequence++
				old, err := repo.CreateProfileChange(ctx, p, "MOBILE", fmt.Sprintf("+86139%08d", mobileSequence))
				if err != nil {
					t.Fatal(err)
				}
				bound := uuid.New()
				if _, err = repo.pool.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at) VALUES($1,$2,'PROFILE_MOBILE','SENT','fixture','v1',clock_timestamp()+interval '1 hour',clock_timestamp(),clock_timestamp());`, bound, old.NewValue); err != nil {
					t.Fatal(err)
				}
				expiry := "-1 hour"
				if boundary == "live" {
					expiry = "1 hour"
				}
				if _, err = repo.pool.Exec(ctx, `UPDATE profile_change_requests SET sms_challenge_id=$2,sla_expires_at=clock_timestamp()+$3::interval WHERE id=$1`, old.ID, bound, expiry); err != nil {
					t.Fatal(err)
				}
				column := map[string]string{"verified-proof": "sms_verified_at", "identity-proof": "identity_updated_at", "committed-proof": "account_committed_at", "revoked-proof": "sessions_revoked_at"}[boundary]
				if column != "" {
					if _, err = repo.pool.Exec(ctx, `UPDATE profile_change_requests SET `+column+`=clock_timestamp() WHERE id=$1`, old.ID); err != nil {
						t.Fatal(err)
					}
				}
				if boundary == "verified-state" {
					if _, err = repo.pool.Exec(ctx, `UPDATE profile_change_requests SET status='VERIFIED' WHERE id=$1`, old.ID); err != nil {
						t.Fatal(err)
					}
				}
				mobile := "+86138" + fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)
				if _, err = repo.pool.Exec(ctx, `UPDATE users SET mobile_e164=$2 WHERE id=$1`, user, mobile); err != nil {
					t.Fatal(err)
				}
				if boundary == "password-already-active" {
					if _, err = repo.pool.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,mobile_e164,password_handle,password_policy_version,fence_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'AUTHENTICATED','VERIFIED',$5,'fixture','v1',2,now(),now()+interval '30 minutes',now(),now())`, uuid.New(), user, p.Issuer, p.Subject, mobile); err != nil {
						t.Fatal(err)
					}
				}
				if method == "authenticated" {
					_, err = repo.CreateAuthenticatedPasswordReset(ctx, p, "fixture-handle", "v1")
				} else {
					resetSMS := uuid.New()
					if _, err = repo.pool.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at) VALUES($1,$2,'PASSWORD_RESET','SENT','fixture','v1',clock_timestamp()+interval '1 hour',clock_timestamp(),clock_timestamp())`, resetSMS, mobile); err != nil {
						t.Fatal(err)
					}
					_, err = repo.CreatePasswordReset(ctx, p.Issuer, resetSMS, mobile, true, "fixture-handle", "v1")
				}
				if boundary == "expired" {
					if err != nil {
						t.Fatal(err)
					}
				} else if !errors.Is(err, ErrConflict) {
					t.Fatalf("protected boundary %s: %v", boundary, err)
				}
				result, readErr := repo.ProfileChange(ctx, old.ID, nil)
				if readErr != nil {
					t.Fatal(readErr)
				}
				expected := old.Status
				if boundary == "verified-state" {
					expected = "VERIFIED"
				}
				auditExpected := 0
				if boundary == "expired" {
					expected = "SUPERSEDED"
					auditExpected = 1
				}
				if result.Status != expected || result.SMSChallengeID == nil || *result.SMSChallengeID != bound {
					t.Fatalf("evidence changed: %#v", result)
				}
				var audits int
				if err = repo.pool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE resource_id=$1 AND action='account.profile_change.superseded'`, old.ID.String()).Scan(&audits); err != nil || audits != auditExpected {
					t.Fatalf("audit=%d err=%v", audits, err)
				}
				if boundary == "expired" {
					if err = repo.CommitProfileSMSVerification(ctx, old.ID, user, bound, true); !errors.Is(err, ErrProfileChangeConflict) {
						t.Fatalf("stale SMS revived profile: %v", err)
					}
				}
			})
		}
	}
}

func TestF01WalletIndex93PreservesApplied87(t *testing.T) {
	originalDirectory := os.Getenv("PINVOU_TEST_ACCOUNT_ORIGINAL_MIGRATIONS_DIR")
	if originalDirectory == "" {
		t.Skip("original immutable migration fixture required")
	}
	original, err := migrations.Load(originalDirectory)
	if err != nil {
		t.Fatal(err)
	}
	var bonus migrations.Migration
	for _, m := range original {
		if m.Version == 87 {
			bonus = m
		}
	}
	if bonus.Name != "registration_bonus_fingerprint" || bonus.Checksum != "1d76a6e9aa6b5fcee84e2eabe8912cbfb8fa53c79ba29ce2bb4c29852be070e1" {
		t.Fatal("original applied migration87 identity changed")
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var index migrations.Migration
	for _, m := range installed {
		if m.Name == "wallet_provisioning_leased_index" {
			index = m
		}
	}
	if index.Version != 93 {
		t.Fatalf("new index version=%d", index.Version)
	}
	fixture := retainedWalletFenceActors(t)
	repo := fixture.repository(t)
	ctx := context.Background()
	cfg := fixture.config.Copy()
	cfg.Database = repo.pool.Config().ConnConfig.Database
	cfg.User = "account_migrator"
	cfg.Password = ""
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	defer migrations.Unlock(context.Background(), conn)
	// This database and both actor roles were freshly created by this test.
	// Remove only the unreleased index fixture to reproduce the pre-93 state.
	if _, err = conn.Exec(ctx, `SET ROLE account_service; DROP INDEX wallet_provisioning_leased_idx; DELETE FROM schema_migrations WHERE version=93`); err != nil {
		t.Fatal(err)
	}
	// F07 installs the real immutable bonus migration. F01's narrower fixture
	// lacks that schema: only there seed the original ledger fact. Preserve an
	// existing record exactly, and reject any unexpected identity.
	var appliedName, appliedChecksum string
	err = conn.QueryRow(ctx, `SELECT name,checksum FROM schema_migrations WHERE version=87`).Scan(&appliedName, &appliedChecksum)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = conn.Exec(ctx, `INSERT INTO schema_migrations(version,name,checksum) VALUES($1,$2,$3)`, bonus.Version, bonus.Name, bonus.Checksum); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	} else if appliedName != bonus.Name || appliedChecksum != bonus.Checksum {
		t.Fatal("installed migration87 identity changed")
	}
	var before string
	if err = conn.QueryRow(ctx, `SELECT row_to_json(m)::text FROM schema_migrations m WHERE version=87`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	combined := append([]migrations.Migration{}, installed...)
	bonusCount := 0
	for _, m := range installed {
		if m.Version == bonus.Version {
			bonusCount++
			if m.Name != bonus.Name || m.Checksum != bonus.Checksum {
				t.Fatal("packaged migration87 identity changed")
			}
		}
	}
	if bonusCount > 1 {
		t.Fatal("duplicate packaged migration87")
	}
	if bonusCount == 0 {
		combined = append(combined, bonus)
	}
	sort.Slice(combined, func(i, j int) bool { return combined[i].Version < combined[j].Version })
	if err = accountschema.Up(ctx, conn, combined); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, conn, combined); err != nil {
		t.Fatal(err)
	}
	var after, definition string
	if err = conn.QueryRow(ctx, `SELECT row_to_json(m)::text FROM schema_migrations m WHERE version=87`).Scan(&after); err != nil || before != after {
		t.Fatalf("original87 ledger changed: %v", err)
	}
	if err = conn.QueryRow(ctx, `SELECT pg_get_indexdef('wallet_provisioning_leased_idx'::regclass)`).Scan(&definition); err != nil || !strings.Contains(definition, "lease_until") || !strings.Contains(definition, "LEASED") {
		t.Fatalf("new93 real index: %s %v", definition, err)
	}
	// Diagnostic combined package only: original87 remains F07's product input,
	// not an F01 business capability. Never apply this fixture to retained data.
}
