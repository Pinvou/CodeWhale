package token

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type governedWorkerRun func(context.Context, func()) error

// tokenGovernanceEpoch owns the cancellation of one active generation. Both
// supervisors replace/close this owner on state changes and close the remaining
// owner on return; no per-iteration deferred cancellations accumulate.
type tokenGovernanceEpoch struct {
	context context.Context
	cancel  context.CancelFunc
}

func newTokenGovernanceEpoch(parent context.Context) *tokenGovernanceEpoch {
	ctx, cancel := context.WithCancel(parent)
	return &tokenGovernanceEpoch{context: ctx, cancel: cancel}
}

func (e *tokenGovernanceEpoch) close() {
	if e != nil && e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
}

// TokenGovernanceSupervisor owns only the two governed polling loops and
// their instance-local readiness. Persistent state/epoch and DB leases remain
// authoritative; readiness cannot authorize a command by itself.
type TokenGovernanceSupervisor struct {
	service  *Service
	reader   TokenCommandGovernanceActivationReader
	factory  func(TokenCommandGovernanceActivation) ([]governedWorkerRun, error)
	interval time.Duration
}

func NewTokenGovernanceSupervisor(service *Service, repository *PostgresRepository, reader TokenCommandGovernanceActivationReader, wallets WalletProvisioningClient, authority OwnerAuthorityClient) (*TokenGovernanceSupervisor, error) {
	if service == nil || repository == nil || reader == nil || wallets == nil || authority == nil {
		return nil, errors.New("governed worker dependencies are incomplete")
	}
	supervisor := &TokenGovernanceSupervisor{service: service, reader: reader, interval: 250 * time.Millisecond}
	service.WithTokenAccountCreateCapability(false)
	supervisor.factory = func(activation TokenCommandGovernanceActivation) ([]governedWorkerRun, error) {
		workerID := "governed-" + uuid.NewString()
		account, err := NewTokenAccountProvisioningWorker(repository, wallets, authority, workerID+"-account", activation)
		if err != nil {
			return nil, err
		}
		lifecycle := NewLifecyclePlanWorker(service, repository, workerID+"-lifecycle")
		wrap := func(process func(context.Context) (bool, error)) governedWorkerRun {
			return func(ctx context.Context, ready func()) error {
				ctx = context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: activation, transactional: true})
				// The handshake is from the running loop after a real persistent
				// fence check. Configuration-only booleans never open creation.
				checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				tx, err := repository.pool.Begin(checkCtx)
				if err == nil {
					err = repository.recheckTokenRouteActivation(checkCtx, tx)
					_ = tx.Rollback(checkCtx)
				}
				cancel()
				if err != nil {
					return err
				}
				ready()
				for {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					iteration, cancel := context.WithTimeout(ctx, 20*time.Second)
					processed, err := process(iteration)
					cancel()
					if err != nil {
						return err
					}
					if !processed {
						timer := time.NewTimer(500 * time.Millisecond)
						select {
						case <-ctx.Done():
							timer.Stop()
							return ctx.Err()
						case <-timer.C:
						}
					}
				}
			}
		}
		return []governedWorkerRun{wrap(account.ProcessOne), wrap(lifecycle.RunOnce)}, nil
	}
	return supervisor, nil
}

func (s *TokenGovernanceSupervisor) Run(ctx context.Context) error {
	if s == nil || s.service == nil || s.reader == nil || s.factory == nil || s.interval <= 0 {
		return errors.New("governance supervisor is not configured")
	}
	var selected TokenCommandGovernanceActivation
	var epoch *tokenGovernanceEpoch
	var readyEvents chan int
	var exits chan error
	readyLoops := map[int]bool{}
	closeEpoch := func() {
		s.service.WithTokenAccountCreateCapability(false)
		epoch.close()
		epoch = nil
		selected = TokenCommandGovernanceActivation{}
		readyEvents = nil
		exits = nil
		readyLoops = map[int]bool{}
	}
	defer closeEpoch()
	read := func() (TokenCommandGovernanceActivation, error) {
		readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return s.reader.CurrentTokenCommandGovernanceActivation(readCtx)
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			activation, err := read()
			if err != nil || activation.State != "ACTIVE" || activation.Epoch < 1 {
				closeEpoch()
			} else if epoch == nil || activation != selected {
				closeEpoch()
				workers, factoryErr := s.factory(activation)
				if factoryErr == nil && len(workers) == 2 && workers[0] != nil && workers[1] != nil {
					epoch = newTokenGovernanceEpoch(ctx)
					workerCtx := epoch.context
					selected = activation
					readyEvents = make(chan int, 2)
					exits = make(chan error, 2)
					for index, run := range workers {
						readyChannel, exitChannel := readyEvents, exits
						go func(index int, run governedWorkerRun) {
							exitChannel <- run(workerCtx, func() {
								select {
								case readyChannel <- index:
								case <-workerCtx.Done():
								}
							})
						}(index, run)
					}
				}
			}
			timer.Reset(s.interval)
		case index := <-readyEvents:
			readyLoops[index] = true
			if len(readyLoops) == 2 {
				current, err := read()
				if err != nil || current != selected {
					closeEpoch()
				} else {
					s.service.WithTokenAccountCreateCapability(true)
				}
			}
		case <-exits:
			// An unexpected worker exit revokes readiness before cancellation.
			// A later poll may safely establish new uniquely-owned DB leases.
			closeEpoch()
		}
	}
}
