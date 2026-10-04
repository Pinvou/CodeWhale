package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

type NATSBroker struct {
	connection *nats.Conn
	stream     jetstream.JetStream
	streamName string
	retry      func(int) time.Duration
}

func NewNATSBroker(ctx context.Context, rawURL, clientName string) (*NATSBroker, error) {
	if strings.TrimSpace(rawURL) == "" || strings.TrimSpace(clientName) == "" {
		return nil, errorsNewConfiguration("NATS URL and client name are required")
	}
	connection, err := nats.Connect(rawURL, nats.Name(clientName), nats.Timeout(5*time.Second), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second))
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
	return &NATSBroker{connection: connection, stream: stream, streamName: "ACCOUNT_PLATFORM"}, nil
}

func (broker *NATSBroker) Publish(ctx context.Context, subject string, body []byte, messageID string) error {
	_, err := broker.stream.Publish(ctx, subject, body, jetstream.WithMsgID(messageID))
	return err
}

type ConsumerHandler func(context.Context, Event) error

func (broker *NATSBroker) Consume(ctx context.Context, durable, filterSubject string, handler ConsumerHandler) (func(), error) {
	if strings.TrimSpace(durable) == "" || strings.TrimSpace(filterSubject) == "" || handler == nil {
		return nil, errorsNewConfiguration("consumer durable, filter subject, and handler are required")
	}
	retry := broker.retry
	if retry == nil {
		retry = retryDelay
	}
	consumer, err := broker.stream.CreateOrUpdateConsumer(ctx, broker.streamName, jetstream.ConsumerConfig{
		Name:          durable,
		Durable:       durable,
		FilterSubject: filterSubject,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second,
		// Twelve business attempts, followed by retryable dead-letter delivery.
		// A broker outage during the last DLQ publish must not strand the event.
		MaxDeliver: -1,
		BackOff:    []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute},
	})
	if err != nil {
		return nil, fmt.Errorf("ensure JetStream consumer: %w", err)
	}
	consumerContext, err := consumer.Consume(func(message jetstream.Msg) {
		metadata, metadataErr := message.Metadata()
		if metadataErr != nil || metadata == nil {
			_ = message.NakWithDelay(time.Second)
			return
		}
		processErr := errors.New("consumer business retry limit exhausted")
		if metadata.NumDelivered <= 12 {
			var event Event
			event, processErr = Unmarshal(message.Data())
			if processErr == nil {
				processErr = handler(ctx, event)
			}
		}
		if processErr == nil {
			_ = message.DoubleAck(ctx)
			return
		}
		if metadata.NumDelivered >= 12 {
			subject := "account-platform.dead-letter." + message.Subject()
			messageID := message.Headers().Get(nats.MsgIdHdr) + ":consumer-dead-letter"
			if messageID == ":consumer-dead-letter" {
				messageID = durable + ":" + metadata.Stream + ":" + fmt.Sprint(metadata.Sequence.Stream)
			}
			if err := broker.Publish(ctx, subject, message.Data(), messageID); err == nil {
				_ = message.TermWithReason(truncateError(processErr))
				return
			}
		}
		_ = message.NakWithDelay(retry(int(metadataDelivery(metadata))))
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
