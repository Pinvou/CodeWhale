package billingschema

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func legacyExpiryInput(t *testing.T, through int64) []migrations.Migration {
	t.Helper()
	all, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	var input []migrations.Migration
	for _, m := range all {
		if m.Version <= through || m.Version == generationVersion || (through >= 20 && m.Version == memberIdentityVersion) {
			input = append(input, m)
		}
	}
	return input
}

// The root test owns the deployment actors through newStageFixture. Each
// subtest owns only its UUID database and never drops an unknown object.
func legacyExpiryDatabase(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual owned PostgreSQL with provisioned billing actors required")
	}
	admin, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("connect owned expiry fixture")
	}
	name := "legacy_expiry_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted+" OWNER billing_schema_owner"); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	var c *pgx.Conn
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		defer admin.Close(clean)
		if c != nil {
			c.Close(clean)
		}
		var owner string
		var active int
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := admin.QueryRow(clean, `SELECT pg_get_userbyid(datdba),(SELECT count(*) FROM pg_stat_activity WHERE datname=$1) FROM pg_database WHERE datname=$1`, name).Scan(&owner, &active); err != nil || owner != "billing_schema_owner" {
				t.Error("expiry fixture identity guard failed")
				return
			}
			if active == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if active != 0 {
			t.Error("expiry fixture still active; cleanup refused")
			return
		}
		if _, err := admin.Exec(clean, "DROP DATABASE "+quoted); err != nil {
			t.Error("expiry fixture cleanup failed", err)
		}
	})
	config, err := pgx.ParseConfig(raw)
	if err != nil {
		t.Fatal("parse expiry fixture")
	}
	config.Database = name
	config.User = "billing_migrator"
	config.Password = ""
	c, err = pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect limited expiry migrator")
	}
	if _, err := c.Exec(ctx, `SET ROLE billing_schema_owner; ALTER SCHEMA public OWNER TO billing_schema_owner; REVOKE CREATE ON SCHEMA public FROM PUBLIC; GRANT USAGE,CREATE ON SCHEMA public TO billing_service; SET ROLE billing_service`); err != nil {
		t.Fatal(err)
	}
	return c
}

func legacyExpiryFacts(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	return stageValue(t, ctx, c, `SELECT jsonb_build_object('journal',(SELECT COALESCE(jsonb_agg(to_jsonb(j) ORDER BY id),'[]') FROM journal_entries j),'postings',(SELECT COALESCE(jsonb_agg(to_jsonb(p) ORDER BY id),'[]') FROM ledger_postings p))::text`)
}

func seedLegacyJournalType(t *testing.T, ctx context.Context, c *pgx.Conn, entryType string) {
	t.Helper()
	id := uuid.New()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'original journal fact',now())`, id, entryType, "original-journal:"+id.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,created_at) SELECT gen_random_uuid(),$1,id,CASE WHEN row_number() OVER(ORDER BY id)=1 THEN -7 ELSE 7 END,now() FROM (SELECT id FROM ledger_accounts ORDER BY id LIMIT 2) accounts`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyExpiry025UpgradePreservesOriginalLedgerPostgres(t *testing.T) {
	// This root runs in the owner-schema phase, before the retained runner
	// provisions any Billing actors. Reuse the absent-actor fixture's identity
	// checks and nonforce cleanup; never borrow a developer's preinstalled roles.
	actorContext, stopActors := context.WithTimeout(context.Background(), 20*time.Minute)
	defer stopActors()
	newStageFixture(t, actorContext, os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL"))
	for through := int64(16); through <= 24; through++ {
		for _, legacy := range []struct {
			name      string
			types     []string
			hasExpiry bool
		}{
			{"NONE", nil, false},
			{"EXPIRE", []string{"EXPIRE"}, true},
			{"REPAIR_CREDIT_ONLY", []string{"REPAIR_CREDIT"}, false},
			{"EXPIRE_AND_REPAIR_CREDIT", []string{"EXPIRE", "REPAIR_CREDIT"}, true},
		} {
			t.Run(fmt.Sprintf("original%03d/%s", through, legacy.name), func(t *testing.T) {
				old := len(legacy.types) > 0
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				c := legacyExpiryDatabase(t, ctx)
				if err := Up(ctx, c, legacyExpiryInput(t, through)); err != nil {
					t.Fatal(err)
				}
				for _, entryType := range legacy.types {
					seedLegacyJournalType(t, ctx, c, entryType)
				}
				facts := legacyExpiryFacts(t, ctx, c)
				ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
				input := legacyExpiryInput(t, 25)
				if err := Up(ctx, c, input); err != nil {
					t.Fatal(err)
				}
				if facts != legacyExpiryFacts(t, ctx, c) {
					t.Fatal("posted expiry journal or postings were rewritten")
				}
				if ledger != stageValue(t, ctx, c, fmt.Sprintf(`SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<=%d OR version=900001 OR (version=900011 AND %d>=20)`, through, through)) {
					t.Fatal("original migration history was rewritten")
				}
				var validated bool
				if err := c.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='journal_entries'::regclass AND conname='journal_entries_entry_type_check'`).Scan(&validated); err != nil || validated == old {
					t.Fatalf("conditional validation=%v old=%v err=%v", validated, old, err)
				}
				var installed string
				if err := c.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=25`).Scan(&installed); err != nil || installed != legacyExpiryChecksum {
					t.Fatalf("025 ledger identity=%s err=%v", installed, err)
				}
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'EXPIRE',$2,'new forbidden spelling',now())`, uuid.New(), uuid.NewString()); err == nil {
					t.Fatal("new EXPIRE accepted")
				} else {
					var pg *pgconn.PgError
					if !errorsAsCheck(err, &pg) {
						t.Fatalf("expected CHECK rejection: %v", err)
					}
				}
				// NOT VALID only preserves old rows. It must not allow new rows
				// of a type excluded by the exact frozen 025 contract.
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'REPAIR_CREDIT',$2,'new repair before 060',now())`, uuid.New(), uuid.NewString()); err == nil {
					t.Fatal("025 accepted new REPAIR_CREDIT")
				} else {
					var pg *pgconn.PgError
					if !errorsAsCheck(err, &pg) {
						t.Fatalf("025 repair CHECK rejection missing: %v", err)
					}
				}
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'EXPIRY',$2,'current expiry spelling',now())`, uuid.New(), uuid.NewString()); err != nil {
					t.Fatal("new EXPIRY rejected", err)
				}
				if err := Up(ctx, c, input); err != nil {
					t.Fatal("replay", err)
				}
				// 060 replaces the CHECK again. A successful 025 boundary alone
				// is not a successful upgrade to the delivered application.
				full, err := migrations.Load("../../migrations/billing")
				if err != nil {
					t.Fatal(err)
				}
				beforeFullFacts := legacyExpiryFacts(t, ctx, c)
				beforeFullHistory := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
				if err := Up(ctx, c, full); err != nil {
					t.Fatal("complete delivered upgrade", err)
				}
				if _, err := VerifyApplied(ctx, c, full); err != nil {
					t.Fatal("complete delivered verification", err)
				}
				if beforeFullFacts != legacyExpiryFacts(t, ctx, c) || beforeFullHistory != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<=25 OR version IN (900001,900011)`) {
					t.Fatal("full upgrade rewrote original financial facts or installed migration records")
				}
				if err := c.QueryRow(ctx, `SELECT convalidated FROM pg_constraint WHERE conrelid='journal_entries'::regclass AND conname='journal_entries_entry_type_check'`).Scan(&validated); err != nil || validated == legacy.hasExpiry {
					t.Fatalf("final conditional validation=%v oldExpiry=%v err=%v", validated, legacy.hasExpiry, err)
				}
				if err := c.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=60`).Scan(&installed); err != nil || installed != repairedExpiryChecksum {
					t.Fatalf("060 ledger identity=%s err=%v", installed, err)
				}
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'EXPIRE',$2,'forbidden after complete upgrade',now())`, uuid.New(), uuid.NewString()); err == nil {
					t.Fatal("full upgrade accepted new EXPIRE")
				} else {
					var pg *pgconn.PgError
					if !errorsAsCheck(err, &pg) {
						t.Fatalf("final CHECK rejection missing: %v", err)
					}
				}
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'EXPIRY',$2,'current expiry after complete upgrade',now())`, uuid.New(), uuid.NewString()); err != nil {
					t.Fatal("full upgrade rejected EXPIRY", err)
				}
				// Exact 060 restores REPAIR_CREDIT. A repair-only old database
				// needs a normally validated CHECK here, unlike EXPIRE history.
				if _, err := c.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,'REPAIR_CREDIT',$2,'current repair after 060',now())`, uuid.New(), uuid.NewString()); err != nil {
					t.Fatal("full upgrade rejected REPAIR_CREDIT", err)
				}
				finalFacts := legacyExpiryFacts(t, ctx, c)
				finalHistory := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
				if err := Up(ctx, c, full); err != nil {
					t.Fatal("complete delivered replay", err)
				}
				if finalFacts != legacyExpiryFacts(t, ctx, c) || finalHistory != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) {
					t.Fatal("full migration replay changed financial facts or migration records")
				}
			})
		}
	}
}

func errorsAsCheck(err error, target **pgconn.PgError) bool {
	return errors.As(err, target) && (*target).Code == "23514"
}

func TestLegacyExpiry025IdentityRejectedBeforeConnection(t *testing.T) {
	for _, version := range []int64{25, 60} {
		for _, field := range []string{"version", "name", "checksum", "sql", "sql-and-checksum"} {
			t.Run(fmt.Sprintf("%03d/%s", version, field), func(t *testing.T) {
				input := legacyExpiryInput(t, version)
				for i := range input {
					if input[i].Version == version {
						switch field {
						case "version":
							input[i].Version = version + 1
						case "name":
							input[i].Name = "different"
						case "checksum":
							input[i].Checksum = strings.Repeat("a", 64)
						case "sql":
							input[i].UpSQL += "\n-- changed"
						case "sql-and-checksum":
							input[i].UpSQL += "\n-- changed"
							input[i].Checksum = sqlChecksum(input[i].UpSQL)
						}
					}
				}
				if err := Up(context.Background(), nil, input); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%03d expiry compatibility", version)) {
					t.Fatalf("identity changed without early rejection: %v", err)
				}
			})
		}
	}
}
