package token

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/jackc/pgx/v5"
)

type accountCredentialAuthorityGuard struct {
	activation *TokenGovernanceStore
	authorizer *authorization.Client
}

type credentialEntryDeadlineKey struct{}

func credentialEntryDeadline(ctx context.Context) time.Time {
	deadline, ok := ctx.Value(credentialEntryDeadlineKey{}).(time.Time)
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	if earlier, ok := ctx.Deadline(); ok && earlier.Before(deadline) {
		deadline = earlier
	}
	return deadline
}

func NewCredentialIsolationAuthorityGuard(activation *TokenGovernanceStore, authorizer *authorization.Client) CredentialIsolationAuthorityGuard {
	return &accountCredentialAuthorityGuard{activation: activation, authorizer: authorizer}
}

// CredentialAuthorityDeadlineMiddleware is installed at the credential origin
// request boundary, and for Token credential routes before authentication or
// any row-lock acquisition. Every guard and downstream request uses this one
// deadline; a slow earlier stage never buys another full five seconds.
func CredentialAuthorityDeadlineMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline := credentialEntryDeadline(r.Context())
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Token's streaming/model routes keep their existing budgets. Only isolated
// credential URLs receive the credential request-entry absolute deadline.
func TokenCredentialDeadlineMiddleware(next http.Handler) http.Handler {
	bounded := CredentialAuthorityDeadlineMiddleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/admin/v1/commands" {
			// Only the deadline value is frozen before body classification; other
			// command types retain their existing request budgets.
			r = r.WithContext(context.WithValue(r.Context(), credentialEntryDeadlineKey{}, credentialEntryDeadline(r.Context())))
		}
		if (strings.HasPrefix(r.URL.Path, "/admin/v1/") && strings.Contains(r.URL.Path, "/credential")) || strings.HasPrefix(r.URL.Path, "/internal/v1/commands/credential-revoke/") {
			bounded.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g *accountCredentialAuthorityGuard) LockAndCheckCredentialIsolationAuthority(ctx context.Context, tx pgx.Tx, snapshot CredentialIsolationAuthoritySnapshot) error {
	deadline, bounded := ctx.Deadline()
	if g == nil || g.activation == nil || g.authorizer == nil || !bounded || !deadline.After(time.Now()) || ctx.Err() != nil {
		return ErrTokenRouteActivationChanged
	}
	selected := TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: snapshot.GateEpoch}
	if err := g.activation.LockAndCheckTokenRouteActivation(ctx, tx, selected); err != nil {
		return err
	}
	principal := identity.Principal{Issuer: snapshot.Issuer, Subject: snapshot.Subject}
	permission := snapshot.RequiredPermission
	if permission == "" {
		permission = "platform.tokens.manage"
	}
	if permission != "platform.tokens.manage" && permission != "platform.tokens.credentials.validate" {
		return ErrTokenRouteActivationChanged
	}
	actor, version, err := g.authorizer.AuthorizedUserIDAll(ctx, principal, permission)
	if err != nil || actor != snapshot.ActorID || version != snapshot.AuthorizationVersion || ctx.Err() != nil {
		return ErrTokenRouteActivationChanged
	}
	return g.activation.LockAndCheckTokenRouteActivation(ctx, tx, selected)
}
