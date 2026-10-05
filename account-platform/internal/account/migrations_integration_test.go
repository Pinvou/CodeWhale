package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/testdb"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func applyMigrationSet(ctx context.Context, conn *pgx.Conn, items []migrations.Migration, down bool) error {
	defer migrations.Unlock(ctx, conn)
	if down {
		return migrations.DownOne(ctx, conn, items)
	}
	return migrations.Up(ctx, conn, items)
}

func TestPostgresA08FourDomainMigrationsTwoRounds(t *testing.T) {
	for _, domain := range []string{"account", "billing", "token", "payment-simulator"} {
		t.Run(domain, func(t *testing.T) {
			pool := testdb.Open(t, domain)
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()
			items, err := migrations.Load(filepath.Join("..", "..", "migrations", domain))
			if err != nil {
				t.Fatal(err)
			}
			for round := 0; round < 2; round++ {
				for range items {
					if err := applyMigrationSet(ctx, conn.Conn(), items, true); err != nil {
						t.Fatal(err)
					}
				}
				var count int
				if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != 0 {
					t.Fatal("full rollback left migration ledger entries")
				}
				if err := applyMigrationSet(ctx, conn.Conn(), items, false); err != nil {
					t.Fatal(err)
				}
				if err := conn.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&count); err != nil || count != len(items) {
					t.Fatal("reapplication did not restore every migration")
				}
			}
		})
	}
}

func TestPostgresA07AccountDataSurvivesA08Migration(t *testing.T) {
	repo := accountDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := repo.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	items, err := migrations.Load(filepath.Join("..", "..", "migrations", "account"))
	if err != nil {
		t.Fatal(err)
	}
	for i := len(items) - 1; i >= 0 && items[i].Version > 9; i-- {
		if err := applyMigrationSet(ctx, conn.Conn(), items, true); err != nil {
			t.Fatal(err)
		}
	}
	userID, challengeID := uuid.New(), uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := conn.Exec(ctx, `INSERT INTO users(id,status,username,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,'ACTIVE','migration_fixture','+8613800138000',$2,$2,$2)`, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES('https://issuer.example','migration-subject',$1,$2,$2)`, userID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138000','REGISTER','CREATED',$2,$3,$3)`, challengeID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO mall_user_sync_jobs(user_id,mobile_e164,status,attempts,next_attempt_at,lease_until,buyer_id,created_at,updated_at) VALUES($1,'+8613800138000','LEASED',3,$2,$3,'synthetic-buyer-1234',$2,$2)`, userID, now, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrationSet(ctx, conn.Conn(), items, false); err != nil {
		t.Fatal(err)
	}
	var status, mobile, userCode, provider string
	var identityUser uuid.UUID
	if err := conn.QueryRow(ctx, `SELECT status,mobile_e164,user_code FROM users WHERE id=$1`, userID).Scan(&status, &mobile, &userCode); err != nil || status != "ACTIVE" || mobile != "+8613800138000" || userCode == "" {
		t.Fatal("upgrade changed existing account facts")
	}
	if err := conn.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer='https://issuer.example' AND subject='migration-subject'`).Scan(&identityUser); err != nil || identityUser != userID {
		t.Fatal("upgrade changed identity binding")
	}
	var challengeStatus string
	var created time.Time
	if err := conn.QueryRow(ctx, `SELECT provider_id,status,created_at FROM sms_challenges WHERE id=$1`, challengeID).Scan(&provider, &challengeStatus, &created); err != nil || provider != "legacy-unattributed" || challengeStatus != "CREATED" || !created.Equal(now) {
		t.Fatal("upgrade invented SMS provider or removed cooldown")
	}
	var mappingStatus, jobStatus, redacted, adapter string
	var attempts int
	var lease *time.Time
	if err := conn.QueryRow(ctx, `SELECT m.provider_id,m.adapter_version,m.status,m.external_reference_redacted,j.status,j.attempts,j.lease_until FROM commerce_customer_mappings m JOIN commerce_customer_sync_jobs j ON j.mapping_id=m.id WHERE m.subject_id=$1`, userID).Scan(&provider, &adapter, &mappingStatus, &redacted, &jobStatus, &attempts, &lease); err != nil || provider != "h3c_mall" || adapter != "h3c-mall-v1" || mappingStatus != "RETRY" || jobStatus != "RETRY" || attempts != 3 || lease != nil || redacted != "***1234" {
		t.Fatal("upgrade lost recoverable legacy commerce facts")
	}
}
