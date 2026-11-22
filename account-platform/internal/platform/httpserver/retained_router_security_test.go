package httpserver

import (
	"bytes"
	"github.com/gin-gonic/gin"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestGETAndHEADShareGroupedAuthenticationAndParameters(t *testing.T) {
	router := NewRouter()
	group := router.Group("/private")
	group.Use(func(c *gin.Context) {
		if c.Request.Header.Get("Authorization") != "fixture-authorized" {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	})
	group.GET("/items/:id", AdaptFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "42" {
			t.Fatal("HEAD lost the route parameter")
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			t.Fatal("unexpected method rewrite")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	router.POST("/mutation", AdaptFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("HEAD invoked a mutation") }))
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, authorized := range []bool{false, true} {
			request := httptest.NewRequest(method, "/private/items/42", nil)
			if authorized {
				request.Header.Set("Authorization", "fixture-authorized")
			}
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			want := http.StatusUnauthorized
			if authorized {
				want = http.StatusNoContent
			}
			if response.Code != want {
				t.Fatal("HEAD did not preserve GET authentication")
			}
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodHead, "/mutation", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatal("non-GET route gained HEAD")
	}
}

func TestRouterRecoveryDoesNotLogPrivateRequestOrPanicValues(t *testing.T) {
	originalMode, originalErrorWriter, originalWriter := gin.Mode(), gin.DefaultErrorWriter, gin.DefaultWriter
	originalLogger := slog.Default()
	t.Cleanup(func() {
		gin.SetMode(originalMode)
		gin.DefaultErrorWriter, gin.DefaultWriter = originalErrorWriter, originalWriter
		slog.SetDefault(originalLogger)
	})
	for _, mode := range []string{gin.DebugMode, gin.ReleaseMode} {
		for _, brokenPipe := range []bool{false, true} {
			name := mode + "/panic"
			if brokenPipe {
				name = mode + "/broken-pipe"
			}
			t.Run(name, func(t *testing.T) {
				var ginLog, structuredLog bytes.Buffer
				gin.SetMode(mode)
				gin.DefaultErrorWriter, gin.DefaultWriter = &ginLog, &ginLog
				slog.SetDefault(slog.New(slog.NewJSONHandler(&structuredLog, nil)))
				router := NewRouter()
				router.GET("/recovery", AdaptFunc(func(http.ResponseWriter, *http.Request) {
					if brokenPipe {
						panic(&net.OpError{Op: "write", Net: "tcp", Err: &os.SyscallError{Syscall: "fixture-panic-marker", Err: syscall.EPIPE}})
					}
					panic("fixture-panic-marker")
				}))
				request := httptest.NewRequest(http.MethodGet, "/recovery?fixture=fixture-query-marker", nil)
				request.Header.Set("Cookie", "fixture=fixture-cookie-marker")
				request.Header.Set("X-CSRF-Token", "fixture-csrf-marker")
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				if !brokenPipe && response.Code != http.StatusInternalServerError {
					t.Fatal("ordinary panic did not return 500")
				}
				if brokenPipe && response.Body.Len() != 0 {
					t.Fatal("broken connection attempted to send a response body")
				}
				for _, marker := range []string{"fixture-query-marker", "fixture-cookie-marker", "fixture-csrf-marker", "fixture-panic-marker"} {
					if strings.Contains(ginLog.String(), marker) || strings.Contains(structuredLog.String(), marker) || strings.Contains(response.Body.String(), marker) {
						t.Fatal("request or panic marker escaped through recovery logging or response")
					}
				}
				if !brokenPipe && !strings.Contains(structuredLog.String(), "http handler panic recovered") {
					t.Fatal("safe recovery observation was lost")
				}
			})
		}
	}
}
