package account

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProviderQuotaConcurrentBoundary(t *testing.T) {
	for _, kind := range []string{"send", "verify"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
			defer cancel()
			raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
			if raw == "" {
				t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for actual PostgreSQL")
			}
			c, err := pgx.Connect(ctx, raw)
			if err != nil {
				t.Fatal("private database connect failed")
			}
			defer c.Close(context.Background())
			schema := "ind_quota_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			quoted := pgx.Identifier{schema}.Sanitize()
			if _, err = c.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
				t.Fatal("create owned schema failed")
			}
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
				defer stop()
				if _, err := c.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
					t.Error("owned schema cleanup failed")
				}
				var remains bool
				if err := c.QueryRow(cleanup, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", schema).Scan(&remains); err != nil || remains {
					t.Error("owned schema absence unconfirmed")
				}
			}()
			if _, err = c.Exec(ctx, "SET search_path TO "+quoted); err != nil {
				t.Fatal("owned schema selection failed")
			}
			installed, err := migrations.Load("../../migrations/account")
			if err != nil {
				t.Fatal("load installed migrations failed")
			}
			if err = accountschema.Up(ctx, c, installed); err != nil {
				t.Fatal("actual account migration failed")
			}
			cfg, err := pgxpool.ParseConfig(raw)
			if err != nil {
				t.Fatal("pool input invalid")
			}
			cfg.ConnConfig.RuntimeParams["search_path"] = schema
			cfg.ConnConfig.RuntimeParams["application_name"] = schema
			cfg.MaxConns = 4
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal("owned pool failed")
			}
			defer pool.Close()
			now := time.Now().UTC()
			repo := &PostgresRepository{pool: pool, now: func() time.Time { return now }}
			provider := "independent-fixture-provider"
			table := "sms_send_attempts"
			limit := 100
			if kind == "verify" {
				table = "sms_verification_attempts"
				limit = 300
			}
			for i := 0; i < limit-1; i++ {
				q := `INSERT INTO ` + table + `(id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at) VALUES($1,decode(repeat('11',32),'hex'),decode(repeat('22',32),'hex'),decode(repeat('33',32),'hex'),'LOGIN',$2,'ACCEPTED',$3)`
				if _, err = c.Exec(ctx, q, uuid.New(), provider, now); err != nil {
					t.Fatal("owned boundary seed failed")
				}
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
				if err = c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event='advisory'`, schema).Scan(&waiting); err != nil {
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
				if e == nil {
					accepted++
				} else if errors.Is(e, ErrSMSRateLimited) {
					limited++
				} else {
					t.Fatal("unexpected actual repository outcome")
				}
			}
			var total int
			if err = c.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE provider_id=$1 AND result='ACCEPTED'`, provider).Scan(&total); err != nil {
				t.Fatal("read actual admission facts failed")
			}
			if accepted != 1 || limited != 1 || total != limit {
				t.Fatalf("PROVIDER_QUOTA_OVERSHOOT accepted=%d limited=%d total=%d limit=%d", accepted, limited, total, limit)
			}
		})
	}
}
