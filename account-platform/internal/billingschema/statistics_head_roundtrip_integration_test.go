package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const statisticsHeadVersion int64 = 900046
const statisticsHeadDefinition = "CREATE INDEX outbox_events_dead_statistics_idx ON public.outbox_events USING btree (event_type) WHERE (dead_lettered_at IS NOT NULL)"
const retainedStatisticsLedger = `SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY version),'[]'::jsonb)::text FROM schema_migrations m WHERE version<>900046`
const usageWaiverEvidenceFacts = `SELECT jsonb_build_object('commands',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_reference),'[]'::jsonb) FROM usage_waiver_commands x),'results',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_reference),'[]'::jsonb) FROM usage_waiver_results x),'rejections',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_reference),'[]'::jsonb) FROM usage_waiver_rejections x),'claims',(SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY hold_id),'[]'::jsonb) FROM usage_waiver_hold_claims x))::text`
const walletReceiptEvidenceFacts = `SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY command_reference),'[]'::jsonb)::text FROM token_account_wallet_provisioning_receipts x`
const statisticsOutboxFacts = `SELECT coalesce(jsonb_agg(to_jsonb(x) ORDER BY event_id),'[]'::jsonb)::text FROM outbox_events x`

// stageShape aggregates TEXT facts: first decode the outer strings, then each
// fact's JSON array. Only the three facts belonging to this exact index may go.
func shapeWithoutStatisticsIndex(t *testing.T, shape string) string {
	t.Helper()
	var facts []string
	if err := json.Unmarshal([]byte(shape), &facts); err != nil {
		t.Fatal(err)
	}
	kept := make([]string, 0, len(facts))
	removed := map[string]int{}
	for _, fact := range facts {
		var fields []json.RawMessage
		if err := json.Unmarshal([]byte(fact), &fields); err != nil || len(fields) < 3 {
			t.Fatal("invalid stage shape fact", err)
		}
		text := func(i int) string {
			var value string
			if err := json.Unmarshal(fields[i], &value); err != nil {
				t.Fatal("invalid stage shape identity", err)
			}
			return value
		}
		kind, schema, name := text(0), text(1), text(2)
		drop := schema == "public" && name == "outbox_events_dead_statistics_idx" && (kind == "relation" || kind == "column")
		if kind == "index" && schema == "public" && name == "outbox_events" && len(fields) == 6 && text(3) == statisticsHeadDefinition {
			drop = true
		}
		if drop {
			removed[kind]++
		} else {
			kept = append(kept, fact)
		}
	}
	if removed["relation"] != 1 || removed["column"] != 1 || removed["index"] != 1 || len(removed) != 3 {
		t.Fatal("statistics shape projection did not select exactly its three facts", removed)
	}
	encoded, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func canonicalStageShape(t *testing.T, shape string) string {
	t.Helper()
	var facts []string
	if err := json.Unmarshal([]byte(shape), &facts); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func assertStatisticsHeadIndex(t *testing.T, ctx context.Context, c *pgx.Conn) {
	t.Helper()
	if stageValue(t, ctx, c, `SELECT (name='outbox_dead_statistics_index' AND checksum='fdb45f949ce1fe97935022f6ffe057003243ba5b5cd1c6e863ffcb32cf1fed40')::text FROM schema_migrations WHERE version=900046`) != "true" {
		t.Fatal("statistics head ledger identity differs")
	}
	var definition, owner string
	var valid bool
	if err := c.QueryRow(ctx, `SELECT pg_get_indexdef(i.indexrelid),pg_get_userbyid(x.relowner),i.indisvalid AND i.indisready FROM pg_index i JOIN pg_class x ON x.oid=i.indexrelid WHERE i.indexrelid=to_regclass('public.outbox_events_dead_statistics_idx') AND i.indrelid='public.outbox_events'::regclass`).Scan(&definition, &owner, &valid); err != nil || definition != statisticsHeadDefinition || owner != "billing_schema_owner" || !valid {
		t.Fatal("statistics head index identity differs", definition, owner, valid, err)
	}
}

func statisticsRollbackPrefix(full []migrations.Migration) []migrations.Migration {
	var prefix []migrations.Migration
	for _, m := range full {
		if m.Version <= statisticsHeadVersion {
			prefix = append(prefix, m)
		}
	}
	return prefix
}

// Callers install the actual 900046 head before checking its index rollback.
// They release their
// own Up locks before entering; each operation here releases only its own lock.
func testStatisticsHeadRoundTrip(t *testing.T, ctx context.Context, c *pgx.Conn, full []migrations.Migration, factsSQL string) {
	t.Helper()
	loaded := statisticsRollbackPrefix(full)
	if head := stageValue(t, ctx, c, `SELECT max(version)::text FROM schema_migrations`); head != "900046" {
		t.Fatal("statistics roundtrip requires the explicit current head", head)
	}
	if _, err := VerifyApplied(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	assertStatisticsHeadIndex(t, ctx, c)
	shape := stageShape(t, ctx, c)
	withoutIndex := shapeWithoutStatisticsIndex(t, shape)
	ledger := stageValue(t, ctx, c, retainedStatisticsLedger)
	facts := stageValue(t, ctx, c, factsSQL)
	outbox := stageValue(t, ctx, c, statisticsOutboxFacts)
	assertBillingCurrentReplay(t, ctx, c, loaded)
	if facts != stageValue(t, ctx, c, factsSQL) || outbox != stageValue(t, ctx, c, statisticsOutboxFacts) {
		t.Fatal("current replay changed retained business facts")
	}
	err := DownOne(ctx, c, loaded)
	unlockErr := migrations.Unlock(ctx, c)
	if err != nil || unlockErr != nil {
		t.Fatal("public statistics Down", err, unlockErr)
	}
	if stageValue(t, ctx, c, `SELECT (to_regclass('public.outbox_events_dead_statistics_idx') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=900046))::text`) != "true" || withoutIndex != canonicalStageShape(t, stageShape(t, ctx, c)) || ledger != stageValue(t, ctx, c, retainedStatisticsLedger) || facts != stageValue(t, ctx, c, factsSQL) || outbox != stageValue(t, ctx, c, statisticsOutboxFacts) {
		t.Fatal("statistics Down changed retained schema, authority, ledger or business facts")
	}
	var restoredLedger string
	for replay := 0; replay < 2; replay++ {
		err = Up(ctx, c, loaded)
		unlockErr = migrations.Unlock(ctx, c)
		if err != nil || unlockErr != nil {
			t.Fatal("complete public statistics restore/replay", err, unlockErr)
		}
		if _, err := VerifyApplied(ctx, c, loaded); err != nil {
			t.Fatal("complete restored identity", err)
		}
		assertStatisticsHeadIndex(t, ctx, c)
		if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, retainedStatisticsLedger) || facts != stageValue(t, ctx, c, factsSQL) || outbox != stageValue(t, ctx, c, statisticsOutboxFacts) {
			t.Fatal("complete restore/replay changed original schema, authority, ledger or business facts")
		}
		currentLedger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
		if replay == 0 {
			restoredLedger = currentLedger
		} else if restoredLedger != currentLedger {
			t.Fatal("idempotent replay changed the restored ledger")
		}
	}
	const prefixLedgerSQL = "SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version <= 900046"
	prefixLedger := stageValue(t, ctx, c, prefixLedgerSQL)
	err = Up(ctx, c, full)
	unlockErr = migrations.Unlock(ctx, c)
	if err != nil || unlockErr != nil {
		t.Fatal("full public upgrade after statistics restore", err, unlockErr)
	}
	if _, err := VerifyApplied(ctx, c, full); err != nil {
		t.Fatal("full public upgrade identity", err)
	}
	assertStatisticsHeadIndex(t, ctx, c)
	assertBillingCurrentReplay(t, ctx, c, full)
	if prefixLedger != stageValue(t, ctx, c, prefixLedgerSQL) || facts != stageValue(t, ctx, c, factsSQL) || outbox != stageValue(t, ctx, c, statisticsOutboxFacts) {
		t.Fatal("full upgrade or replay changed statistics-era ledger or business facts")
	}
}

// Install a genuine prefix in a fresh owned database, never manufacture an old
// head by editing a current ledger or merely shortening Down's input package.
func testHistoricalEvidenceRefusal(t *testing.T, ctx context.Context, c *pgx.Conn, loaded []migrations.Migration, version int64, message, factsSQL string) {
	t.Helper()
	var prefix []migrations.Migration
	for _, m := range loaded {
		if m.Version <= version {
			prefix = append(prefix, m)
		}
	}
	if err := Up(ctx, c, prefix); err != nil {
		t.Fatal("historical public installation", err)
	}
	if err := migrations.Unlock(ctx, c); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err := c.QueryRow(ctx, `SELECT max(version) FROM schema_migrations`).Scan(&head); err != nil || head != version {
		t.Fatal("historical refusal has the wrong installed head", head, err)
	}
	if _, err := VerifyApplied(ctx, c, prefix); err != nil {
		t.Fatal(err)
	}
	var actor bool
	if err := c.QueryRow(ctx, `SELECT session_user='billing_migrator' AND current_user='billing_service' AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&actor); err != nil || !actor {
		t.Fatal("historical rollback deployment identity", actor, err)
	}
	if _, err := c.Exec(ctx, stageBillingSeed); err != nil {
		t.Fatal("historical wallet and event facts", err)
	}
	walletEvents := func() string {
		return stageValue(t, ctx, c, `SELECT jsonb_build_object('wallets',(SELECT jsonb_agg(to_jsonb(w) ORDER BY id) FROM wallets w),'outbox',(SELECT jsonb_agg(to_jsonb(e) ORDER BY event_id) FROM outbox_events e))::text`)
	}
	shape := stageShape(t, ctx, c)
	ledger := stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`)
	facts := stageValue(t, ctx, c, factsSQL)
	seededFacts := walletEvents()
	err := DownOne(ctx, c, prefix)
	unlockErr := migrations.Unlock(ctx, c)
	var rejected *pgconn.PgError
	if !errors.As(err, &rejected) || rejected.Code != "P0001" || rejected.Message != message || unlockErr != nil {
		t.Fatal("historical forward-only refusal differs", err, unlockErr)
	}
	if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) || facts != stageValue(t, ctx, c, factsSQL) || seededFacts != walletEvents() {
		t.Fatal("refused historical Down changed persistent facts")
	}
	if err := Up(ctx, c, prefix); err != nil {
		t.Fatal("replay after refused historical Down", err)
	}
	if err := migrations.Unlock(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyApplied(ctx, c, prefix); err != nil {
		t.Fatal(err)
	}
	if shape != stageShape(t, ctx, c) || ledger != stageValue(t, ctx, c, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`) || facts != stageValue(t, ctx, c, factsSQL) || seededFacts != walletEvents() {
		t.Fatal("historical replay changed persistent facts")
	}
	if err := Up(ctx, c, loaded); err != nil {
		t.Fatal("complete upgrade after historical refusal", err)
	}
	if err := migrations.Unlock(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyApplied(ctx, c, loaded); err != nil {
		t.Fatal(err)
	}
	var versions []int64
	for _, m := range prefix {
		versions = append(versions, m.Version)
	}
	var retained string
	if err := c.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version=ANY($1)`, versions).Scan(&retained); err != nil || retained != ledger || facts != stageValue(t, ctx, c, factsSQL) || seededFacts != walletEvents() {
		t.Fatal("complete upgrade changed historical ledger or evidence", err)
	}
	assertBillingCurrentReplay(t, ctx, c, loaded)
	if seededFacts != walletEvents() || facts != stageValue(t, ctx, c, factsSQL) {
		t.Fatal("current replay changed historical billing facts")
	}
}
