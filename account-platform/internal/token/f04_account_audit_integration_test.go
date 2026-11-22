package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This opt-in test keeps the original isolated public-migration/runtime fixture.
// Provider configuration and HTTP authorization are controlled. Commands, route
// tombstones, immutable receipts/audits and the registered audit read are real.
func TestF04AccountAuditIncludesGovernedRouteHistory(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	actor, account, foreign, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, id := range []uuid.UUID{account, foreign} {
		if _, err := r.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'controlled-audit','ACTIVE',100,$4,$5,$5,'PLATFORM')`, id, uuid.New(), uuid.New(), actor, now); err != nil {
			t.Fatal(err)
		}
		// createAccountEffect persists this required sharing row with the account.
		if _, err := r.pool.Exec(ctx, `INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$2)`, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := r.SyncCatalog(ctx, "controlled-audit", "v1", []DiscoveredModel{{UpstreamModelRef: "audit-model", DisplayName: "Audit model"}}, 32000, 1024, actor); err != nil {
		t.Fatal(err)
	}
	_, models, err := r.ListCatalog(ctx)
	if err != nil || len(models) != 1 {
		t.Fatal("catalog fixture", err, len(models))
	}
	model := models[0]
	cipher, err := r.secrets.Encrypt([]byte("controlled-audit-provider-secret"), []byte(account.String()+":"+credential.String()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.pool.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,validation_status,allowed_models,created_by,created_at) VALUES($1,$2,$3,'https://provider.invalid',$4,$5,$6,1,'ACTIVE','VALID',$7,$8,$9)`, credential, account, model.ProviderID, cipher, r.secrets.KeyID(), strings.Repeat("a", 64), []string{model.UpstreamModelRef}, actor, now); err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Bool
	allowed.Store(true)
	s := f04AuthorizedService(t, r, &actor, &allowed)
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
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='ACTIVE',epoch=1,handler_set_digest=$1,policy_set_digest=$1,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='Controlled audit query regression',request_id=$2 WHERE singleton`, digest, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err = r.ConfigureAdminPagination(AdminPaginationConfig{Enabled: true, CursorHMACKey: []byte(strings.Repeat("q", 32))}); err != nil {
		t.Fatal(err)
	}
	if err = r.SetAdminPaginationGate(ctx, true); err != nil {
		t.Fatal(err)
	}
	r.routeActivationGuard = store
	pre := f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck))
	post := f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand))
	s.WithTokenCommandGovernanceActivation(store)
	execute := func(kind, targetType string, target string, command any) TokenCommandResult {
		t.Helper()
		input := TokenCommandPrecheckRequest{CommandType: kind, TargetResourceType: targetType, TargetResourceID: target, Command: mustMarshalJSON(t, command)}
		p := f04Request(pre, http.MethodPost, "/precheck", mustMarshalJSON(t, input), "")
		var projection TokenCommandPrecheckResult
		if p.Code != 200 || json.Unmarshal(p.Body.Bytes(), &projection) != nil {
			t.Fatalf("actual precheck %s: %d %s", kind, p.Code, p.Body.String())
		}
		if len(projection.Blockers) != 0 {
			t.Fatalf("controlled command %s is not admissible: %v", kind, projection.Blockers)
		}
		envelope := modelAccountEnvelope(t, kind, targetType, target, input.Command, "Controlled audit history", projection)
		key := "f04-account-audit-" + uuid.NewString()
		body := mustMarshalJSON(t, envelope)
		first := f04Request(post, http.MethodPost, "/admin/v1/commands", body, key)
		var receipt TokenCommandResult
		if first.Code != 200 && first.Code != 201 || json.Unmarshal(first.Body.Bytes(), &receipt) != nil || receipt.Status != "SUCCEEDED" {
			t.Fatalf("actual command %s: %d %s", kind, first.Code, first.Body.String())
		}
		second := f04Request(post, http.MethodPost, "/admin/v1/commands", body, key)
		var replay TokenCommandResult
		if second.Code != 200 || json.Unmarshal(second.Body.Bytes(), &replay) != nil || replay.CommandReference != receipt.CommandReference || replay.Status != "SUCCEEDED" {
			t.Fatal("receipt-first replay changed audit identity", second.Code, second.Body.String())
		}
		return receipt
	}
	upsert := execute("TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), map[string]any{"token_account_id": account, "model_id": model.ModelID, "protocol_version_id": model.ProtocolDefinitionID, "priority": 100, "default_scope": "NONE"})
	var route uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT id FROM routing_policies WHERE token_account_id=$1 AND model_id=$2`, account, model.ModelID).Scan(&route); err != nil {
		t.Fatal(err)
	}
	deleted := execute("TOKEN_ROUTE_DELETE", "TOKEN_ROUTE", route.String(), map[string]any{"token_account_id": account, "route_id": route})
	changed := execute("TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", strings.ToUpper(account.String()), map[string]any{"target_status": "DISABLED"})
	resumed := execute("TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", "urn:uuid:"+account.String(), map[string]any{"target_status": "ACTIVE"})
	stopped := execute("TOKEN_ACCOUNT_STATUS_CHANGE", "TOKEN_ACCOUNT", "{"+account.String()+"}", map[string]any{"target_status": "DISABLED"})
	var live, retained, tombstones, commandAudits int
	if err := r.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM routing_policies WHERE id=$1 AND reference_status<>'DELETED'),(SELECT count(*) FROM routing_policies WHERE id=$1 AND token_account_id=$2 AND model_id=$4 AND protocol_version_id=$5 AND reference_status='DELETED'),(SELECT count(*) FROM routing_policy_tombstones WHERE route_id=$1 AND token_account_id=$2),(SELECT count(*) FROM token_audit_logs WHERE summary->>'command_reference'=ANY($3))`, route, account, []string{upsert.CommandReference.String(), deleted.CommandReference.String(), changed.CommandReference.String(), resumed.CommandReference.String(), stopped.CommandReference.String()}, model.ModelID, model.ProtocolDefinitionID).Scan(&live, &retained, &tombstones, &commandAudits); err != nil || live != 0 || retained != 1 || tombstones != 1 || commandAudits != 5 {
		t.Fatal("actual soft deletion/retained identity/tombstone/replay audit facts", live, retained, tombstones, commandAudits, err)
	}
	legacy := func(id uuid.UUID, reason string) {
		t.Helper()
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := recordTokenAudit(ctx, tx, actor, "TOKEN_ACCOUNT_CREATED", "TOKEN_ACCOUNT", id, `jsonb_build_object('reason',$6)`, reason); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	legacy(account, "controlled legacy account history")
	legacy(foreign, "controlled foreign account history")
	read, _ := f04LegacyOverviewHandler(t, r, []string{"platform.tokens.read", "platform.audit.read"})
	get := func(h http.Handler, params url.Values) (*httptest.ResponseRecorder, struct {
		Items []AdminResourceAudit `json:"items"`
		Next  *string              `json:"next_cursor"`
	}) {
		t.Helper()
		w := f04Request(h, http.MethodGet, "/admin/v1/resource-audit?"+params.Encode(), nil, "")
		var result struct {
			Items []AdminResourceAudit `json:"items"`
			Next  *string              `json:"next_cursor"`
		}
		if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatal("invalid actual query envelope")
		}
		return w, result
	}
	params := func(id uuid.UUID) url.Values {
		return url.Values{"resource_type": {"TOKEN_ACCOUNT"}, "resource_id": {id.String()}}
	}
	t.Run("existing-account-query-discovers-governed-and-legacy", func(t *testing.T) {
		w, page := get(read, params(account))
		if w.Code != 200 || len(page.Items) != 9 {
			t.Fatal("account audit cannot discover existing governed history", w.Code, w.Body.String())
		}
		counts := map[string]int{}
		for _, item := range page.Items {
			counts[item.Action]++
		}
		if !reflect.DeepEqual(counts, map[string]int{"TOKEN_ACCOUNT_CREATED": 1, "TOKEN_ACCOUNT_STATUS_CHANGE": 3, "TOKEN_ACCOUNT_STATUS_CHANGED": 3, "TOKEN_ROUTE_UPSERT": 1, "TOKEN_ROUTE_DELETE": 1}) {
			t.Fatal("legacy/governed route/account audit duplication or loss", counts)
		}
	})
	t.Run("account-and-action-filters-retain-isolation", func(t *testing.T) {
		p := params(account)
		p.Set("action", "TOKEN_ROUTE_DELETE")
		w, page := get(read, p)
		if w.Code != 200 || len(page.Items) != 1 || page.Items[0].Action != "TOKEN_ROUTE_DELETE" {
			t.Fatal("deleted route audit filter", w.Code, w.Body.String())
		}
		w, page = get(read, params(foreign))
		if w.Code != 200 || len(page.Items) != 1 || page.Items[0].ResourceID != foreign {
			t.Fatal("foreign account absorbed governed route history", w.Code, w.Body.String())
		}
	})
	t.Run("malformed-and-unbound-summary-do-not-create-membership", func(t *testing.T) {
		for _, mutation := range []string{`jsonb_set(summary,'{command_reference}','"not-a-uuid"')`, `jsonb_set(summary,'{command_reference}','null')`, `jsonb_set(summary,'{before_state,token_account_id}',to_jsonb($2::text))`, `jsonb_set(summary,'{status}','"PENDING"')`} {
			args := []any{upsert.CommandReference.String()}
			if strings.Contains(mutation, "$2") {
				args = append(args, foreign.String())
			}
			if _, err := r.pool.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) SELECT gen_random_uuid(),actor_user_id,action,resource_type,resource_id,`+mutation+`,clock_timestamp() FROM token_audit_logs WHERE summary->>'command_reference'=$1 AND action='TOKEN_ROUTE_UPSERT'`, args...); err != nil {
				t.Fatal(err)
			}
		}
		w, page := get(read, params(account))
		if w.Code != 200 || len(page.Items) != 9 {
			t.Fatal("malformed/mismatched receipt created audit membership or broke query", w.Code, w.Body.String())
		}
	})
	t.Run("permissions-and-unsupported-type-stay-closed", func(t *testing.T) {
		denied, _ := f04LegacyOverviewHandler(t, r, []string{"platform.models.read", "platform.audit.read"})
		w, _ := get(denied, params(account))
		if w.Code != 403 {
			t.Fatal("account read permission weakened", w.Code)
		}
		p := params(account)
		p.Set("resource_type", "TOKEN_ROUTE")
		w, _ = get(read, p)
		if w.Code != 400 {
			t.Fatal("new public resource selector silently admitted", w.Code)
		}
	})
	t.Run("cursor-keeps-original-membership-and-account-scope", func(t *testing.T) {
		p := params(account)
		p.Set("limit", "1")
		w, page := get(read, p)
		if w.Code != 200 || len(page.Items) != 1 || page.Next == nil {
			t.Fatal("first audit snapshot", w.Code, w.Body.String())
		}
		original := *page.Next
		legacy(account, "late history outside original snapshot")
		ids := map[uuid.UUID]bool{page.Items[0].ID: true}
		for page.Next != nil {
			p.Set("cursor", *page.Next)
			w, page = get(read, p)
			if w.Code != 200 {
				t.Fatal("snapshot continuation", w.Code, w.Body.String())
			}
			for _, item := range page.Items {
				if ids[item.ID] {
					t.Fatal("duplicate audit across cursor pages")
				}
				ids[item.ID] = true
			}
		}
		if len(ids) != 9 {
			t.Fatal("late audit changed original snapshot membership", len(ids))
		}
		p.Set("cursor", original)
		p.Set("resource_id", foreign.String())
		w, _ = get(read, p)
		if w.Code != 400 {
			t.Fatal("cursor crossed account scope", w.Code, w.Body.String())
		}
	})
	t.Run("create-request-history-belongs-to-final-account-not-owner-uuid", func(t *testing.T) {
		// Controlled immutable history in the current schema: this is a query
		// fixture, not a claim that the CREATE authority/SAGA was executed. The
		// original request remains PENDING and uses the owner UUID as resource.
		// A distinct authorized query actor gets the original six-per-minute
		// budget; preceding independent cases do not consume this case's quota.
		historyRead, _ := f04LegacyOverviewHandler(t, r, []string{"platform.tokens.read", "platform.audit.read"})
		reference := uuid.New()
		insert, err := r.pool.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,after_state,redacted_result,audit_request_id,completed_at)
SELECT $1,'TOKEN_ACCOUNT_CREATE',policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,'TOKEN_ACCOUNT',($4::uuid)::text,reason,jsonb_build_object('owner_type','USER','owner_id',$4::uuid),precheck_fingerprint,'SUCCEEDED',jsonb_build_object('owner_type','USER','owner_id',$4::uuid),jsonb_build_object('token_account_id',$3::uuid,'owner_type','USER','owner_id',$4::uuid,'status','ACTIVE','account_version',1),jsonb_build_object('token_account_id',$3::uuid,'owner_type','USER','owner_id',$4::uuid,'status','ACTIVE','account_version',1),audit_request_id,clock_timestamp()
FROM token_command_results WHERE command_reference=$2`, reference, changed.CommandReference, foreign, account)
		if err != nil || insert.RowsAffected() != 1 {
			t.Fatal("controlled historical CREATE receipt", err)
		}
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err = recordTokenAudit(ctx, tx, actor, "TOKEN_ACCOUNT_PROVISIONING_REQUESTED", "TOKEN_ACCOUNT", account, `jsonb_build_object('command_reference',$6,'owner_type',$7,'owner_id',$8,'status','PENDING')`, reference, "USER", account); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		p := params(foreign)
		p.Set("action", "TOKEN_ACCOUNT_PROVISIONING_REQUESTED")
		w, page := get(historyRead, p)
		if w.Code != 200 || len(page.Items) != 1 || page.Items[0].ResourceID != account || !strings.Contains(string(page.Items[0].Summary), `"PENDING"`) {
			t.Fatal("original PENDING request did not follow completed receipt account", w.Code, w.Body.String())
		}
		p.Set("resource_id", account.String())
		w, page = get(historyRead, p)
		if w.Code != 200 || len(page.Items) != 0 {
			t.Fatal("owner UUID collision falsely became account audit membership", w.Code, w.Body.String())
		}
		for _, mutation := range []string{`jsonb_set(summary,'{command_reference}','"not-a-uuid"')`, `jsonb_set(summary,'{owner_id}',to_jsonb($2::text))`, `jsonb_set(summary,'{owner_type}','"ORG"')`, `jsonb_set(summary,'{status}','"SUCCEEDED"')`} {
			args := []any{reference.String()}
			if strings.Contains(mutation, "$2") {
				args = append(args, foreign.String())
			}
			if _, err = r.pool.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) SELECT gen_random_uuid(),actor_user_id,action,resource_type,resource_id,`+mutation+`,clock_timestamp() FROM token_audit_logs WHERE summary->>'command_reference'=$1 AND action='TOKEN_ACCOUNT_PROVISIONING_REQUESTED'`, args...); err != nil {
				t.Fatal(err)
			}
		}
		p.Set("resource_id", foreign.String())
		w, page = get(historyRead, p)
		if w.Code != 200 || len(page.Items) != 1 {
			t.Fatal("invalid historical owner/reference/status changed account membership", w.Code, w.Body.String())
		}
	})

}
