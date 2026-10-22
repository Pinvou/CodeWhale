package billingschema

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Historical input only: this alternate SQL is applied to an owned predecessor
// fixture to prove explicit refusal, never registered as supported production SQL.
//
//go:embed testdata/legacy016_alternate.up.sql
var alternate016FixtureSQL string

func TestBillingUpStaticValidationPrecedesDatabase(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"changed-body", "changed-checksum", "changed-name", "missing-016"} {
		t.Run(kind, func(t *testing.T) {
			input := append([]migrations.Migration(nil), loaded...)
			for i := range input {
				if input[i].Version != 16 {
					continue
				}
				switch kind {
				case "changed-body":
					input[i].UpSQL += "\nSELECT 1;"
				case "changed-checksum":
					input[i].Checksum = strings.Repeat("a", 64)
				case "changed-name":
					input[i].Name = "unrelated"
				case "missing-016":
					input = append(input[:i], input[i+1:]...)
				}
				break
			}
			// Invalid deliveries must be rejected before even reading a database.
			err := Up(context.Background(), nil, input)
			if err == nil || !strings.Contains(err.Error(), "016") {
				t.Fatalf("invalid016 delivery did not fail statically: %v", err)
			}
			if kind == "missing-016" && !errors.Is(err, ErrPublishedEntitlementMigrationRequired) {
				t.Fatalf("missing016 admission error changed: %v", err)
			}
		})
	}
}

func TestLegacyEntitlementAdmissionIdentity(t *testing.T) {
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"changed-body", "changed-checksum", "changed-name"} {
		t.Run(kind, func(t *testing.T) {
			input := append([]migrations.Migration(nil), loaded...)
			for i := range input {
				if input[i].Version == 16 {
					switch kind {
					case "changed-body":
						input[i].UpSQL += "\nSELECT 1;"
					case "changed-checksum":
						input[i].Checksum = strings.Repeat("a", 64)
					case "changed-name":
						input[i].Name = "unrelated"
					}
				}
			}
			called := false
			if err := withLegacyEntitlementAdmission(context.Background(), nil, input, func() error { called = true; return nil }); err == nil || called {
				t.Fatal("changed016 reached execution")
			}
		})
	}
	t.Run("prefix-does-not-claim-016-upgrade", func(t *testing.T) {
		var prefix []migrations.Migration
		for _, m := range loaded {
			if m.Version <= 15 || m.Version == generationVersion {
				prefix = append(prefix, m)
			}
		}
		called := false
		if err := withLegacyEntitlementAdmission(context.Background(), nil, prefix, func() error { called = true; return nil }); err != nil || !called {
			t.Fatal("actual <=015 prefix with original early fence changed", err)
		}
	})
	for _, kind := range []string{"complete-without016", "original017", "later-forward-fence", "unrelated-forward-fence"} {
		t.Run(kind, func(t *testing.T) {
			var input []migrations.Migration
			for _, m := range loaded {
				if m.Version == 16 {
					continue
				}
				if kind == "complete-without016" || (kind == "original017" && m.Version == 17) || (kind == "later-forward-fence" && m.Version == intentVersion) || (kind == "unrelated-forward-fence" && m.Version == generationVersion) {
					input = append(input, m)
				}
			}
			if kind == "unrelated-forward-fence" {
				input[0].Name = "unrelated"
			}
			called := false
			if err := withLegacyEntitlementAdmission(context.Background(), nil, input, func() error { called = true; return nil }); !errors.Is(err, ErrPublishedEntitlementMigrationRequired) || called {
				t.Fatal("later delivery missing016 reached execution", err)
			}
		})
	}
}

func TestLegacyEntitlementUpgradeAdmissionPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("actual dedicated PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/billing")
	if err != nil {
		t.Fatal(err)
	}
	prefix := func(max int64) []migrations.Migration {
		var out []migrations.Migration
		for _, m := range loaded {
			if m.Version <= max || m.Version == generationVersion {
				out = append(out, m)
			}
		}
		return out
	}
	original016 := func() migrations.Migration {
		for _, m := range loaded {
			if m.Version == 16 {
				return m
			}
		}
		t.Fatal("missing016")
		return migrations.Migration{}
	}
	create := func(t *testing.T) *pgx.Conn { return f.database(t, ctx, "billing", "billing_schema_owner") }
	connect := func(t *testing.T, c *pgx.Conn) *pgx.Conn {
		other, e := pgx.Connect(ctx, f.serviceURL(t, c, "billing_migrator"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { other.Close(context.Background()) })
		if _, e = other.Exec(ctx, `SET ROLE billing_service`); e != nil {
			t.Fatal(e)
		}
		return other
	}
	snapshot := func(t *testing.T, c *pgx.Conn) string {
		t.Helper()
		// Diagnostic read identity may inspect owner-controlled tables introduced
		// by an unsafe negative control; actual Up always uses billing_service.
		config := f.config.Copy()
		config.Database = c.Config().Database
		reader, e := pgx.ConnectConfig(ctx, config)
		if e != nil {
			t.Fatal(e)
		}
		defer reader.Close(context.Background())
		c = reader
		facts := stageShape(t, ctx, c)
		rows, e := c.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='public' AND c.relkind IN ('r','p') ORDER BY c.relname`)
		if e != nil {
			t.Fatal(e)
		}
		var tables []string
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				t.Fatal(e)
			}
			tables = append(tables, name)
		}
		if e = rows.Err(); e != nil {
			t.Fatal(e)
		}
		rows.Close()
		for _, name := range tables {
			facts += name + stageValue(t, ctx, c, `SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY to_jsonb(x)::text),'[]'::jsonb)::text FROM public.`+pgx.Identifier{name}.Sanitize()+` x`)
		}
		return facts
	}
	assertRejected := func(t *testing.T, c *pgx.Conn, want error) {
		t.Helper()
		before := snapshot(t, c)
		e := Up(ctx, c, loaded)
		if e == nil || (want != nil && !errors.Is(e, want)) {
			t.Errorf("unsupported upgrade not explicitly rejected: %v", e)
		}
		if after := snapshot(t, c); after != before {
			t.Fatal("rejected upgrade changed full schema, migration ledger or business facts")
		}
	}
	// Seed only immutable public<=015 schema. Every journal is balanced, and all
	// owner/account/lot/sign facts are retained byte-for-byte by the refusal.
	seed := func(t *testing.T, c *pgx.Conn, used bool) {
		t.Helper()
		tx, e := c.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		owner, wallet, account, lot := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		remaining, reserved := int64(1000), int64(0)
		if used {
			remaining = 700
			reserved = 300
		}
		if _, e = tx.Exec(ctx, `INSERT INTO wallets(id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,created_at,updated_at) VALUES($1,'PERSONAL',$2,'ACTIVE',$3,$4,$5,$5)`, wallet, owner, remaining, reserved, now); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO ledger_accounts(id,wallet_id,code,account_type,created_at) VALUES($1,$2,$3,'WALLET_LIABILITY',$4)`, account, wallet, "wallet:"+wallet.String(), now); e != nil {
			t.Fatal(e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO credit_lots(id,wallet_id,source_type,source_reference,original_microcredits,remaining_microcredits,refundable,expires_at,created_at) VALUES($1,$2,'RECHARGE',$3,1000,$4,true,NULL,$5)`, lot, wallet, "old:"+lot.String(), remaining, now); e != nil {
			t.Fatal(e)
		}
		facts := []struct {
			kind    string
			amount  int64
			counter string
		}{{"RECHARGE", 1000, "00000000-0000-7000-8000-000000000001"}}
		if used {
			facts = append(facts, struct {
				kind    string
				amount  int64
				counter string
			}{"USAGE", -200, "00000000-0000-7000-8000-000000000002"}, struct {
				kind    string
				amount  int64
				counter string
			}{"REFUND", -100, "00000000-0000-7000-8000-000000000003"})
		}
		for _, fact := range facts {
			entry := uuid.New()
			if _, e = tx.Exec(ctx, `INSERT INTO journal_entries(id,entry_type,business_reference,description,created_at) VALUES($1,$2,$3,'old immutable financial fact',$4)`, entry, fact.kind, "old-entry:"+entry.String(), now); e != nil {
				t.Fatal(e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO ledger_postings(id,journal_entry_id,ledger_account_id,delta_microcredits,credit_lot_id,created_at) VALUES($1,$2,$3,$4,$5,$6),($7,$2,$8,$9,NULL,$6)`, uuid.New(), entry, account, fact.amount, lot, now, uuid.New(), uuid.MustParse(fact.counter), -fact.amount); e != nil {
				t.Fatal(e)
			}
		}
		if used {
			if _, e = tx.Exec(ctx, `INSERT INTO usage_holds(id,usage_request_id,wallet_id,status,max_microcredits,expires_at,created_at,updated_at) VALUES($1,$2,$3,'REVIEW',300,$4,$5,$5)`, uuid.New(), uuid.New(), wallet, now.Add(time.Hour), now); e != nil {
				t.Fatal(e)
			}
		}
		if e = tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		var session, current string
		var super bool
		if e = c.QueryRow(ctx, `SELECT session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&session, &current, &super); e != nil || session != "billing_migrator" || current != "billing_service" || super {
			t.Fatal("real restricted migration fixture identity", e)
		}
		t.Logf("actual migration session=%s role=%s super=%v; original wallet/account/lot journal facts seeded", session, current, super)
	}
	t.Run("fresh-complete-public-bootstrap", func(t *testing.T) {
		c := create(t)
		if e := Up(ctx, c, loaded); e != nil {
			t.Fatal(e)
		}
		if _, e := VerifyApplied(ctx, c, loaded); e != nil {
			t.Fatal(e)
		}
		if got := stageValue(t, ctx, c, `SELECT checksum FROM schema_migrations WHERE version=16`); got != originalComponentMigrationChecksum {
			t.Fatal("original016 history changed")
		}
		other := connect(t, c)
		var acquired bool
		if e := other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || acquired {
			t.Fatal("generic migration lock released early", e)
		}
		migrations.Unlock(ctx, c)
		if e := other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || !acquired {
			t.Fatal("extra admission lock leaked after caller Unlock", e)
		}
		migrations.Unlock(ctx, other)
	})
	t.Run("fresh-empty-ledger-only", func(t *testing.T) {
		c := create(t)
		if e := migrations.Up(ctx, c, nil); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		if e := Up(ctx, c, loaded); e != nil {
			t.Fatal("empty ledger blocked fresh public bootstrap", e)
		}
		migrations.Unlock(ctx, c)
	})
	t.Run("prefix001-no-guarded-write", func(t *testing.T) {
		c := create(t)
		var one []migrations.Migration
		for _, m := range loaded {
			if m.Version == 1 {
				one = append(one, m)
			}
		}
		if e := migrations.Up(ctx, c, one); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		assertRejected(t, c, ErrLegacyEntitlementUpgradeUnsupported)
	})
	for _, max := range []int64{6, 15} {
		t.Run("existing-prefix-"+map[int64]string{6: "006-high-fence", 15: "015-zero-funds"}[max], func(t *testing.T) {
			c := create(t)
			if e := Up(ctx, c, prefix(max)); e != nil {
				t.Fatal(e)
			}
			migrations.Unlock(ctx, c)
			assertRejected(t, c, ErrLegacyEntitlementUpgradeUnsupported)
		})
	}
	t.Run("prefix015-consumed-null-review-facts-unchanged", func(t *testing.T) {
		c := create(t)
		if e := Up(ctx, c, prefix(15)); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		seed(t, c, true)
		assertRejected(t, c, ErrLegacyEntitlementUpgradeUnsupported)
		if got := stageValue(t, ctx, c, `SELECT jsonb_build_array(w.posted_microcredits,w.reserved_microcredits,l.original_microcredits,l.remaining_microcredits,l.expires_at,h.status,h.max_microcredits)::text FROM wallets w JOIN credit_lots l ON l.wallet_id=w.id JOIN usage_holds h ON h.wallet_id=w.id`); got != `[700, 300, 1000, 700, null, "REVIEW", 300]` {
			t.Fatal("old funds/hold or NULL changed", got)
		}
	})
	t.Run("untracked-public-business-schema-no-ledger-created", func(t *testing.T) {
		c := create(t)
		var one []migrations.Migration
		for _, m := range loaded {
			if m.Version == 1 {
				one = append(one, m)
			}
		}
		if e := migrations.Up(ctx, c, one); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		if _, e := c.Exec(ctx, `DROP TABLE schema_migrations`); e != nil {
			t.Fatal(e)
		}
		assertRejected(t, c, ErrLegacyEntitlementUpgradeUnsupported)
		if got := stageValue(t, ctx, c, `SELECT (to_regclass('public.schema_migrations') IS NULL)::text`); got != "true" {
			t.Fatal("refusal created ledger")
		}
	})
	for _, kind := range []string{"original-LF", "original-CRLF", "alternate-LF", "alternate-CRLF", "unknown-checksum", "wrong-name"} {
		t.Run(kind, func(t *testing.T) {
			c := create(t)
			if e := Up(ctx, c, prefix(15)); e != nil {
				t.Fatal(e)
			}
			migrations.Unlock(ctx, c)
			m := original016()
			if strings.HasPrefix(kind, "alternate") {
				seed(t, c, true)
				m.UpSQL = alternate016FixtureSQL
				m.Checksum = sqlChecksum(m.UpSQL)
			} else if strings.HasPrefix(kind, "original") {
				seed(t, c, false)
			}
			if strings.HasSuffix(kind, "CRLF") {
				m.UpSQL = strings.ReplaceAll(strings.ReplaceAll(m.UpSQL, "\r\n", "\n"), "\n", "\r\n")
				m.Checksum = raw016FixtureChecksum(m.UpSQL)
			}
			if kind == "unknown-checksum" {
				m.Checksum = strings.Repeat("a", 64)
			}
			if kind == "wrong-name" {
				m.Name = "unrelated"
			}
			// Recreate an actual older release's installation from its exact SQL. The
			// current Billing admission is deliberately not a supported old-upgrade path.
			if e := migrations.Up(ctx, c, []migrations.Migration{m}); e != nil {
				t.Fatal("historical016 installation fixture", e)
			}
			migrations.Unlock(ctx, c)
			if strings.HasPrefix(kind, "original") {
				before := stageValue(t, ctx, c, `SELECT jsonb_agg(jsonb_build_array(l.id,l.expires_at,p.valid_until) ORDER BY l.id)::text FROM credit_lots l JOIN entitlement_components c ON c.credit_lot_id=l.id JOIN entitlement_packages p ON p.id=c.package_id`)
				ledger := stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=16`)
				if e := Up(ctx, c, loaded); e != nil {
					t.Fatal("supported original016 upgrade", e)
				}
				migrations.Unlock(ctx, c)
				if before != stageValue(t, ctx, c, `SELECT jsonb_agg(jsonb_build_array(l.id,l.expires_at,p.valid_until) ORDER BY l.id)::text FROM credit_lots l JOIN entitlement_components c ON c.credit_lot_id=l.id JOIN entitlement_packages p ON p.id=c.package_id`) || ledger != stageValue(t, ctx, c, `SELECT to_jsonb(s)::text FROM schema_migrations s WHERE version=16`) {
					t.Fatal("already applied original016 expiry or history was guessed/reapplied")
				}
			} else {
				var want error
				if strings.HasPrefix(kind, "alternate") {
					want = ErrAlternateEntitlementHistoryUnsupported
				}
				assertRejected(t, c, want)
			}
		})
	}
	t.Run("later-delivery-missing-016-rejected-before-all-writes", func(t *testing.T) {
		c := create(t)
		if e := Up(ctx, c, prefix(15)); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		var incomplete []migrations.Migration
		for _, m := range loaded {
			if m.Version != 16 {
				incomplete = append(incomplete, m)
			}
		}
		before := snapshot(t, c)
		if e := Up(ctx, c, incomplete); !errors.Is(e, ErrPublishedEntitlementMigrationRequired) {
			t.Errorf("later delivery without published016 not rejected at static admission: %v", e)
		}
		if after := snapshot(t, c); after != before {
			t.Fatal("missing016 later delivery changed schema, ledger or business facts before refusal")
		}
	})
	t.Run("rejected-upgrade-preserves-caller-lock-layer", func(t *testing.T) {
		c := create(t)
		if e := Up(ctx, c, prefix(15)); e != nil {
			t.Fatal(e)
		} // Caller still owns the generic lock.
		assertRejected(t, c, ErrLegacyEntitlementUpgradeUnsupported)
		other := connect(t, c)
		var acquired bool
		if e := other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || acquired {
			t.Fatal("admission unlocked caller layer", e)
		}
		migrations.Unlock(ctx, c)
		if e := other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || !acquired {
			t.Fatal("refusal leaked its extra layer", e)
		}
		migrations.Unlock(ctx, other)
	})
	for _, kind := range []string{"cancelled-apply", "failed-apply"} {
		t.Run(kind, func(t *testing.T) {
			c := create(t)
			if _, e := c.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); e != nil {
				t.Fatal(e)
			}
			work, stop := context.WithCancel(ctx)
			defer stop()
			sentinel := errors.New("controlled apply failure")
			e := withLegacyEntitlementAdmission(work, c, loaded, func() error {
				if kind == "cancelled-apply" {
					stop()
					return context.Canceled
				}
				return sentinel
			})
			if (kind == "cancelled-apply" && !errors.Is(e, context.Canceled)) || (kind == "failed-apply" && !errors.Is(e, sentinel)) {
				t.Fatal(e)
			}
			other := connect(t, c)
			var acquired bool
			if e = other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || acquired {
				t.Fatal("failed/cancelled apply unlocked caller layer", e)
			}
			migrations.Unlock(ctx, c)
			if e = other.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || !acquired {
				t.Fatal("failed/cancelled apply leaked admission layer", e)
			}
			migrations.Unlock(ctx, other)
		})
	}
	t.Run("blocked-acquisition-cancel-keeps-other-owner", func(t *testing.T) {
		c := create(t)
		if e := Up(ctx, c, prefix(15)); e != nil {
			t.Fatal(e)
		}
		migrations.Unlock(ctx, c)
		owner := connect(t, c)
		if _, e := owner.Exec(ctx, `SELECT pg_advisory_lock(hashtext('pinvou_account_platform_migrations'))`); e != nil {
			t.Fatal(e)
		}
		before := snapshot(t, c)
		work, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		pid := c.PgConn().PID()
		go func() { done <- Up(work, c, loaded) }()
		blocked := false
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if e := f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND wait_event='advisory' AND query LIKE '%pg_advisory_lock%')`, pid).Scan(&blocked); e != nil {
				t.Fatal(e)
			}
			if blocked {
				break
			}
		}
		if !blocked {
			stop()
			<-done
			t.Fatal("did not observe actual blocked admission lock")
		}
		stop()
		select {
		case e := <-done:
			if e == nil {
				t.Fatal("cancelled lock acquisition succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancel did not release waiter")
		}
		if before != snapshot(t, owner) {
			t.Fatal("cancelled preflight changed schema/ledger/facts")
		}
		observer := connect(t, owner)
		var acquired bool
		if e := observer.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || acquired {
			t.Fatal("cancellation removed other session owner lock", e)
		}
		migrations.Unlock(ctx, owner)
		if e := observer.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('pinvou_account_platform_migrations'))`).Scan(&acquired); e != nil || !acquired {
			t.Fatal("cancelled waiter left admission layer", e)
		}
		migrations.Unlock(ctx, observer)
	})
}

func raw016FixtureChecksum(sql string) string {
	digest := sha256.Sum256([]byte(sql))
	return hex.EncodeToString(digest[:])
}
