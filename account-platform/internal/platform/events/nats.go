package events

import (
	"context"
	"fmt"

	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/nats-io/nats.go"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/codes"
)

type NATSBroker struct {
	connection        *nats.Conn
	stream            jetstream.JetStream
	provisioned       bool
	publishOnly       bool
	clientName        string
	approvalResources *ApprovalConsumerResourceBinding
}

func NewNATSBroker(ctx context.Context, rawURL, clientName string, configuration ...NATSConfig) (*NATSBroker, error) {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(clientName) == "" {
		return nil, errorsNewConfiguration("NATS URL and client name are required")
	}
	if len(configuration) > 1 {
		return nil, ErrNATSResources
	}
	cfg := NATSConfig{}
	if len(configuration) == 1 {
		cfg = configuration[0]
	}
	provisioned, err := cfg.provisioned()
	if err != nil {
		return nil, err
	}
	connection, stream, err := connectNATS(rawURL, clientName, cfg)
	if err != nil {
		return nil, err
	}
	if provisioned {
		err = checkedStream(ctx, stream)
	} else {
		_, err = stream.CreateOrUpdateStream(ctx, platformStreamConfig())
	}
	if err != nil {
		connection.Close()
		return nil, ErrNATSResources
	}
	return &NATSBroker{connection: connection, stream: stream, provisioned: provisioned, clientName: clientName, approvalResources: cfg.ApprovalConsumerResources}, nil
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

func (broker *NATSBroker) Close() {
	if broker != nil && broker.connection != nil {
		_ = broker.connection.Drain()
		broker.connection.Close()
	}
}

type configurationError string

func (err configurationError) Error() string { return string(err) }

func errorsNewConfiguration(message string) error { return configurationError(message) }

func NewNATSPublishOnlyBroker(ctx context.Context, rawURL, clientName string, cfg NATSConfig) (*NATSBroker, error) {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(clientName) == "" || !cfg.RequireExplicitAuthentication {
		return nil, ErrNATSResources
	}
	if provisioned, err := cfg.provisioned(); err != nil || !provisioned {
		return nil, ErrNATSResources
	}
	connection, stream, err := connectNATS(rawURL, clientName, cfg)
	if err != nil {
		return nil, err
	}
	return &NATSBroker{connection: connection, stream: stream, provisioned: true, publishOnly: true, clientName: clientName}, nil
}

func (broker *NATSBroker) Consume(ctx context.Context, durable, filterSubject string, handler ConsumerHandler) (func(), error) {
	return broker.consume(ctx, durable, filterSubject, handler, false)
}

func (broker *NATSBroker) ConsumeUntilResolved(ctx context.Context, durable, filterSubject string, handler ConsumerHandler) (func(), error) {
	return broker.consume(ctx, durable, filterSubject, handler, true)
}

func (broker *NATSBroker) consume(ctx context.Context, durable, filterSubject string, handler ConsumerHandler, retainUnresolved bool) (func(), error) {
	if broker == nil || broker.publishOnly || strings.TrimSpace(durable) == "" || strings.TrimSpace(filterSubject) == "" || handler == nil {
		return nil, errorsNewConfiguration("consumer durable, filter subject, and handler are required")
	}
	expected := eventConsumerConfig(durable, filterSubject, retainUnresolved)
	var consumer jetstream.Consumer
	var err error
	if broker.provisioned {
		if !consumerOwnedBy(broker.clientName, expected) {
			return nil, ErrNATSResources
		}
		consumer, err = checkedConsumer(ctx, broker.stream, expected)
	} else {
		consumer, err = broker.stream.CreateOrUpdateConsumer(ctx, platformStream, expected)
	}
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

type ConsumerHandler func(context.Context, Event) error
