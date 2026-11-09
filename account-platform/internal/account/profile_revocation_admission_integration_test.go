package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/profilerevocation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func committedRevocationProfile(t *testing.T, repository *PostgresRepository) ProfileChange {
	t.Helper()
	change := historyProfile(t, repository)
	if err := repository.MarkProfileIdentityUpdated(context.Background(), change.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitProfileChange(context.Background(), change.ID); err != nil {
		t.Fatal(err)
	}
	return change
}

func TestProfileRevocationDurableAdmissionRuntimePostgres(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	fixture := retainedWalletFenceActors(t)
	ctx := context.Background()
	t.Run("capacity-before-grant-confirmed-replay-and-unknown-retention", func(t *testing.T) {
		repository := fixture.repository(t)
		if err := repository.ConfigureProfileRevocationCapacity(1); err != nil {
			t.Fatal(err)
		}
		first := committedRevocationProfile(t, repository)
		second := committedRevocationProfile(t, repository)
		third := committedRevocationProfile(t, repository)
		if granted, err := repository.StartProfileSessionRevocation(ctx, first.ID); err != nil || !granted {
			t.Fatal("first admission", granted, err)
		}
		if granted, err := repository.StartProfileSessionRevocation(ctx, second.ID); !errors.Is(err, ErrProfileSessionRevocationCapacity) || granted {
			t.Fatal("capacity did not precede first grant", granted, err)
		}
		var untouched bool
		if err := repository.pool.QueryRow(ctx, `SELECT sessions_revocation_started_at IS NULL AND NOT EXISTS(SELECT 1 FROM profile_session_revocation_receipts WHERE operation_id=$1) AND NOT EXISTS(SELECT 1 FROM audit_logs WHERE resource_id=$1::uuid::text AND action='account.profile_change.revocation_started') FROM profile_change_requests WHERE id=$1`, second.ID).Scan(&untouched); err != nil || !untouched {
			t.Fatal("rejected admission consumed state", untouched, err)
		}
		directory := &historyLogoutDirectory{}
		client := historyAdapter(t, directory, repository)
		if err := client.RevokeProfileSessions(ctx, first.IdentitySubject, first.ID, true); err != nil {
			t.Fatal("confirm first", err)
		}
		if granted, err := repository.StartProfileSessionRevocation(ctx, second.ID); err != nil || !granted {
			t.Fatal("confirmation did not release admission capacity", granted, err)
		}
		// A new adapter and an already full admission pool still replay the ACK.
		restarted := historyAdapter(t, directory, repository)
		if err := restarted.RevokeProfileSessions(ctx, first.IdentitySubject, first.ID, false); err != nil || directory.calls.Load() != 1 {
			t.Fatal("durable confirmed replay", err, directory.calls.Load())
		}
		unknownDirectory := &historyLogoutDirectory{unknown: true}
		unknown := historyAdapter(t, unknownDirectory, repository)
		if err := unknown.RevokeProfileSessions(ctx, second.IdentitySubject, second.ID, true); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) {
			t.Fatal("unknown provider ACK", err)
		}
		if granted, err := repository.StartProfileSessionRevocation(ctx, third.ID); !errors.Is(err, ErrProfileSessionRevocationCapacity) || granted {
			t.Fatal("unknown result lost its capacity slot", granted, err)
		}
		if err := unknown.RevokeProfileSessions(ctx, second.IdentitySubject, second.ID, true); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) || unknownDirectory.calls.Load() != 1 {
			t.Fatal("unknown dispatch repeated", err, unknownDirectory.calls.Load())
		}
	})
	t.Run("cancel-before-authority-preserves-reservation", func(t *testing.T) {
		repository := fixture.repository(t)
		change := committedRevocationProfile(t, repository)
		if _, err := repository.StartProfileSessionRevocation(ctx, change.ID); err != nil {
			t.Fatal(err)
		}
		directory := &historyLogoutDirectory{}
		client := historyAdapter(t, directory, repository)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if err := client.RevokeProfileSessions(cancelled, change.IdentitySubject, change.ID, true); err == nil || directory.calls.Load() != 0 {
			t.Fatal("cancelled request dispatched", err)
		}
		if err := client.RevokeProfileSessions(ctx, change.IdentitySubject, change.ID, false); err != nil || directory.calls.Load() != 1 {
			t.Fatal("proven reservation failed to resume", err)
		}
	})
	t.Run("dispatch-token-and-legacy-command-cannot-regrant", func(t *testing.T) {
		repository := fixture.repository(t)
		change := committedRevocationProfile(t, repository)
		if _, err := repository.StartProfileSessionRevocation(ctx, change.ID); err != nil {
			t.Fatal(err)
		}
		command := profilerevocation.Command{OperationID: change.ID, Subject: change.IdentitySubject, DispatchToken: uuid.New(), ReplayOnly: true}
		if _, err := repository.ClaimProfileRevocation(ctx, command); !errors.Is(err, profilerevocation.ErrUnconfirmed) {
			t.Fatal("legacy replay granted a new dispatch", err)
		}
		command.ReplayOnly = false
		if result, err := repository.ClaimProfileRevocation(ctx, command); err != nil || result.State != "DISPATCH" {
			t.Fatal("first claim", result, err)
		}
		if _, err := repository.ClaimProfileRevocation(ctx, command); !errors.Is(err, profilerevocation.ErrUnconfirmed) {
			t.Fatal("same-token retry repeated dispatch", err)
		}
		forged := command
		forged.DispatchToken = uuid.New()
		if err := repository.ConfirmProfileRevocation(ctx, forged); !errors.Is(err, profilerevocation.ErrUnconfirmed) {
			t.Fatal("another attempt forged confirmation", err)
		}
		if err := repository.ConfirmProfileRevocation(ctx, command); err != nil {
			t.Fatal("confirm exact token", err)
		}
		command.ReplayOnly = true
		if result, err := repository.ClaimProfileRevocation(ctx, command); err != nil || result.State != "CONFIRMED" {
			t.Fatal("legacy confirmed replay", result, err)
		}
	})
	t.Run("concurrent-global-admission-across-profiles", func(t *testing.T) {
		repository := fixture.repository(t)
		if err := repository.ConfigureProfileRevocationCapacity(1); err != nil {
			t.Fatal(err)
		}
		changes := []ProfileChange{committedRevocationProfile(t, repository), committedRevocationProfile(t, repository)}
		blocker, err := repository.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('profile-revocation-admission',0))`); err != nil {
			t.Fatal(err)
		}
		type admission struct {
			first bool
			err   error
		}
		results := make(chan admission, 2)
		for _, change := range changes {
			go func(id uuid.UUID) {
				first, err := repository.StartProfileSessionRevocation(ctx, id)
				results <- admission{first, err}
			}(change.ID)
		}
		wallClockWait(t, ctx, fixture.admin, `SELECT count(*)=2 FROM pg_stat_activity WHERE datname=$1 AND wait_event='advisory' AND query LIKE '%profile-revocation-admission%'`, repository.pool.Config().ConnConfig.Database)
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		accepted, rejected := 0, 0
		for range changes {
			result := <-results
			if result.first && result.err == nil {
				accepted++
			} else if !result.first && errors.Is(result.err, ErrProfileSessionRevocationCapacity) {
				rejected++
			} else {
				t.Fatal("unexpected admission outcome", result)
			}
		}
		if accepted != 1 || rejected != 1 {
			t.Fatal("global capacity crossed", accepted, rejected)
		}
		var receipts, grants int
		if err = repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM profile_session_revocation_receipts),(SELECT count(*) FROM profile_change_requests WHERE sessions_revocation_started_at IS NOT NULL)`).Scan(&receipts, &grants); err != nil || receipts != 1 || grants != 1 {
			t.Fatal("global admission left partial grants", receipts, grants, err)
		}
	})
	t.Run("legacy-upgrade-preserves-confirmed-and-unknown-receipts", func(t *testing.T) {
		var confirmed, unknown ProfileChange
		var previousLedger string
		repository := fixture.repositoryWithInitialStage(t, func(ctx context.Context, connection *pgx.Conn, all []migrations.Migration) error {
			var previous []migrations.Migration
			for _, migration := range all {
				if migration.Version != 900019 {
					previous = append(previous, migration)
				}
			}
			if err := accountschema.Up(ctx, connection, previous); err != nil {
				return err
			}
			configuration, err := pgxpool.ParseConfig(connection.Config().ConnString())
			if err != nil {
				return err
			}
			configuration.ConnConfig.Database = connection.Config().Database
			configuration.ConnConfig.User = "account_service"
			configuration.ConnConfig.Password = ""
			pool, err := pgxpool.NewWithConfig(ctx, configuration)
			if err != nil {
				return err
			}
			defer pool.Close()
			oldRepository := &PostgresRepository{pool: pool, now: func() time.Time { return time.Now().UTC() }}
			confirmed = committedRevocationProfile(t, oldRepository)
			unknown = committedRevocationProfile(t, oldRepository)
			// Reproduce the published legacy grant writer before installing the
			// receipt table. A pre-existing local ACK is the only success fact.
			if _, err = pool.Exec(ctx, `UPDATE profile_change_requests SET sessions_revocation_started_at=now(),version=version+1,updated_at=now() WHERE id=ANY($1)`, []uuid.UUID{confirmed.ID, unknown.ID}); err != nil {
				return err
			}
			if err = oldRepository.MarkProfileSessionsRevoked(ctx, confirmed.ID); err != nil {
				return err
			}
			return connection.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m`).Scan(&previousLedger)
		})
		var currentLedger, state string
		if err := repository.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(m) ORDER BY version)::text FROM schema_migrations m WHERE version<>900019`).Scan(&currentLedger); err != nil || currentLedger != previousLedger {
			t.Fatal("receipt upgrade changed existing migration identities", err)
		}
		for _, change := range []ProfileChange{confirmed, unknown} {
			if err := repository.pool.QueryRow(ctx, `SELECT state FROM profile_session_revocation_receipts WHERE operation_id=$1`, change.ID).Scan(&state); err != nil {
				t.Fatal(err)
			}
			expected := "UNKNOWN"
			if change.ID == confirmed.ID {
				expected = "CONFIRMED"
			}
			if state != expected {
				t.Fatal("legacy receipt invented or lost an ACK", state, expected)
			}
		}
		directory := &historyLogoutDirectory{}
		client := historyAdapter(t, directory, repository)
		if err := client.RevokeProfileSessions(ctx, confirmed.IdentitySubject, confirmed.ID, false); err != nil {
			t.Fatal("legacy ACK replay", err)
		}
		if err := client.RevokeProfileSessions(ctx, unknown.IdentitySubject, unknown.ID, true); !errors.Is(err, ErrProfileSessionRevocationUnconfirmed) || directory.calls.Load() != 0 {
			t.Fatal("legacy grant dispatched again", err, directory.calls.Load())
		}
		installed, err := migrations.Load("../../migrations/account")
		if err != nil {
			t.Fatal(err)
		}
		const receiptSnapshot = `SELECT jsonb_build_object('ledger',(SELECT jsonb_agg(to_jsonb(m) ORDER BY version) FROM schema_migrations m),'receipts',(SELECT jsonb_agg(to_jsonb(r) ORDER BY operation_id) FROM profile_session_revocation_receipts r))::text`
		var beforeDown, afterDown string
		if err = repository.pool.QueryRow(ctx, receiptSnapshot).Scan(&beforeDown); err != nil {
			t.Fatal(err)
		}
		migrationConnection, err := repository.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		err = accountschema.DownOne(ctx, migrationConnection.Conn(), installed)
		migrations.Unlock(ctx, migrationConnection.Conn())
		migrationConnection.Release()
		var refused *pgconn.PgError
		if !errors.As(err, &refused) || refused.Code != "55000" {
			t.Fatal("public receipt rollback was not refused", err)
		}
		if err = repository.pool.QueryRow(ctx, receiptSnapshot).Scan(&afterDown); err != nil || beforeDown != afterDown {
			t.Fatal("refused rollback changed receipt or ledger facts", err)
		}
		if err = repository.pool.QueryRow(ctx, `SELECT state FROM profile_session_revocation_receipts WHERE operation_id=$1`, unknown.ID).Scan(&state); err != nil || state != "UNKNOWN" {
			t.Fatal("refused rollback lost unknown evidence", state, err)
		}
	})
	t.Run("capacity-public-complete-resumes-original-operation", func(t *testing.T) {
		repository := fixture.repository(t)
		if err := repository.ConfigureProfileRevocationCapacity(1); err != nil {
			t.Fatal(err)
		}
		first := committedRevocationProfile(t, repository)
		second := committedRevocationProfile(t, repository)
		if _, err := repository.StartProfileSessionRevocation(ctx, first.ID); err != nil {
			t.Fatal(err)
		}
		directory := &historyLogoutDirectory{}
		client := historyAdapter(t, directory, repository)
		service := NewService(repository, nil).WithProfileChanges(&ProfileChangeService{store: repository, identities: client, now: time.Now})
		principal := identity.Principal{Issuer: second.IdentityIssuer, Subject: second.IdentitySubject, SessionID: "controlled-profile-session", AuthenticatedAt: time.Now()}
		handler := service.Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
		complete := func() *httptest.ResponseRecorder {
			request := httptest.NewRequest(http.MethodPost, "/v1/me/profile-change-requests/"+second.ID.String()+"/actions/complete", strings.NewReader(`{}`))
			request.Header.Set("Authorization", "Bearer controlled-profile-user")
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			return response
		}
		pending := complete()
		if pending.Code != http.StatusAccepted || !strings.Contains(pending.Body.String(), `"reason":"SESSION_REVOCATION_CAPACITY"`) || directory.calls.Load() != 0 {
			t.Fatal("public overload receipt did not remain actionable", pending.Code, pending.Body.String())
		}
		var untouched bool
		if err := repository.pool.QueryRow(ctx, `SELECT sessions_revocation_started_at IS NULL AND NOT EXISTS(SELECT 1 FROM profile_session_revocation_receipts WHERE operation_id=$1) FROM profile_change_requests WHERE id=$1`, second.ID).Scan(&untouched); err != nil || !untouched {
			t.Fatal("public overload consumed first grant", err)
		}
		if err := client.RevokeProfileSessions(ctx, first.IdentitySubject, first.ID, true); err != nil {
			t.Fatal(err)
		}
		completed := complete()
		if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), `"status":"COMPLETED"`) || directory.calls.Load() != 2 {
			t.Fatal("original operation failed to converge after capacity released", completed.Code, completed.Body.String(), directory.calls.Load())
		}
	})
	t.Run("lease-expiry-during-global-admission-wait", func(t *testing.T) {
		repository := fixture.repository(t)
		var clock atomic.Int64
		clock.Store(time.Now().UnixNano())
		repository.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
		change := committedRevocationProfile(t, repository)
		claim, err := repository.ClaimProfileChange(ctx)
		if err != nil || claim == nil || claim.ID != change.ID {
			t.Fatal("profile recovery claim", err)
		}
		claimed, cancel, err := withIdentityRecoveryClaim(ctx, "profile", claim.ID, claim.RecoveryGeneration, claim.RecoveryLeaseUntil)
		if err != nil {
			t.Fatal(err)
		}
		defer cancel()
		detached := context.WithoutCancel(claimed)
		before := historyClaimSnapshot(t, repository, "profile_change_requests", change.ID)
		blocker, err := repository.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('profile-revocation-admission',0))`); err != nil {
			t.Fatal(err)
		}
		type admission struct {
			first bool
			err   error
		}
		finished := make(chan admission, 1)
		go func() {
			first, err := repository.StartProfileSessionRevocation(detached, change.ID)
			finished <- admission{first, err}
		}()
		wallClockWait(t, ctx, fixture.admin, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=$1 AND wait_event='advisory' AND query LIKE '%profile-revocation-admission%')`, repository.pool.Config().ConnConfig.Database)
		clock.Store(claim.RecoveryLeaseUntil.Add(time.Second).UnixNano())
		if err = blocker.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		result := <-finished
		if result.first || !errors.Is(result.err, ErrIdentityRecoveryClaimLost) || before != historyClaimSnapshot(t, repository, "profile_change_requests", change.ID) {
			t.Fatal("expired worker persisted a grant after admission wait", result.first, result.err)
		}
	})
}
