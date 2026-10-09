package token

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// The activation transition is an explicit controlled operator fixture through
// the immutable public trigger. The runtime guard and all command/CAS/audit
// queries use the genuine runtime role, not a fake guard or bypassed trigger.
// This is not an offline release-issuance or deployed authorization proof.
func TestUsageCommandLookupThroughCurrentHandler(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	handler, err := TokenCommandHandlerDigest()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		t.Fatal(err)
	}
	store, err := r.GovernanceStore(handler, policy)
	if err != nil {
		t.Fatal(err)
	}
	r.WithTokenRouteActivationTransactionGuard(store)
	config, err := pgx.ParseConfig(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.Database = r.pool.Config().ConnConfig.Database
	operator, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close(context.Background())
	if _, err = operator.Exec(ctx, `SET ROLE token_schema_owner`); err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
	makeInput := func(t *testing.T) runtimeTokenCommandBegin {
		id := retainedCancellationUsage(t, r, ctx, uuid.New(), uuid.New(), nil, "REVIEW")
		var updated time.Time
		if err := r.pool.QueryRow(ctx, `UPDATE usage_requests SET status='REVIEW',execution_status='REVIEW',cancel_status='NOT_REQUESTED',cancel_requested_at=NULL,updated_at=clock_timestamp() WHERE id=$1 RETURNING updated_at`, id).Scan(&updated); err != nil {
			t.Fatal(err)
		}
		command, _ := json.Marshal(map[string]any{"usage_request_id": id.String(), "expected_usage_version": updated.UTC().Format(time.RFC3339Nano), "evidence_high_watermark": 0, "replay_model_request": false})
		envelope := TokenCommandEnvelope{CommandType: "TOKEN_USAGE_RECONCILIATION_RETRY", Reason: "controlled genuine reconciliation retry", TargetResourceType: "USAGE_REQUEST", TargetResourceID: id.String(), Command: command}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expected, fingerprint, err := runtimeCommandPrecheckInTx(ctx, tx, envelope)
		tx.Rollback(ctx)
		if err != nil {
			t.Fatal(err)
		}
		envelope.ExpectedVersions = expected
		envelope.PrecheckFingerprint = fingerprint
		return runtimeTokenCommandBegin{tokenCommandBegin: tokenCommandBegin{Envelope: envelope, ActorUserID: actor, ActorPermission: "platform.usage.reconcile.retry", AuthorizationVersion: 1, IdempotencyKey: uuid.NewString(), AuditRequestID: uuid.NewString()}, RequestDigest: strings.Repeat("a", 64), KeyDigest: digestString([]byte(id.String())), IdempotencyFingerprint: digestString(command)}
	}
	selected := func(state string, epoch int64) context.Context {
		return context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: state, Epoch: epoch}, transactional: true})
	}
	transition := func(state string, epoch int64) {
		if _, err := operator.Exec(ctx, `UPDATE public.token_command_governance_activation SET state=$1,epoch=$2,policy_set_digest=$3,handler_set_digest=$4,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='current release fixture',request_id=$5,updated_at=clock_timestamp() WHERE singleton=TRUE`, state, epoch, policy, handler, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	transition("ACTIVE", 1)
	t.Run("current-release-readiness-and-result-replay", func(t *testing.T) {
		service := (&Service{}).WithTokenCommandGovernanceActivation(store)
		response := httptest.NewRecorder()
		service.adminUsageReconciliationCommandReadiness(response, httptest.NewRequest(http.MethodGet, "/readiness", nil).WithContext(ctx))
		var readiness usageReconciliationCommandReadiness
		if json.Unmarshal(response.Body.Bytes(), &readiness) != nil || !readiness.CommandsEnabled || readiness.GovernanceEpoch != 1 {
			t.Fatalf("current release readiness missing: %s", response.Body.String())
		}
		input := makeInput(t)
		input.KeyDigest = digestString([]byte(input.IdempotencyKey))
		result, created, err := r.ExecuteRuntimeTokenCommand(selected("ACTIVE", 1), input)
		if err != nil || !created || result.Status != "SUCCEEDED" {
			t.Fatalf("genuine command failed: created=%v err=%v", created, err)
		}

		foreign := uuid.New()
		authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			var query struct {
				Subject     string   `json:"subject"`
				Permissions []string `json:"all_permissions"`
			}
			if json.NewDecoder(request.Body).Decode(&query) != nil || len(query.Permissions) != 1 || query.Permissions[0] != "platform.usage.reconcile.retry" {
				t.Error("lookup did not request current command permission")
				w.WriteHeader(400)
				return
			}
			user, allowed := actor, true
			if query.Subject == "foreign" {
				user = foreign
			}
			if query.Subject == "denied" {
				allowed = false
			}
			w.Header().Set("X-Authorization-Version", "1")
			_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": user})
		}))
		defer authority.Close()
		authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
		if err != nil {
			t.Fatal(err)
		}
		lookup := (&Service{repository: r, authorizer: authorizer})
		handler := identity.Middleware(func(_ context.Context, bearer string) (identity.Principal, error) {
			return identity.Principal{Issuer: "https://issuer.example", Subject: bearer, SessionID: "controlled", AuthenticatedAt: time.Now()}, nil
		}, http.HandlerFunc(lookup.adminTokenCommandByIdempotencyKey))
		for _, check := range []struct {
			name, subject, key, command string
			status                      int
		}{
			{"same-actor-original-key", "actor", input.IdempotencyKey, "TOKEN_USAGE_RECONCILIATION_RETRY", 200},
			{"foreign-actor-cannot-read", "foreign", input.IdempotencyKey, "TOKEN_USAGE_RECONCILIATION_RETRY", 404},
			{"permission-denied", "denied", input.IdempotencyKey, "TOKEN_USAGE_RECONCILIATION_RETRY", 403},
			{"missing-original-key", "actor", uuid.NewString(), "TOKEN_USAGE_RECONCILIATION_RETRY", 404},
			{"short-key", "actor", "short", "TOKEN_USAGE_RECONCILIATION_RETRY", 400},
			{"future-runtime-denied", "actor", input.IdempotencyKey, "TOKEN_USAGE_FUTURE_RUNTIME", 400},
			{"unknown-command-denied", "actor", input.IdempotencyKey, "TOKEN_FUTURE_UNKNOWN", 400},
		} {
			t.Run(check.name, func(t *testing.T) {
				request := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type="+check.command, nil)
				request.Header.Set("Authorization", "Bearer "+check.subject)
				request.Header.Set("Idempotency-Key", check.key)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != check.status {
					t.Fatalf("status=%d expected=%d body=%s", response.Code, check.status, response.Body.String())
				}
				if check.status == 200 {
					var actual TokenCommandResult
					if json.Unmarshal(response.Body.Bytes(), &actual) != nil || actual.CommandReference != result.CommandReference || actual.Status != result.Status || actual.ActorUserID != actor {
						t.Fatalf("lookup changed original receipt: %s", response.Body.String())
					}
					if response.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("result cacheable")
					}
				}
			})
		}
		var count int
		if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE command_reference=$1`, result.CommandReference).Scan(&count); err != nil || count != 1 {
			t.Fatalf("read mutated receipts: count=%d err=%v", count, err)
		}
	})

}

// Installed command reads retain their authentication boundary. Unknown
// runtime families must not become reachable through the current usage lookup.
func TestCurrentCommandKeyLookupDispatchBoundaries(t *testing.T) {
	service := &Service{authorizer: &authorization.Client{}}
	for _, entry := range []struct {
		command string
		status  int
	}{
		{"TOKEN_USAGE_RECONCILIATION_RETRY", http.StatusUnauthorized},
		{"TOKEN_ROUTE_UPSERT", http.StatusUnauthorized},
		{"TOKEN_ROUTE_DELETE", http.StatusUnauthorized},
		{"TOKEN_ACCOUNT_CREATE", http.StatusUnauthorized},
		{"TOKEN_ROUTE_PROVISIONING_REPAIR", http.StatusUnauthorized},
		{"TOKEN_FUTURE_UNKNOWN", http.StatusBadRequest},
	} {
		t.Run(entry.command, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type="+entry.command, nil)
			request.Header.Set("Idempotency-Key", uuid.NewString())
			service.adminTokenCommandByIdempotencyKey(response, request)
			if response.Code != entry.status {
				t.Fatalf("dispatch status=%d expected=%d body=%s", response.Code, entry.status, response.Body.String())
			}
		})
	}
}
