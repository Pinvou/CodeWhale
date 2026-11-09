package billingschema

import (
	"context"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// c is a separate, newly leased database: never filter the input of a database
// which already installed newer migrations and pretend its ledger is historical.
func assertHistoricalBillingRollback(t *testing.T, ctx context.Context, c *pgx.Conn, full []migrations.Migration, version int64, code, message string) {
	t.Helper()
	var stage []migrations.Migration
	for _, m := range full {
		if m.Version > version {
			continue
		}
		// The published pre-activation-lock stage retains correction 60, but
		// excludes later business modules together with their owner capabilities.
		if version == 900012 && m.Version < 900000 && m.Version > 58 && m.Version != 60 {
			continue
		}
		stage = append(stage, m)
	}
	if _, err := Ordered(stage); err != nil {
		t.Fatal("invalid historical rollback stage", version, err)
	}
	if err := Up(ctx, c, stage); err != nil {
		t.Fatal("public historical rollback installation", version, err)
	}
	defer migrations.Unlock(ctx, c)
	if _, err := VerifyApplied(ctx, c, stage); err != nil {
		t.Fatal("verify historical rollback installation", err)
	}
	var maximum int64
	var actor bool
	if err := c.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&maximum); err != nil || maximum != version {
		t.Fatal("unexpected historical rollback boundary", maximum, version, err)
	}
	if err := c.QueryRow(ctx, `SELECT session_user='billing_migrator' AND current_user='billing_service' AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor); err != nil || !actor {
		t.Fatal("historical rollback deployment identity", actor, err)
	}
	if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
		t.Fatal("historical wallet and event facts", err)
	}
	facts := func() string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'outbox',(SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e))::text`)
	}
	ledger := func() string {
		return stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
	}
	beforeFacts, beforeLedger, beforeShape := facts(), ledger(), stageShape(t, ctx, c)
	err := DownOne(ctx, c, stage)
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != code || rejected.Message != message {
		t.Fatal("unexpected historical rollback refusal", version, err)
	}
	if _, err := VerifyApplied(ctx, c, stage); err != nil {
		t.Fatal("verify refused historical rollback", err)
	}
	if facts() != beforeFacts || ledger() != beforeLedger || stageShape(t, ctx, c) != beforeShape {
		t.Fatal("historical rollback refusal changed facts, ledger or schema", version)
	}
	if err := Up(ctx, c, full); err != nil {
		t.Fatal("historical rollback fixture current upgrade", err)
	}
	if facts() != beforeFacts {
		t.Fatal("current upgrade changed historical wallet/event facts")
	}
	assertBillingCurrentReplay(t, ctx, c, full)
	if facts() != beforeFacts {
		t.Fatal("current replay changed historical wallet/event facts")
	}
}

func assertBillingCurrentReplay(t *testing.T, ctx context.Context, c *pgx.Conn, full []migrations.Migration) {
	t.Helper()
	beforeShape := stageShape(t, ctx, c)
	beforeLedger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
	if err := Up(ctx, c, full); err != nil {
		t.Fatal("public current replay", err)
	}
	if _, err := VerifyApplied(ctx, c, full); err != nil {
		t.Fatal("verify public current replay", err)
	}
	migrations.Unlock(ctx, c)
	if stageShape(t, ctx, c) != beforeShape || stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) != beforeLedger {
		t.Fatal("current replay changed schema or ledger")
	}
}
