package billing

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func TestF08MembershipReasonRejectsInvalidPayloadBeforeDatabase(t *testing.T) {
	// All mandatory envelope and identity facts are valid. The nil pool makes
	// accidentally accepting these inputs fail instead of passing a fake DB call.
	consumer := NewMemberLifecycleConsumer(&PostgresRepository{})
	for _, test := range []struct{ name, status, extra string }{
		{"null", "REVOKED", `,"reason":null`},
		{"empty", "REVOKED", `,"reason":""`},
		{"unknown-reason", "REVOKED", `,"reason":"OTHER"`},
		{"number", "REVOKED", `,"reason":1`},
		{"boolean", "REVOKED", `,"reason":true`},
		{"object", "REVOKED", `,"reason":{}`},
		{"array", "REVOKED", `,"reason":[]`},
		{"active-closure", "ACTIVE", `,"reason":"ACCOUNT_CLOSURE"`},
		{"suspended-closure", "SUSPENDED", `,"reason":"ACCOUNT_CLOSURE"`},
		{"unknown-field", "REVOKED", `,"reason":"ACCOUNT_CLOSURE","unexpected":true`},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := `{"organization_id":"` + uuid.NewString() + `","user_id":"` + uuid.NewString() + `","membership_status":"` + test.status + `","membership_version":2` + test.extra + `}`
			event := events.Event{ID: uuid.New(), Type: "account.membership.changed.v1", AggregateID: uuid.New(), AggregateVersion: 2, TraceID: "f08-controlled-reason", Payload: json.RawMessage(payload)}
			if err := consumer.Handle(context.Background(), event); !errors.Is(err, events.ErrInvalidEvent) {
				t.Fatalf("invalid payload reached processing: %v", err)
			}
		})
	}
	t.Run("trailing-json", func(t *testing.T) {
		payload, err := json.Marshal(map[string]any{"organization_id": uuid.New(), "user_id": uuid.New(), "membership_status": "REVOKED", "membership_version": 2, "reason": "ACCOUNT_CLOSURE"})
		if err != nil {
			t.Fatal(err)
		}
		event := events.Event{ID: uuid.New(), Type: "account.membership.changed.v1", AggregateID: uuid.New(), AggregateVersion: 2, TraceID: "f08-controlled-reason", Payload: append(payload, []byte(` {}`)...)}
		if err := consumer.Handle(context.Background(), event); !errors.Is(err, events.ErrInvalidEvent) {
			t.Fatalf("trailing JSON accepted: %v", err)
		}
	})
}
