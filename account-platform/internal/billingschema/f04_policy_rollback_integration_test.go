package billingschema

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Numeric-only legacy fixtures expose the original lower versions. A normal
// current deployment retains its high-numbered forward-only migration fences.
func TestF04RetainedAccountPolicyRollbackPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive owned PostgreSQL schema fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	loaded, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	legacy := func(through int64) *pgx.Conn {
		t.Helper()
		c := f.database(t, ctx, "account", "account_service")
		var numeric []migrations.Migration
		for _, m := range loaded {
			if m.Version <= through {
				numeric = append(numeric, m)
			}
		}
		if err := migrations.Up(ctx, c, numeric); err != nil {
			t.Fatal("original numeric-only legacy setup", err)
		}
		return c
	}
	policies := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(p) ORDER BY command_type,policy_version)::text FROM approval_command_policies p`)
	}
	ledger := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
	}
	trigger := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('definition',pg_get_triggerdef(oid),'enabled',tgenabled,'function',tgfoid::text)::text FROM pg_trigger WHERE tgrelid='approval_command_policies'::regclass AND tgname='approval_command_policies_immutable'`)
	}
	identity := func(c *pgx.Conn) string {
		return stageValue(t, ctx, c, `SELECT session_user||':'||current_user`)
	}
	for _, scenario := range []struct {
		version  int64
		commands []string
	}{
		{24, []string{"TOKEN_MODEL_DRAFT_SAVE", "TOKEN_MODEL_PUBLISH", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_SYNC", "TOKEN_MODEL_CATALOG_ROLLBACK"}},
		{25, []string{"TOKEN_ROUTE_PROVISIONING_REPAIR", "TOKEN_ORGANIZATION_MODEL_POLICY_UPDATE"}},
		{26, []string{"TOKEN_ACCOUNT_CREATE"}},
	} {
		t.Run(fmt.Sprintf("policy-%d-public-deny-no-effects", scenario.version), func(t *testing.T) {
			c := legacy(scenario.version)
			var count int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM approval_command_policies WHERE policy_version=1 AND command_type=ANY($1)`, scenario.commands).Scan(&count); err != nil || count != len(scenario.commands) {
				t.Fatal("original command policy rows were not installed")
			}
			beforePolicy, beforeLedger, beforeTrigger := policies(c), ledger(c), trigger(c)
			beforeShape, beforeIdentity := stageShape(t, ctx, c), identity(c)
			assertUnchanged := func() {
				t.Helper()
				if policies(c) != beforePolicy || ledger(c) != beforeLedger || trigger(c) != beforeTrigger || stageShape(t, ctx, c) != beforeShape || identity(c) != beforeIdentity {
					t.Fatal("rejected rollback changed policy, ledger, trigger, schema or role identity")
				}
			}
			// The frozen SQL already refuses this statement. This controlled
			// original runner is contrast evidence, not an operator bypass.
			var pgErr *pgconn.PgError
			if err := migrations.DownOne(ctx, c, loaded); !errors.As(err, &pgErr) || pgErr.Code != "55000" || !strings.Contains(pgErr.Message, "append-only") {
				t.Fatalf("original statement-level immutable SQL outcome: %v", err)
			}
			assertUnchanged()
			if err := accountschema.DownOne(ctx, c, loaded); err == nil || !strings.Contains(err.Error(), "append-only policy history") {
				t.Fatalf("public Account invariant guard outcome: %v", err)
			}
			assertUnchanged()
		})
	}
	t.Run("existing-31-empty-history-positive", func(t *testing.T) {
		c := legacy(31)
		if stageValue(t, ctx, c, `SELECT count(*)::text FROM manual_review_cases`) != "0" {
			t.Fatal("positive control requires no retained manual-review history")
		}
		beforePolicy, beforeTrigger, beforeIdentity := policies(c), trigger(c), identity(c)
		remaining := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>31`)
		if err := accountschema.DownOne(ctx, c, loaded); err != nil {
			t.Fatal("unrelated existing original31 rollback", err)
		}
		if ledger(c) != remaining || policies(c) != beforePolicy || trigger(c) != beforeTrigger || identity(c) != beforeIdentity || stageValue(t, ctx, c, `SELECT (to_regclass('manual_review_cases') IS NULL)::text`) != "true" {
			t.Fatal("unrelated rollback changed policy/trigger/role or failed its original effect")
		}
	})
}
