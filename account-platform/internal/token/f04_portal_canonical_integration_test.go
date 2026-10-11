package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This opt-in test uses the existing isolated Token repository and controlled
// identity/authorization helpers. The envelope comes from the actual Portal
// JavaScript producer, not a second implementation of its fingerprint logic.
func TestF04PortalCanonicalProtocolDraftPostgres(t *testing.T) {
	r, ctx := retainedManagementTokenRepository(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("configured integration test requires Node:", err)
	}
	fixture, err := filepath.Abs(filepath.Join("..", "..", "..", "pinvou3-app", "tests", "fixtures", "f04-governed-envelope.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	actor := uuid.New()
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
	if _, err = owner.Exec(ctx, `UPDATE token_command_governance_activation SET state='ACTIVE',epoch=1,handler_set_digest=$1,policy_set_digest=$1,approved_runtime_roles=ARRAY['token_gateway'],operator_id='controlled-fixture',reason='Controlled Portal canonical validation',request_id=$2 WHERE singleton`, digest, uuid.New()); err != nil {
		t.Fatal(err)
	}
	r.routeActivationGuard = store
	preHandler := f04IdentityHandler(http.HandlerFunc(s.adminTokenCommandPrecheck))
	submit := f04IdentityHandler(approvedCommandFixtureHandler(s, "/admin/v1/commands", s.adminCreateTokenCommand))
	// Replace the shared helper's synthetic activation with the persisted fence.
	s.WithTokenCommandGovernanceActivation(store)
	recovery := f04RecoveryHandler(s)

	cases := []struct {
		name   string
		schema json.RawMessage
	}{
		{"ascii", json.RawMessage(`{"type":"object","title":"ASCII"}`)},
		{"html-and-separators", json.RawMessage("{\"type\":\"object\",\"title\":\"<tag>&\u2028\u2029\"}")},
		{"integer-keys", json.RawMessage(`{"type":"object","properties":{"10":{"title":"ten"},"2":{"title":"two"}}}`)},
		{"non-bmp-and-bmp-keys", json.RawMessage("{\"type\":\"object\",\"properties\":{\"\U00010000\":{\"title\":\"astral\"},\"\uE000\":{\"title\":\"bmp\"}}}")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code := "F04_CANON_" + strings.ToUpper(strings.ReplaceAll(tc.name, "-", "_"))
			command := map[string]any{"protocol_code": code, "request_schema": tc.schema}
			envelope := func(command map[string]any) []byte {
				t.Helper()
				input := TokenCommandPrecheckRequest{CommandType: "TOKEN_PROTOCOL_DRAFT_CREATE", TargetResourceType: "PROTOCOL", TargetResourceID: code, Command: mustMarshalJSON(t, command)}
				pre := f04Request(preHandler, http.MethodPost, "/precheck", mustMarshalJSON(t, input), "")
				if pre.Code != http.StatusOK || !json.Valid(pre.Body.Bytes()) {
					t.Fatalf("actual precheck: status=%d body=%s", pre.Code, pre.Body.String())
				}
				var projection TokenCommandPrecheckResult
				if err := json.Unmarshal(pre.Body.Bytes(), &projection); err != nil || len(projection.ExpectedVersions) == 0 || projection.PrecheckFingerprint == "" {
					t.Fatalf("precheck lacks authoritative versions/fingerprint: %v", err)
				}
				nodeInput := mustMarshalJSON(t, map[string]any{"commandType": input.CommandType, "targetResourceType": input.TargetResourceType, "targetResourceID": code, "reason": "Controlled Portal canonical draft", "impact": json.RawMessage(pre.Body.Bytes()), "command": command})
				nodeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				process := exec.CommandContext(nodeCtx, node, fixture)
				process.Stdin = bytes.NewReader(nodeInput)
				var stdout, stderr bytes.Buffer
				process.Stdout, process.Stderr = &stdout, &stderr
				if err := process.Run(); err != nil {
					t.Fatalf("actual Portal envelope producer: %v stderr=%s", err, stderr.String())
				}
				if !json.Valid(stdout.Bytes()) {
					t.Fatal("actual Portal producer returned invalid JSON")
				}
				return append([]byte(nil), stdout.Bytes()...)
			}
			body := envelope(command)
			key := "f04-portal-canonical-" + uuid.NewString()
			decodeResult := func(status int, raw []byte) TokenCommandResult {
				var result TokenCommandResult
				if (status != http.StatusOK && status != http.StatusCreated) || json.Unmarshal(raw, &result) != nil || result.Status != "SUCCEEDED" || result.CommandReference == uuid.Nil {
					t.Fatalf("command receipt: status=%d body=%s", status, raw)
				}
				return result
			}
			firstResponse := f04Request(submit, http.MethodPost, "/admin/v1/commands", body, key)
			if firstResponse.Code != http.StatusCreated {
				t.Fatalf("fresh command: status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
			}
			first := decodeResult(firstResponse.Code, firstResponse.Body.Bytes())
			var draft struct {
				ID uuid.UUID `json:"protocol_definition_id"`
			}
			if err := json.Unmarshal(first.RedactedResult, &draft); err != nil || draft.ID == uuid.Nil {
				t.Fatalf("missing persisted draft identity: %v", err)
			}
			var firstProjection any
			if err := json.Unmarshal(first.RedactedResult, &firstProjection); err != nil {
				t.Fatal(err)
			}
			assertOne := func() {
				t.Helper()
				var count, receipts int
				var persisted uuid.UUID
				var exactSchema bool
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM protocol_definitions WHERE protocol_code=$1`, code).Scan(&count); err != nil || count != 1 {
					t.Fatalf("draft count=%d err=%v", count, err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT id,request_schema=$2::jsonb FROM protocol_definitions WHERE protocol_code=$1`, code, string(tc.schema)).Scan(&persisted, &exactSchema); err != nil || persisted != draft.ID || !exactSchema {
					t.Fatalf("draft identity/schema changed: id=%s exact=%v err=%v", persisted, exactSchema, err)
				}
				if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE actor_user_id=$1 AND command_type='TOKEN_PROTOCOL_DRAFT_CREATE' AND target_resource_id=$2`, actor, code).Scan(&receipts); err != nil || receipts != 1 {
					t.Fatalf("receipt count=%d err=%v", receipts, err)
				}
			}
			assertOne()
			for _, response := range []*httptest.ResponseRecorder{
				f04Request(submit, http.MethodPost, "/admin/v1/commands", body, key),
				f04Request(recovery, http.MethodGet, "/recovery?command_type=TOKEN_PROTOCOL_DRAFT_CREATE", nil, key),
			} {
				if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("replay/recovery: status=%d cache=%q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Body.String())
				}
				result := decodeResult(response.Code, response.Body.Bytes())
				var recoveredProjection any
				if err := json.Unmarshal(result.RedactedResult, &recoveredProjection); err != nil {
					t.Fatal(err)
				}
				if result.CommandReference != first.CommandReference || result.IdempotencyFingerprint != first.IdempotencyFingerprint || !reflect.DeepEqual(recoveredProjection, firstProjection) {
					t.Fatal("replay/recovery changed the immutable receipt")
				}
			}
			// Obtain a valid fresh precheck for another payload, so rejection must
			// come from reuse of the committed key, not a fabricated fingerprint.
			changed := envelope(map[string]any{"protocol_code": code, "request_schema": json.RawMessage(`{"type":"object","changed":true}`)})
			rejected := f04Request(submit, http.MethodPost, "/admin/v1/commands", changed, key)
			if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
				t.Fatalf("changed payload reused committed key: status=%d body=%s", rejected.Code, rejected.Body.String())
			}
			assertOne()
		})
	}
}
