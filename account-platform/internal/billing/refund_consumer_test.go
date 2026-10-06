package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/google/uuid"
)

func TestRefundCommandConsumerRejectsInvalidEventsBeforeProviderExecution(t *testing.T) {
	consumer := NewRefundCommandConsumer(&PaymentCoordinator{})
	if err := consumer.Handle(context.Background(), events.Event{Type: "billing.refund.succeeded.v1"}); err == nil {
		t.Fatal("unsupported refund event was accepted")
	}
	refundID := uuid.Must(uuid.NewV7())
	if err := consumer.Handle(context.Background(), events.Event{Type: "billing.refund.requested.v1", AggregateID: refundID, Payload: []byte(`{"refund_id":"00000000-0000-0000-0000-000000000000"}`)}); err == nil {
		t.Fatal("mismatched refund event target was accepted")
	}
}

func TestPostgresRefundConsumerTransportFailureKeepsSafeRetryIntent(t *testing.T) {
	ctx := context.Background()
	p, wallet, user := paymentFixture(t)
	provider := &adminRefundProvider{recoverableProvider: &recoverableProvider{}, refunds: map[string]ProviderRefund{}}
	p.provider = provider
	order := creditedOrder(t, p, wallet, user, uuid.NewString())
	source, packageID := refundTargetFixture(t, p, order.ID)
	refund, _, err := p.CreateRefund(ctx, order.ID, user, source, packageID, uuid.NewString(), "controlled refundable recharge")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	address := server.URL
	server.Close()
	const privateToken = "controlled-private-refund-token"
	transport, err := paymentsimulator.New(address, privateToken)
	if err != nil {
		t.Fatal("controlled transport constructor failed")
	}
	p.provider = transport
	payload, err := json.Marshal(map[string]any{"refund_id": refund.ID})
	if err != nil {
		t.Fatal(err)
	}
	event := events.Event{Type: "billing.refund.requested.v1", AggregateID: refund.ID, Payload: payload}
	consumer := NewRefundCommandConsumer(p)
	const snapshotSQL = `SELECT jsonb_build_object(
		'refund',(SELECT to_jsonb(r) FROM refunds r WHERE id=$1),
		'wallet',(SELECT to_jsonb(w) FROM wallets w WHERE id=$2),
		'package',(SELECT to_jsonb(p) FROM entitlement_packages p WHERE id=$3),
		'components',(SELECT jsonb_agg(to_jsonb(c) ORDER BY id) FROM entitlement_components c WHERE package_id=$3),
		'journal',(SELECT jsonb_agg(to_jsonb(j) ORDER BY id) FROM journal_entries j))::text`
	var before, after string
	if err = p.repository.pool.QueryRow(ctx, snapshotSQL, refund.ID, wallet.ID, packageID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	// Call the actual dispatcher at the broker's twelve-attempt boundary. The
	// separate NATS regression verifies delivery/advisory behavior itself.
	for attempt := 0; attempt < 12; attempt++ {
		err = consumer.Handle(ctx, event)
		if !errors.Is(err, ErrPaymentProvider) || err.Error() != ErrPaymentProvider.Error() || strings.Contains(err.Error(), address) || strings.Contains(err.Error(), privateToken) {
			t.Fatal("refund retry exposed provider details or lost its retryable error classification")
		}
	}
	if err = p.repository.pool.QueryRow(ctx, snapshotSQL, refund.ID, wallet.ID, packageID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("failed provider retries changed pending financial intent or funds")
	}
	p.provider = provider
	for attempt := 0; attempt < 2; attempt++ {
		if err = consumer.Handle(ctx, event); err != nil {
			t.Fatal("same pending intent did not recover and replay")
		}
	}
	var status string
	var journals int
	if err = p.repository.pool.QueryRow(ctx, `SELECT status FROM refunds WHERE id=$1`, refund.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = p.repository.pool.QueryRow(ctx, `SELECT count(*) FROM journal_entries WHERE entry_type='REFUND'`).Scan(&journals); err != nil {
		t.Fatal(err)
	}
	if status != "SUCCEEDED" || journals != 1 {
		t.Fatal("successful retry failed to converge to one refund fact")
	}
	assertBalanced(t, p.repository)
}
