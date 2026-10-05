// This fixture uses the production envelope codec; no broker or database is mocked as executed.
package main

import (
	"encoding/json"
	"os"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

func main() {
	id := uuid.MustParse("01962000-0000-7000-8000-000000000001")
	fixtureTime := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	fixtures := map[string]json.RawMessage{}
	for _, item := range []struct {
		name    string
		version int64
		payload map[string]any
	}{
		{"account.profile.changed.v1", 2, map[string]any{"user_id": id, "field": "MOBILE", "profile_version": 2}},
		{"account.wallet.provisioning.requested.v1", 1, map[string]any{"user_id": id, "registration_operation_id": id, "policy_version": "wallet-v1"}},
		{"account.wallet.provisioning.completed.v1", 2, map[string]any{"user_id": id}},
	} {
		payload, err := json.Marshal(item.payload)
		if err != nil {
			panic(err)
		}
		event := events.Event{ID: id, Type: item.name, AggregateID: id, AggregateVersion: item.version, OccurredAt: fixtureTime, TraceID: "fixture-trace", Payload: payload}
		body, err := event.Marshal()
		if err != nil {
			panic(err)
		}
		if _, err := events.Unmarshal(body); err != nil {
			panic(err)
		}
		fixtures[item.name] = body
	}
	if err := json.NewEncoder(os.Stdout).Encode(fixtures); err != nil {
		panic(err)
	}
}
