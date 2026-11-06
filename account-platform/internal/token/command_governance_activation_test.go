package token

import (
	"context"

	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

type tokenCommandActivationReaderFunc func(context.Context) (TokenCommandGovernanceActivation, error)

func (read tokenCommandActivationReaderFunc) CurrentTokenCommandGovernanceActivation(ctx context.Context) (TokenCommandGovernanceActivation, error) {
	return read(ctx)
}

func TestNewTokenCommandGovernanceFailsClosedUntilActive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name   string
		reader TokenCommandGovernanceActivationReader
		status int
		called int
	}{
		{name: "record absent", status: http.StatusServiceUnavailable},
		{name: "reader failure", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{}, errors.New("activation unavailable")
		}), status: http.StatusServiceUnavailable},
		{name: "pre activation", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{State: "PRE_ACTIVATION", Epoch: 1}, nil
		}), status: http.StatusServiceUnavailable},
		{name: "active without epoch", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{State: "ACTIVE"}, nil
		}), status: http.StatusServiceUnavailable},
		{name: "active", reader: tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
			return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}, nil
		}), status: http.StatusNoContent, called: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{commandGovernanceActivation: test.reader}
			router := gin.New()
			called := 0
			router.POST("/command", service.requireNewTokenCommandGovernanceActive(), func(ctx *gin.Context) {
				called++
				ctx.Status(http.StatusNoContent)
			})
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/command", nil)
			router.ServeHTTP(response, request)
			if response.Code != test.status || called != test.called {
				t.Fatalf("status=%d called=%d, want status=%d called=%d; body=%s", response.Code, called, test.status, test.called, response.Body.String())
			}
			if test.status == http.StatusServiceUnavailable && response.Body.String() == "" {
				t.Fatal("fail-closed response must include the stable error envelope")
			}
		})
	}
}
