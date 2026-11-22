package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type holdExtensionHTTPRepository struct {
	Repository
	managementHoldRepository
	commands []HoldExtension
}

func (r *holdExtensionHTTPRepository) ExtendHold(_ context.Context, c HoldExtension) (Hold, error) {
	r.commands = append(r.commands, c)
	return Hold{ID: c.HoldID, UsageRequestID: c.UsageRequestID}, nil
}

func TestHoldExtensionHTTPContract(t *testing.T) {
	usage, hold, operation := uuid.New(), uuid.New(), uuid.New()
	valid := fmt.Sprintf(`{"usage_request_id":%q,"hold_id":%q,"operation_id":%q,"additional_microcredits":9007199254740993,"lease_seconds":900}`, usage, hold, operation)
	for name, payload := range map[string]string{
		"positive-missing-operation": strings.Replace(valid, fmt.Sprintf(`"operation_id":%q,`, operation), "", 1),
		"positive-nil-operation":     strings.Replace(valid, operation.String(), uuid.Nil.String(), 1),
		"malformed-operation":        strings.Replace(valid, operation.String(), "invalid", 1),
		"negative":                   strings.Replace(valid, "9007199254740993", "-1", 1),
		"lease-too-short":            strings.Replace(valid, `"lease_seconds":900`, `"lease_seconds":29`, 1),
		"lease-too-long":             strings.Replace(valid, `"lease_seconds":900`, `"lease_seconds":1801`, 1),
		"unknown":                    strings.TrimSuffix(valid, "}") + `,"extra":true}`,
		"trailing":                   valid + ` {}`,
		"oversize":                   valid + strings.Repeat(" ", 1<<20),
		"zero-with-operation":        strings.Replace(valid, "9007199254740993", "0", 1),
	} {
		t.Run(name, func(t *testing.T) {
			repo := &holdExtensionHTTPRepository{}
			out := httptest.NewRecorder()
			NewService(repo).extendHold(out, httptest.NewRequest(http.MethodPost, "/internal/v1/holds/extend", strings.NewReader(payload)))
			if out.Code != 400 || len(repo.commands) != 0 {
				t.Fatalf("invalid extension dispatched: status=%d calls=%d", out.Code, len(repo.commands))
			}
		})
	}
	t.Run("positive-exact-int-and-default-lease", func(t *testing.T) {
		repo := &holdExtensionHTTPRepository{}
		out := httptest.NewRecorder()
		NewService(repo).extendHold(out, httptest.NewRequest(http.MethodPost, "/internal/v1/holds/extend", strings.NewReader(strings.Replace(valid, `"lease_seconds":900`, `"lease_seconds":0`, 1))))
		if out.Code != 200 || len(repo.commands) != 1 {
			t.Fatalf("valid extension rejected: %d", out.Code)
		}
		got := repo.commands[0]
		if got != (HoldExtension{UsageRequestID: usage, HoldID: hold, OperationID: operation, LeaseSeconds: 900, AdditionalMicrocredits: 9007199254740993}) {
			t.Fatalf("operation facts changed: %+v", got)
		}
	})
	t.Run("zero-renewal-original-body", func(t *testing.T) {
		repo := &holdExtensionHTTPRepository{}
		out := httptest.NewRecorder()
		payload := fmt.Sprintf(`{"usage_request_id":%q,"hold_id":%q,"additional_microcredits":0,"lease_seconds":900}`, usage, hold)
		NewService(repo).extendHold(out, httptest.NewRequest(http.MethodPost, "/internal/v1/holds/extend", strings.NewReader(payload)))
		if out.Code != 200 || len(repo.commands) != 1 || repo.commands[0].OperationID != uuid.Nil || repo.commands[0].AdditionalMicrocredits != 0 {
			t.Fatalf("ordinary renewal changed: %d %+v", out.Code, repo.commands)
		}
	})
	t.Run("authentication-still-precedes-repository", func(t *testing.T) {
		t.Setenv("APP_ENV", "test")
		t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
		t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
		repo := &holdExtensionHTTPRepository{}
		out := httptest.NewRecorder()
		NewService(repo).Handler("controlled-extension-token").ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/internal/v1/holds/extend", strings.NewReader(valid)))
		if out.Code != http.StatusUnauthorized || len(repo.commands) != 0 {
			t.Fatalf("unauthenticated extension reached repository: %d", out.Code)
		}
	})
}

func TestHoldExtensionStableCommandFacts(t *testing.T) {
	original := HoldExtension{UsageRequestID: uuid.New(), HoldID: uuid.New(), OperationID: uuid.New(), AdditionalMicrocredits: 1}
	a, err := original.normalized()
	if err != nil {
		t.Fatal(err)
	}
	original.LeaseSeconds = 900
	b, err := original.normalized()
	if err != nil || a.fingerprint() != b.fingerprint() {
		t.Fatal("equivalent default lease changed command identity", err)
	}
	encoded, err := json.Marshal(a)
	if err != nil || !strings.Contains(string(encoded), a.OperationID.String()) {
		t.Fatal("operation identity lost", err)
	}
}

func TestHoldExtensionCommitOutcomeClassification(t *testing.T) {
	serialization := &pgconn.PgError{Code: "40001"}
	for _, tc := range []struct {
		name    string
		err     error
		aborted bool
	}{
		{"server-serialization-abort", serialization, true},
		{"wrapped-server-serialization-abort", fmt.Errorf("commit: %w", serialization), true},
		{"deadlock-is-not-the-reviewed-retry-contract", &pgconn.PgError{Code: "40P01"}, false},
		{"domain-conflict-is-not-commit-proof", ErrConflict, false},
		{"mixed-server-and-transport-error", errors.Join(serialization, errors.New("connection lost")), false},
		{"wrapped-mixed-error", fmt.Errorf("commit: %w", errors.Join(serialization, context.DeadlineExceeded)), false},
		{"closed-transaction-alone-proves-nothing", pgx.ErrTxClosed, false},
		{"unknown-network-outcome", errors.New("connection lost"), false},
		{"timeout-is-unknown", context.DeadlineExceeded, false},
		{"successful-commit", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := holdExtensionCommitAborted(tc.err); got != tc.aborted {
				t.Fatalf("commit outcome retry eligibility=%v want=%v error=%v", got, tc.aborted, tc.err)
			}
		})
	}
}
