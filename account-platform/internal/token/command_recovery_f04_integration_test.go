package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type f04ProbeCounter struct{ calls atomic.Int32 }

func (p *f04ProbeCounter) Validate(context.Context, string, []byte, []string) error {
	p.calls.Add(1)
	return nil
}

func f04AuthorizedService(t *testing.T, r *PostgresRepository, actor *uuid.UUID, allowed *atomic.Bool) *Service {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Authorization-Version", "19")
		if !allowed.Load() {
			_, _ = w.Write([]byte(`{"allowed":false}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": *actor})
	}))
	t.Cleanup(server.Close)
	client, err := authorization.NewClient(server.URL, "controlled-service-auth")
	if err != nil {
		t.Fatal(err)
	}
	return &Service{repository: r, authorizer: client}
}
func f04IdentityHandler(next http.Handler) http.Handler {
	return identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://controlled.invalid", Subject: "operator", AuthenticatedAt: time.Now()}, nil
	}, next)
}
func f04RecoveryHandler(s *Service) http.Handler {
	router := gin.New()
	router.GET("/recovery", s.requireKnownTokenCommandGovernance(), func(c *gin.Context) { s.adminTokenCommandByIdempotencyKey(c.Writer, c.Request) })
	return f04IdentityHandler(router)
}
func f04Request(h http.Handler, method, path string, raw []byte, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer controlled-fixture")
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestF04CredentialPrecheckSubmitsAndRejectsStaleFacts(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor, account, credential := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'controlled','ACTIVE',100,$2,$4,$4,'PLATFORM')`, account, actor, uuid.New(), now); err != nil {
		t.Fatal(err)
	}
	cipher, err := r.secrets.Encrypt([]byte("controlled-provider-fact"), []byte(account.String()+":"+credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at) VALUES($1,$2,'controlled','https://provider.invalid',$3,$4,$5,1,'ACTIVE',ARRAY['model'],$6,$7)`, credential, account, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), actor, now); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	allowed.Store(true)
	s := f04AuthorizedService(t, r, &actor, &allowed)
	probe := &f04ProbeCounter{}
	s.validator = probe
	// Select real persisted activation and its transaction fence in this owned DB.
	digest := strings.Repeat("a", 64)
	store, err := r.GovernanceStore(digest, digest)
	if err != nil {
		t.Fatal(err)
	}
	config := r.pool.Config().ConnConfig.Copy()
	config.User = "token_migrator"
	owner, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close(context.Background())
	if _, err = owner.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='ACTIVE',epoch=1,handler_set_digest=$1,policy_set_digest=$1,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='Controlled HTTP validation',request_id=$2 WHERE singleton`, digest, uuid.New()); err != nil {
		t.Fatal(err)
	}
	r.routeActivationGuard = store
	r.WithCredentialIsolationAuthorityGuard(NewCredentialIsolationAuthorityGuard(store, s.authorizer))
	command := mustMarshalJSON(t, map[string]any{"token_account_id": account, "credential_id": credential})
	preInput := TokenCommandPrecheckRequest{CommandType: "TOKEN_CREDENTIAL_VALIDATE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credential.String(), Command: command}
	preHandler := f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck))
	submit := TokenCredentialDeadlineMiddleware(f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand)))
	s.WithTokenCommandGovernanceActivation(store)
	getPre := func() TokenCommandPrecheckResult {
		w := f04Request(preHandler, "POST", "/precheck", mustMarshalJSON(t, preInput), "")
		if w.Code != 200 {
			t.Fatalf("precheck %d %s", w.Code, w.Body.String())
		}
		var p TokenCommandPrecheckResult
		if json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatal("decode precheck")
		}
		return p
	}
	pre := getPre()
	envelope := TokenCommandEnvelope{CommandType: preInput.CommandType, TargetResourceType: preInput.TargetResourceType, TargetResourceID: preInput.TargetResourceID, Reason: "Controlled credential verification", ExpectedVersions: pre.ExpectedVersions, PrecheckFingerprint: pre.PrecheckFingerprint, Command: command}
	expiredRequest := httptest.NewRequest("POST", "/admin/v1/commands", bytes.NewReader(mustMarshalJSON(t, envelope)))
	expiredRequest.Header.Set("Authorization", "Bearer controlled-fixture")
	expiredRequest.Header.Set("Idempotency-Key", "f04-expired-budget-key")
	expiredRequest = expiredRequest.WithContext(context.WithValue(expiredRequest.Context(), credentialEntryDeadlineKey{}, time.Now().Add(-time.Second)))
	expired := httptest.NewRecorder()
	submit.ServeHTTP(expired, expiredRequest)
	if expired.Code != 503 || !strings.Contains(expired.Body.String(), "CREDENTIAL_REQUEST_DEADLINE_EXCEEDED") || probe.calls.Load() != 0 {
		t.Fatalf("expired entry %d %s calls%d", expired.Code, expired.Body.String(), probe.calls.Load())
	}
	var expiredReceipts int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results`).Scan(&expiredReceipts); err != nil || expiredReceipts != 0 {
		t.Fatalf("expired dispatched write count%d err%v", expiredReceipts, err)
	}
	// An expired credential-entry value must not impose a budget on other commands.
	other := envelope
	other.CommandType = "TOKEN_FORMULA_DRAFT_CREATE"
	other.TargetResourceType = "PROTOCOL"
	otherRequest := httptest.NewRequest("POST", "/admin/v1/commands", bytes.NewReader(mustMarshalJSON(t, other)))
	otherRequest.Header.Set("Authorization", "Bearer controlled-fixture")
	otherRequest = otherRequest.WithContext(context.WithValue(otherRequest.Context(), credentialEntryDeadlineKey{}, time.Now().Add(-time.Second)))
	otherResult := httptest.NewRecorder()
	submit.ServeHTTP(otherResult, otherRequest)
	if otherResult.Code != 400 || probe.calls.Load() != 0 {
		t.Fatalf("noncredential budget %d %s", otherResult.Code, otherResult.Body.String())
	}
	bad := envelope
	bad.PrecheckFingerprint = strings.Repeat("0", 64)
	w := f04Request(submit, "POST", "/admin/v1/commands", mustMarshalJSON(t, bad), "f04-bad-fingerprint-key")
	if w.Code != 409 || probe.calls.Load() != 0 {
		t.Fatalf("fingerprint %d %s calls%d", w.Code, w.Body.String(), probe.calls.Load())
	}
	changedRequest := envelope
	changedRequest.Command = mustMarshalJSON(t, map[string]any{"token_account_id": uuid.New(), "credential_id": credential})
	w = f04Request(submit, "POST", "/admin/v1/commands", mustMarshalJSON(t, changedRequest), "f04-changed-request-key")
	if w.Code != 409 || probe.calls.Load() != 0 {
		t.Fatalf("changed request %d %s", w.Code, w.Body.String())
	}
	if _, err = r.pool.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1 WHERE id=$1`, account); err != nil {
		t.Fatal(err)
	}
	w = f04Request(submit, "POST", "/admin/v1/commands", mustMarshalJSON(t, envelope), "f04-stale-version-key")
	if w.Code != 409 || probe.calls.Load() != 0 {
		t.Fatalf("version %d %s", w.Code, w.Body.String())
	}
	pre = getPre()
	envelope.ExpectedVersions = pre.ExpectedVersions
	envelope.PrecheckFingerprint = pre.PrecheckFingerprint
	w = f04Request(submit, "POST", "/admin/v1/commands", mustMarshalJSON(t, envelope), "f04-valid-credential-key")
	if w.Code != 200 || probe.calls.Load() != 1 {
		t.Fatalf("submit %d %s calls%d", w.Code, w.Body.String(), probe.calls.Load())
	}
	var result TokenCommandResult
	if json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Status != "SUCCEEDED" {
		t.Fatalf("result %s", w.Body.String())
	}
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='SAFE_ROLLBACK',epoch=2,operator_id='controlled-fixture',reason='Controlled recovery read',request_id=$1 WHERE singleton`, uuid.New()); err != nil {
		t.Fatal(err)
	}
	recovery := f04RecoveryHandler(s)
	w = f04Request(recovery, "GET", "/recovery?command_type=TOKEN_CREDENTIAL_VALIDATE", nil, "f04-valid-credential-key")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || probe.calls.Load() != 1 {
		t.Fatalf("recovery %d %s", w.Code, w.Body.String())
	}
}

func TestF04CommandReceiptRecoveryIsActorAndTypeScoped(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor := uuid.New()
	owner := actor
	var allowed atomic.Bool
	allowed.Store(true)
	s := f04AuthorizedService(t, r, &actor, &allowed)
	probe := &f04ProbeCounter{}
	s.validator = probe
	digestConfig := strings.Repeat("a", 64)
	store, err := r.GovernanceStore(digestConfig, digestConfig)
	if err != nil {
		t.Fatal(err)
	}
	s.WithTokenCommandGovernanceActivation(store)
	handler := f04RecoveryHandler(s)
	key := "f04-controlled-recovery-key"
	hash := sha256.Sum256([]byte(key))
	digest := hex.EncodeToString(hash[:])
	for command, dispatch := range tokenGovernanceHandlers {
		if dispatch.transport != "COMMAND" || command == "TOKEN_ACCOUNT_CREATE" {
			continue
		}
		t.Run(command, func(t *testing.T) {
			reference := uuid.New()
			if _, err := r.pool.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,audit_request_id) VALUES($1,$2,1,$3,'controlled',19,$4,$4,$4,'CONTROLLED','controlled','Controlled fixture receipt','{}',$4,'PENDING','controlled')`, reference, command, owner, digest); err != nil {
				t.Fatal(err)
			}
			w := f04Request(handler, "GET", "/recovery?command_type="+command, nil, key)
			if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("pending %d %s", w.Code, w.Body.String())
			}
			var result TokenCommandResult
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			if result.CommandReference != reference || result.Status != "PENDING" {
				t.Fatalf("wrong receipt %#v", result)
			}
			actor = uuid.New()
			w = f04Request(handler, "GET", "/recovery?command_type="+command, nil, key)
			if w.Code != 404 {
				t.Fatalf("other actor %d %s", w.Code, w.Body.String())
			}
			actor = owner
			w = f04Request(handler, "GET", "/recovery?command_type="+command, nil, key+"-other")
			if w.Code != 404 {
				t.Fatalf("other key %d", w.Code)
			}
			allowed.Store(false)
			w = f04Request(handler, "GET", "/recovery?command_type="+command, nil, key)
			if w.Code != 403 {
				t.Fatalf("revoked permission %d %s", w.Code, w.Body.String())
			}
			allowed.Store(true)
			if _, err := r.pool.Exec(ctx, `UPDATE token_command_results SET status='SUCCEEDED',completed_at=transaction_timestamp() WHERE command_reference=$1`, reference); err != nil {
				t.Fatal(err)
			}
			w = f04Request(handler, "GET", "/recovery?command_type="+command, nil, key)
			_ = json.Unmarshal(w.Body.Bytes(), &result)
			if w.Code != 200 || result.Status != "SUCCEEDED" {
				t.Fatalf("terminal %d %s", w.Code, w.Body.String())
			}
		})
	}
	request := httptest.NewRequest("GET", "/recovery?command_type=TOKEN_CREDENTIAL_VALIDATE", nil)
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, request)
	if unauthorized.Code != 401 {
		t.Fatalf("missing identity %d", unauthorized.Code)
	}
	invalidKey := f04Request(handler, "GET", "/recovery?command_type=TOKEN_CREDENTIAL_VALIDATE", nil, "short")
	if invalidKey.Code != 400 {
		t.Fatalf("invalid key %d", invalidKey.Code)
	}
	for _, command := range []string{"UNKNOWN", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_CREDENTIAL_ENROLL", "TOKEN_CREDENTIAL_REVOKE"} {
		w := f04Request(handler, "GET", "/recovery?command_type="+command, nil, key)
		if w.Code != 400 {
			t.Fatalf("uninstalled receipt family %s %d", command, w.Code)
		}
	}
	if probe.calls.Load() != 0 {
		t.Fatal("read recovery called provider")
	}
}
