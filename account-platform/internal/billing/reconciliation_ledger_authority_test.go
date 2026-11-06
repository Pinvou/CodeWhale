package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLedgerRepairAuthorityHTTPContract(t *testing.T) {
	id := ledgerRepairProofIdentity{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 7}
	var calls int
	mode := "ok"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/ledger-repair/authorization-proofs:batch" || r.Header.Get("Authorization") != "Bearer controlled-test" {
			t.Error("request did not use the expected endpoint and service identity")
		}
		var request struct {
			SchemaVersion int                         `json:"schema_version"`
			Items         []ledgerRepairProofIdentity `json:"items"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.SchemaVersion != 1 || len(request.Items) != 1 || request.Items[0] != id {
			t.Error("unexpected proof request")
		}
		switch mode {
		case "http":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "unauthorized":
			w.WriteHeader(http.StatusUnauthorized)
		case "malformed":
			_, _ = w.Write([]byte(`{"schema_version":1,"items":[],"unexpected":1}`))
		case "mismatch":
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "items": []any{map[string]any{"approval_request_id": uuid.New(), "authorization_event_id": id.AuthorizationEventID, "approval_epoch": id.ApprovalEpoch, "status": "VERIFIED"}}})
		case "trailing":
			_, _ = w.Write([]byte(`{"schema_version":1,"items":[]} {}`))
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "items": []any{map[string]any{"approval_request_id": id.ApprovalRequestID, "authorization_event_id": id.AuthorizationEventID, "approval_epoch": id.ApprovalEpoch, "status": strings.ToUpper(mode)}}})
		}
	}))
	defer server.Close()
	client, err := NewHTTPLedgerRepairProofAuthority(server.URL, "controlled-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if results, err := client.ReadLedgerRepairProofs(context.Background(), nil); err != nil || len(results) != 0 || calls != 0 {
		t.Fatal("empty proof set contacted authority", calls, err)
	}
	mode = "verified"
	if results, err := client.ReadLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id}); err != nil || len(results) != 1 || results[0].Status != "VERIFIED" {
		t.Fatal("valid response rejected", err)
	}
	mode = "unavailable"
	if results, err := client.ReadLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id}); err != nil || len(results) != 1 || results[0].Status != "UNAVAILABLE" {
		t.Fatal("per-item unavailable was lost", err)
	}
	for _, bad := range []string{"http", "unauthorized", "malformed", "mismatch", "trailing", "unknown"} {
		mode = bad
		if _, err := client.ReadLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id}); !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) {
			t.Fatal("invalid authority response accepted", bad, err)
		}
	}
	prior := calls
	if _, err := client.ReadLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id, id}); !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) || calls != prior {
		t.Fatal("duplicate request reached authority", err)
	}
}

func TestLedgerRepairAuthorityBatchesKeepOrderAndUnavailable(t *testing.T) {
	var sizes []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			SchemaVersion int                         `json:"schema_version"`
			Items         []ledgerRepairProofIdentity `json:"items"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		sizes = append(sizes, len(request.Items))
		items := make([]ledgerRepairAuthorizationProof, 0, len(request.Items))
		for i, id := range request.Items {
			status := "VERIFIED"
			if len(sizes) == 2 && i == 1 {
				status = "UNAVAILABLE"
			}
			items = append(items, ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: status})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "items": items})
	}))
	defer server.Close()
	authority, err := NewHTTPLedgerRepairProofAuthority(server.URL, "controlled-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	items := make([]ledgerRepairProofIdentity, 33)
	for i := range items {
		items[i] = ledgerRepairProofIdentity{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: int64(i + 1)}
	}
	got, err := authority.ReadLedgerRepairProofs(context.Background(), items)
	if err != nil || len(got) != len(items) || len(sizes) != 3 || sizes[0] != 16 || sizes[1] != 16 || sizes[2] != 1 {
		t.Fatal("unexpected chunking", sizes, err)
	}
	for i := range got {
		if got[i].ledgerRepairProofIdentity != items[i] || (got[i].Status == "UNAVAILABLE") != (i == 17) {
			t.Fatal("batch order or unavailable status changed", i)
		}
	}
}

func TestLedgerRepairAuthorityTrustedURLs(t *testing.T) {
	for _, tc := range []struct {
		address, env string
		allowed      bool
	}{
		{"http://account-service:8080", "production-new", true},
		{"http://release-account-platform-account-service:8080", "production-new", true},
		{"https://account.example:443", "production-new", true},
		{"http://127.0.0.1:18080", "test", true},
		{"http://[::1]:18080", "development", true},
		{"http://127.0.0.1:18080", "production-new", false},
		{"http://attacker.example:8080", "production-new", false},
		{"http://account-service.attacker.example:8080", "production-new", false},
		{"http://account-service:80", "production-new", false},
		{"http://account-service:8080/prefix", "production-new", false},
		{"http://localhost:8080", "staging", false},
		{"http://127.0.0.1:8080", "", false},
		{"https://user:secret@account.example", "test", false},
		{"https://account.example?x=1", "test", false},
	} {
		_, err := NewHTTPLedgerRepairProofAuthority(tc.address, "controlled-test", tc.env)
		if (err == nil) != tc.allowed {
			t.Fatal("unexpected URL acceptance", tc.address, tc.env, err)
		}
	}
}

func TestLedgerRepairAuthorityDoesNotForwardCredentialOnRedirect(t *testing.T) {
	reached := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	client, err := NewHTTPLedgerRepairProofAuthority(redirect.URL, "controlled-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	id := ledgerRepairProofIdentity{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1}
	if _, err = client.ReadLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id}); !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) || reached {
		t.Fatal("redirect carried service identity", err)
	}
}

func TestLedgerRepairAuthoritySlowBatchPreservesPriorResults(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(500 * time.Millisecond):
			}
			return
		}
		var request struct {
			Items []ledgerRepairProofIdentity `json:"items"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid request")
			return
		}
		items := make([]ledgerRepairAuthorizationProof, 0, len(request.Items))
		for _, id := range request.Items {
			items = append(items, ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "VERIFIED"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"schema_version": 1, "items": items})
	}))
	defer server.Close()
	client, err := NewHTTPLedgerRepairProofAuthority(server.URL, "controlled-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]ledgerRepairProofIdentity, 20)
	for i := range ids {
		ids[i] = ledgerRepairProofIdentity{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	got, err := client.ReadLedgerRepairProofs(ctx, ids)
	if !errors.Is(err, ErrLedgerRepairAuthorityUnavailable) || len(got) != 16 || calls.Load() != 2 || time.Since(started) > 2*time.Second {
		t.Fatal("slow second chunk lost first chunk or exceeded budget", len(got), calls.Load(), err)
	}
	for i := range got {
		if got[i].ledgerRepairProofIdentity != ids[i] {
			t.Fatal("completed chunk changed", i)
		}
	}
}

type waitingLedgerAuthority struct {
	proof ledgerRepairAuthorizationProof
}

func (a waitingLedgerAuthority) ReadLedgerRepairProofs(ctx context.Context, _ []ledgerRepairProofIdentity) ([]ledgerRepairAuthorizationProof, error) {
	<-ctx.Done()
	return []ledgerRepairAuthorizationProof{a.proof}, ErrLedgerRepairAuthorityUnavailable
}

func TestLedgerRepairAuthorityBudgetAndCallerCancellation(t *testing.T) {
	id := ledgerRepairProofIdentity{ApprovalRequestID: uuid.New(), AuthorizationEventID: uuid.New(), ApprovalEpoch: 1}
	p := &PaymentCoordinator{ledgerRepairAuthority: waitingLedgerAuthority{proof: ledgerRepairAuthorizationProof{ledgerRepairProofIdentity: id, Status: "VERIFIED"}}}
	start := time.Now()
	proofs, err := p.fetchLedgerRepairProofs(context.Background(), []ledgerRepairProofIdentity{id})
	if err != nil || proofs[id].Status != "VERIFIED" || time.Since(start) > 6*time.Second {
		t.Fatal("authority budget did not preserve partial results", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.fetchLedgerRepairProofs(ctx, []ledgerRepairProofIdentity{id}); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation was swallowed", err)
	}
}
