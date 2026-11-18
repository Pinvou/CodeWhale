package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func prepareProfileRevocationClosure(t *testing.T, repository *PostgresRepository, change ProfileChange) (*AccountClosureRequest, int64) {
	t.Helper()
	ctx := context.Background()
	principal := identity.Principal{Issuer: change.IdentityIssuer, Subject: change.IdentitySubject, AuthenticatedAt: time.Now()}
	created, err := repository.CreateAccountClosure(ctx, principal, uuid.NewString(), accountClosureConfirmation)
	if err != nil {
		t.Fatal(err)
	}
	claim := func() *AccountClosureRequest {
		t.Helper()
		item, err := repository.ClaimAccountClosure(ctx)
		if err != nil || item == nil || item.ID != created.ID {
			t.Fatal("claim original closure", err)
		}
		return item
	}
	item := claim()
	if err = repository.EnterAccountClosureQuiescing(ctx, *item); err != nil {
		t.Fatal(err)
	}
	item = claim()
	if err = repository.AdvanceAccountClosureToIdentityCleanup(ctx, *item); err != nil {
		t.Fatal(err)
	}
	item = claim()
	preparation, err := repository.PrepareAccountClosureCleanup(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	return item, preparation.SubjectLifecycleVersion
}

func anonymizeProfileRevocationSubject(t *testing.T, repository *PostgresRepository, item *AccountClosureRequest, lifecycle int64) {
	t.Helper()
	ctx := context.Background()
	// External cleanup ACKs are controlled inputs, while the original closure
	// writes, lifecycle gates and identity redaction execute against public SQL.
	if err := repository.MarkAccountClosureIdentityDeleted(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkAccountClosureGrantsIsolated(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkAccountClosureReplayCleared(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
	if err := repository.AnonymizeAccountClosure(ctx, item, lifecycle); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRevocationClosureRuntimePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	fixture := retainedWalletFenceActors(t)
	ctx := context.Background()
	t.Run("reserved-closure-seals-no-dispatch-and-releases-capacity", func(t *testing.T) {
		repository := fixture.repository(t)
		if err := repository.ConfigureProfileRevocationCapacity(1); err != nil {
			t.Fatal(err)
		}
		change := committedRevocationProfile(t, repository)
		if _, err := repository.StartProfileSessionRevocation(ctx, change.ID); err != nil {
			t.Fatal(err)
		}
		item, lifecycle := prepareProfileRevocationClosure(t, repository, change)
		var sealed bool
		if err := repository.pool.QueryRow(ctx, `SELECT state='NOT_DISPATCHED' AND not_dispatched_at IS NOT NULL AND dispatch_token_hash IS NULL AND dispatched_at IS NULL FROM profile_session_revocation_receipts WHERE operation_id=$1`, change.ID).Scan(&sealed); err != nil || !sealed {
			t.Fatal("closure failed to seal the undispatched receipt", sealed, err)
		}
		another := committedRevocationProfile(t, repository)
		if first, err := repository.StartProfileSessionRevocation(ctx, another.ID); err != nil || !first {
			t.Fatal("closed reservation retained capacity", first, err)
		}
		anonymizeProfileRevocationSubject(t, repository, item, lifecycle)
		directory := &historyLogoutDirectory{}
		client := historyAdapter(t, directory, repository)
		if err := client.RevokeProfileSessions(ctx, change.IdentitySubject, change.ID, true); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) || directory.calls.Load() != 0 {
			t.Fatal("sealed receipt regained dispatch", err, directory.calls.Load())
		}
	})
	for _, mode := range []string{"confirmed", "late-confirmation"} {
		t.Run(mode+"-replays-after-anonymization", func(t *testing.T) {
			repository := fixture.repository(t)
			if err := repository.ConfigureProfileRevocationCapacity(1); err != nil {
				t.Fatal(err)
			}
			change := committedRevocationProfile(t, repository)
			if _, err := repository.StartProfileSessionRevocation(ctx, change.ID); err != nil {
				t.Fatal(err)
			}
			directory := &historyLogoutDirectory{}
			client := historyAdapter(t, directory, repository)
			var completed chan error
			if mode == "late-confirmation" {
				directory.entered = make(chan struct{})
				directory.proceed = make(chan struct{})
				t.Cleanup(func() {
					select {
					case <-directory.proceed:
					default:
						close(directory.proceed)
					}
				})
				completed = make(chan error, 1)
				go func() { completed <- client.RevokeProfileSessions(ctx, change.IdentitySubject, change.ID, true) }()
				select {
				case <-directory.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("provider dispatch did not arrive")
				}
			} else if err := client.RevokeProfileSessions(ctx, change.IdentitySubject, change.ID, true); err != nil {
				t.Fatal(err)
			}
			item, lifecycle := prepareProfileRevocationClosure(t, repository, change)
			anonymizeProfileRevocationSubject(t, repository, item, lifecycle)
			var redacted bool
			if err := repository.pool.QueryRow(ctx, `SELECT identity_subject<>$2 AND identity_issuer='closed' FROM profile_change_requests WHERE id=$1`, change.ID, change.IdentitySubject).Scan(&redacted); err != nil || !redacted {
				t.Fatal("actual subject redaction did not execute", err)
			}
			if mode == "late-confirmation" {
				another := committedRevocationProfile(t, repository)
				if first, err := repository.StartProfileSessionRevocation(ctx, another.ID); !errors.Is(err, ErrProfileSessionRevocationCapacity) || first {
					t.Fatal("closure released an already dispatched outcome", first, err)
				}
				close(directory.proceed)
				if err := <-completed; err != nil {
					t.Fatal("late provider ACK lost its immutable subject binding", err)
				}
			}
			restarted := historyAdapter(t, directory, repository)
			if err := restarted.RevokeProfileSessions(ctx, change.IdentitySubject, change.ID, false); err != nil || directory.calls.Load() != 1 {
				t.Fatal("confirmed replay after anonymization", err, directory.calls.Load())
			}
			if err := restarted.RevokeProfileSessions(ctx, "another-subject", change.ID, false); !errors.Is(err, ErrProfileChangeConflict) || directory.calls.Load() != 1 {
				t.Fatal("wrong subject borrowed a historical ACK", err)
			}
		})
	}
}
