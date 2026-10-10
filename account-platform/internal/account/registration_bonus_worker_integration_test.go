package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Account state, eligibility, saga and lease completion are real PostgreSQL.
// The Billing HTTP authority is controlled here; its financial posting contract
// is separately exercised by the Billing registration-bonus integration test.
func TestRegistrationBonusWorkerRetainsFactsAfterLostAcknowledgement(t *testing.T) {
	repository := retainedWalletFenceActors(t).repository(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := repository.ConfigureRegistrationBonusFingerprint("registration-bonus-v1", bytes.Repeat([]byte{0x31}, 32)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.PrepareRegistrationBonusEligibility(ctx); err != nil {
		t.Fatal(err)
	}
	userID := commitRegistrationBonusTestOperation(t, ctx, repository, "bonus_worker", "+8613700137000")
	assertRegistrationBonusJobMode(t, ctx, repository, userID, "EVALUATE")
	walletID, claimID := uuid.New(), uuid.New()
	var frozen RegistrationWalletProvisionRequest
	var frozenKey string
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controlled-service-token" {
			t.Error("missing frozen service identity")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/internal/v1/registration-bonus-grants":
			var input RegistrationWalletProvisionRequest
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if calls.Add(1) == 1 {
				frozen, frozenKey = input, r.Header.Get("Idempotency-Key")
				// A committed external result with its first ACK unavailable.
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if !reflect.DeepEqual(input, frozen) || r.Header.Get("Idempotency-Key") != frozenKey || frozenKey == "" {
				t.Error("retry changed immutable operation facts or key")
			}
			json.NewEncoder(w).Encode(map[string]any{"registration_operation_id": input.RegistrationOperationID, "user_id": input.UserID, "wallet_id": walletID, "claim_id": claimID, "policy_revision": 1, "outcome": "GRANTED", "granted_microcredits": 10000, "created_at": "2026-10-10T00:00:00Z"})
		case "/internal/v1/wallets/ensure":
			json.NewEncoder(w).Encode(map[string]any{"wallet_id": walletID, "owner_type": "PERSONAL", "owner_id": userID, "status": "ACTIVE", "version": 1})
		default:
			t.Error("unexpected authority route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	provisioner, err := NewHTTPWalletProvisioner(server.URL, "controlled-service-token")
	if err != nil {
		t.Fatal(err)
	}
	worker := NewWalletProvisioningWorker(repository, provisioner, time.Second)
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var status, userStatus string
	var nextAttempt time.Time
	if err := repository.pool.QueryRow(ctx, `SELECT j.status,u.status,j.next_attempt_at FROM wallet_provisioning_jobs j JOIN users u ON u.id=j.user_id WHERE j.user_id=$1`, userID).Scan(&status, &userStatus, &nextAttempt); err != nil || status != "RETRY" || userStatus != "PROVISIONING" {
		t.Fatalf("unknown ACK prematurely completed: %s %s %v", status, userStatus, err)
	}
	repository.now = func() time.Time { return nextAttempt.Add(time.Second) }
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT j.status,u.status FROM wallet_provisioning_jobs j JOIN users u ON u.id=j.user_id WHERE j.user_id=$1`, userID).Scan(&status, &userStatus); err != nil || status != "COMPLETED" || userStatus != "ACTIVE" || calls.Load() != 2 {
		t.Fatalf("durable retry did not complete: %s %s calls=%d %v", status, userStatus, calls.Load(), err)
	}
	if err := worker.RunOnce(ctx); err != nil || calls.Load() != 2 {
		t.Fatalf("completed job dispatched again: calls=%d %v", calls.Load(), err)
	}
}
