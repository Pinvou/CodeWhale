package account

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuditExportValidationIsDefiniteBadRequest(t *testing.T) {
	now := time.Now().UTC()
	for _, reason := range []string{"short", strings.Repeat("界", 501), strings.Repeat("😀", 501), string([]byte{0xff})} {
		t.Run(reasonLabel(reason), func(t *testing.T) {
			owner := &AuditExportService{now: func() time.Time { return now }}
			_, err := owner.Create(context.Background(), identity.Principal{AuthenticatedAt: now}, AuditExportRequest{Reason: reason, IdempotencyKey: "controlled-key", Filter: AuditExportFilter{From: now.Add(-time.Minute), To: now}})
			if err == nil {
				t.Fatal("invalid request accepted")
			}
			w := httptest.NewRecorder()
			(&Service{}).writeError(w, httptest.NewRequest(http.MethodPost, "/admin/v1/audit-exports", nil), err)
			if w.Code != 400 {
				t.Fatalf("invalid request status=%d want400", w.Code)
			}
		})
	}
}
func reasonLabel(s string) string {
	if s == "short" {
		return "short"
	}
	if s[0] == 0xff {
		return "invalid-utf8"
	}
	if strings.HasPrefix(s, "😀") {
		return "emoji501"
	}
	return "unicode501"
}
func TestAuditExportErrorHTTPClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{{"quota", ErrAuditExportQuota, 429}, {"true-conflict", ErrConflict, 409}, {"forbidden", ErrForbidden, 403}} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Service{}).writeError(w, httptest.NewRequest(http.MethodPost, "/admin/v1/audit-exports", nil), tc.err)
			if w.Code != tc.want {
				t.Fatalf("status=%d want%d", w.Code, tc.want)
			}
		})
	}
}
