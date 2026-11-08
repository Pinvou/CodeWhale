package account

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	billingdomain "github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
)

type billingNonTargetDelivery struct {
	raw      []byte
	id       string
	ack, nak int
}

func (d *billingNonTargetDelivery) DeliveryBytes() []byte { return d.raw }
func (d *billingNonTargetDelivery) MessageID() string     { return d.id }
func (d *billingNonTargetDelivery) Metadata() events.AuthorizationDeliveryMetadata {
	return events.AuthorizationDeliveryMetadata{Stream: "ACCOUNT_PLATFORM", StreamSequence: 1, NumDelivered: 1}
}
func (d *billingNonTargetDelivery) DoubleAck(context.Context) error  { d.ack++; return nil }
func (d *billingNonTargetDelivery) NakWithDelay(time.Duration) error { d.nak++; return nil }
func (d *billingNonTargetDelivery) PublishDeadLetter(context.Context) (events.AuthorizationDLQPubAck, error) {
	panic("non-target Token authorization must not publish a Billing DLQ")
}

// The external Token proof/precheck and broker publish/ACK transports are
// controlled. Account's actual writer, immutable stored wire, relay and signed
// route HTTP, and all installed Billing consumer Handle entries are real.
func TestTokenAuthorizationWriterToBillingNonTargetPostgres(t *testing.T) {
	rawURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if rawURL == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	repository, connection, relay, _ := newEntitlementApprovalFixture(t, ctx, rawURL)
	requester := seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "routing-requester")
	approver := seedEntitlementApprovalActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "routing-approver")
	now := time.Now().UTC().Truncate(time.Second)
	owner, reviewer := entitlementApprovalPrincipal(requester), entitlementApprovalPrincipal(approver)
	owner.AuthenticatedAt, reviewer.AuthenticatedAt = now, now
	draft := tokenRecentAuthDraft("TOKEN_CREDENTIAL_REVOKE", now)
	repository.WithTokenGovernanceActivation(&tokenRecentAuthActivation{true}).
		WithApprovalSnapshotSource(&tokenRecentAuthSnapshot{draft: draft}).WithApprovalProofSource(&tokenRecentAuthProof{})
	created, err := repository.CreateApprovalDraft(ctx, owner, draft)
	if err != nil {
		t.Fatal("real Account create", err)
	}
	submitted, err := repository.SubmitApprovalRequest(ctx, owner, created.ID, created.Version)
	if err != nil {
		t.Fatal("real Account submit", err)
	}
	approved, err := repository.DecideApprovalRequest(ctx, reviewer, created.ID, submitted.Version, "APPROVED", "independent non-target routing review", draft.CriticalResourceVersions)
	if err != nil || approved.Status != "APPROVED" {
		t.Fatal("real Account approval", err)
	}
	if _, err = connection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	var eventID uuid.UUID
	var stored []byte
	var serialization string
	err = connection.QueryRow(ctx, `SELECT o.event_id,s.delivery_bytes,s.serialization_version FROM outbox_events o JOIN account_approval_authorization.approval_authorization_event_bytes s ON s.event_id=o.event_id WHERE o.aggregate_id=$1 AND o.event_type='account.approval-command.authorized.v1'`, created.ID).Scan(&eventID, &stored, &serialization)
	if err != nil || serialization != approvalencoding.ApprovalSerializationVersion {
		t.Fatal("actual proof-era wire missing", err)
	}
	if _, err = connection.Exec(ctx, `RESET ROLE; SET ROLE account_service`); err != nil {
		t.Fatal(err)
	}
	claims, err := relay.ClaimFrozen(ctx, "token-billing-routing-relay", 1, 30*time.Second)
	if err != nil || len(claims) != 1 || claims[0].EventID != eventID {
		t.Fatal("actual frozen claim", err)
	}
	disposition, err := relay.ProcessFrozen(ctx, "token-billing-routing-relay", claims[0], 12, time.Now().UTC(), func(_ context.Context, message events.FrozenMessage) events.FrozenPublishResult {
		if message.EventID != eventID || !bytes.Equal(message.Body, stored) {
			t.Fatal("relay changed stored authorization")
		}
		return events.FrozenPublishResult{Disposition: events.FrozenPublished}
	})
	if err != nil || disposition != events.FrozenPublished {
		t.Fatal("actual relay publication state", err)
	}
	_, executorToken := configureEntitlementProofIdentity(t)
	actualHandler := NewService(repository, nil).WithServiceToken("unused-static-token").Handler(nil)
	var routeReads atomic.Int64
	accountServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/route") {
			t.Error("non-target consumer called Account state/execution endpoint", r.URL.Path)
		} else {
			routeReads.Add(1)
		}
		actualHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(accountServer.Close)
	gate, err := billingdomain.NewHTTPApprovalExecutionGate(accountServer.URL, executorToken)
	if err != nil {
		t.Fatal(err)
	}
	route, err := gate.ResolveAuthorizationRoute(ctx, eventID)
	if err != nil || route.CommandType != draft.CommandType || route.CanonicalWorkload != approvalconsumer.TokenWorkload || route.CanonicalDurable != "token-credential-revoke-executor-v1" || route.SerializationVersion != serialization {
		t.Fatal("actual signed Account route", err)
	}
	before := tokenRecentAuthFacts(t, ctx, repository)
	baselineReads := routeReads.Load()
	for _, descriptor := range approvalconsumer.BillingCommands() {
		t.Run(descriptor.Durable, func(t *testing.T) {
			// No Billing repository is supplied: any accidental local execution is a
			// failing path, and cannot be masked by a successful Billing test stub.
			consumer, e := billingdomain.NewAuthorizationCommandConsumer(billingdomain.NewService(nil), gate, descriptor.Durable)
			if e != nil {
				t.Fatal(e)
			}
			delivery := &billingNonTargetDelivery{raw: stored, id: eventID.String()}
			consumer.Handle(ctx, delivery)
			if delivery.ack != 1 || delivery.nak != 0 {
				t.Fatalf("%s ack=%d nak=%d", descriptor.Durable, delivery.ack, delivery.nak)
			}
		})
	}
	if routeReads.Load()-baselineReads != int64(len(approvalconsumer.BillingCommands())) || tokenRecentAuthFacts(t, ctx, repository) != before {
		t.Fatal("non-target delivery changed Account facts or missed actual route reads")
	}
	if _, err = connection.Exec(ctx, `SET ROLE account_approval_authorization_owner`); err != nil {
		t.Fatal(err)
	}
	var states int
	if err = connection.QueryRow(ctx, `SELECT count(*) FROM account_approval_authorization.approval_delivery_states WHERE approval_request_id=$1`, created.ID).Scan(&states); err != nil || states != 0 {
		t.Fatal("non-target delivery created execution/delivery state", err)
	}
	var after []byte
	if err = connection.QueryRow(ctx, `SELECT delivery_bytes FROM account_approval_authorization.approval_authorization_event_bytes WHERE event_id=$1`, eventID).Scan(&after); err != nil || !bytes.Equal(after, stored) {
		t.Fatal("immutable Account wire changed", err)
	}
	t.Log("ACTUAL_ACCOUNT_WRITER_STORED_WIRE_SIGNED_ROUTE_AND_ALL_BILLING_NON_TARGET_HANDLES_PASSED")
}
