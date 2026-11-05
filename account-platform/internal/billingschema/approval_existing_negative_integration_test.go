package billingschema

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
)

func TestExistingBrokerOperatorCannotUpdateRefund(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned TLS NATS fixture required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	b := existingActualBroker(t, ctx, binary)
	seed, err := os.ReadFile(b.cfg.SeedFile)
	if err != nil {
		t.Fatal("owned operator seed unavailable")
	}
	defer clear(seed)
	pair, err := nkeys.FromSeed(seed)
	if err != nil {
		t.Fatal("owned operator seed invalid")
	}
	defer pair.Wipe()
	public, err := pair.PublicKey()
	if err != nil {
		t.Fatal("owned operator key unavailable")
	}
	ca, err := os.ReadFile(b.cfg.CAFile)
	if err != nil {
		t.Fatal("owned CA unavailable")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("owned CA invalid")
	}
	connection, err := nats.Connect(b.url, nats.Secure(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}), nats.Nkey(public, pair.Sign), nats.CustomInboxPrefix("_INBOX.event-resource-operator"), nats.Timeout(time.Second), nats.ErrorHandler(func(*nats.Conn, *nats.Subscription, error) {}))
	if err != nil {
		t.Fatal("owned operator TLS connection failed")
	}
	defer connection.Close()
	js, err := jetstream.New(connection)
	if err != nil {
		t.Fatal("owned operator API failed")
	}
	stream, err := js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal("retained stream INFO denied")
	}
	consumer, err := stream.Consumer(ctx, "billing-refund-executor-v1")
	if err != nil {
		t.Fatal("retained refund INFO denied")
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatal("retained refund INFO failed")
	}
	want := info.Config
	want.MaxAckPending = 999
	observer, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
	if err != nil {
		t.Fatal("owned observer stream unavailable")
	}
	before := existingAllConsumerInfo(t, ctx, observer)
	bounded, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	if _, err := stream.UpdateConsumer(bounded, want); err == nil {
		t.Fatal("operator acquired refund UPDATE through CREATE subject")
	}
	if !bytes.Equal(before, existingAllConsumerInfo(t, ctx, observer)) {
		t.Fatal("denied operator changed a retained consumer")
	}
	// Same request succeeds for the fixture-only unrestricted observer, proving
	// the denial came from the operator ACL rather than an invalid update shape.
	if _, err := observer.UpdateConsumer(ctx, want); err != nil {
		t.Fatal("negative control could not perform valid owned update")
	}
}

func TestExistingBrokerDriftRejectsBeforeSiblingWrites(t *testing.T) {
	binary := os.Getenv("PINVOU_TEST_NATS_SERVER_BINARY")
	if binary == "" {
		t.Skip("explicit owned TLS NATS fixture required")
	}
	for _, scenario := range []string{"unknown-resource", "retained-authorization", "legacy-ack-drift", "legacy-backoff-drift", "later-token-drift", "replacement-instance", "message-removed-after-plan", "message-added-after-plan", "wrong-plan-digest", "wrong-control", "wrong-operator", "billing-start-drift", "member-config-drift"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			b := existingActualBroker(t, ctx, binary)
			stream, err := b.js.Stream(ctx, "ACCOUNT_PLATFORM")
			if err != nil {
				t.Fatal("owned stream missing")
			}
			facts := events.FreshApprovalControl{Version: 3, ApprovalEpoch: 2, RelayEpoch: 2}
			prepare := func(token bool) {
				t.Helper()
				raw, e := events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), token)
				if e != nil {
					t.Fatal("owned plan setup failed")
				}
				b.files.PlanFile = existingPublicWrite(t, b.resources, uuid.NewString()+".json", raw)
				b.files.PlanSHA256 = existingPublicHash(raw)
			}
			preparationFailure := scenario == "unknown-resource" || scenario == "retained-authorization" || scenario == "legacy-ack-drift" || scenario == "legacy-backoff-drift"
			if !preparationFailure {
				prepare(true)
			}
			billing := scenario == "billing-start-drift" || scenario == "member-config-drift"
			if billing {
				if _, err := events.UpgradeLegacyTokenConsumers(ctx, b.url, b.cfg, b.files, facts, true, false); err != nil {
					t.Fatal("owned Token transition setup failed")
				}
				prepare(false)
			}
			mutateLastToken := func(change func(*jetstream.ConsumerConfig)) {
				t.Helper()
				c, e := stream.Consumer(ctx, "token-credential-revoke-executor-v1")
				if e != nil {
					t.Fatal("owned consumer missing")
				}
				i, e := c.Info(ctx)
				if e != nil {
					t.Fatal("owned INFO failed")
				}
				configuration := i.Config
				change(&configuration)
				if _, e := stream.UpdateConsumer(ctx, configuration); e != nil {
					t.Fatal("owned drift setup failed")
				}
			}
			switch scenario {
			case "unknown-resource":
				if _, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: "unreviewed", Durable: "unreviewed", AckPolicy: jetstream.AckExplicitPolicy, FilterSubject: "account.fixture.ordinary.v1"}); err != nil {
					t.Fatal("owned extra resource setup failed")
				}
			case "retained-authorization":
				if _, err := b.js.Publish(ctx, "account.approval-command.authorized.v1", []byte(`{"fixture":true}`)); err != nil {
					t.Fatal("owned retained history setup failed")
				}
			case "legacy-ack-drift", "later-token-drift":
				mutateLastToken(func(c *jetstream.ConsumerConfig) { c.MaxAckPending = 999 })
			case "legacy-backoff-drift":
				mutateLastToken(func(c *jetstream.ConsumerConfig) {
					c.BackOff = append([]time.Duration(nil), c.BackOff...)
					c.BackOff[1] = 3 * time.Second
				})
			case "replacement-instance":
				c, e := stream.Consumer(ctx, "token-credential-revoke-executor-v1")
				if e != nil {
					t.Fatal("owned consumer missing")
				}
				i, e := c.Info(ctx)
				if e != nil {
					t.Fatal("owned INFO failed")
				}
				if stream.DeleteConsumer(ctx, i.Name) != nil {
					t.Fatal("owned replacement setup failed")
				}
				if _, e := stream.CreateConsumer(ctx, i.Config); e != nil {
					t.Fatal("owned replacement setup failed")
				}
			case "wrong-plan-digest":
				b.files.PlanSHA256 = strings.Repeat("0", 64)
			case "message-removed-after-plan":
				if err := stream.DeleteMsg(ctx, 1); err != nil {
					t.Fatal("owned message removal setup failed")
				}
			case "message-added-after-plan":
				if _, err := b.js.Publish(ctx, "account.fixture.ordinary.v1", []byte(`{"later":true}`)); err != nil {
					t.Fatal("owned message addition setup failed")
				}
			case "wrong-control":
				facts.Version++
			case "wrong-operator":
				pair := freshNKey(t)
				defer pair.Wipe()
				seed, _ := pair.Seed()
				b.cfg.SeedFile = existingPublicWrite(t, filepath.Dir(b.cfg.SeedFile), "unissued.seed", seed)
			case "billing-start-drift":
				_, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: "billing-wallet-adjustment-executor-p1-v1", Durable: "billing-wallet-adjustment-executor-p1-v1", FilterSubject: "account.approval-command.authorized.v1", AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: 64, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: 3})
				if err != nil {
					t.Fatal("owned Billing drift setup failed")
				}
			case "member-config-drift":
				_, err := stream.CreateConsumer(ctx, jetstream.ConsumerConfig{Name: "billing-member-lifecycle-v1", Durable: "billing-member-lifecycle-v1", FilterSubject: "account.membership.changed.v1", AckPolicy: jetstream.AckExplicitPolicy, AckWait: 30 * time.Second, MaxDeliver: -1, MaxAckPending: 999, BackOff: []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}})
				if err != nil {
					t.Fatal("owned member drift setup failed")
				}
			}
			before := existingAllConsumerInfo(t, ctx, stream)
			stateBefore, err := stream.Info(ctx)
			if err != nil {
				t.Fatal("owned stream INFO failed")
			}
			switch {
			case preparationFailure:
				_, err = events.PrepareExistingBrokerPlan(ctx, b.url, b.cfg, b.files.ApprovalConsumerResourceFiles, facts, uuid.NewString(), true)
			case billing:
				_, err = events.InitializeExistingBrokerConsumers(ctx, b.url, b.cfg, b.files, facts, true, true)
			default:
				_, err = events.UpgradeLegacyTokenConsumers(ctx, b.url, b.cfg, b.files, facts, true, true)
			}
			if err == nil {
				t.Fatal("unsupported precondition reached effects")
			}
			if !bytes.Equal(before, existingAllConsumerInfo(t, ctx, stream)) {
				t.Fatal("rejection changed a resource or earlier sibling")
			}
			stateAfter, err := stream.Info(ctx)
			if err != nil || stateBefore.State.Msgs != stateAfter.State.Msgs || stateBefore.State.Bytes != stateAfter.State.Bytes || stateBefore.State.FirstSeq != stateAfter.State.FirstSeq || stateBefore.State.LastSeq != stateAfter.State.LastSeq {
				t.Fatal("rejection changed existing messages")
			}
		})
	}
}
