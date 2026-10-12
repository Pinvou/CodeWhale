package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/google/uuid"
)

type approvalActivationStub struct {
	state string
	epoch int64
}

func (r *approvalActivationStub) Snapshot(context.Context) (tokengovernance.Snapshot, error) {
	return tokengovernance.Snapshot{State: r.state, Epoch: r.epoch}, nil
}

func TestTokenApprovalConsumerHTTPRecovery(t *testing.T) {
	for _, scenario := range []string{"success and duplicate", "domain response loss", "begin response loss", "safe", "invalid lease", "jsonb event whitespace", "large integer lease mismatch"} {
		t.Run(scenario, func(t *testing.T) {
			requestID, actor, approver, accountID, credentialID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
			fingerprint := strings.Repeat("a", 64)
			now := time.Now().UTC()
			command, _ := json.Marshal(credentialRevokeCommand{TokenAccountID: accountID, ExpectedAccountVersion: 3, CredentialID: credentialID, CredentialVersion: 1})
			critical, _ := json.Marshal(credentialRevokeCritical{TokenAccountID: accountID, TokenAccountVersion: 3, CredentialID: credentialID, CredentialVersion: 1, CredentialStatus: "ACTIVE", CredentialFingerprint: strings.Repeat("b", 64), ActiveRouteSetFingerprint: fingerprint, ProvisioningLeaseSetFingerprint: fingerprint, InflightRequestSetFingerprint: fingerprint, PolicyVersion: 1})
			if scenario == "large integer lease mismatch" {
				critical = bytes.Replace(critical, []byte(`"token_account_version":3`), []byte(`"token_account_version":9007199254740992`), 1)
			}
			policy, _ := approvalpolicy.Lookup("TOKEN_CREDENTIAL_REVOKE")
			authorized := tokenAuthorizedEvent{ApprovalRequestID: requestID, CommandType: policy.CommandType, CommandFingerprint: fingerprint, PolicyVersion: policy.PolicyVersion, ExecutorIdentity: "token-command-executor", ExecutionDeadline: now.Add(time.Minute), NormalizedCommand: command, TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`)}
			payload, _ := json.Marshal(authorized)
			if scenario == "jsonb event whitespace" {
				// PostgreSQL JSONB does not preserve the producer's compact encoding
				// or key order. The subsequent precheck intentionally stays compact
				// and uses the original struct order, but represents identical facts.
				var reordered any
				if err := json.Unmarshal(payload, &reordered); err != nil {
					t.Fatal(err)
				}
				payload, _ = json.MarshalIndent(reordered, "", "  ")
				var frozen tokenAuthorizedEvent
				if err := json.Unmarshal(payload, &frozen); err != nil || len(frozen.CriticalResourceVersions) <= len(critical) || !jsonSemanticEqual(frozen.CriticalResourceVersions, critical) {
					t.Fatal("fixture must freeze longer JSONB-like bytes for the same precheck facts")
				}
			}
			frozenPayload := append([]byte(nil), payload...)
			event := events.Event{ID: uuid.New(), Type: "account.approval-command.authorized.v1", AggregateID: requestID, AggregateVersion: 3, TraceID: "approval-test", OccurredAt: now, Payload: payload}
			lease := tokenApprovalLease{ApprovalRequestID: requestID, CommandType: policy.CommandType, PolicyVersion: policy.PolicyVersion, CommandFingerprint: fingerprint, NormalizedCommand: command, TargetResourceType: authorized.TargetResourceType, TargetResourceID: authorized.TargetResourceID, CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`), RequesterUserID: actor, RequesterAuthorizationVersion: 9, RequesterReason: "A sufficiently detailed revoke reason", ApproverUserID: approver, DecisionReason: "Independent review passed", ExecutionIdempotencyKey: fmt.Sprintf("approval:%s:%s:%s", requestID, policy.CommandType, fingerprint), ExecutorIdentity: "token-command-executor", Status: "EXECUTING"}
			if scenario == "invalid lease" {
				lease.ExecutionIdempotencyKey = "do-not-log-fixture-secret"
			}
			if scenario == "large integer lease mismatch" {
				lease.CriticalResourceVersions = bytes.Replace(critical, []byte(`9007199254740992`), []byte(`9007199254740993`), 1)
				if bytes.Equal(lease.CriticalResourceVersions, critical) {
					t.Fatal("large version fixture did not introduce adjacent integer drift")
				}
			}
			var persisted, claimed bool
			posts, begins, gets, completed, alerts := 0, 0, 0, 0, 0
			domain := TokenCommandResult{CommandReference: uuid.New(), CommandType: policy.CommandType, PolicyVersion: policy.PolicyVersion, ActorUserID: actor, AuthorizationVersion: 9, TargetResourceType: authorized.TargetResourceType, TargetResourceID: authorized.TargetResourceID, Status: "SUCCEEDED", ApprovalRequestID: &requestID, CreatedAt: now, CompletedAt: &now}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-store")
				if r.Header.Get("Authorization") == "" {
					t.Error("missing workload credential")
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/result"):
					gets++
					if !persisted {
						w.WriteHeader(404)
						return
					}
					_ = json.NewEncoder(w).Encode(domain)
				case strings.HasSuffix(r.URL.Path, "/precheck"):
					_ = json.NewEncoder(w).Encode(ApprovalSnapshotProjection{CommandType: policy.CommandType, TargetResourceType: authorized.TargetResourceType, TargetResourceID: authorized.TargetResourceID, CriticalResourceVersions: critical, FinancialSnapshot: json.RawMessage(`{}`), PrecheckFingerprint: fingerprint, TokenObservedAt: now})
				case strings.HasSuffix(r.URL.Path, "/executions"):
					begins++
					lease.FirstClaim = !claimed
					claimed = true
					if scenario == "begin response loss" && begins == 1 {
						connection, _, _ := w.(http.Hijacker).Hijack()
						_ = connection.Close()
						return
					}
					_ = json.NewEncoder(w).Encode(lease)
				case strings.HasSuffix(r.URL.Path, "/execute"):
					posts++
					persisted = true
					var body approvedTokenCommandBody
					if json.NewDecoder(r.Body).Decode(&body) != nil || body.AuthorizationVersion != 9 || body.Envelope.Reason != lease.RequesterReason {
						t.Error("unfrozen dispatch body")
					}
					if scenario == "domain response loss" {
						connection, _, _ := w.(http.Hijacker).Hijack()
						_ = connection.Close()
						return
					}
					_ = json.NewEncoder(w).Encode(domain)
				case strings.HasSuffix(r.URL.Path, "/complete"):
					completed++
					lease.Status = "EXECUTED"
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			reader := &approvalActivationStub{state: "ACTIVE", epoch: 1}
			if scenario == "safe" {
				reader.state = "SAFE_ROLLBACK"
				reader.epoch = 2
			}
			consumer, err := NewTokenApprovalConsumer(policy.CommandType, server.URL, server.URL, "test-workload", reader)
			if err != nil {
				t.Fatal(err)
			}
			consumer.alert = func(id uuid.UUID) {
				if id != requestID {
					t.Error("wrong alert reference")
				}
				alerts++
			}
			first := consumer.Handle(context.Background(), event)
			second := consumer.Handle(context.Background(), event)
			if !bytes.Equal(frozenPayload, event.Payload) {
				t.Fatal("consumer modified the authorized event bytes")
			}
			if scenario == "invalid lease" || scenario == "large integer lease mismatch" {
				for _, result := range []error{first, second} {
					if !errors.Is(result, ErrApprovalResultUnconfirmed) || result.Error() != "TOKEN_APPROVAL_RESULT_UNCONFIRMED: APPROVAL_LEASE_BINDING_INVALID" || strings.Contains(result.Error(), lease.ExecutionIdempotencyKey) || strings.Contains(result.Error(), "900719925474099") {
						t.Fatalf("unconfirmed lease diagnosis changed contract or exposed input: %v", result)
					}
				}
				if posts != 0 || completed != 0 || alerts != 2 {
					t.Fatal("invalid lease was executed, completed or silently acknowledged")
				}
				return
			}
			if scenario == "safe" {
				if first == nil || second == nil || posts != 0 || begins != 0 || gets != 0 || alerts != 2 {
					t.Fatalf("SAFE wrote/read lease: %v %v counts=%d/%d/%d/%d", first, second, posts, begins, gets, alerts)
				}
				return
			}
			if scenario == "begin response loss" {
				if first == nil || second == nil || posts != 0 || completed != 0 || begins != 2 || alerts != 2 {
					t.Fatalf("lost begin resent domain: %v %v counts=%d/%d/%d/%d", first, second, posts, begins, completed, alerts)
				}
				return
			}
			if first != nil || second != nil || posts != 1 || completed != 2 || alerts != 0 {
				t.Fatalf("recovery not closed: %v %v posts/completed/alerts=%d/%d/%d", first, second, posts, completed, alerts)
			}
			if scenario == "domain response loss" && gets < 3 {
				t.Fatal("domain response loss was not recovered by GET")
			}
		})
	}
}

func TestTokenApprovalJSONComparisonPreservesIntegerVersions(t *testing.T) {
	left := json.RawMessage(`{"version":9007199254740992,"nested":{"last":9223372036854775807}}`)
	if !equalTokenApprovalJSONObjects(left, json.RawMessage(` { "nested": {"last":9223372036854775807}, "version":9007199254740992 } `)) {
		t.Fatal("equivalent objects with large integer versions were rejected")
	}
	for _, right := range []json.RawMessage{
		json.RawMessage(`{"version":9007199254740993,"nested":{"last":9223372036854775807}}`),
		json.RawMessage(`{"version":9007199254740992,"nested":{"last":9223372036854775806}}`),
	} {
		if equalTokenApprovalJSONObjects(left, right) {
			t.Fatal("different exact integer versions compared equal")
		}
	}
	for _, invalid := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`{} {}`), json.RawMessage(`{} garbage`)} {
		if equalTokenApprovalJSONObjects(invalid, invalid) || equalTokenApprovalJSONObjects(left, invalid) {
			t.Fatal("non-single-object input compared equal")
		}
	}
}

func TestApprovalTransportRejectsDuplicateAndUnknownFields(t *testing.T) {
	for _, raw := range []string{`{"state":"ACTIVE","state":"SAFE"}`, `{"unknown":true}`, `{"normalized_command":{"id":1,"id":2}}`, `{} {}`} {
		var value tokenAuthorizedEvent
		if decodeApprovalTransport([]byte(raw), &value) == nil {
			t.Fatalf("ambiguous transport accepted: %s", raw)
		}
	}
	type claim struct {
		FirstClaim bool `json:"first_claim"`
	}
	var value claim
	if err := decodeApprovalTransport([]byte(`{"first_claim":false}`), &value); err != nil || value.FirstClaim {
		t.Fatalf("valid false claim rejected: %v", err)
	}
	for _, raw := range []string{`{"first_claim":false,"First_Claim":true}`, `{"First_Claim":true}`, `{"first_claim":null}`, `{}`, `{"first_claim":false,"first_claim":true}`} {
		if decodeApprovalTransport([]byte(raw), &value) == nil {
			t.Fatalf("claim authority ambiguity accepted: %s", raw)
		}
	}
}
