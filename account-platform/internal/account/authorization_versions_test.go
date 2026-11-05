package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/gin-gonic/gin"
)

type versionFallbackRepository struct {
	personalBoundaryRepository
	reads int
	err   error
}

func (r *versionFallbackRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	r.reads++
	return Me{ActiveContext: Context{ContextVersion: 2, AuthorizationVersion: 3, PersonalAuthorizationVersion: 4, PlatformAuthorizationVersion: 5, OrganizationAuthorizationVersion: 6}}, r.err
}

type versionNarrowRepository struct {
	versionFallbackRepository
	narrowReads int
}

func (r *versionNarrowRepository) GetAuthorizationVersions(context.Context, identity.Principal) (AuthorizationVersions, error) {
	r.narrowReads++
	return AuthorizationVersions{2, 3, 4, 5, 6}, r.err
}

func TestAuthorizationVersionHeadersPreserveFallbackAndErrorBoundary(t *testing.T) {
	for _, narrow := range []bool{false, true} {
		for _, failed := range []bool{false, true} {
			t.Run(map[bool]string{true: "narrow", false: "fallback"}[narrow]+map[bool]string{true: "-error", false: "-success"}[failed], func(t *testing.T) {
				base := &versionFallbackRepository{}
				if failed {
					base.err = errors.New("controlled version read failure")
				}
				var repository Repository = base
				n := &versionNarrowRepository{versionFallbackRepository: *base}
				if narrow {
					repository = n
				}
				router := gin.New()
				router.Use(identity.GinMiddleware(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "controlled", Subject: "subject", SessionID: "session"}, nil
				}), (&Service{repository: repository}).contextVersionHeaders())
				router.GET("/read", func(c *gin.Context) { c.Status(http.StatusNoContent) })
				req := httptest.NewRequest(http.MethodGet, "/read", nil)
				req.Header.Set("Authorization", "Bearer controlled")
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				if out.Code != http.StatusNoContent {
					t.Fatal("version helper changed handler behavior", out.Code)
				}
				for key, want := range map[string]string{"X-Context-Version": "2", "X-Authorization-Version": "3", "X-Personal-Authorization-Version": "4", "X-Platform-Authorization-Version": "5", "X-Organization-Authorization-Version": "6"} {
					if failed {
						want = ""
					}
					if out.Header().Get(key) != want {
						t.Fatal("incorrect version header", key)
					}
				}
				if narrow && (n.narrowReads != 1 || n.reads != 0) {
					t.Fatal("narrow path read full profile")
				}
				if !narrow && base.reads != 1 {
					t.Fatal("fallback not preserved")
				}
			})
		}
	}
}
