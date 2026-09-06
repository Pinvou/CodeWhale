package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This gate exercises the production composition, not a replacement authority
// guard. Account decisions are an explicit HTTP protocol/fault fixture; Account
// policy computation and the operator CLI have separate real database gates.
func TestProductionCredentialAuthorityPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	control, databaseURL := newThreeServiceDatabase(t, ctx, tokenPaginationIntegrationDatabaseURL(t), "token_saga_")
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx, control, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, control)
	if _, err = control.Exec(ctx, `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	role := "credential_guard_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	password := strings.ReplaceAll(uuid.NewString()+uuid.NewString(), "-", "")
	if _, err = control.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN INHERIT PASSWORD '"+password+"'; GRANT token_gateway TO "+pgx.Identifier{role}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := control.Exec(cleanup, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Errorf("drop exact temporary runtime role: %v", err)
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgresql" && parsed.Scheme != "postgres") {
		t.Fatal("credential authority gate requires a PostgreSQL URL")
	}
	parsed.User = url.UserPassword(role, password)
	secrets, err := NewLocalAESProvider("authority-composition-key", bytes.Repeat([]byte{19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := NewPostgresRepository(ctx, parsed.String(), secrets)
	if err != nil {
		t.Fatal("connect isolated runtime repository failed")
	}
	t.Cleanup(repository.Close)
	var database, sessionUser, currentUser string
	var super bool
	if err = repository.pool.QueryRow(ctx, `SELECT current_database(),session_user,current_user,(SELECT rolsuper FROM pg_roles WHERE rolname=session_user)`).Scan(&database, &sessionUser, &currentUser, &super); err != nil || database != strings.TrimPrefix(parsed.Path, "/") || sessionUser != role || currentUser != role || super {
		t.Fatalf("real non-super runtime identity mismatch: err=%v super=%v", err, super)
	}
	actor := uuid.New()
	const serviceToken = "local-authority-composition-service-token"
	fixture := &credentialAuthorityHTTPFixture{actor: actor, version: 7, permission: "platform.tokens.manage"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		fixture.serve(t, w, request, serviceToken)
	}))
	t.Cleanup(server.Close)
	authorizer, err := authorization.NewClient(server.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	store, err := repository.InstallProductionGovernance(authorizer)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := repository.credentialAuthGuard.(*accountCredentialAuthorityGuard); !ok || repository.routeActivationGuard != store {
		t.Fatal("production installation did not wire both concrete guards")
	}
	// The fixture establishes one valid activation through its real trigger and
	// history. It does not claim to test the separately verified operator CLI.
	if _, err = control.Exec(ctx, `UPDATE public.token_command_governance_activation SET state='ACTIVE',epoch=1,policy_set_digest=$1,handler_set_digest=$2,approved_runtime_roles=$3,operator_id='credential-authority-fixture',reason='exercise installed production guard',request_id=$4,updated_at=transaction_timestamp() WHERE singleton=TRUE`, store.policyDigest, store.handlerDigest, []string{role}, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if observed, err := store.Snapshot(ctx); err != nil || observed.State != "ACTIVE" || observed.Epoch != 1 {
		t.Fatalf("real production activation read failed: %v", err)
	}
	authority := CredentialIsolationAuthoritySnapshot{ActorID: actor, Issuer: "https://identity.example", Subject: "credential-operator", AuthorizationVersion: 7, GateEpoch: 1}
	newLaunch := func(t *testing.T) CredentialLaunch {
		t.Helper()
		fixture.reset(actor, 7, "platform.tokens.manage", "allow")
		account := uuid.New()
		if _, err := control.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Authority guard fixture','ACTIVE',$4,transaction_timestamp(),transaction_timestamp(),'PLATFORM')`, account, uuid.New(), uuid.New(), actor); err != nil {
			t.Fatal(err)
		}
		launchContext, stop := context.WithTimeout(withCredentialIsolationAuthority(ctx, authority), 5*time.Second)
		defer stop()
		launch, err := repository.CreateCredentialLaunch(launchContext, account, actor, CredentialLaunchRequest{Action: "ENROLL", ExpectedAccountVersion: 1, ExpectedCredentialVersion: 0, Slot: "PRIMARY", Reason: "verify installed credential authority composition"}, "authority-launch-"+uuid.NewString(), uuid.NewString())
		if err != nil || launch.LaunchID == uuid.Nil || fixture.count() != 2 {
			t.Fatalf("launch must check production authority before effects and commit: calls=%d err=%v", fixture.count(), err)
		}
		return launch
	}
	consume := func(operation context.Context, launch CredentialLaunch) (credentialIsolationSession, error) {
		return repository.ConsumeCredentialLaunch(operation, launch.LaunchID, "https://credentials.example", strings.Repeat("b", 64), strings.Repeat("s", 64), strings.Repeat("n", 64), strings.Repeat("v", 64), strings.Repeat("c", 64))
	}
	assertUnlocked := func(t *testing.T, launch CredentialLaunch) {
		t.Helper()
		checkContext, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		tx, err := control.Begin(checkContext)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(checkContext)
		if _, err = tx.Exec(checkContext, `SELECT id FROM credential_launches WHERE id=$1 FOR UPDATE NOWAIT`, launch.LaunchID); err != nil {
			t.Fatalf("failed authority operation retained a row lock: %v", err)
		}
		if _, err = tx.Exec(checkContext, `SELECT singleton FROM public.token_command_governance_activation FOR UPDATE NOWAIT`); err != nil {
			t.Fatalf("failed authority operation retained its activation row lock: %v", err)
		}
	}
	t.Run("installed_guard_commits_frozen_session", func(t *testing.T) {
		launch := newLaunch(t)
		fixture.reset(actor, 7, "platform.tokens.manage", "allow")
		operation, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		session, err := consume(operation, launch)
		if err != nil || session.ID == uuid.Nil || fixture.count() != 2 {
			t.Fatalf("session must use both installed authority checks: calls=%d err=%v", fixture.count(), err)
		}
		var frozen CredentialIsolationAuthoritySnapshot
		var status string
		if err = control.QueryRow(ctx, `SELECT s.actor_user_id,l.actor_issuer,l.actor_subject,s.authorization_version,s.gate_epoch,l.status FROM credential_isolation_sessions s JOIN credential_launches l ON l.id=s.launch_id WHERE s.id=$1`, session.ID).Scan(&frozen.ActorID, &frozen.Issuer, &frozen.Subject, &frozen.AuthorizationVersion, &frozen.GateEpoch, &status); err != nil || frozen != authority || status != "CONSUMED" {
			t.Fatalf("session did not persist frozen authority: err=%v status=%s", err, status)
		}
		assertUnlocked(t, launch)
	})
	for _, mode := range []string{"actor_changed", "version_changed", "permission_denied", "account_timeout", "second_check_denied"} {
		t.Run(mode, func(t *testing.T) {
			launch := newLaunch(t)
			fixture.reset(actor, 7, "platform.tokens.manage", mode)
			before := productionCredentialAuthorityFacts(t, ctx, repository)
			operation, stop := context.WithTimeout(ctx, 350*time.Millisecond)
			defer stop()
			result, err := consume(operation, launch)
			wantCalls := 1
			if mode == "second_check_denied" {
				wantCalls = 2
			}
			if !errors.Is(err, ErrTokenRouteActivationChanged) || result.ID != uuid.Nil || fixture.count() != wantCalls {
				t.Fatalf("authority refusal must be exact and before effects: calls=%d err=%v", fixture.count(), err)
			}
			if before != productionCredentialAuthorityFacts(t, ctx, repository) {
				t.Fatal("authority refusal changed complete persisted credential facts")
			}
			assertUnlocked(t, launch)
		})
	}
	t.Run("validate_permission_is_exact", func(t *testing.T) {
		for _, permission := range []string{"platform.tokens.credentials.validate", "platform.tokens.credentials.revoke.request"} {
			fixture.reset(actor, 7, permission, "allow")
			operation, stop := context.WithTimeout(ctx, time.Second)
			tx, err := repository.pool.Begin(operation)
			if err != nil {
				stop()
				t.Fatal(err)
			}
			selected := authority
			selected.RequiredPermission = permission
			err = repository.credentialAuthGuard.LockAndCheckCredentialIsolationAuthority(operation, tx, selected)
			_ = tx.Rollback(operation)
			stop()
			if permission == "platform.tokens.credentials.validate" {
				if err != nil || fixture.count() != 1 {
					t.Fatalf("validate must check its exact Account permission: %v", err)
				}
			} else if !errors.Is(err, ErrTokenRouteActivationChanged) || fixture.count() != 0 {
				t.Fatal("unsupported permission must be rejected before authority HTTP")
			}
		}
	})
	t.Run("row_wait_consumes_original_deadline", func(t *testing.T) {
		launch := newLaunch(t)
		fixture.reset(actor, 7, "platform.tokens.manage", "account_timeout")
		before := productionCredentialAuthorityFacts(t, ctx, repository)
		blocker, err := control.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `SELECT id FROM credential_launches WHERE id=$1 FOR UPDATE`, launch.LaunchID); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(1500 * time.Millisecond)
		operation, stop := context.WithDeadline(ctx, deadline)
		defer stop()
		done := make(chan error, 1)
		go func() {
			_, err := consume(operation, launch)
			done <- err
		}()
		// A separate observer proves a real lock wait before consuming budget.
		observer, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		defer observer.Close(ctx)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var waiting bool
			if err = observer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND cardinality(pg_blocking_pids(pid))>0)`, role).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("operation returned before the required lock wait: %v", err)
			case <-operation.Done():
				t.Fatal("did not observe credential launch row-lock wait")
			case <-ticker.C:
			}
		}
		if fixture.count() != 0 {
			t.Fatal("Account authority ran before obtaining the locked launch")
		}
		release := time.NewTimer(time.Until(deadline.Add(-250 * time.Millisecond)))
		defer release.Stop()
		<-release.C
		if err = blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err = <-done:
		case <-time.After(time.Until(deadline.Add(500 * time.Millisecond))):
			t.Fatal("authority renewed the deadline after waiting for a row lock")
		}
		if !errors.Is(err, ErrTokenRouteActivationChanged) || !errors.Is(operation.Err(), context.DeadlineExceeded) || fixture.count() != 1 {
			t.Fatalf("remaining original budget must expire inside authority HTTP: calls=%d err=%v", fixture.count(), err)
		}
		if began := fixture.lastRequestAt(); !began.Before(deadline) || deadline.Sub(began) > 400*time.Millisecond {
			t.Fatal("authority HTTP did not receive only the remaining original budget")
		}
		if before != productionCredentialAuthorityFacts(t, ctx, repository) {
			t.Fatal("deadline exhaustion after row wait changed credential facts")
		}
		assertUnlocked(t, launch)
	})
	t.Run("stale_epoch_prevents_authority_and_effects", func(t *testing.T) {
		launch := newLaunch(t)
		if _, err = control.Exec(ctx, `UPDATE public.token_command_governance_activation SET state='SAFE_ROLLBACK',epoch=2,request_id=$1,updated_at=transaction_timestamp() WHERE singleton=TRUE`, uuid.New()); err != nil {
			t.Fatal(err)
		}
		fixture.reset(actor, 7, "platform.tokens.manage", "allow")
		before := productionCredentialAuthorityFacts(t, ctx, repository)
		operation, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		result, err := consume(operation, launch)
		if !errors.Is(err, ErrTokenRouteActivationChanged) || result.ID != uuid.Nil || fixture.count() != 0 || before != productionCredentialAuthorityFacts(t, ctx, repository) {
			t.Fatalf("stale epoch must cause zero HTTP and zero credential effects: %v", err)
		}
		assertUnlocked(t, launch)
	})
}

type credentialAuthorityHTTPFixture struct {
	mu         sync.Mutex
	actor      uuid.UUID
	version    int64
	permission string
	mode       string
	calls      int
	requestAt  time.Time
}

func (f *credentialAuthorityHTTPFixture) reset(actor uuid.UUID, version int64, permission, mode string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actor, f.version, f.permission, f.mode, f.calls, f.requestAt = actor, version, permission, mode, 0, time.Time{}
}

func (f *credentialAuthorityHTTPFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *credentialAuthorityHTTPFixture) lastRequestAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requestAt
}

func (f *credentialAuthorityHTTPFixture) serve(t *testing.T, w http.ResponseWriter, request *http.Request, token string) {
	t.Helper()
	f.mu.Lock()
	f.calls++
	f.requestAt = time.Now()
	actor, version, permission, mode, calls := f.actor, f.version, f.permission, f.mode, f.calls
	f.mu.Unlock()
	var payload struct {
		Issuer         string   `json:"issuer"`
		Subject        string   `json:"subject"`
		AnyPermissions []string `json:"any_permissions"`
		AllPermissions []string `json:"all_permissions"`
	}
	if request.Method != http.MethodPost || request.URL.Path != "/internal/v1/authorization/check" || request.Header.Get("Authorization") != "Bearer "+token || json.NewDecoder(request.Body).Decode(&payload) != nil || payload.Issuer != "https://identity.example" || payload.Subject != "credential-operator" || len(payload.AnyPermissions) != 0 || len(payload.AllPermissions) != 1 || payload.AllPermissions[0] != permission {
		t.Error("production authorization client sent incorrect frozen identity or exact permission")
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if mode == "account_timeout" {
		<-request.Context().Done()
		return
	}
	if mode == "actor_changed" {
		actor = uuid.New()
	}
	if mode == "version_changed" {
		version++
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Authorization-Version", strconv.FormatInt(version, 10))
	allowed := mode != "permission_denied" && !(mode == "second_check_denied" && calls == 2)
	_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": actor})
}

func productionCredentialAuthorityFacts(t *testing.T, ctx context.Context, repository *PostgresRepository) [8]string {
	t.Helper()
	var facts [8]string
	for index, table := range []string{"token_accounts", "provider_credentials", "credential_launches", "credential_isolation_sessions", "credential_isolation_results", "token_audit_logs", "outbox_events", "token_command_results"} {
		if err := repository.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(f) ORDER BY to_jsonb(f)::text),'[]'::jsonb)::text FROM `+pgx.Identifier{table}.Sanitize()+` f`).Scan(&facts[index]); err != nil {
			t.Fatal(err)
		}
	}
	return facts
}
