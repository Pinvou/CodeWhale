package account

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

type RegistrationRecoveryRepository interface {
	LeaseRegistrationRecoveries(context.Context, int, time.Duration) ([]RegistrationOperation, error)
}

type RegistrationRecoveryWorker struct {
	repository RegistrationRecoveryRepository
	auth       *MobileAuthService
	interval   time.Duration
}

func NewRegistrationRecoveryWorker(repository RegistrationRecoveryRepository, auth *MobileAuthService, interval time.Duration) *RegistrationRecoveryWorker {
	return &RegistrationRecoveryWorker{repository: repository, auth: auth, interval: interval}
}

func (w *RegistrationRecoveryWorker) Run(ctx context.Context) {
	if w == nil || w.repository == nil || w.auth == nil {
		return
	}
	if w.interval <= 0 {
		w.interval = 5 * time.Second
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.RunOnce(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "registration recovery cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *RegistrationRecoveryWorker) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	operations, err := w.repository.LeaseRegistrationRecoveries(ctx, 20, 30*time.Second)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// A failed operation must not abandon the other independently leased rows.
	var failures []error
	for _, operation := range operations {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		itemCtx, cancel, err := withIdentityRecoveryClaim(ctx, "registration", operation.ID, operation.RecoveryGeneration, operation.RecoveryLeaseUntil)
		var itemErr error
		if err == nil {
			_, err = w.auth.resumeRegistration(itemCtx, operation)
			// Inspect the lease context before cancel releases its resources.
			itemErr = itemCtx.Err()
			cancel()
		}
		if err != nil && !errors.Is(err, ErrRegistrationRecovering) && !errors.Is(err, ErrRegistrationRestart) {
			failures = append(failures, err)
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if itemErr != nil {
			return errors.Join(append(failures, itemErr)...)
		}
	}
	return errors.Join(failures...)
}
