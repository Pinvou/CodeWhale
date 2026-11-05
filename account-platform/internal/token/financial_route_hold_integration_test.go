package token

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"strings"
	"sync"
	"testing"
	"time"
)

type financialRouteFixture struct {
	r                                                         *PostgresRepository
	ctx                                                       context.Context
	account, model, route, credential, user, wallet, protocol uuid.UUID
	capability                                                uuid.UUID
	catalog                                                   int64
}

func financialRouteSetup(t *testing.T) financialRouteFixture {
	t.Helper()
	r, ctx := retainedManagementTokenRepository(t)
	f := financialRouteFixture{r: r, ctx: ctx, account: uuid.New(), model: uuid.New(), route: uuid.New(), credential: uuid.New(), user: uuid.New(), wallet: uuid.New(), protocol: uuid.MustParse("00000000-0000-7000-8200-000000000000")}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := r.pool.Exec(ctx, q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec(`INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'controlled','ACTIVE',100,$2,now(),now(),'PLATFORM')`, f.account, f.user, f.wallet)
	exec(`INSERT INTO token_share_policies(token_account_id,scope,updated_at,updated_by) VALUES($1,'OWNER_UNIT_ONLY',now(),$2)`, f.account, f.user)
	encrypted, e := r.secrets.Encrypt([]byte("controlled-financial-hold-secret"), []byte(f.account.String()+":"+f.credential.String()))
	if e != nil {
		t.Fatal(e)
	}
	exec(`INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validation_status,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],'VALID',$6,now())`, f.credential, f.account, encrypted, r.secrets.KeyID(), strings.Repeat("a", 64), f.user)
	exec(`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'controlled','new-api-v1','model',$2,$3,now())`, f.model, digestString([]byte("model")), f.user)
	exec(`INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,health_observed_at,health_expires_at,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status) VALUES($1,$2,'model','CHAT',100,'NONE','HEALTHY',now(),now()+interval '1 minute',now(),$3,1,$4,$5,'RESOLVED')`, f.route, f.account, f.user, f.model, f.protocol)
	exec(`INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($1,'PERSONAL',$2,$3,$4,'model','CHAT',$5,1,1,$6,'READY','VERIFIED',1,'{}',8,now()+interval '1 hour',$7,$8,$2,now(),now())`, uuid.New(), f.user, f.wallet, f.model, f.protocol, strings.Repeat("b", 64), f.account, f.credential)
	f.capability = uuid.New()
	exec(`INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, f.capability, f.model, f.protocol, f.user)
	if e := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled financial hold',$2,now(),now()) RETURNING version`, uuid.New(), f.user).Scan(&f.catalog); e != nil {
		t.Fatal(e)
	}
	return f
}
func (f financialRouteFixture) request() UsageRequest {
	generation := int64(1)
	return UsageRequest{CatalogVersion: &f.catalog, CapabilityVersionID: &f.capability, ProtocolDefinitionID: &f.protocol, RouteGeneration: &generation, ModelSnapshot: []byte(`{"controlled":true}`), ID: uuid.New(), IdempotencyKeyDigest: digestString([]byte(uuid.NewString())), IdempotencyNamespace: "model-inference", CanonicalVersion: 1, RequestFingerprint: strings.Repeat("c", 64), UserID: f.user, SessionID: "controlled", TokenAccountID: f.account, CredentialID: f.credential, WalletID: f.wallet, Model: f.model.String(), ModelID: &f.model, MaximumMicrocredits: 100}
}
func (f financialRouteFixture) review(t *testing.T) UsageRequest {
	t.Helper()
	q := f.request()
	q.Model = "model"
	if _, fresh, e := f.r.BeginUsage(f.ctx, q, map[string]any{}); e != nil || !fresh {
		t.Fatal("source usage", e)
	}
	if _, e := f.r.pool.Exec(f.ctx, `UPDATE usage_requests SET status='REVIEW',error_class='hold_unconfirmed',reconciliation_stopped_at=now(),started_at=now()-interval '10 hours' WHERE id=$1`, q.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.r.SyncManualReviewCases(f.ctx, 100); e != nil {
		t.Fatal(e)
	}
	return q
}
func TestFinancialRouteStopLossPostgres(t *testing.T) {
	t.Run("open_cases_recover_only_after_all_resolved", func(t *testing.T) {
		f := financialRouteSetup(t)
		r, ctx := f.r, f.ctx
		a, b := f.review(t), f.review(t)
		worker := &RouteHealthWorker{repository: r, workerID: "financial-health"}
		old, ok := worker.claim(ctx)
		if !ok {
			t.Fatal("original probe")
		}
		// The route is already unhealthy: financial ownership must still be recorded.
		if _, e := r.pool.Exec(ctx, `UPDATE routing_policies SET health_status='UNHEALTHY' WHERE id=$1`, f.route); e != nil {
			t.Fatal(e)
		}
		if n, e := r.EscalateOverdueManualReviews(ctx, manualreview.OnCallSchedule{}, 100); e != nil || n != 2 {
			t.Fatal("stoploss", n, e)
		}
		var diagnostic string
		if e := r.pool.QueryRow(ctx, `SELECT health_diagnostic_code FROM routing_policies WHERE id=$1`, f.route).Scan(&diagnostic); e != nil || diagnostic != "MANUAL_REVIEW_REQUIRED" {
			t.Fatal("unhealthy did not acquire stoploss", diagnostic, e)
		}
		worker.complete(ctx, old, true, "PROBE_SUCCEEDED", time.Millisecond)
		// Even a changed health diagnostic/evidence cannot clear the financial owner.
		if _, e := r.pool.Exec(ctx, `UPDATE routing_policies SET health_status='HEALTHY',health_diagnostic_code='CONTROLLED_OTHER_WRITER',health_expires_at=now()+interval '1 minute',next_probe_at=now() WHERE id=$1`, f.route); e != nil {
			t.Fatal(e)
		}
		assertHeld := func() {
			t.Helper()
			c, e := r.ListCandidates(ctx, f.model.String(), f.protocol.String())
			if e != nil || len(c) != 0 {
				t.Fatal("held selected", c, e)
			}
			if _, ok := worker.claim(ctx); ok {
				t.Fatal("held claimed")
			}
			if _, _, e := r.BeginUsage(ctx, f.request(), map[string]any{}); !errors.Is(e, ErrNoEligibleRoute) {
				t.Fatal("held admitted", e)
			}
		}
		assertHeld()
		if _, fresh, e := r.BeginUsage(ctx, a, map[string]any{}); e != nil || fresh {
			t.Fatal("old receipt denied", fresh, e)
		}
		other := f.request()
		other.Model = "different-model"
		other.ModelID = nil
		other.CatalogVersion = nil
		other.CapabilityVersionID = nil
		other.ProtocolDefinitionID = nil
		other.RouteGeneration = nil
		other.ModelSnapshot = nil
		if _, fresh, e := r.BeginUsage(ctx, other, map[string]any{}); e != nil || !fresh {
			t.Fatal("unrelated model blocked", e)
		}
		for i, q := range []UsageRequest{a, b} {
			// Controlled terminal usage fact, followed by the actual existing domain sync;
			// no Billing settlement or HTTP workflow is claimed by this fixture.
			if _, e := r.pool.Exec(ctx, `UPDATE usage_requests SET status='COMMITTED' WHERE id=$1`, q.ID); e != nil {
				t.Fatal(e)
			}
			if _, e := r.SyncManualReviewCases(ctx, 100); e != nil {
				t.Fatal(e)
			}
			if i == 0 {
				assertHeld()
			}
		}
		// Closing is not itself HEALTHY evidence: exercise original claim/complete.
		if _, e := r.pool.Exec(ctx, `UPDATE routing_policies SET health_status='UNHEALTHY',health_expires_at=NULL,next_probe_at=now() WHERE id=$1`, f.route); e != nil {
			t.Fatal(e)
		}
		fresh, ok := worker.claim(ctx)
		if !ok {
			t.Fatal("resolved route not probed")
		}
		worker.complete(ctx, fresh, true, "PROBE_SUCCEEDED", time.Millisecond)
		c, e := r.ListCandidates(ctx, f.model.String(), f.protocol.String())
		if e != nil || len(c) != 1 {
			t.Fatal("resolved route not recovered", c, e)
		}
		if _, fresh, e := r.BeginUsage(ctx, f.request(), map[string]any{}); e != nil || !fresh {
			t.Fatal("recovered admission", e)
		}
	})
	t.Run("stoploss_first_waiting_admission_and_probe", func(t *testing.T) {
		f := financialRouteSetup(t)
		q := f.review(t)
		r, ctx := f.r, f.ctx
		worker := &RouteHealthWorker{repository: r, workerID: "waiting-health"}
		probe, ok := worker.claim(ctx)
		if !ok {
			t.Fatal("probe")
		}
		var caseID uuid.UUID
		if e := r.pool.QueryRow(ctx, `SELECT id FROM manual_review_cases WHERE resource_id=$1`, q.ID).Scan(&caseID); e != nil {
			t.Fatal(e)
		}
		tx, e := r.pool.Begin(ctx)
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback(ctx)
		if e = r.evaluateTokenStopLoss(ctx, tx, tokenManualReviewOverdue{id: caseID, resourceID: q.ID, caseType: "USAGE_RECONCILIATION_REVIEW", severity: "P1"}, time.Now().UTC()); e != nil {
			t.Fatal(e)
		}
		pids := make(chan uint32, 2)
		cfg := r.pool.Config()
		cfg.ConnConfig.Tracer = credentialProvisioningTracer{start: func(_ context.Context, c *pgx.Conn, sql string) {
			if strings.HasPrefix(sql, "SELECT id FROM routing_policies WHERE") || sql == "SELECT id FROM token_accounts WHERE id=$1 FOR UPDATE" {
				pids <- c.PgConn().PID()
			}
		}}
		pool, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer pool.Close()
		defer tx.Rollback(ctx)
		rr := &PostgresRepository{pool: pool, secrets: r.secrets, now: r.now}
		done := make(chan error, 1)
		go func() { _, _, e := rr.BeginUsage(ctx, f.request(), map[string]any{}); done <- e }()
		probeDone := make(chan struct{})
		go func() {
			defer close(probeDone)
			(&RouteHealthWorker{repository: rr, workerID: worker.workerID}).complete(ctx, probe, true, "PROBE_SUCCEEDED", time.Millisecond)
		}()
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for i := 0; i < 2; i++ {
			var pid uint32
			select {
			case pid = <-pids:
			case <-waitCtx.Done():
				t.Fatal("writer lock not reached")
			}
			for {
				var blocked bool
				if e := r.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); e != nil {
					t.Fatal(e)
				}
				if blocked {
					break
				}
				select {
				case <-time.After(time.Millisecond):
				case <-waitCtx.Done():
					t.Fatal("writer did not block")
				}
			}
		}
		if e := tx.Commit(ctx); e != nil {
			t.Fatal(e)
		}
		select {
		case e := <-done:
			if !errors.Is(e, ErrNoEligibleRoute) {
				t.Fatal("waiter missed committed hold", e)
			}
		case <-waitCtx.Done():
			t.Fatal("admission did not finish")
		}
		select {
		case <-probeDone:
		case <-waitCtx.Done():
			t.Fatal("probe did not finish")
		}
		var health string
		if e := r.pool.QueryRow(ctx, `SELECT health_status FROM routing_policies WHERE id=$1`, f.route).Scan(&health); e != nil || health != "UNHEALTHY" {
			t.Fatal("waiter probe revived hold", health, e)
		}
	})
	t.Run("admission_first_stoploss_waits", func(t *testing.T) {
		f := financialRouteSetup(t)
		f.review(t)
		r := f.r
		ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
		defer cancel()
		// Include a second actual protocol route to exercise identical multi-row order.
		protocol := uuid.New()
		if _, e := r.pool.Exec(ctx, `INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at,published_at) SELECT $1,protocol_code,version+1,status,request_schema,created_at,published_at FROM protocol_definitions WHERE id=$2`, protocol, f.protocol); e != nil {
			t.Fatal(e)
		}
		if _, e := r.pool.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status) VALUES($1,$2,'model','CHAT',100,'NONE','UNKNOWN',now(),$3,1,$4,$5,'RESOLVED')`, uuid.New(), f.account, f.user, f.model, protocol); e != nil {
			t.Fatal(e)
		}
		admitted := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		unlock := func() { once.Do(func() { close(release) }) }
		defer unlock()
		cfg := r.pool.Config()
		cfg.ConnConfig.Tracer = credentialProvisioningTracer{end: func(c context.Context, _ *pgx.Conn, sql string, e error) {
			if e == nil && strings.HasPrefix(sql, "SELECT EXISTS(SELECT 1 FROM (") && strings.Contains(sql, "financial_hold") {
				close(admitted)
				select {
				case <-release:
				case <-c.Done():
				}
			}
		}}
		pool, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer pool.Close()
		defer cancel()
		defer unlock()
		rr := &PostgresRepository{pool: pool, secrets: r.secrets, now: r.now}
		accepted := make(chan error, 1)
		go func() {
			_, fresh, e := rr.BeginUsage(ctx, f.request(), map[string]any{})
			if e == nil && !fresh {
				e = errors.New("new request not accepted")
			}
			accepted <- e
		}()
		select {
		case <-admitted:
		case <-ctx.Done():
			t.Fatal("admission did not hold routes")
		}
		stopPID := make(chan uint32, 1)
		cfg = r.pool.Config()
		cfg.ConnConfig.Tracer = credentialProvisioningTracer{start: func(_ context.Context, c *pgx.Conn, sql string) {
			if strings.HasPrefix(sql, "SELECT id FROM routing_policies WHERE") || sql == "SELECT id FROM token_accounts WHERE id=$1 FOR UPDATE" {
				stopPID <- c.PgConn().PID()
			}
		}}
		sp, e := pgxpool.NewWithConfig(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		defer sp.Close()
		defer cancel()
		defer unlock()
		sr := &PostgresRepository{pool: sp, secrets: r.secrets, now: r.now}
		stopped := make(chan error, 1)
		go func() { _, e := sr.EscalateOverdueManualReviews(ctx, manualreview.OnCallSchedule{}, 100); stopped <- e }()
		var pid uint32
		select {
		case pid = <-stopPID:
		case <-ctx.Done():
			t.Fatal("stoploss not at route lock")
		}
		for {
			var blocked bool
			if e := r.pool.QueryRow(ctx, `SELECT cardinality(pg_blocking_pids($1))>0`, pid).Scan(&blocked); e != nil {
				t.Fatal(e)
			}
			if blocked {
				break
			}
			select {
			case <-time.After(time.Millisecond):
			case <-ctx.Done():
				t.Fatal("stoploss did not wait")
			}
		}
		unlock()
		select {
		case e := <-accepted:
			if e != nil {
				t.Fatal(e)
			}
		case <-ctx.Done():
			t.Fatal("admission stuck")
		}
		select {
		case e := <-stopped:
			if e != nil {
				t.Fatal(e)
			}
		case <-ctx.Done():
			t.Fatal("stoploss stuck")
		}
		if _, _, e := r.BeginUsage(ctx, f.request(), map[string]any{}); !errors.Is(e, ErrNoEligibleRoute) {
			t.Fatal("next new request admitted", e)
		}
	})

}
