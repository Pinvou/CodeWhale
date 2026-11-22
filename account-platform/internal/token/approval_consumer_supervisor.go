package token

import (
	"context"
	"errors"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
)

type approvalEventBroker interface {
	ConsumeAuthorization(context.Context, string, events.AuthorizationRawHandler) (func(), error)
}

// RunTokenApprovalConsumers registers exactly the three approved descriptors
// for a known ACTIVE epoch. Durable subscriptions survive cancellation; SAFE
// never ACKs unresolved events and resumption uses the original event/lease.
func RunTokenApprovalConsumers(ctx context.Context, broker approvalEventBroker, reader tokengovernance.Reader, tokenURL, accountURL, serviceToken string) error {
	if broker == nil || reader == nil {
		return errors.New("approval consumer supervisor is not configured")
	}
	lifecycle, err := NewTokenAuthorizationCommandConsumer("TOKEN_MODEL_LIFECYCLE_CHANGE", "token-model-lifecycle-executor-v1", tokenURL, accountURL, serviceToken, reader)
	if err != nil {
		return err
	}
	catalogRollback, err := NewTokenAuthorizationCommandConsumer("TOKEN_MODEL_CATALOG_ROLLBACK", "token-catalog-rollback-executor-v1", tokenURL, accountURL, serviceToken, reader)
	if err != nil {
		return err
	}
	credentialRevoke, err := NewTokenAuthorizationCommandConsumer("TOKEN_CREDENTIAL_REVOKE", "token-credential-revoke-executor-v1", tokenURL, accountURL, serviceToken, reader)
	if err != nil {
		return err
	}
	registrations := []func(context.Context) (func(), error){
		func(ctx context.Context) (func(), error) {
			return broker.ConsumeAuthorization(ctx, "token-model-lifecycle-executor-v1", lifecycle.Handle)
		},
		func(ctx context.Context) (func(), error) {
			return broker.ConsumeAuthorization(ctx, "token-catalog-rollback-executor-v1", catalogRollback.Handle)
		},
		func(ctx context.Context) (func(), error) {
			return broker.ConsumeAuthorization(ctx, "token-credential-revoke-executor-v1", credentialRevoke.Handle)
		},
	}
	var generation *tokenGovernanceEpoch
	var stops []func()
	var epoch int64
	closeEpoch := func() {
		generation.close()
		for _, stop := range stops {
			stop()
		}
		stops = nil
		generation = nil
		epoch = 0
	}
	defer closeEpoch()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
			snapshot, err := reader.Snapshot(readCtx)
			readCancel()
			if err != nil || snapshot.State != "ACTIVE" || snapshot.Epoch < 1 {
				closeEpoch()
			} else if generation == nil || epoch != snapshot.Epoch {
				closeEpoch()
				generation = newTokenGovernanceEpoch(ctx)
				epoch = snapshot.Epoch
				for _, register := range registrations {
					stop, err := register(generation.context)
					if err != nil {
						closeEpoch()
						return errors.New("Token approval durable registration failed")
					}
					stops = append(stops, stop)
				}
			}
			timer.Reset(250 * time.Millisecond)
		}
	}
}
