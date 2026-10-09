package billingschema

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestRegistrationBonusPublicSchemaAndInputFacts(t *testing.T) {
	if os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL") == "" {
		t.Skip("dedicated PostgreSQL schema fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	account, err := migrations.Load(filepath.Join("..", "..", "migrations", "account"))
	if err != nil {
		t.Fatal(err)
	}
	bill, err := migrations.Load(filepath.Join("..", "..", "migrations", "billing"))
	if err != nil {
		t.Fatal(err)
	}
	ac := f.database(t, ctx, "account", "account_service")
	if err = accountschema.Up(ctx, ac, account); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, ac, account); err != nil {
		t.Fatal(err)
	}
	if _, err = ac.Exec(ctx, strings.Split(stageAccountSeed, "INSERT INTO role_assignments")[0]); err != nil {
		t.Fatal(err)
	}
	const user = "11111111-1111-4111-8111-111111111111"
	challenge, op := uuid.New(), uuid.New()
	if _, err = ac.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,expires_at,created_at,updated_at,provider_id,adapter_version) VALUES($1,'+8613800000000','REGISTER','EXPIRED',now(),now(),now(),'fixture','v1')`, challenge); err != nil {
		t.Fatal(err)
	}
	if _, err = ac.Exec(ctx, `INSERT INTO registration_operations(id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,password_policy_version,committed_user_id,status,next_retry_at,created_at,updated_at) VALUES($1,$2,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'f07-owned-fixture','+8613800000000',$3,'v1',$4,'EXPIRED',now(),now(),now())`, op, uuid.New(), challenge, user); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		key         any
		fingerprint any
		valid       bool
	}{
		{"null-key", nil, make([]byte, 32), false}, {"null-fingerprint", "fixture-key", nil, false}, {"both-null", nil, nil, false}, {"complete", "fixture-key", make([]byte, 32), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			tx, err := ac.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)
			_, err = tx.Exec(ctx, `INSERT INTO wallet_provisioning_jobs(user_id,registration_operation_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at,registration_bonus_mode,registration_bonus_fingerprint_key_id,registration_bonus_fingerprint) VALUES($1,$2,'PENDING','v1',now()+interval '1 hour',now(),now(),now(),'EVALUATE',$3,$4)`, user, op, c.key, c.fingerprint)
			if c.valid {
				if err != nil {
					t.Fatal(err)
				}
				if _, err = tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
					t.Fatal(err)
				}
				return
			}
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "23514" {
				t.Fatalf("nullable tuple was not rejected by CHECK; error=%v", err)
			}
		})
	}
	t.Run("account-public-down-preserves-facts", func(t *testing.T) {
		ordered, err := accountschema.Ordered(account)
		if err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, ac, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		if err = migrations.DownOne(ctx, ac, ordered); err == nil {
			t.Fatal("forward-only account public down unexpectedly succeeded")
		}
		if after := stageValue(t, ctx, ac, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`); after != before {
			t.Fatal("failed account Down changed ledger")
		}
		var present bool
		if err = ac.QueryRow(ctx, `SELECT to_regclass('registration_bonus_mobile_eligibility') IS NOT NULL`).Scan(&present); err != nil || !present {
			t.Fatal("bonus eligibility table lost")
		}
	})
	{
		t.Run("public-prior-delivery-data-upgrade", func(t *testing.T) {
			prior := make([]migrations.Migration, 0, len(account))
			for _, item := range account {
				if item.Version != 87 && item.Version != 900004 {
					prior = append(prior, item)
				}
			}
			upgrade := f.database(t, ctx, "account", "account_service")
			if err := accountschema.Up(ctx, upgrade, prior); err != nil {
				t.Fatal(err)
			}
			if _, err := upgrade.Exec(ctx, strings.Split(stageAccountSeed, "INSERT INTO role_assignments")[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := upgrade.Exec(ctx, `INSERT INTO wallet_provisioning_jobs(user_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at) VALUES($1,'PENDING','f07-retained-v1',now()+interval '1 hour',now(),now(),now())`, user); err != nil {
				t.Fatal(err)
			}
			before := stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(user_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at))::text FROM wallet_provisioning_jobs`)
			if err := accountschema.Up(ctx, upgrade, account); err != nil {
				t.Fatal(err)
			}
			if _, err := migrations.VerifyApplied(ctx, upgrade, account); err != nil {
				t.Fatal(err)
			}
			if stageValue(t, ctx, upgrade, `SELECT jsonb_agg(jsonb_build_array(user_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at))::text FROM wallet_provisioning_jobs`) != before {
				t.Fatal("upgrade changed existing job facts")
			}
			if stageValue(t, ctx, upgrade, `SELECT registration_bonus_mode||':'||(registration_bonus_fingerprint_key_id IS NULL)::text||':'||(registration_bonus_fingerprint IS NULL)::text FROM wallet_provisioning_jobs`) != "NOT_APPLICABLE:true:true" {
				t.Fatal("upgrade falsely granted old job bonus mode")
			}
		})
	}
	bc := f.database(t, ctx, "billing", "billing_schema_owner")
	if err = Up(ctx, bc, bill); err != nil {
		t.Fatal(err)
	}
	if _, err = migrations.VerifyApplied(ctx, bc, bill); err != nil {
		t.Fatal(err)
	}
	repository, err := billing.NewPostgresRepository(ctx, f.serviceURL(t, bc, "billing_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer repository.Close()
	for _, c := range []struct{ name, reason, key string }{
		{"short-unicode-reason", strings.Repeat("中", 5), strings.Repeat("k", 16)},
		{"short-unicode-key", strings.Repeat("r", 10), strings.Repeat("中", 8)},
		{"invalid-utf8-reason", strings.Repeat("r", 10) + string([]byte{255}), strings.Repeat("k", 16)},
		{"too-many-reason-characters", strings.Repeat("中", 501), strings.Repeat("k", 16)},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, method := range []string{"Replay", "Update"} {
				var err error
				if method == "Replay" {
					_, _, err = repository.ReplayRegistrationBonusPolicy(ctx, uuid.New(), false, 0, 30, 1, c.reason, c.key)
				} else {
					_, _, err = repository.UpdateRegistrationBonusPolicy(ctx, uuid.New(), false, 0, 30, 1, c.reason, c.key)
				}
				if !errors.Is(err, billing.ErrRegistrationBonusInvalid) {
					t.Errorf("%s invalid character facts escaped input boundary: %v", method, err)
				}
			}
		})
	}
	t.Run("unicode-sql-upper-bounds-and-replay", func(t *testing.T) {
		actor := uuid.New()
		reason, key := strings.Repeat("中", 500), strings.Repeat("鍵", 128)
		policy, replayed, err := repository.UpdateRegistrationBonusPolicy(ctx, actor, false, 0, 30, 1, reason, key)
		if err != nil || replayed || policy.Revision != 2 {
			t.Fatalf("valid SQL character limits rejected: %v", err)
		}
		replay, ok, err := repository.ReplayRegistrationBonusPolicy(ctx, actor, false, 0, 30, 1, reason, key)
		if err != nil || !ok || replay.Revision != policy.Revision {
			t.Fatalf("exact Unicode replay lost: %v", err)
		}
	})
	t.Run("billing-public-down-preserves-facts", func(t *testing.T) {
		ordered, err := Ordered(bill)
		if err != nil {
			t.Fatal(err)
		}
		before := stageValue(t, ctx, bc, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		commandsBefore := stageValue(t, ctx, bc, `SELECT count(*)::text FROM registration_bonus_policy_commands`)
		if err = migrations.DownOne(ctx, bc, ordered); err == nil {
			t.Fatal("forward-only billing public down unexpectedly succeeded")
		}
		if stageValue(t, ctx, bc, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) != before {
			t.Fatal("failed billing Down changed ledger")
		}
		if stageValue(t, ctx, bc, `SELECT count(*)::text FROM registration_bonus_policy_commands`) != commandsBefore {
			t.Fatal("policy facts lost")
		}
	})
}
