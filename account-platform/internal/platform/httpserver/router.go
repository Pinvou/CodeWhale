package httpserver

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// NewRouter returns the common Gin router configuration used by every account
// platform HTTP service. Access logging and metrics stay in the outer server
// middleware so they also cover health checks and non-Gin handlers.
func NewRouter() *gin.Engine {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	router.Use(gin.CustomRecovery(func(c *gin.Context, recovered any) {
		slog.Error("http handler panic recovered", "panic", fmt.Sprint(recovered), "request_id", RequestID(c.Request.Context()))
		c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
			"code":       "INTERNAL_ERROR",
			"message":    "the request could not be completed",
			"request_id": RequestID(c.Request.Context()),
		})
	}))
	return router
}

// Adapt keeps domain handlers expressed in standard net/http types while Gin
// owns routing, route groups, recovery and middleware execution. It also maps
// Gin parameters back to Request.PathValue so domain code is framework-neutral.
func Adapt(handler http.Handler) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, parameter := range c.Params {
			c.Request.SetPathValue(parameter.Key, strings.TrimPrefix(parameter.Value, "/"))
		}
		c.Request.Pattern = routePattern(c.Request.Method, c.FullPath())
		handler.ServeHTTP(c.Writer, c.Request)
	}
}

func AdaptFunc(handler http.HandlerFunc) gin.HandlerFunc {
	return Adapt(handler)
}

func routePattern(method, path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		switch {
		case strings.HasPrefix(segment, ":"):
			segments[index] = "{" + strings.TrimPrefix(segment, ":") + "}"
		case strings.HasPrefix(segment, "*"):
			segments[index] = "{" + strings.TrimPrefix(segment, "*") + "...}"
		}
	}
	return strings.TrimSpace(method + " " + strings.Join(segments, "/"))
}
