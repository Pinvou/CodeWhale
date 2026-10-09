package account

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Both production workers select the same overdue row before either can update
// it. The real row lock supplies the barrier; no production hook or sleep makes
// the second caller's decision for it.
func TestManualReviewConcurrentEscalationIsOncePerWindow(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is required for actual PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("owned database connection failed")
	}
	defer c.Close(context.Background())
	schema := "sla_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = c.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal("owned schema create failed")
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := c.Exec(cleanup, "DROP SCHEMA "+quoted+" CASCADE"); err != nil {
			t.Error("owned schema cleanup failed")
		}
		var exists bool
		if err := c.QueryRow(cleanup, `SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)`, schema).Scan(&exists); err != nil || exists {
			t.Error("owned schema absence unconfirmed")
		}
	}()
	if _, err = c.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal("owned schema selection failed")
	}
	// Install the unchanged table contracts used by this repository operation,
	// rather than claiming that this finite fixture runs the full migration set.
	foundation, err := os.ReadFile("../../migrations/account/000001_foundation.up.sql")
	if err != nil {
		t.Fatal("outbox contract read failed")
	}
	end := strings.Index(string(foundation), "CREATE TABLE inbox_messages")
	if end < 0 {
		t.Fatal("outbox contract boundary missing")
	}
	manual, err := os.ReadFile("../../migrations/account/000031_manual_review_sla.up.sql")
	if err != nil {
		t.Fatal("manual review contract read failed")
	}
	if _, err = c.Exec(ctx, string(foundation[:end])+string(manual)); err != nil {
		t.Fatal("finite unchanged table contracts failed")
	}
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		t.Fatal("owned pool input invalid")
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["application_name"] = schema
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("owned pool create failed")
	}
	defer pool.Close()
	now := time.Now().UTC()
	r := &PostgresRepository{pool: pool, now: func() time.Time { return now }}
	caseID := uuid.New()
	if _, err = c.Exec(ctx, `INSERT INTO manual_review_cases(id,domain,case_type,resource_type,resource_id,severity,status,created_at,claim_due_at,first_response_due_at,mitigation_due_at,resolution_due_at,updated_at) VALUES($1,'ACCOUNT','PROFILE_CHANGE_REPAIR','PROFILE_CHANGE_REQUEST',$2,'P2','OPEN',$3,$4,$5,$5,$5,$3)`, caseID, uuid.New(), now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour)); err != nil {
		t.Fatal("owned overdue case seed failed")
	}
	schedule, err := manualreview.ParseOnCallSchedule(fmt.Sprintf("%s,%s", uuid.New(), uuid.New()), true)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := c.Begin(ctx)
	if err != nil {
		t.Fatal("owned row gate failed")
	}
	defer gate.Rollback(context.Background())
	if _, err = gate.Exec(ctx, `SELECT id FROM manual_review_cases WHERE id=$1 FOR UPDATE`, caseID); err != nil {
		t.Fatal("owned row lock failed")
	}
	type result struct {
		count int
		err   error
	}
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { n, e := r.EscalateOverdueManualReviews(ctx, schedule, 100); results <- result{n, e} }()
	}
	waiting := 0
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if err = pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'UPDATE manual_review_cases SET escalation_level=%'`, schema).Scan(&waiting); err != nil {
			t.Fatal("owned worker observation failed")
		}
		if waiting == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waiting != 2 {
		t.Fatal("both actual workers did not reach the contested update")
	}
	if err = gate.Commit(ctx); err != nil {
		t.Fatal("owned row gate release failed")
	}
	accepted := 0
	for i := 0; i < 2; i++ {
		v := <-results
		if v.err != nil {
			t.Fatal("actual escalation failed")
		}
		accepted += v.count
	}
	var level, assignments, evidence, deliveries int
	if err = c.QueryRow(ctx, `SELECT escalation_level,(SELECT count(*) FROM manual_review_assignments WHERE case_id=$1),(SELECT count(*) FROM manual_review_case_events WHERE case_id=$1 AND event_type='SLA_ESCALATED'),(SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.manual-review.sla-escalated.v1') FROM manual_review_cases WHERE id=$1`, caseID).Scan(&level, &assignments, &evidence, &deliveries); err != nil {
		t.Fatal("actual escalation facts read failed")
	}
	if accepted != 1 || level != 1 || assignments != 1 || evidence != 1 || deliveries != 1 {
		t.Fatalf("SLA_DUPLICATE_ESCALATION accepted=%d level=%d assignments=%d evidence=%d deliveries=%d", accepted, level, assignments, evidence, deliveries)
	}
	if n, err := r.EscalateOverdueManualReviews(ctx, schedule, 100); err != nil || n != 0 {
		t.Fatal("same-window retry escalated again")
	}
}
