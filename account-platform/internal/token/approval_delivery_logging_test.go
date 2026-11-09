package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
)

type tokenLoggingDelivery struct {
	*tokenAuthorizationDeliveryStub
	delay time.Duration
}

func (delivery *tokenLoggingDelivery) NakWithDelay(delay time.Duration) error {
	delivery.delay = delay
	return delivery.tokenAuthorizationDeliveryStub.NakWithDelay(delay)
}

func TestTokenAuthorizationFailureLogsKeepSafeIdentityAndRetry(t *testing.T) {
	const marker = "private-authorization-error-canary"
	previous, writer, flags, prefix := slog.Default(), log.Writer(), log.Flags(), log.Prefix()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(writer)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	for _, nakFails := range []bool{false, true} {
		name := "nak-success"
		if nakFails {
			name = "nak-failure"
		}
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			slog.SetDefault(observability.NewLogger(&output, observability.Config{Service: "token-command-executor", Environment: "test"}))
			fixture, server := newTokenAuthorizationHTTPFixture(t, "approval")
			defer server.Close()
			consumer, err := NewTokenAuthorizationCommandConsumer(fixture.event.CommandType, fixture.binding.Durable, server.URL, server.URL, "token-workload", &approvalActivationStub{state: "ACTIVE", epoch: 1})
			if err != nil {
				t.Fatal(err)
			}
			broken := &tokenLoggingDelivery{tokenAuthorizationDeliveryStub: fixture.delivery(1)}
			broken.id = marker // Malformed ingress must not disclose message identity or bytes.
			broken.raw = []byte(marker)
			if nakFails {
				broken.nakErr = errors.New(marker)
			}
			consumer.Handle(context.Background(), broken)
			if broken.ack != 0 || broken.nak != 1 || broken.publish != 0 || broken.delay != 2*time.Second || fixture.routeReads != 0 || fixture.domainCalls != 0 || fixture.begins != 0 {
				t.Fatalf("failure altered retry or reached domain: delivery=%+v delay=%s", broken.tokenAuthorizationDeliveryStub, broken.delay)
			}
			expected := []struct{ event, code string }{{"token_approval_authorization_delivery_pending", "DELIVERY_PENDING"}}
			if nakFails {
				expected = append(expected, struct{ event, code string }{"token_approval_authorization_nak_failed", "NAK_FAILED"})
			}
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != len(expected) || strings.Contains(output.String(), marker) {
				t.Fatalf("unsafe or missing failure logs: %s", output.String())
			}
			for index, want := range expected {
				var entry map[string]any
				if err := json.Unmarshal([]byte(lines[index]), &entry); err != nil {
					t.Fatal(err)
				}
				if entry["event"] != want.event || entry["msg"] != want.event || entry["error_code"] != want.code || entry["result"] != "failure" || entry["level"] != "ERROR" {
					t.Fatalf("incorrect failure record: %#v", entry)
				}
			}
			// The same real consumer remains usable after the malformed delivery.
			output.Reset()
			next := fixture.delivery(2)
			consumer.Handle(context.Background(), next)
			if next.ack != 1 || next.nak != 0 || fixture.domainCalls != 1 || fixture.state.State != "TERMINAL_CONFIRMED" || output.Len() != 0 {
				t.Fatalf("next delivery did not recover cleanly: ack=%d nak=%d calls=%d state=%s logs=%s", next.ack, next.nak, fixture.domainCalls, fixture.state.State, output.String())
			}
		})
	}
}
