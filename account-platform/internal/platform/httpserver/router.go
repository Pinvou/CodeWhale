package httpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
)

// NewRouter returns the common Gin router configuration used by every account
// platform HTTP service. Access logging and metrics stay in the outer server
// middleware so they also cover health checks and non-Gin handlers.
func NewRouter() *Router {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	router.RemoveExtraSlash = false
	router.Use(func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				c.Abort()
				if recovered == http.ErrAbortHandler {
					panic(http.ErrAbortHandler)
				}
				if err, ok := recovered.(error); ok {
					if errors.Is(err, http.ErrAbortHandler) {
						panic(http.ErrAbortHandler)
					}
					if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
						if c.Writer.Written() {
							panic(http.ErrAbortHandler)
						}
						_ = c.Error(err)
						return
					}
				}
				slog.ErrorContext(c.Request.Context(), "http handler panic recovered", "error_code", "HTTP_HANDLER_PANIC")
				if c.Writer.Written() {
					panic(http.ErrAbortHandler)
				}
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"code":       "INTERNAL_ERROR",
					"message":    "the request could not be completed",
					"request_id": RequestID(c.Request.Context()),
				})
			}
		}()
		c.Next()
	})
	return &Router{Engine: router}
}

// Router gives HEAD the same route and authentication as its GET handler.
type Router struct{ *gin.Engine }

func (router *Router) GET(relativePath string, handlers ...gin.HandlerFunc) gin.IRoutes {
	router.Engine.GET(relativePath, handlers...)
	return router.Engine.HEAD(relativePath, handlers...)
}

func (router *Router) Group(relativePath string, handlers ...gin.HandlerFunc) *RouteGroup {
	return &RouteGroup{RouterGroup: router.Engine.Group(relativePath, handlers...)}
}

type RouteGroup struct{ *gin.RouterGroup }

func (group *RouteGroup) GET(relativePath string, handlers ...gin.HandlerFunc) gin.IRoutes {
	group.RouterGroup.GET(relativePath, handlers...)
	return group.RouterGroup.HEAD(relativePath, handlers...)
}

func (group *RouteGroup) Group(relativePath string, handlers ...gin.HandlerFunc) *RouteGroup {
	return &RouteGroup{RouterGroup: group.RouterGroup.Group(relativePath, handlers...)}
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
