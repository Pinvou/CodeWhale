package token

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Existing READY resources are retained fixture facts; the credential handshake,
// activation, CAS, rebind, rollback and result lookup use production writers.
func testCredentialRotationReadyBinding(t *testing.T) {
	for _, mode := range []string{"ready", "route_drift", "not_ready"} {
		t.Run(mode, func(t *testing.T) {
			r, ctx := retainedManagementTokenRepository(t)
			actor, account, wallet, model, credential, backup, route, provision := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "https://controlled.invalid/issuer", Subject: "controlled", AuthorizationVersion: 1, GateEpoch: 1}
			r.WithCredentialIsolationAuthorityGuard(credentialAuthorityGuardFunc(func(_ context.Context, _ pgx.Tx, got CredentialIsolationAuthoritySnapshot) error {
				if got != authority {
					return ErrForbidden
				}
				return nil
			}))
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := r.pool.Exec(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$1::uuid::text,'ACTIVE',100,$2,now(),now(),'PLATFORM')`, account, actor, wallet)
			exec(`INSERT INTO token_share_policies(token_account_id,scope,updated_at,updated_by) VALUES($1,'OWNER_UNIT_ONLY',now(),$2)`, account, actor)
			// Two retained credentials preserve the legitimate original backup case.
			for i, id := range []uuid.UUID{backup, credential} {
				ciphertext, err := r.secrets.Encrypt([]byte("controlled-retained-secret"), []byte(account.String()+":"+id.String()))
				if err != nil {
					t.Fatal(err)
				}
				status := "RETIRED"
				if i == 1 {
					status = "ACTIVE"
				}
				exec(`INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validation_status,created_by,created_at) VALUES($1,$2,'CONTROLLED','https://provider.invalid',$3,$4,$5,$6,$7,ARRAY['model'],'VALID',$8,now())`, id, account, ciphertext, r.secrets.KeyID(), strings.Repeat(string(rune('a'+i)), 64), i+1, status, actor)
			}
			exec(`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'CONTROLLED','openai-compatible-v1','model',$2,$3,now())`, model, digestString([]byte("model")), actor)
			generation := int64(1)
			if mode == "route_drift" {
				generation = 2
			}
			exec(`INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,health_observed_at,health_expires_at,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status) VALUES($1,$2,'model','CHAT',100,'NONE','HEALTHY',now(),now()+interval '1 minute',now(),$3,$4,$5,$6,'RESOLVED')`, route, account, actor, generation, model, protocol)
			status := "READY"
			if mode == "not_ready" {
				status = "FAILED"
			}
			exec(`INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES($1,'PERSONAL',$2,$3,$4,'model','CHAT',$5,1,1,$6,$7,'VERIFIED',1,'{}',8,now()+interval '1 hour',$8,$9,$2,now(),now())`, provision, actor, wallet, model, protocol, strings.Repeat("c", 64), status, account, credential)
			worker := &RouteHealthWorker{repository: r, workerID: "controlled-old-credential-probe"}
			var oldProbe routeProbeTarget
			if mode == "ready" {
				var claimed bool
				oldProbe, claimed = worker.claim(ctx)
				if !claimed || oldProbe.CredentialID != credential {
					t.Fatal("original probe not claimed")
				}
			}
			launch, err := r.CreateCredentialLaunch(withCredentialIsolationAuthority(ctx, authority), account, actor, CredentialLaunchRequest{Action: "ROTATE", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 2, Slot: "PRIMARY", Reason: "controlled readiness rotation"}, uuid.NewString(), "controlled-rotation")
			if err != nil {
				t.Fatal(err)
			}
			binding := strings.Repeat("b", 64)
			csrf := strings.Repeat("c", 64)
			session, err := r.ConsumeCredentialLaunch(ctx, launch.LaunchID, "https://credentials.pinvou.com", binding, strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), csrf)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = r.ConfirmCredentialSession(ctx, session.ID, binding); err != nil {
				t.Fatal(err)
			}
			if err = r.AuthenticateCredentialSession(ctx, session.ID, actor, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			policy, err := NewProviderURLPolicy("test", []string{"provider.invalid"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.CompleteCredentialSession(ctx, session.ID, session.CredentialOrigin, binding, csrf, "CONTROLLED", "https://provider.invalid", []string{"model"}, []byte("controlled-replacement-secret"), retainedExpiryValidator{advance: func() {}}, policy)
			var bound uuid.UUID
			var actualStatus, oldStatus, backupStatus, health string
			var count int
			if e := r.pool.QueryRow(ctx, `SELECT active_credential_id,status FROM route_provisionings WHERE id=$1`, provision).Scan(&bound, &actualStatus); e != nil {
				t.Fatal(e)
			}
			if e := r.pool.QueryRow(ctx, `SELECT (SELECT status FROM provider_credentials WHERE id=$1),(SELECT status FROM provider_credentials WHERE id=$2),(SELECT count(*) FROM provider_credentials WHERE token_account_id=$3),(SELECT health_status FROM routing_policies WHERE id=$4)`, credential, backup, account, route).Scan(&oldStatus, &backupStatus, &count, &health); e != nil {
				t.Fatal(e)
			}
			if mode == "route_drift" {
				if !errors.Is(err, ErrCatalogVersionConflict) || bound != credential || oldStatus != "ACTIVE" || count != 2 || health != "HEALTHY" {
					t.Fatal("drift did not roll back", err, bound, oldStatus, count, health)
				}
				return
			}
			if err != nil || result.Status != "SUCCEEDED" || oldStatus != "RETIRED" || backupStatus != "RETIRED" || count != 3 || health != "UNKNOWN" {
				t.Fatal("rotation result", err, result, oldStatus, backupStatus, count, health)
			}
			if mode == "ready" {
				worker.complete(ctx, oldProbe, true, "PROBE_SUCCEEDED", time.Millisecond)
				var after string
				if e := r.pool.QueryRow(ctx, `SELECT health_status FROM routing_policies WHERE id=$1`, route).Scan(&after); e != nil || after != "UNKNOWN" {
					t.Fatal("old credential probe escaped", after, e)
				}
				fresh, ok := worker.claim(ctx)
				if !ok || fresh.CredentialID != *result.CredentialID {
					t.Fatal("new probe did not select C2", fresh, ok)
				}
				worker.complete(ctx, fresh, true, "PROBE_SUCCEEDED", time.Millisecond)
				candidates, e := r.ListCandidates(ctx, model.String(), protocol.String())
				if e != nil || len(candidates) != 1 || candidates[0].CredentialID != *result.CredentialID {
					t.Fatal("rebound READY did not admit C2", candidates, e)
				}
			}
			if mode == "ready" && (bound != *result.CredentialID || actualStatus != "READY") {
				t.Fatal("READY not rebound exactly", bound, result, actualStatus)
			}
			if mode == "not_ready" && (bound != credential || actualStatus != "FAILED") {
				t.Fatal("nonready promoted", bound, actualStatus)
			}
			replay, e := r.CredentialIsolationResult(ctx, actor, launch.ResultRef)
			if e != nil || replay.CredentialID == nil || *replay.CredentialID != *result.CredentialID {
				t.Fatal("original result replay", e)
			}
		})
	}
}

// The tracer only schedules actual production queries; it never supplies query
// results or substitutes a lock. PostgreSQL confirms the new resource backend
// is waiting on the old completion backend before either may continue.
type credentialProvisioningTraceKey struct{}
type credentialProvisioningTracer struct {
	start func(context.Context, *pgx.Conn, string)
	end   func(context.Context, *pgx.Conn, string, error)
}

func (tr credentialProvisioningTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if tr.start != nil {
		tr.start(ctx, conn, d.SQL)
	}
	return context.WithValue(ctx, credentialProvisioningTraceKey{}, d.SQL)
}
func (tr credentialProvisioningTracer) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, d pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(credentialProvisioningTraceKey{}).(string)
	if tr.end != nil {
		tr.end(ctx, conn, sql, d.Err)
	}
}

func testCredentialProvisioningLockOrder(t *testing.T) {
	for _, mode := range []string{"known_account", "new_account"} {
		t.Run(mode, func(t *testing.T) {
			r, ctx := retainedManagementTokenRepository(t)
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cfg := r.pool.Config().ConnConfig.Copy()
			cfg.User = "token_policy_operator"
			operator, err := pgx.ConnectConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = operator.Exec(ctx, `SELECT * FROM public.apply_token_account_owner_cap_policy(20,NULL,$1,'controlled lock ordering',$2)`, uuid.NewString(), uuid.New())
			operator.Close(ctx)
			if err != nil {
				t.Fatal(err)
			}
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := r.pool.Exec(ctx, q, args...); err != nil {
					t.Fatal(err)
				}
			}
			user, wallet := uuid.New(), uuid.New()
			protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
			model := CatalogModel{ModelID: uuid.New(), ProtocolDefinitionID: protocol, ProtocolCode: "CHAT", ProviderID: "controlled", AdapterVersion: "new-api-v1", UpstreamModelRef: "lock-" + user.String(), LifecycleStatus: "PUBLISHED", SourceAvailable: true}
			exec(`INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,now())`, model.ModelID, model.ProviderID, model.AdapterVersion, model.UpstreamModelRef, digestString([]byte(model.UpstreamModelRef)), user)
			capability := uuid.New()
			exec(`INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($1,$2,1,$3,1000,100,'["TEXT"]','controlled',$4,now())`, capability, model.ModelID, protocol, user)
			var version int64
			if err := r.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'controlled lock ordering',$2,now(),now()) RETURNING version`, uuid.New(), user).Scan(&version); err != nil {
				t.Fatal(err)
			}
			exec(`INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($1,$2,$3,$3,'PUBLISHED',$4,$5,true)`, version, model.ModelID, model.UpstreamModelRef, capability, protocol)
			provider := func(repo *PostgresRepository) *PostgresDefaultRouteProvisioner {
				t.Helper()
				p, e := NewDefaultRouteProvisioner(repo, ProviderURLPolicy{environment: "test", allowedHosts: map[string]bool{"example.invalid": true}}, DefaultRouteConfig{Provider: "controlled", AdapterVersion: "new-api-v1", BaseURL: "https://example.invalid", Secret: []byte("controlled-provider-secret"), Priority: 2})
				if e != nil {
					t.Fatal(e)
				}
				return p.WithTokenAccountCreateCapability(true)
			}
			base := provider(r)
			resolved := AuthorizationContext{UserID: user, ContextType: "PERSONAL"}
			item, err := base.Ensure(ctx, resolved, wallet, model)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "known_account" {
				if err := base.provisionResources(ctx, resolved, wallet, model, item.DesiredGeneration); err != nil {
					t.Fatal(err)
				}
			}
			observed, continueSnapshot := make(chan struct{}), make(chan struct{})
			accountLocked, continueCompletion := make(chan uint32, 1), make(chan struct{})
			var snapshotOnce, completionOnce, snapshotRelease, completionRelease sync.Once
			releaseSnapshot := func() { snapshotRelease.Do(func() { close(continueSnapshot) }) }
			releaseCompletion := func() { completionRelease.Do(func() { close(continueCompletion) }) }
			defer releaseSnapshot()
			defer releaseCompletion()
			var afterSnapshot atomic.Bool
			var deadlocks atomic.Int32
			captureError := func(err error) {
				var pgerr *pgconn.PgError
				if errors.As(err, &pgerr) && pgerr.Code == "40P01" {
					deadlocks.Add(1)
				}
			}
			oldTrace := credentialProvisioningTracer{end: func(c context.Context, conn *pgx.Conn, sql string, err error) {
				captureError(err)
				if err != nil {
					return
				}
				if strings.HasPrefix(sql, "SELECT ta.id,pc.id,rp.route_generation") && !strings.Contains(sql, "ta.id=$4") {
					snapshotOnce.Do(func() {
						close(observed)
						select {
						case <-continueSnapshot:
							afterSnapshot.Store(true)
						case <-c.Done():
						}
					})
				}
				if afterSnapshot.Load() && strings.HasSuffix(sql, "FOR UPDATE OF ta,sp") {
					completionOnce.Do(func() {
						accountLocked <- conn.PgConn().PID()
						select {
						case <-continueCompletion:
						case <-c.Done():
						}
					})
				}
			}}
			tracedRepo := func(trace credentialProvisioningTracer) *PostgresRepository {
				t.Helper()
				cfg := r.pool.Config()
				cfg.ConnConfig.Tracer = trace
				pool, e := pgxpool.NewWithConfig(ctx, cfg)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(pool.Close)
				return &PostgresRepository{pool: pool, secrets: r.secrets, now: r.now}
			}
			old := provider(tracedRepo(oldTrace))
			claimed, attempt, ok := old.claim(ctx)
			if !ok || claimed.ID != item.ID {
				t.Fatal("old claim unavailable")
			}
			oldDone := make(chan struct{})
			go func() { defer close(oldDone); old.execute(ctx, claimed, attempt) }()
			select {
			case <-observed:
			case <-ctx.Done():
				t.Fatal("old resource snapshot not reached")
			}
			// Expire only our owned lease, then use the real reclaimer/new-attempt writer.
			exec(`UPDATE route_provisionings SET lease_until=clock_timestamp()-interval '1 second' WHERE id=$1`, item.ID)
			resourcePID := make(chan uint32, 1)
			var resourceOnce sync.Once
			fresh := provider(tracedRepo(credentialProvisioningTracer{
				start: func(_ context.Context, c *pgx.Conn, sql string) {
					if strings.HasPrefix(sql, "SELECT id FROM token_accounts WHERE owner_type=") {
						resourceOnce.Do(func() { resourcePID <- c.PgConn().PID() })
					}
				},
				end: func(_ context.Context, _ *pgx.Conn, _ string, e error) { captureError(e) },
			}))
			newer, newAttempt, ok := fresh.claim(ctx)
			if !ok || newer.ID != item.ID || newAttempt == attempt {
				t.Fatal("replacement claim unavailable")
			}
			releaseSnapshot()
			var oldPID uint32
			select {
			case oldPID = <-accountLocked:
			case <-ctx.Done():
				t.Fatal("old completion account lock not reached")
			}
			resourceDone := make(chan error, 1)
			go func() {
				resourceDone <- fresh.provisionResourcesForAttempt(ctx, resolved, wallet, model, newer.DesiredGeneration, &newer, newAttempt)
			}()
			var newPID uint32
			select {
			case newPID = <-resourcePID:
			case <-ctx.Done():
				t.Fatal("new resource account lock not reached")
			}
			// Await an observed database dependency, rather than guessing from sleep.
			for {
				var blocked bool
				if err := r.pool.QueryRow(ctx, `SELECT $1::integer=ANY(pg_blocking_pids($2::integer))`, oldPID, newPID).Scan(&blocked); err != nil {
					t.Fatal(err)
				}
				if blocked {
					break
				}
				select {
				case <-time.After(5 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("resource did not wait on completion account")
				}
			}
			t.Logf("observed resource backend %d waiting on completion backend %d", newPID, oldPID)
			releaseCompletion()
			select {
			case <-oldDone:
			case <-ctx.Done():
				t.Fatal("old completion failed to finish")
			}
			select {
			case e := <-resourceDone:
				if e != nil {
					t.Error("new resource failed", e)
				}
			case <-ctx.Done():
				t.Fatal("new resource failed to finish")
			}
			if n := deadlocks.Load(); n != 0 {
				t.Errorf("actual PostgreSQL deadlock count=%d", n)
			}
			var status, attemptStatus, oldStatus string
			var count int
			if err := r.pool.QueryRow(ctx, `SELECT p.status,a.status,old.status,p.attempt_count FROM route_provisionings p JOIN route_provisioning_attempts a ON a.id=$2 JOIN route_provisioning_attempts old ON old.id=$3 WHERE p.id=$1`, item.ID, newAttempt, attempt).Scan(&status, &attemptStatus, &oldStatus, &count); err != nil {
				t.Fatal(err)
			}
			if status != "PENDING" || attemptStatus != "RUNNING" || oldStatus != "TIMED_OUT" || count != 2 {
				t.Fatal("old completion changed replacement", status, attemptStatus, oldStatus, count)
			}
			// The next actual execution remains usable and publishes the current tuple.
			fresh.execute(ctx, newer, newAttempt)
			detail, e := fresh.Detail(ctx, item.ID)
			if e != nil || detail.Status != "READY" || detail.AttemptCount != 2 {
				t.Fatal("replacement execution not READY", detail, e)
			}
		})
	}
}
