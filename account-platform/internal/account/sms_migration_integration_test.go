package account

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPostgresLegacySMSMigrationPreservesUnknownProviderAndCooldown(t *testing.T) {
	repo := accountDatabase(t)
	ctx := context.Background()
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join("..", "..", "migrations", "account", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	// Reconstruct only this owned schema's prior risk-dimension version, then
	// apply the real migration to its existing provider-neutral challenge.
	if _, err := repo.pool.Exec(ctx, read("000012_sms_risk_dimensions.down.sql")); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := repo.pool.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138000','REGISTER','CREATED',$2,$3,$3)`, id, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.pool.Exec(ctx, read("000012_sms_risk_dimensions.up.sql")); err != nil {
		t.Fatal(err)
	}
	var provider, status string
	var created time.Time
	if err := repo.pool.QueryRow(ctx, `SELECT provider_id,status,created_at FROM sms_challenges WHERE id=$1`, id).Scan(&provider, &status, &created); err != nil {
		t.Fatal(err)
	}
	if provider != "legacy-unattributed" || status != "CREATED" || !created.Equal(now) {
		t.Fatal("migration invented provider facts or erased cooldown")
	}
	challenge := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138000", Purpose: SMSPurposeRegister, ExpiresAt: now.Add(time.Minute), Provider: SMSProviderDescriptor{ID: "http", AdapterVersion: "v1"}}
	if err := repo.CreateSMSChallenge(ctx, challenge); !errorsIsSMSRateLimit(err) {
		t.Fatal("historical unknown delivery no longer limits repeat sends")
	}
}

func errorsIsSMSRateLimit(err error) bool {
	var limited *SMSRateLimitError
	return errors.As(err, &limited)
}

func TestPostgresProfileAndResetSMSPurposeHaveMatchingRiskAudit(t *testing.T) {
	for _, purpose := range []SMSPurpose{SMSPurposeProfileMobile, SMSPurposePasswordReset} {
		t.Run(string(purpose), func(t *testing.T) {
			repo := accountDatabase(t)
			challenge := SMSChallenge{ID: uuid.New(), Mobile: "+8613800138000", Purpose: purpose, ExpiresAt: time.Now().Add(time.Minute), Provider: SMSProviderDescriptor{ID: "http", AdapterVersion: "v1"}}
			if err := repo.CreateSMSChallenge(context.Background(), challenge); err != nil {
				t.Fatal("new SMS purpose failed to persist its risk audit")
			}
			var found string
			if err := repo.pool.QueryRow(context.Background(), `SELECT purpose FROM sms_send_attempts WHERE challenge_id=$1`, challenge.ID).Scan(&found); err != nil || found != string(purpose) {
				t.Fatal("challenge purpose and risk audit disagree")
			}
		})
	}
}
