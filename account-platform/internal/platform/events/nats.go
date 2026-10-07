package events

import (
	"context"

	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/nats-io/nats.go"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/codes"
)

type NATSBroker struct {
	connection  *nats.Conn
	stream      jetstream.JetStream
	provisioned bool
	publishOnly bool
	clientName  string
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
	return &NATSBroker{connection: connection, stream: stream, provisioned: provisioned, clientName: clientName}, nil
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
