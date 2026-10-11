package httpserver

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestProcessLoggingRequiresExplicitEnvironment(t *testing.T) {
	for _, environment := range []string{"", " \t\n"} {
		t.Setenv("APP_ENV", environment)
		if err := ConfigureProcessLogging("fixture-service"); err == nil {
			t.Fatal("missing environment silently initialized development logging")
		}
	}
}

func TestProcessGinModeControlsRouteDiagnostics(t *testing.T) {
	originalMode, originalWriter := gin.Mode(), gin.DefaultWriter
	t.Cleanup(func() { gin.SetMode(originalMode); gin.DefaultWriter = originalWriter })
	for _, test := range []struct {
		environment, explicitMode, wantMode string
		wantRouteLog                        bool
	}{
		{"production", "", gin.ReleaseMode, false},
		{"production-blue", "", gin.ReleaseMode, false},
		{"staging", "", gin.ReleaseMode, false},
		{"development", "", gin.DebugMode, true},
		{"test", "", gin.TestMode, false},
		{"development", "release", gin.ReleaseMode, false},
		{"production", "debug", gin.DebugMode, true},
	} {
		t.Run(test.environment+"/"+test.explicitMode, func(t *testing.T) {
			t.Setenv("GIN_MODE", test.explicitMode)
			var output bytes.Buffer
			gin.DefaultWriter = &output
			if err := configureGinMode(test.environment); err != nil {
				t.Fatal(err)
			}
			if gin.Mode() != test.wantMode {
				t.Fatalf("mode = %s, want %s", gin.Mode(), test.wantMode)
			}
			router := NewRouter()
			router.GET("/mode-boundary", AdaptFunc(func(http.ResponseWriter, *http.Request) {}))
			if strings.Contains(output.String(), "/mode-boundary") != test.wantRouteLog {
				t.Fatalf("unexpected route diagnostics: %q", output.String())
			}
		})
	}
	t.Setenv("GIN_MODE", "invalid-mode")
	priorMode := gin.Mode()
	if err := configureGinMode("production"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if gin.Mode() != priorMode {
		t.Fatal("invalid mode changed process configuration")
	}
}
