package account

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Controlled storage ports exercise the actual worker and resumeRegistration.
// They do not replace the production PostgreSQL lease/CAS integration tests.
type registrationRecoveryBatchRepository struct {
	mobileAuthRepositoryFake
	jobs                    []RegistrationOperation
	states                  map[uuid.UUID]RegistrationOperation
	markErrors              map[uuid.UUID]error
	firstAttempt            map[uuid.UUID]bool
	leaseErr                error
	leaseCalls              int
	marks, commits          []uuid.UUID
	contexts                []context.Context
	retryCalls, retryWrites int
	compensations           int
	leaseHook               func()
	markHook                func(context.Context, uuid.UUID) error
	commitHook              func()
}

func recoveryBatch(count int) *registrationRecoveryBatchRepository {
	r := &registrationRecoveryBatchRepository{states: map[uuid.UUID]RegistrationOperation{}, markErrors: map[uuid.UUID]error{}, firstAttempt: map[uuid.UUID]bool{}}
	until := time.Now().Add(time.Minute)
	for range count {
		op := RegistrationOperation{ID: uuid.New(), Status: "IDENTITY_CREATED", RecoveryGeneration: 7, RecoveryLeaseUntil: &until, Version: 11}
		r.jobs = append(r.jobs, op)
		r.states[op.ID] = op
	}
	return r
}

func (r *registrationRecoveryBatchRepository) LeaseRegistrationRecoveries(_ context.Context, limit int, lease time.Duration) ([]RegistrationOperation, error) {
	r.leaseCalls++
	if limit != 20 || lease != 30*time.Second {
		panic("worker changed its existing lease bounds")
	}
	if r.leaseHook != nil {
		r.leaseHook()
	}
	return append([]RegistrationOperation(nil), r.jobs...), r.leaseErr
}

func (r *registrationRecoveryBatchRepository) claim(ctx context.Context, id uuid.UUID) error {
	claim, ok := ctx.Value(identityRecoveryClaimKey{}).(identityRecoveryClaim)
	op := r.states[id]
	if !ok || claim.kind != "registration" || claim.id != id || claim.generation != op.RecoveryGeneration || op.RecoveryLeaseUntil == nil || !claim.until.Equal(*op.RecoveryLeaseUntil) || !claim.until.After(time.Now()) {
		return ErrIdentityRecoveryClaimLost
	}
	if deadline, ok := ctx.Deadline(); ok && deadline.After(claim.until) {
		return errors.New("worker widened the leased deadline")
	}
	return ctx.Err()
}

func (r *registrationRecoveryBatchRepository) MarkRegistrationIdentityCreating(ctx context.Context, id uuid.UUID) error {
	r.marks = append(r.marks, id)
	r.contexts = append(r.contexts, ctx)
	if err := r.claim(ctx, id); err != nil {
		return err
	}
	if r.markHook != nil {
		if err := r.markHook(ctx, id); err != nil {
			return err
		}
	}
	return r.markErrors[id]
}

func (r *registrationRecoveryBatchRepository) CommitRegistration(ctx context.Context, id uuid.UUID, _ string) (RegistrationOperation, error) {
	r.commits = append(r.commits, id)
	r.contexts = append(r.contexts, ctx)
	if err := r.claim(ctx, id); err != nil {
		return RegistrationOperation{}, err
	}
	op := r.states[id]
	op.Status = "PROVISIONING"
	r.states[id] = op
	if r.commitHook != nil {
		r.commitHook()
	}
	return op, nil
}

func (r *registrationRecoveryBatchRepository) RegistrationOperation(_ context.Context, id uuid.UUID) (RegistrationOperation, error) {
	return r.states[id], nil
}
func (r *registrationRecoveryBatchRepository) MarkRegistrationCreateAttempted(ctx context.Context, id uuid.UUID) (bool, int64, error) {
	if err := r.claim(ctx, id); err != nil {
		return false, 0, err
	}
	return r.firstAttempt[id], 11, nil
}
func (r *registrationRecoveryBatchRepository) ScheduleRegistrationRetry(ctx context.Context, id uuid.UUID, _ string) error {
	r.retryCalls++
	if err := r.claim(ctx, id); err != nil {
		return err
	}
	r.retryWrites++
	return nil
}
func (r *registrationRecoveryBatchRepository) CompensatePreIdentityRegistration(ctx context.Context, id uuid.UUID, _ string, _ int64) error {
	if err := r.claim(ctx, id); err != nil {
		return err
	}
	r.compensations++
	op := r.states[id]
	op.Status = "COMPENSATED"
	r.states[id] = op
	return nil
}

type registrationRecoveryBatchIdentity struct {
	identityProvisionerFake
	lookups, creates int
	createErr        error
	lookupHook       func(context.Context) error
}

func (i *registrationRecoveryBatchIdentity) FindMobileUserByRegistrationOperation(ctx context.Context, _ uuid.UUID) (string, bool, error) {
	i.lookups++
	if i.lookupHook != nil {
		return "", false, i.lookupHook(ctx)
	}
	return "", false, nil
}
func (i *registrationRecoveryBatchIdentity) CreateMobileUserFirstAttempt(context.Context, uuid.UUID, string, string, string, string) (string, error) {
	i.creates++
	return "", i.createErr
}

func (r *registrationRecoveryBatchRepository) worker(identity *registrationRecoveryBatchIdentity) *RegistrationRecoveryWorker {
	return NewRegistrationRecoveryWorker(r, &MobileAuthService{repository: r, identities: identity, issuer: "https://controlled.invalid/realms/pinvou"}, time.Second)
}
func (r *registrationRecoveryBatchRepository) poison(index int, err error) uuid.UUID {
	op := r.jobs[index]
	op.Status = "IDENTITY_CREATING"
	r.jobs[index], r.states[op.ID], r.markErrors[op.ID] = op, op, err
	return op.ID
}
func (r *registrationRecoveryBatchRepository) released(t *testing.T) {
	t.Helper()
	for _, ctx := range r.contexts {
		if ctx.Err() == nil {
			t.Fatal("processed operation retained a live lease context")
		}
	}
}

func TestRegistrationRecoveryBatchIsolation(t *testing.T) {
	t.Run("poison-first-does-not-abandon-nineteen-valid-leases", func(t *testing.T) {
		r := recoveryBatch(20)
		poisonErr := errors.New("controlled poison storage failure")
		poison := r.poison(0, poisonErr)
		identity := &registrationRecoveryBatchIdentity{}
		err := r.worker(identity).RunOnce(context.Background())
		if !errors.Is(err, poisonErr) || len(r.marks) != 1 || len(r.commits) != 19 || r.leaseCalls != 1 || identity.creates != 0 || r.states[poison].Status != "IDENTITY_CREATING" {
			t.Fatal(err, len(r.commits), r.leaseCalls, identity.creates)
		}
		for _, op := range r.jobs[1:] {
			if r.states[op.ID].Status != "PROVISIONING" || r.states[op.ID].RecoveryGeneration != op.RecoveryGeneration {
				t.Fatal("healthy leased operation was not committed under its original claim")
			}
		}
		r.released(t)
	})
	t.Run("all-item-errors-remain-discoverable-after-later-success", func(t *testing.T) {
		r := recoveryBatch(4)
		first, second := errors.New("first controlled failure"), errors.New("second controlled failure")
		r.poison(0, first)
		r.poison(2, second)
		err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(context.Background())
		if !errors.Is(err, first) || !errors.Is(err, second) || len(r.commits) != 2 || len(r.marks) != 2 {
			t.Fatal(err, len(r.commits), len(r.marks))
		}
		r.released(t)
	})
	t.Run("missing-claim-is-an-item-failure-not-a-batch-dispatch", func(t *testing.T) {
		r := recoveryBatch(20)
		r.jobs[0].RecoveryGeneration = 0
		err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(context.Background())
		if !errors.Is(err, ErrIdentityRecoveryClaimLost) || len(r.commits) != 19 || len(r.marks) != 0 || r.states[r.jobs[0].ID].Status != "IDENTITY_CREATED" {
			t.Fatal(err, len(r.commits), len(r.marks))
		}
		r.released(t)
	})
	for _, downstream := range []error{context.DeadlineExceeded, context.Canceled} {
		t.Run("downstream-"+downstream.Error()+"-with-live-context-does-not-poison-batch", func(t *testing.T) {
			r := recoveryBatch(2)
			r.poison(0, fmt.Errorf("controlled adapter: %w", downstream))
			err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(context.Background())
			if !errors.Is(err, downstream) || len(r.commits) != 1 {
				t.Fatal(err, len(r.commits))
			}
			r.released(t)
		})
	}
	t.Run("lease-failure-is-returned-without-resuming-any-returned-rows", func(t *testing.T) {
		r := recoveryBatch(2)
		r.leaseErr = errors.New("controlled lease failure")
		if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(context.Background()); err != r.leaseErr || len(r.commits) != 0 || len(r.marks) != 0 || r.leaseCalls != 1 {
			t.Fatal(err, r.leaseCalls, r.commits)
		}
	})
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("already-done-parent-context-deadline-%t-never-leases", deadline), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if deadline {
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			} else {
				cancel()
			}
			defer cancel()
			r := recoveryBatch(2)
			if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(ctx); !errors.Is(err, ctx.Err()) || r.leaseCalls != 0 {
				t.Fatal(err, r.leaseCalls)
			}
		})
	}
	for _, count := range []int{0, 2} {
		t.Run(fmt.Sprintf("parent-canceled-by-lease-count-%d-stops-before-first-item", count), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := recoveryBatch(count)
			r.leaseHook = cancel
			if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(ctx); !errors.Is(err, context.Canceled) || len(r.commits) != 0 {
				t.Fatal(err, r.commits)
			}
		})
	}
	t.Run("parent-canceled-after-success-is-returned-without-starting-next-item", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := recoveryBatch(2)
		r.commitHook = cancel
		if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(ctx); !errors.Is(err, context.Canceled) || len(r.commits) != 1 || r.states[r.jobs[0].ID].Status != "PROVISIONING" || r.states[r.jobs[1].ID].Status != "IDENTITY_CREATED" {
			t.Fatal(err, r.commits)
		}
		r.released(t)
	})
	t.Run("cancellation-retains-earlier-and-current-item-errors", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := recoveryBatch(3)
		first, second := errors.New("earlier item failure"), errors.New("current item failure")
		r.poison(0, first)
		last := r.poison(1, second)
		r.markHook = func(_ context.Context, id uuid.UUID) error {
			if id == last {
				cancel()
			}
			return nil
		}
		err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(ctx)
		if !errors.Is(err, first) || !errors.Is(err, second) || !errors.Is(err, context.Canceled) || len(r.marks) != 2 || len(r.commits) != 0 {
			t.Fatal(err, len(r.marks), len(r.commits))
		}
		r.released(t)
	})
	t.Run("parent-deadline-during-item-stops-later-work", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		r := recoveryBatch(2)
		r.poison(0, nil)
		r.markHook = func(ctx context.Context, _ uuid.UUID) error { <-ctx.Done(); return ctx.Err() }
		if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(ctx); !errors.Is(err, context.DeadlineExceeded) || len(r.commits) != 0 || len(r.marks) != 1 {
			t.Fatal(err, len(r.commits), len(r.marks))
		}
		r.released(t)
	})
	t.Run("expired-item-context-is-not-hidden-by-recovering-or-detached-cleanup", func(t *testing.T) {
		r := recoveryBatch(2)
		id := r.poison(0, nil)
		until := time.Now().Add(40 * time.Millisecond)
		r.jobs[0].RecoveryLeaseUntil = &until
		r.states[id] = r.jobs[0]
		identity := &registrationRecoveryBatchIdentity{lookupHook: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }}
		if err := r.worker(identity).RunOnce(context.Background()); !errors.Is(err, context.DeadlineExceeded) || len(r.commits) != 0 || identity.creates != 0 || r.retryCalls != 1 || r.retryWrites != 0 || r.states[id].Status != "IDENTITY_CREATING" {
			t.Fatal(err, r.commits, identity.creates, r.retryCalls, r.retryWrites)
		}
		r.released(t)
	})
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing-recovering-restart-%t-remains-nonfatal-and-never-redispatches-unknown", restart), func(t *testing.T) {
			r := recoveryBatch(2)
			id := r.poison(0, nil)
			r.firstAttempt[id] = restart
			identity := &registrationRecoveryBatchIdentity{createErr: ErrIdentityCreateNotDispatched}
			if err := r.worker(identity).RunOnce(context.Background()); err != nil || len(r.commits) != 1 || identity.lookups != 1 || identity.creates != map[bool]int{false: 0, true: 1}[restart] {
				t.Fatal(err, r.commits, identity.lookups, identity.creates)
			}
			if restart && (r.compensations != 1 || r.states[id].Status != "COMPENSATED") || !restart && (r.retryWrites != 1 || r.states[id].Status != "IDENTITY_CREATING") {
				t.Fatal("existing saga outcome changed", r.compensations, r.retryWrites, r.states[id].Status)
			}
			r.released(t)
		})
	}
	for _, count := range []int{0, 3} {
		t.Run(fmt.Sprintf("healthy-or-empty-batch-%d-returns-nil", count), func(t *testing.T) {
			r := recoveryBatch(count)
			if err := r.worker(&registrationRecoveryBatchIdentity{}).RunOnce(context.Background()); err != nil || len(r.commits) != count || r.leaseCalls != 1 {
				t.Fatal(err, len(r.commits), r.leaseCalls)
			}
			r.released(t)
		})
	}
}
