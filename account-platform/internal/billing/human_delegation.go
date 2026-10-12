package billing

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const humanDelegationHeader = "X-Pinvou-Human-Delegation"

func (s *Service) requireHumanDelegation(permission, resourceType, resourceParameter string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.humanDelegations == nil {
			api.WriteError(c.Writer, http.StatusServiceUnavailable, "HUMAN_DELEGATION_UNAVAILABLE", "human delegation verification is unavailable", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20))
		if err != nil {
			api.WriteError(c.Writer, http.StatusBadRequest, "INVALID_REQUEST", "invalid delegated request", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		var organizationID *uuid.UUID
		if raw := strings.TrimSpace(c.Param("organization_id")); raw != "" {
			parsed, parseErr := uuid.Parse(raw)
			if parseErr != nil {
				api.WriteError(c.Writer, http.StatusBadRequest, "INVALID_REQUEST", "invalid delegated target", httpserver.RequestID(c.Request.Context()))
				c.Abort()
				return
			}
			organizationID = &parsed
		}
		resourceID := strings.TrimSpace(c.Param(resourceParameter))
		claims, err := s.humanDelegations.Verify(c.GetHeader(humanDelegationHeader), humandelegation.Expectation{
			Audience: "billing-service", Permission: permission, OrganizationID: organizationID,
			ResourceType: resourceType, ResourceID: resourceID, HTTPMethod: c.Request.Method,
			HTTPPath: c.Request.URL.Path, RequestSHA256: humandelegation.RequestDigest(body),
		})
		if err != nil {
			api.WriteError(c.Writer, http.StatusForbidden, "HUMAN_DELEGATION_REQUIRED", "a valid human operation delegation is required", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Request = c.Request.WithContext(humandelegation.WithClaims(c.Request.Context(), claims))
		c.Next()
	}
}

type delegatedAuditFacts struct {
	WorkloadService      string
	WorkloadSubject      string
	DelegationID         uuid.UUID
	Permission           string
	AuthorizationVersion int64
}

func delegatedAudit(ctx context.Context) *delegatedAuditFacts {
	workload, workloadOK := serviceauth.FromContext(ctx)
	delegation, delegationOK := humandelegation.FromContext(ctx)
	if !workloadOK || !delegationOK || workload.Service == "" || workload.Subject == "" || delegation.DelegationID == uuid.Nil {
		return nil
	}
	return &delegatedAuditFacts{WorkloadService: workload.Service, WorkloadSubject: workload.Subject, DelegationID: delegation.DelegationID, Permission: delegation.Permission, AuthorizationVersion: delegation.AuthorizationVersion}
}
