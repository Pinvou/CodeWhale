package events

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel/codes"
)

// FrozenClaim identifies one committed outbox lease. It intentionally omits
// the delivery body: the store must re-lock and re-validate persisted bytes in
// the transaction that spans Publish.
type FrozenClaim struct {
	EventID           uuid.UUID
	EventType         string
	TraceID           string
	ClaimToken        uuid.UUID
	RelayControlEpoch int64
}

type FrozenMessage struct {
	EventID uuid.UUID
	Subject string
	Body    []byte
	MsgID   string
}

type FrozenDisposition string

const (
	FrozenPublished    FrozenDisposition = "PUBLISHED"
	FrozenRetry        FrozenDisposition = "RETRY"
	FrozenDeadLettered FrozenDisposition = "DEAD_LETTERED"
	// FrozenUnknown means the broker may have accepted the message. The store
	// must not advance attempts or a dead-letter state from this observation.
	FrozenUnknown FrozenDisposition = "UNKNOWN"
)

type FrozenPublishResult struct {
	Disposition FrozenDisposition
	RetryAt     time.Time
	ErrorCode   string
}

type FrozenPublisher func(context.Context, FrozenMessage) FrozenPublishResult

// FrozenPublishObserver observes only the interval in which a frozen
// authorization delivery is inside Broker.Publish. It is process-local
// operational evidence, not a substitute for the database claim protocol.
type FrozenPublishObserver interface {
	BeginFrozenPublish()
	EndFrozenPublish()
}

// FrozenOutboxStore owns the database transaction and locks which surround
// publisher. Process must not expose the body before the claim is revalidated.
type FrozenOutboxStore interface {
	ClaimFrozen(context.Context, string, int, time.Duration) ([]FrozenClaim, error)
	ProcessFrozen(context.Context, string, FrozenClaim, int, time.Time, FrozenPublisher) (FrozenDisposition, error)
}

type FrozenRelay struct {
	Store       FrozenOutboxStore
	Broker      Broker
	WorkerID    string
	BatchSize   int
	Lease       time.Duration
	MaxAttempts int
	Now         func() time.Time
	Observer    FrozenPublishObserver
}

func (relay FrozenRelay) RunOnce(ctx context.Context) (RelayResult, error) {
	if relay.Store == nil || relay.Broker == nil || relay.WorkerID == "" {
		return RelayResult{}, errors.New("frozen event relay is not configured")
	}
	if relay.BatchSize < 1 || relay.BatchSize > 1000 {
		relay.BatchSize = 100
	}
	if relay.Lease <= 0 {
		relay.Lease = 30 * time.Second
	}
	if relay.MaxAttempts < 1 {
		relay.MaxAttempts = 12
	}
	if relay.Now == nil {
		relay.Now = func() time.Time { return time.Now().UTC() }
	}
	claims, err := relay.Store.ClaimFrozen(ctx, relay.WorkerID, relay.BatchSize, relay.Lease)
	result := RelayResult{Claimed: len(claims)}
	if err != nil {
		return result, err
	}
	for _, claim := range claims {
		eventContext := observability.ContextForTrace(ctx, claim.EventID.String(), claim.TraceID)
		eventContext, span := observability.StartSpan(eventContext, "event.relay.publish-frozen")
		disposition, processErr := relay.Store.ProcessFrozen(eventContext, relay.WorkerID, claim, relay.MaxAttempts, relay.Now(), func(publishContext context.Context, message FrozenMessage) FrozenPublishResult {
			publishErr := relay.publish(publishContext, message)
			if publishErr == nil {
				return FrozenPublishResult{Disposition: FrozenPublished}
			}
			return FrozenPublishResult{Disposition: classifyFrozenPublishError(publishErr), ErrorCode: safeDeliveryError(publishErr)}
		})
		if processErr != nil {
			span.SetStatus(codes.Error, "FROZEN_EVENT_DELIVERY_FAILED")
			span.End()
			return result, processErr
		}
		switch disposition {
		case FrozenPublished:
			result.Published++
		case FrozenRetry:
			result.Failed++
		case FrozenDeadLettered:
			result.DeadLettered++
		default:
			span.SetStatus(codes.Error, "FROZEN_EVENT_DELIVERY_INVALID_OUTCOME")
			span.End()
			return result, errors.New("frozen event relay returned an invalid disposition")
		}
		span.End()
	}
	return result, nil
}

func (relay FrozenRelay) publish(ctx context.Context, message FrozenMessage) error {
	if relay.Observer != nil {
		relay.Observer.BeginFrozenPublish()
		defer relay.Observer.EndFrozenPublish()
	}
	return relay.Broker.Publish(ctx, message.Subject, message.Body, message.MsgID)
}

func classifyFrozenPublishError(err error) FrozenDisposition {
	if err == nil {
		return FrozenPublished
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) || errors.Is(err, nats.ErrConnectionReconnecting) ||
		errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrAuthExpired) || errors.Is(err, nats.ErrAuthRevoked) ||
		errors.Is(err, nats.ErrDisconnected) || errors.Is(err, nats.ErrNoServers) || errors.Is(err, nats.ErrStaleConnection) {
		return FrozenUnknown
	}
	// Only failures which prove that NATS rejected the message before accepting
	// it may advance an attempt. In particular, a generic JetStream API error
	// is not sufficient: unknown and future server error codes remain UNKNOWN.
	if errors.Is(err, nats.ErrPermissionViolation) || errors.Is(err, nats.ErrAuthorization) ||
		errors.Is(err, nats.ErrBadSubject) || errors.Is(err, nats.ErrMaxPayload) || errors.Is(err, nats.ErrInvalidMsg) {
		return FrozenRetry
	}
	return FrozenUnknown
}
