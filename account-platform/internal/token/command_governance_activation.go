package token

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

var ErrTokenRouteActivationChanged = errors.New("token route governance activation changed during mutation")

// TokenCommandGovernanceActivation is the narrow read model that APT-138
// will back with its single persistent activation record. Until then no
// production reader is installed, which deliberately keeps new write paths
// unreachable even though their handlers and policies are present.
type TokenCommandGovernanceActivation struct {
	State string
	Epoch int64
}

type TokenCommandGovernanceActivationReader interface {
	CurrentTokenCommandGovernanceActivation(context.Context) (TokenCommandGovernanceActivation, error)
}

// TokenRouteActivationTransactionGuard is the transaction seam APT-138 will
// implement with its persistent activation row. It must lock and compare the
// selected state and epoch inside the route mutation transaction.
type TokenRouteActivationTransactionGuard interface {
	LockAndCheckTokenRouteActivation(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error
}

type tokenRouteActivationContextKey struct{}

type tokenRouteActivationSelection struct {
	TokenCommandGovernanceActivation
	transactional bool
}

func selectTokenRouteActivation(ctx *gin.Context, activation TokenCommandGovernanceActivation, transactional bool) {
	selection := tokenRouteActivationSelection{TokenCommandGovernanceActivation: activation, transactional: transactional}
	ctx.Request = ctx.Request.WithContext(context.WithValue(ctx.Request.Context(), tokenRouteActivationContextKey{}, selection))
}

func (r *PostgresRepository) WithTokenRouteActivationTransactionGuard(guard TokenRouteActivationTransactionGuard) *PostgresRepository {
	r.routeActivationGuard = guard
	return r
}

func (r *PostgresRepository) recheckTokenRouteActivation(ctx context.Context, tx pgx.Tx) error {
	selection, ok := ctx.Value(tokenRouteActivationContextKey{}).(tokenRouteActivationSelection)
	if !ok || !selection.transactional {
		return nil
	}
	if r.routeActivationGuard == nil {
		return ErrTokenRouteActivationChanged
	}
	return r.routeActivationGuard.LockAndCheckTokenRouteActivation(ctx, tx, selection.TokenCommandGovernanceActivation)
}

func (s *Service) WithTokenCommandGovernanceActivation(reader TokenCommandGovernanceActivationReader) *Service {
	s.commandGovernanceActivation = reader
	return s
}

func (s *Service) requireNewTokenCommandGovernanceActive() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if s.commandGovernanceActivation == nil {
			writeTokenCommandGovernanceInactive(ctx)
			return
		}
		activation, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx.Request.Context())
		if err != nil || activation.State != "ACTIVE" || activation.Epoch < 1 {
			writeTokenCommandGovernanceInactive(ctx)
			return
		}
		selectTokenRouteActivation(ctx, activation, true)
		ctx.Next()
	}
}

// rejectProtocolFormulaDirectWrite keeps the resource-specific APT-122 URLs
// as compatibility sentinels only. PRE/SAFE fail closed in the activation
// middleware and ACTIVE directs every human mutation through /commands, so a
// second audit/result path cannot become reachable.
func rejectProtocolFormulaDirectWrite() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		api.WriteError(ctx.Writer, http.StatusGone, "DIRECT_TOKEN_COMMAND_RETIRED", "use the governed Token command endpoint", httpserver.RequestID(ctx.Request.Context()))
		ctx.Abort()
	}
}

func writeTokenCommandGovernanceInactive(ctx *gin.Context) {
	api.WriteError(ctx.Writer, http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "Token command governance is not active", httpserver.RequestID(ctx.Request.Context()))
	ctx.Abort()
}

// selectLegacyTokenDirect keeps the immutable v1 path selected while no
// activation reader is installed. Once APT-138 installs the persistent reader,
// ACTIVE rejects human direct mutation and any unreadable state fails closed.
func (s *Service) selectLegacyTokenDirect() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if s.commandGovernanceActivation == nil {
			ctx.Next()
			return
		}
		activation, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx.Request.Context())
		if err != nil {
			writeTokenCommandGovernanceInactive(ctx)
			return
		}
		if activation.State == "ACTIVE" && activation.Epoch > 0 {
			api.WriteError(ctx.Writer, http.StatusForbidden, "DIRECT_APPROVAL_COMMAND_FORBIDDEN", "submit this command through the approval workflow", httpserver.RequestID(ctx.Request.Context()))
			ctx.Abort()
			return
		}
		if activation.State == "PRE_ACTIVATION" && activation.Epoch == 0 {
			ctx.Next()
			return
		}
		// SAFE_ROLLBACK and malformed/mixed-version states stop all new writes;
		// they must never revive the historical direct branch.
		writeTokenCommandGovernanceInactive(ctx)
	}
}

// selectLegacyTokenRouteDirect keeps the v1 text/health contract available
// during PRE_ACTIVATION. The atomic APT-138 transition retires it before v2
// stable writes can be selected, so no mixed write contract is reachable.
func (s *Service) selectLegacyTokenRouteDirect() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		if s.commandGovernanceActivation == nil {
			selectTokenRouteActivation(ctx, TokenCommandGovernanceActivation{State: "PRE_ACTIVATION", Epoch: 0}, false)
			ctx.Next()
			return
		}
		activation, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(ctx.Request.Context())
		if err != nil {
			writeTokenCommandGovernanceInactive(ctx)
			return
		}
		if activation.State == "PRE_ACTIVATION" && activation.Epoch == 0 {
			selectTokenRouteActivation(ctx, activation, true)
			ctx.Next()
			return
		}
		if activation.State == "ACTIVE" && activation.Epoch > 0 {
			api.WriteError(ctx.Writer, http.StatusGone, "LEGACY_TOKEN_ROUTE_CONTRACT_RETIRED", "use the stable route API", httpserver.RequestID(ctx.Request.Context()))
			ctx.Abort()
			return
		}
		writeTokenCommandGovernanceInactive(ctx)
	}
}
