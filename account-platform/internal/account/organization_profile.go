package account

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
	"net/http"
)

// The existing models assembly is the default. The personal assembly explicitly
// closes organization producers before permissions or domain repositories run.
func (s *Service) WithOrganizationsEnabled(enabled bool) *Service {
	s.organizationsDisabled = !enabled
	return s
}

func (s *Service) requireOrganizationsEnabled() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.organizationsDisabled {
			s.writeOrganizationsUnavailable(c.Writer, c.Request)
			c.Abort()
			return
		}
		c.Next()
	}
}

func (s *Service) writeOrganizationsUnavailable(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNAVAILABLE", "organization service is unavailable", httpserver.RequestID(r.Context()))
}
