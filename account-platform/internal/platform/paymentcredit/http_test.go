package paymentcredit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// The real signed-workload authorization is covered by the Account F12 PG
// integration. These controlled responses test transport binding and rejection.
func TestPaymentCreditHTTPRejectsAmbiguousOrUnboundDecisions(t *testing.T) {
	input := Request{ID: uuid.New(), UserID: uuid.New(), OrderID: uuid.New(), TransactionDigest: Digest([]byte("transaction")), FactsDigest: Digest([]byte("facts"))}
	valid := Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: "ADMITTED"}
	encoded, _ := json.Marshal(valid)
	for _, test := range []struct {
		name, body string
		status     int
		accepted   bool
	}{
		{"admitted", string(encoded), 200, true},
		{"closed", strings.Replace(string(encoded), "ADMITTED", "CLOSED", 1), 200, true},
		{"wrong-operation", strings.Replace(string(encoded), input.ID.String(), uuid.NewString(), 1), 200, false},
		{"wrong-facts", strings.Replace(string(encoded), input.Fingerprint(), Digest([]byte("different-facts")), 1), 200, false},
		{"duplicate-state", strings.TrimSuffix(string(encoded), "}") + `,"state":"ADMITTED"}`, 200, false},
		{"unknown-state", strings.Replace(string(encoded), "ADMITTED", "UNREVIEWED", 1), 200, false},
		{"unknown-field", strings.TrimSuffix(string(encoded), "}") + `,"extra":true}`, 200, false},
		{"trailing-document", string(encoded) + `{}`, 200, false},
		{"oversized", strings.Repeat(" ", 4097) + string(encoded), 200, false},
		{"dependency-error", `private-response-canary`, 503, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/payment-credit-admissions" || r.Header.Get("Authorization") != "Bearer controlled-transport-credential" {
					t.Error("wrong admission transport")
				}
				var actual Request
				if json.NewDecoder(r.Body).Decode(&actual) != nil || actual != input {
					t.Error("wrong bound admission request")
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := NewHTTPClient(server.URL, "controlled-transport-credential")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.Admit(context.Background(), input)
			if test.accepted {
				if err != nil || !result.Matches(input) {
					t.Fatal("valid bound response rejected", err)
				}
			} else if !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "private-response-canary") {
				t.Fatal("ambiguous/failed response was not safely rejected", err)
			}
		})
	}
}

func TestPaymentCreditHTTPDoesNotForwardCredentialOnRedirect(t *testing.T) {
	var forwarded atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer sink.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	client, err := NewHTTPClient(origin.URL, "controlled-redirect-credential")
	if err != nil {
		t.Fatal(err)
	}
	input := Request{ID: uuid.New(), UserID: uuid.New(), OrderID: uuid.New(), TransactionDigest: Digest([]byte("transaction")), FactsDigest: Digest([]byte("facts"))}
	if _, err = client.Admit(context.Background(), input); !errors.Is(err, ErrProtocol) {
		t.Fatal("redirect accepted a financial grant", err)
	}
	if forwarded.Load() != 0 {
		t.Fatal("credential-bearing request reached redirect target")
	}
}

func TestPaymentCreditRetentionDecisionCannotBecomeFinancialOutcome(t *testing.T) {
	input := Request{ID: uuid.New(), UserID: uuid.New(), OrderID: uuid.New(), TransactionDigest: Digest([]byte("retention-transaction")), FactsDigest: Digest([]byte("retention-facts"))}
	for _, state := range []string{"RETENTION_RETAIN", "RETENTION_RELEASE", "APPLIED", "ABORTED", "ADMITTED"} {
		t.Run(state, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/payment-credit-admissions/retention-check" {
					t.Error("wrong retention route")
				}
				_ = json.NewEncoder(w).Encode(Decision{ID: input.ID, Fingerprint: input.Fingerprint(), State: state})
			}))
			defer server.Close()
			client, err := NewHTTPClient(server.URL, "test-credential")
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.CheckPaymentCreditRetention(context.Background(), input)
			accepted := state == "RETENTION_RETAIN" || state == "RETENTION_RELEASE"
			if (err == nil) != accepted {
				t.Fatal("financial/retention response states were mixed", state, err)
			}
		})
	}
}
