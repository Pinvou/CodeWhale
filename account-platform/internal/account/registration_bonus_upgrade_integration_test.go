package account

import (
	"context"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRegistrationBonusApplied87UpgradesToRequiredFactsGuard(t *testing.T) {
	repository := retainedWalletFenceActors(t).repositoryWithInitialStage(t, func(ctx context.Context, connection *pgx.Conn, loaded []migrations.Migration) error {
		ordered, err := accountschema.Ordered(loaded)
		if err != nil {
			return err
		}
		legacy := make([]migrations.Migration, 0, len(ordered)-1)
		for _, migration := range ordered {
			if migration.Version != 900004 {
				legacy = append(legacy, migration)
			}
		}
		// Install the actual frozen predecessor SQL in the public schema as the
		// genuine non-super migrator. No synthetic ledger row replaces history.
		if err := migrations.Up(ctx, connection, legacy); err != nil {
			return err
		}
		var checksum string
		if err := connection.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=87`).Scan(&checksum); err != nil {
			return err
		}
		if checksum != "1d76a6e9aa6b5fcee84e2eabe8912cbfb8fa53c79ba29ce2bb4c29852be070e1" {
			t.Fatal("original registration 87 identity changed")
		}
		return nil
	})
	ctx := context.Background()
	var guarded bool
	if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900004)`).Scan(&guarded); err != nil || !guarded {
		t.Fatalf("guard upgrade absent: %v", err)
	}
	userID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,created_at,updated_at) VALUES($1,generate_unique_user_code(),'PROVISIONING',$2,$2,now(),now())`, userID, "bonus_guard_upgrade"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO wallet_provisioning_jobs(user_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at,registration_bonus_mode) VALUES($1,'PENDING','personal-wallet-v1',now()+interval '1 hour',now(),now(),now(),'EVALUATE')`, userID); err == nil {
		t.Fatal("legacy nullable EVALUATE accepted after real upgrade")
	}
}
