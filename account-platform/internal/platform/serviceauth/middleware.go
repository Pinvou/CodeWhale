package serviceauth

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/gin-gonic/gin"
)

func Middleware(expectedToken string, next http.Handler) http.Handler {
	expectedToken = strings.TrimSpace(expectedToken)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Fields(r.Header.Get("Authorization"))
		valid := expectedToken != "" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && len(parts[1]) == len(expectedToken)
		if valid {
			valid = subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedToken)) == 1
		}
		if !valid {
			api.WriteError(w, http.StatusUnauthorized, "SERVICE_AUTH_REQUIRED", "valid service authentication is required", httpserver.RequestID(r.Context()))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func GinMiddleware(expectedToken string) gin.HandlerFunc {
	expectedToken = strings.TrimSpace(expectedToken)
	return func(c *gin.Context) {
		parts := strings.Fields(c.Request.Header.Get("Authorization"))
		valid := expectedToken != "" && len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && len(parts[1]) == len(expectedToken)
		if valid {
			valid = subtle.ConstantTimeCompare([]byte(parts[1]), []byte(expectedToken)) == 1
		}
		if !valid {
			api.WriteError(c.Writer, http.StatusUnauthorized, "SERVICE_AUTH_REQUIRED", "valid service authentication is required", httpserver.RequestID(c.Request.Context()))
			c.Abort()
			return
		}
		c.Next()
	}
}
