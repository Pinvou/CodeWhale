package token

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
)

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
		ctx.Next()
	}
}

func writeTokenCommandGovernanceInactive(ctx *gin.Context) {
	api.WriteError(ctx.Writer, http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "Token command governance is not active", httpserver.RequestID(ctx.Request.Context()))
	ctx.Abort()
}
