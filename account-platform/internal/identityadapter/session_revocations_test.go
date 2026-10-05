package identityadapter

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

type revocationDirectory struct {
	directoryFake
	calls   atomic.Int64
	unknown bool
}

func (d *revocationDirectory) RevokeSessions(context.Context, string) error {
	d.calls.Add(1)
	if d.unknown {
		return errors.New("controlled acknowledgement lost")
	}
	return nil
}

func revocationService(t *testing.T, directory *revocationDirectory) *Service {
	t.Helper()
	vault, err := NewPasswordVault([]byte(strings.Repeat("k", 32)), directory)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(vault, "controlled-token")
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func callRevocation(service *Service, subject string, operationID uuid.UUID, first bool) int {
	body, _ := json.Marshal(map[string]any{"operation_id": operationID, "first_attempt": first})
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/users/"+subject+"/revoke-sessions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer controlled-token")
	res := httptest.NewRecorder()
	service.Handler().ServeHTTP(res, req)
	return res.Code
}

func TestConfirmedProfileRevocationReplayDoesNotRevokeLaterLogin(t *testing.T) {
	directory := &revocationDirectory{}
	service := revocationService(t, directory)
	id := uuid.New()
	if got := callRevocation(service, "subject", id, true); got != 204 {
		t.Fatalf("first status %d", got)
	}
	// A new remote login may now exist. Replaying either the original request or
	// the Account retry must only acknowledge the confirmed immutable receipt.
	for _, first := range []bool{false, true, false} {
		if got := callRevocation(service, "subject", id, first); got != 204 {
			t.Fatalf("replay status %d", got)
		}
	}
	if directory.calls.Load() != 1 {
		t.Fatal("confirmed replay revoked a later remote login")
	}
}

func TestProfileRevocationMissingUnknownAndConflictingReceiptFailClosed(t *testing.T) {
	for _, scenario := range []string{"missing", "restart", "unknown", "subject-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			directory := &revocationDirectory{unknown: scenario == "unknown"}
			service := revocationService(t, directory)
			id := uuid.New()
			if scenario != "missing" {
				want := 204
				if scenario == "unknown" {
					want = 503
				}
				if got := callRevocation(service, "subject", id, true); got != want {
					t.Fatalf("first status %d", got)
				}
			}
			if scenario == "restart" {
				service = revocationService(t, directory)
			}
			subject := "subject"
			want := 503
			if scenario == "subject-conflict" {
				subject = "different-subject"
				want = 409
			}
			before := directory.calls.Load()
			if got := callRevocation(service, subject, id, false); got != want {
				t.Fatalf("unproven retry status %d", got)
			}
			if directory.calls.Load() != before {
				t.Fatal("retry guessed a new remote revocation")
			}
		})
	}
}

func TestProfileRevocationConcurrentReplaysHaveOneRemoteCall(t *testing.T) {
	directory := &revocationDirectory{}
	service := revocationService(t, directory)
	id := uuid.New()
	if got := callRevocation(service, "subject", id, true); got != 204 {
		t.Fatalf("first status %d", got)
	}
	results := make(chan int, 8)
	for range 8 {
		go func() { results <- callRevocation(service, "subject", id, false) }()
	}
	for range 8 {
		if got := <-results; got != 204 {
			t.Fatalf("concurrent replay status %d", got)
		}
	}
	if directory.calls.Load() != 1 {
		t.Fatal("concurrent retry repeated remote side effect")
	}
}

func TestProfileRevocationRequiresBothOperationAndFirstAttemptFacts(t *testing.T) {
	for name, payload := range map[string]string{
		"empty": "{}", "missing-first": `{"operation_id":"a371c757-7266-4663-89ec-a6a9d47268d3"}`,
		"missing-operation": `{"first_attempt":true}`, "null-first": `{"operation_id":"a371c757-7266-4663-89ec-a6a9d47268d3","first_attempt":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			directory := &revocationDirectory{}
			service := revocationService(t, directory)
			req := httptest.NewRequest(http.MethodPost, "/internal/v1/users/subject/revoke-sessions", strings.NewReader(payload))
			req.Header.Set("Authorization", "Bearer controlled-token")
			res := httptest.NewRecorder()
			service.Handler().ServeHTTP(res, req)
			if res.Code != 400 || directory.calls.Load() != 0 {
				t.Fatal("missing facts reached remote directory")
			}
		})
	}
}
