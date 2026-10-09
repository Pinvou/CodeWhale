package events

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const authorizationSubject = "account.approval-command.authorized.v1"
const authorizationOperationTimeout = 5 * time.Second
const authorizationMaximumNAKDelay = 512 * time.Second

var ErrAuthorizationDelivery = errors.New("AUTHORIZATION_DELIVERY_INVALID")

// AuthorizationDeliveryMetadata identifies this physical delivery, not the
// logical event's attempt record. A later stream sequence can carry the same ID.
type AuthorizationDeliveryMetadata struct {
	Stream         string
	StreamSequence uint64
	NumDelivered   uint64
}

type AuthorizationDLQPubAck struct {
	Stream         string
	StreamSequence uint64
	Duplicate      bool
}

// AuthorizationRawDelivery never exposes a caller-selectable publish subject,
// message ID or body. Business code must validate the wire against Account's
// authoritative binding before invoking any terminal operation.
type AuthorizationRawDelivery interface {
	DeliveryBytes() []byte
	MessageID() string
	Metadata() AuthorizationDeliveryMetadata
	DoubleAck(context.Context) error
	NakWithDelay(time.Duration) error
	PublishDeadLetter(context.Context) (AuthorizationDLQPubAck, error)
}

type AuthorizationRawHandler func(context.Context, AuthorizationRawDelivery)

type authorizationMessageTransport interface {
	DoubleAck(context.Context) error
	NakWithDelay(time.Duration) error
}

type authorizationRawDelivery struct {
	message   authorizationMessageTransport
	body      []byte
	id        string
	durable   string
	meta      AuthorizationDeliveryMetadata
	publish   func(context.Context, []byte, string) (*jetstream.PubAck, error)
	lifetime  context.Context
	close     context.CancelFunc
	active    atomic.Bool
	operation sync.Mutex
}

func (d *authorizationRawDelivery) deactivate() {
	d.active.Store(false)
	d.close()
}

// A delivery capability belongs to one live callback. Concurrent operations
// are rejected, not queued until after the callback has returned.
func (d *authorizationRawDelivery) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if ctx == nil || !d.active.Load() || !d.operation.TryLock() {
		return nil, nil, ErrAuthorizationDelivery
	}
	if !d.active.Load() || d.lifetime.Err() != nil {
		d.operation.Unlock()
		return nil, nil, ErrAuthorizationDelivery
	}
	bounded, cancel := context.WithTimeout(ctx, authorizationOperationTimeout)
	stop := context.AfterFunc(d.lifetime, cancel)
	finish := func() { stop(); cancel(); d.operation.Unlock() }
	if err := bounded.Err(); err != nil {
		finish()
		return nil, nil, err
	}
	return bounded, finish, nil
}

func (d *authorizationRawDelivery) DeliveryBytes() []byte { return bytes.Clone(d.body) }
func (d *authorizationRawDelivery) MessageID() string     { return d.id }
func (d *authorizationRawDelivery) Metadata() AuthorizationDeliveryMetadata {
	return d.meta
}

func (d *authorizationRawDelivery) DoubleAck(ctx context.Context) error {
	bounded, finish, err := d.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer finish()
	// nats.go's synchronous ACK only returns nil after its request receives
	// the server reply. Never substitute Ack/Term or ignore this result.
	if err := d.message.DoubleAck(bounded); err != nil {
		return err
	}
	d.deactivate()
	return nil
}

func (d *authorizationRawDelivery) NakWithDelay(delay time.Duration) error {
	if delay <= 0 || delay > authorizationMaximumNAKDelay {
		return ErrAuthorizationDelivery
	}
	_, finish, err := d.beginOperation(d.lifetime)
	if err != nil {
		return err
	}
	defer finish()
	if err := d.message.NakWithDelay(delay); err != nil {
		return err
	}
	d.deactivate()
	return nil
}

func (d *authorizationRawDelivery) PublishDeadLetter(ctx context.Context) (AuthorizationDLQPubAck, error) {
	if ctx == nil || d.publish == nil {
		return AuthorizationDLQPubAck{}, ErrAuthorizationDelivery
	}
	bounded, finish, err := d.beginOperation(ctx)
	if err != nil {
		return AuthorizationDLQPubAck{}, err
	}
	defer finish()
	ack, err := d.publish(bounded, bytes.Clone(d.body), d.id+":"+d.durable+":dead-letter")
	if err != nil {
		return AuthorizationDLQPubAck{}, err
	}
	if ack == nil || ack.Stream != platformStream || ack.Sequence == 0 {
		return AuthorizationDLQPubAck{}, ErrAuthorizationDelivery
	}
	return AuthorizationDLQPubAck{Stream: ack.Stream, StreamSequence: ack.Sequence, Duplicate: ack.Duplicate}, nil
}

func (broker *NATSBroker) newAuthorizationRawDelivery(ctx context.Context, message jetstream.Msg, durable string, billingDLQAllowed bool) (*authorizationRawDelivery, error) {
	if message == nil || message.Subject() != authorizationSubject {
		return nil, ErrAuthorizationDelivery
	}
	ids := message.Headers().Values(nats.MsgIdHdr)
	if len(ids) != 1 {
		return nil, ErrAuthorizationDelivery
	}
	id, err := uuid.Parse(ids[0])
	if err != nil || id == uuid.Nil || id.String() != ids[0] {
		return nil, ErrAuthorizationDelivery
	}
	metadata, err := message.Metadata()
	if err != nil || metadata == nil || metadata.Stream != platformStream || metadata.Consumer != durable || metadata.Sequence.Stream == 0 || metadata.NumDelivered == 0 {
		return nil, ErrAuthorizationDelivery
	}
	delivery := &authorizationRawDelivery{message: message, body: bytes.Clone(message.Data()), id: ids[0], durable: durable,
		meta: AuthorizationDeliveryMetadata{Stream: metadata.Stream, StreamSequence: metadata.Sequence.Stream, NumDelivered: metadata.NumDelivered}}
	delivery.lifetime, delivery.close = context.WithCancel(ctx)
	delivery.active.Store(true)
	// This capability is returned only after authorizationConsumer checks the
	// exact workload/durable tuple, never inferred from a caller-chosen name.
	if billingDLQAllowed {
		delivery.publish = func(ctx context.Context, body []byte, messageID string) (*jetstream.PubAck, error) {
			return broker.stream.Publish(ctx, "account-platform.dead-letter."+authorizationSubject, body, jetstream.WithMsgID(messageID))
		}
	}
	return delivery, nil
}

// ConsumeAuthorization is exclusively for the approval authorization subject.
// Its resource binding is checked separately against the closed registry and
// trusted resource configuration; it never creates or updates a consumer here.
// Unlike generic Consume, it does not decode Event, auto-ACK handler success,
// impose a physical retry limit or Term a message. The handler owns disposition.
func (broker *NATSBroker) ConsumeAuthorization(ctx context.Context, durable string, handler AuthorizationRawHandler) (func(), error) {
	if ctx == nil || broker == nil || broker.publishOnly || handler == nil || durable == "" || strings.TrimSpace(durable) != durable || strings.ContainsAny(durable, "\r\n\t\x00") {
		return nil, ErrAuthorizationDelivery
	}
	consumer, billingDLQAllowed, err := broker.authorizationConsumer(ctx, durable)
	if err != nil {
		return nil, err
	}
	consuming, err := consumer.Consume(func(message jetstream.Msg) {
		delivery, err := broker.newAuthorizationRawDelivery(ctx, message, durable, billingDLQAllowed)
		if err != nil {
			// Malformed transport metadata is never a terminal authorization
			// decision. Keep the same delivery and emit only stable diagnostics.
			slog.Error("AUTHORIZATION_DELIVERY_INVALID")
			if nakErr := message.NakWithDelay(authorizationMaximumNAKDelay); nakErr != nil {
				slog.Error("AUTHORIZATION_DELIVERY_NAK_FAILED")
			}
			return
		}
		defer delivery.deactivate()
		handler(ctx, delivery)
	}, jetstream.PullMaxMessages(64))
	if err != nil {
		return nil, err
	}
	return consuming.Stop, nil
}
