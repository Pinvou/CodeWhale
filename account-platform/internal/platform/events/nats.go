package events

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/codes"
)

type NATSBroker struct {
	connection *nats.Conn
	stream     jetstream.JetStream
}

func NewNATSBroker(ctx context.Context, rawURL, clientName string) (*NATSBroker, error) {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(clientName) == "" {
		return nil, errorsNewConfiguration("NATS URL and client name are required")
	}
	options := []nats.Option{nats.Name(clientName), nats.Timeout(5 * time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second)}
	if credentialsFile := strings.TrimSpace(os.Getenv("NATS_CREDENTIALS_FILE")); credentialsFile != "" {
		options = append(options, nats.UserCredentials(credentialsFile))
	}
	connection, err := nats.Connect(rawURL, options...)
	if err != nil {
		return nil, fmt.Errorf("connect NATS: %w", err)
	}
	stream, err := jetstream.New(connection)
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("create JetStream context: %w", err)
	}
	_, err = stream.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        "ACCOUNT_PLATFORM",
		Description: "Pinvou account platform domain events",
		Subjects:    []string{"account.>", "billing.>", "token.>", "account-platform.dead-letter.>"},
		Storage:     jetstream.FileStorage,
		Retention:   jetstream.LimitsPolicy,
		MaxAge:      30 * 24 * time.Hour,
		Duplicates:  24 * time.Hour,
	})
	if err != nil {
		connection.Close()
		return nil, fmt.Errorf("ensure JetStream stream: %w", err)
	}
	return &NATSBroker{connection: connection, stream: stream}, nil
}

func (broker *NATSBroker) Publish(ctx context.Context, subject string, body []byte, messageID string) error {
	ctx, span := observability.StartSpan(ctx, "messaging.publish")
	defer span.End()
	_, err := broker.stream.Publish(ctx, subject, body, jetstream.WithMsgID(messageID))
	if err != nil {
		span.SetStatus(codes.Error, "EVENT_BROKER_PUBLISH_FAILED")
	}
	return err
}

func (broker *NATSBroker) Ping(ctx context.Context) error {
	if broker == nil || broker.connection == nil || !broker.connection.IsConnected() {
		return errorsNewConfiguration("NATS is not connected")
	}
	return broker.connection.FlushWithContext(ctx)
}

type ConsumerHandler func(context.Context, Event) error

func (broker *NATSBroker) Consume(ctx context.Context, durable, filterSubject string, handler ConsumerHandler) (func(), error) {
	return broker.consume(ctx, durable, filterSubject, handler, false)
}

// ConsumeUntilResolved retains an unresolved authorization delivery instead
// of terminating it after a retry budget. The handler must emit a stable
// operator-visible alert and may ACK only a confirmed terminal result or a
// validated non-target event. Existing consumers keep their original DLQ policy.
func (broker *NATSBroker) ConsumeUntilResolved(ctx context.Context, durable, filterSubject string, handler ConsumerHandler) (func(), error) {
	return broker.consume(ctx, durable, filterSubject, handler, true)
}

func (broker *NATSBroker) consume(ctx context.Context, durable, filterSubject string, handler ConsumerHandler, retainUnresolved bool) (func(), error) {
	if strings.TrimSpace(durable) == "" || strings.TrimSpace(filterSubject) == "" || handler == nil {
		return nil, errorsNewConfiguration("consumer durable, filter subject, and handler are required")
	}
	maxDeliver := 12
	if retainUnresolved {
		maxDeliver = -1
	}
	consumer, err := broker.stream.CreateOrUpdateConsumer(ctx, "ACCOUNT_PLATFORM", jetstream.ConsumerConfig{
		Name:          durable,
		Durable:       durable,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		MaxDeliver:    maxDeliver,
		BackOff:       []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute},
	})
	if err != nil {
		return nil, fmt.Errorf("ensure JetStream consumer: %w", err)
	}
	consumerContext, err := consumer.Consume(func(message jetstream.Msg) {
		event, processErr := Unmarshal(message.Data())
		if processErr == nil {
			eventContext := observability.ContextForTrace(ctx, event.ID.String(), event.TraceID)
			eventContext, span := observability.StartSpan(eventContext, "messaging.consume")
			processErr = handler(eventContext, event)
			if processErr != nil {
				span.SetStatus(codes.Error, "EVENT_CONSUMER_FAILED")
			}
			span.End()
		}
		if processErr == nil {
			_ = message.DoubleAck(ctx)
			return
		}
		metadata, _ := message.Metadata()
		if !retainUnresolved && metadata != nil && metadata.NumDelivered >= 12 {
			subject := "account-platform.dead-letter." + message.Subject()
			messageID := message.Headers().Get(nats.MsgIdHdr) + ":consumer-dead-letter"
			if messageID == ":consumer-dead-letter" {
				messageID = durable + ":" + metadata.Stream + ":" + fmt.Sprint(metadata.Sequence.Stream)
			}
			if err := broker.Publish(context.Background(), subject, message.Data(), messageID); err == nil {
				_ = message.TermWithReason(safeDeliveryError(processErr))
				return
			}
		}
		_ = message.NakWithDelay(retryDelay(int(metadataDelivery(metadata))))
	}, jetstream.PullMaxMessages(64))
	if err != nil {
		return nil, fmt.Errorf("start JetStream consumer: %w", err)
	}
	return consumerContext.Stop, nil
}

func metadataDelivery(metadata *jetstream.MsgMetadata) uint64 {
	if metadata == nil || metadata.NumDelivered == 0 {
		return 1
	}
	return metadata.NumDelivered
}

func (broker *NATSBroker) Close() {
	if broker != nil && broker.connection != nil {
		_ = broker.connection.Drain()
		broker.connection.Close()
	}
}

type configurationError string

func (err configurationError) Error() string      { return string(err) }
func errorsNewConfiguration(message string) error { return configurationError(message) }
