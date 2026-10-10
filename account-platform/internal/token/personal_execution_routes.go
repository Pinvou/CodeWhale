package token

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

// Invocation and public usage are registered only with actual execution dependencies.
func (s *Service) registerPersonalExecutionRoutes(router *httpserver.Router, protected *httpserver.RouteGroup) {
	if !s.personalExecutionEnabled {
		return
	}
	protected.GET("/models", httpserver.AdaptFunc(s.listCatalogModels))
	protected.GET("/model-protocols", httpserver.AdaptFunc(s.modelProtocols))
	protected.POST("/chat/completions", httpserver.AdaptFunc(s.chatCompletions))
	protected.GET("/usage-requests/by-idempotency-key", httpserver.AdaptFunc(s.getPersonalUsageByIdempotencyKey))
	protected.GET("/usage-requests/:usage_request_id", httpserver.AdaptFunc(s.getUsageRequest))
	protected.POST("/usage-requests/:usage_request_id/cancel", httpserver.AdaptFunc(s.cancelUsageRequest))
	protected.GET("/me/usage-requests", httpserver.AdaptFunc(s.listMyUsageRequests))
	protected.GET("/me/usage-turns", httpserver.AdaptFunc(s.listMyUsageTurns))
	router.GET("/v1/client-compatibility", httpserver.AdaptFunc(s.clientCompatibility))
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))
	internal.GET("/client-readiness", serviceauth.Require("internal.token.client-readiness.read"), httpserver.AdaptFunc(s.clientReadinessStatus))
	internal.POST("/client-readiness/attestations", serviceauth.Require("internal.token.client-readiness.attest"), httpserver.AdaptFunc(s.issueClientReadinessAttestation))
}
