package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

func TestApprovalAuthorizationHTTPClientClosedRequests(t *testing.T) {
	eventID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	requestID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	durable := "billing-wallet-adjustment-executor-p1-v1"
	errorCode := "DOMAIN_PRECONDITION_REJECTED"
	binding := ApprovalAuthorizationBinding{EventID: eventID, Durable: durable, Workload: "billing-command-executor", ApprovalRequestID: requestID, ApprovedRequestVersion: 7, ApprovalEpoch: 2, SerializationVersion: "APPROVAL_RFC8785_DECIMAL_V1", Provenance: "FROZEN_AT_APPROVAL", EventDigest: strings.Repeat("a", 64), DigestKind: "DELIVERY_BYTES", CommandType: "WALLET_ADJUSTMENT", PolicyVersion: 1, ExecutorIdentity: "billing-command-executor"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer service-token" {
			t.Fatal("missing service credential")
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/binding"):
			if r.URL.Query().Get("durable") != durable {
				t.Fatalf("binding durable=%q", r.URL.Query().Get("durable"))
			}
			_ = json.NewEncoder(w).Encode(binding)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/deliveries/"+durable):
			_ = json.NewEncoder(w).Encode(ApprovalAuthorizationDeliveryState{DomainAttemptCount: 12, State: "DOMAIN_INFLIGHT", Version: 25, LastErrorCode: &errorCode})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/observe"):
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream_sequence"] != "18446744073709551615" || body["workload"] != nil {
				t.Fatalf("observe body=%v", body)
			}
			_ = json.NewEncoder(w).Encode(ApprovalAuthorizationDeliveryState{State: "READY", Version: 1})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewHTTPApprovalExecutionGate(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := client.ResolveAuthorizationBinding(context.Background(), eventID, durable)
	if err != nil || resolved != binding {
		t.Fatalf("resolved=%+v err=%v", resolved, err)
	}
	observed, err := client.ObserveAuthorizationDelivery(context.Background(), binding, "ACCOUNT_PLATFORM", approvalconsumer.Sequence(^uint64(0)))
	if err != nil || observed.State != "READY" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
	read, err := client.AuthorizationDeliveryState(context.Background(), binding)
	if err != nil || read.LastErrorCode == nil || *read.LastErrorCode != errorCode {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestApprovalAuthorizationHTTPClientRejectsOpenResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"domain_attempt_count":0,"state":"READY","version":1,"unexpected":true}`))
	}))
	t.Cleanup(server.Close)
	client, err := NewHTTPApprovalExecutionGate(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	binding := ApprovalAuthorizationBinding{EventID: uuid.New(), Durable: "billing-command-executor-v1"}
	if _, err := client.AuthorizationDeliveryState(context.Background(), binding); err == nil {
		t.Fatal("open Account response was accepted")
	}
}
