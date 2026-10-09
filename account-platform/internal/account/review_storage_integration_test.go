package account

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReviewStorageRetentionAndAudit(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("requires isolated PostgreSQL fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture connection failed")
	}
	defer c.Close(context.Background())
	schema := "review_storage_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = c.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := c.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned schema cleanup failed")
		}
	}()
	if _, err = c.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err = accountschema.Up(ctx, c, installed); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("fixture pool configuration failed")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("fixture pool failed")
	}
	defer pool.Close()
	repo := &PostgresRepository{pool: pool}
	t.Run("all audit actions are immutable", func(t *testing.T) {
		for _, action := range []string{"security.fixture", "account.password_reset.fixture", "account.wallet.provision", "account.profile_change.fixture"} {
			id := uuid.New()
			if _, err := c.Exec(ctx, `INSERT INTO audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,occurred_at) VALUES($1,$2,'fixture','fixture','SUCCEEDED','fixture','fixture',now())`, id, action); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{`UPDATE audit_logs SET result='FAILED' WHERE id=$1`, `DELETE FROM audit_logs WHERE id=$1`} {
				if _, err := c.Exec(ctx, query, id); err == nil {
					t.Fatal("audit mutation allowed")
				}
			}
		}
		if _, err := c.Exec(ctx, `TRUNCATE audit_logs`); err == nil {
			t.Fatal("audit truncate allowed")
		}
	})
	t.Run("leased wallet index exists", func(t *testing.T) {
		var definition string
		if err := c.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE schemaname=$1 AND indexname='wallet_provisioning_leased_idx'`, schema).Scan(&definition); err != nil || !strings.Contains(definition, "lease_until") || !strings.Contains(definition, "LEASED") {
			t.Fatal("leased index missing")
		}
	})
	t.Run("retention protects live and referenced records", func(t *testing.T) {
		oldSMS, liveSMS, uncertainSMS := uuid.New(), uuid.New(), uuid.New()
		for _, item := range []struct {
			id               uuid.UUID
			expires, updated string
			uncertain        bool
		}{
			{oldSMS, "-100 days", "-100 days", false}, {liveSMS, "1 day", "-100 days", false}, {uncertainSMS, "-100 days", "-100 days", true},
		} {
			if _, err := c.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version,delivery_started_at,delivery_uncertain_at) VALUES($1,'+8613800138000','LOGIN',NULL,'SENT',now()+$2::interval,now()-interval '100 days',now()+$3::interval,'fixture','v1',CASE WHEN $4 THEN now()-interval '100 days' END,CASE WHEN $4 THEN now()-interval '100 days' END)`, item.id, item.expires, item.updated, item.uncertain); err != nil {
				t.Fatal(err)
			}
		}
		for _, table := range []string{"sms_send_attempts", "sms_verification_attempts"} {
			extraColumn, extraValue := "", ""
			if table == "sms_send_attempts" {
				extraColumn = ",adapter_version"
				extraValue = ",'v1'"
			}
			for _, id := range []uuid.UUID{oldSMS, liveSMS, uncertainSMS} {
				if _, err := c.Exec(ctx, `INSERT INTO `+table+`(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at`+extraColumn+`) VALUES($1,$2,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),'LOGIN','fixture','ACCEPTED',now()-interval '100 days'`+extraValue+`)`, uuid.New(), id); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, age := range []string{"2 days", "12 hours"} {
			if _, err := c.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138000','LOGIN',20,'CREATED',now()-interval '1 hour',now()-$2::interval,now()-$2::interval)`, uuid.New(), age); err != nil {
				t.Fatal(err)
			}
		}
		if err := repo.CleanupMobileRecords(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM sms_challenges`).Scan(&count); err != nil || count != 2 {
			t.Fatalf("retained SMS=%d err=%v", count, err)
		}
		if err := c.QueryRow(ctx, `SELECT count(*) FROM slider_captcha_challenges`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retained captcha=%d err=%v", count, err)
		}
		for _, table := range []string{"sms_send_attempts", "sms_verification_attempts"} {
			if err := c.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 2 {
				t.Fatalf("retained attempts=%d err=%v", count, err)
			}
		}
		if err := repo.CleanupMobileRecords(ctx); err != nil {
			t.Fatal("cleanup was not idempotent")
		}
	})
	t.Run("retention age and active lease boundaries", func(t *testing.T) {
		user := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO users(id,status,user_code,created_at,updated_at) VALUES($1,'ACTIVE',generate_unique_user_code(),now(),now())`, user); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"registration", "profile", "password"} {
			id := uuid.New()
			if _, err := c.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at) VALUES($1,'+8613800138003','LOGIN','SENT','fixture','v1',now()-interval '100 days',now()-interval '100 days',now()-interval '100 days')`, id); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "registration":
				_, err = c.Exec(ctx, `INSERT INTO registration_operations(id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,password_policy_version,status,next_retry_at,created_at,updated_at) VALUES($1,$2,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),'retention_fixture','+8613800138003',$3,'v1','EXPIRED',now(),now(),now())`, uuid.New(), uuid.New(), id)
			case "profile":
				_, err = c.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,sms_challenge_id,status,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'fixture','fixture','MOBILE','old','new',1,$3,'COMPLETED',now(),now(),now(),now())`, uuid.New(), user, id)
			case "password":
				_, err = c.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,sms_challenge_id,status,method,mobile_e164,password_handle,password_policy_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,'fixture','fixture',$3,'FAILED','SMS','+8613800138003','fixture','v1',now(),now(),now(),now())`, uuid.New(), user, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.CleanupMobileRecords(ctx); err != nil {
				t.Fatalf("%s business reference cleanup: %v", kind, err)
			}
			var exists bool
			if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sms_challenges WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
				t.Fatalf("%s reference deleted: %v", kind, err)
			}
		}
		for _, age := range []string{"89 days", "91 days"} {
			id := uuid.New()
			if _, err := c.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at,delivery_lease_until) VALUES($1,'+8613800138001','LOGIN','SENT','fixture','v1',now()-interval '100 days',now()-interval '100 days',now()-$2::interval,NULL)`, id, age); err != nil {
				t.Fatal(err)
			}
			if err := repo.CleanupMobileRecords(ctx); err != nil {
				t.Fatal(err)
			}
			var exists bool
			if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sms_challenges WHERE id=$1)`, id).Scan(&exists); err != nil || exists != (age == "89 days") {
				t.Fatalf("age=%s retained=%v err=%v", age, exists, err)
			}
		}
		id := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,status,provider_id,adapter_version,expires_at,created_at,updated_at,delivery_lease_id,delivery_lease_until) VALUES($1,'+8613800138001','LOGIN','SENT','fixture','v1',now()-interval '100 days',now()-interval '100 days',now()-interval '100 days',gen_random_uuid(),now()+interval '1 hour')`, id); err != nil {
			t.Fatal(err)
		}
		if err := repo.CleanupMobileRecords(ctx); err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sms_challenges WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
			t.Fatalf("active lease deleted: %v", err)
		}
		for _, table := range []string{"sms_send_attempts", "sms_verification_attempts"} {
			for _, age := range []string{"89 days", "91 days"} {
				id := uuid.New()
				extraColumn, extraValue := "", ""
				if table == "sms_send_attempts" {
					extraColumn, extraValue = ",adapter_version", ",'v1'"
				}
				if _, err := c.Exec(ctx, `INSERT INTO `+table+`(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at`+extraColumn+`) VALUES($1,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),'LOGIN','fixture','ACCEPTED',now()-$2::interval`+extraValue+`)`, id, age); err != nil {
					t.Fatal(err)
				}
				if err := repo.CleanupMobileRecords(ctx); err != nil {
					t.Fatal(err)
				}
				if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, id).Scan(&exists); err != nil || exists != (age == "89 days") {
					t.Fatalf("%s age=%s retained=%v err=%v", table, age, exists, err)
				}
			}
		}
	})
	t.Run("captcha bounded batches skip locked and preserve live", func(t *testing.T) {
		if _, err := c.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) SELECT gen_random_uuid(),'+8613800138002','LOGIN',20,'CREATED',now()-interval '1 hour',now()-interval '25 hours',now()-interval '25 hours' FROM generate_series(1,501)`); err != nil {
			t.Fatal(err)
		}
		live := uuid.New()
		if _, err := c.Exec(ctx, `INSERT INTO slider_captcha_challenges(id,mobile_e164,purpose,target_x,status,expires_at,created_at,updated_at) VALUES($1,'+8613800138002','LOGIN',20,'CREATED',now()+interval '1 day',now()-interval '100 days',now()-interval '100 days')`, live); err != nil {
			t.Fatal(err)
		}
		lock, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Rollback(context.Background())
		var locked uuid.UUID
		if err := lock.QueryRow(ctx, `SELECT id FROM slider_captcha_challenges WHERE mobile_e164='+8613800138002' AND id<>$1 ORDER BY id LIMIT 1 FOR UPDATE`, live).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if err := repo.CleanupMobileRecords(ctx); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := c.QueryRow(ctx, `SELECT count(*) FROM slider_captcha_challenges WHERE mobile_e164='+8613800138002'`).Scan(&count); err != nil || count != 2 {
			t.Fatalf("first bounded batch remaining=%d err=%v", count, err)
		}
		if err := lock.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := repo.CleanupMobileRecords(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRow(ctx, `SELECT count(*) FROM slider_captcha_challenges WHERE mobile_e164='+8613800138002'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("second batch remaining=%d err=%v", count, err)
		}
	})
}
