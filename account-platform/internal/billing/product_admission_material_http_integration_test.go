package billing

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/evidenceenvelope"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This controlled KEK implements actual authenticated wrapping; it is not an
// OpenBao acceptance test. File AEAD and Service/SQL paths are production.
type materialHTTPKeys struct {
	key       []byte
	afterOpen func()
}

func (k *materialHTTPKeys) KeyID() string { return "controlled-key" }
func (k *materialHTTPKeys) EncryptWithKeyIDContext(ctx context.Context, id string, plain, aad []byte) ([]byte, error) {
	if ctx.Err() != nil || id != k.KeyID() {
		return nil, errors.New("unavailable")
	}
	b, _ := aes.NewCipher(k.key)
	g, _ := cipher.NewGCM(b)
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, plain, aad), nil
}
func (k *materialHTTPKeys) DecryptWithKeyIDContext(ctx context.Context, id string, body, aad []byte) ([]byte, error) {
	if ctx.Err() != nil || id != k.KeyID() {
		return nil, errors.New("unavailable")
	}
	b, _ := aes.NewCipher(k.key)
	g, _ := cipher.NewGCM(b)
	if len(body) < g.NonceSize() {
		return nil, errors.New("invalid")
	}
	out, err := g.Open(nil, body[:g.NonceSize()], body[g.NonceSize():], aad)
	if err == nil && k.afterOpen != nil {
		k.afterOpen()
	}
	return out, err
}

type materialHTTPValidator struct {
	calls  int
	reject bool
}

type materialHTTPRecorder struct {
	*httptest.ResponseRecorder
	deadline time.Time
}

func (w *materialHTTPRecorder) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (v *materialHTTPValidator) Validate(ctx context.Context, media string, body []byte) error {
	v.calls++
	deadline, bounded := ctx.Deadline()
	if remaining := time.Until(deadline); !bounded || remaining < 55*time.Second || remaining > 60*time.Second {
		return errors.New("upload validation did not inherit the processing budget")
	}
	if v.reject {
		return errImageValidatorInvalid
	}
	if media != "image/png" || len(body) == 0 {
		return errImageValidatorInvalid
	}
	return nil
}
func TestProductAdmissionMaterialHTTPPostgres(t *testing.T) {
	_ = tokenWalletWorkload(t)
	r, ctx := retainedFinanceRepository(t)
	item, _ := catalogAtomicFixture(t, r, ctx, "delete-draft")
	actor := uuid.New()
	var allowed atomic.Bool
	allowed.Store(true)
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct{ Subject, Permission string }
		if req.URL.Path != "/internal/v1/authorization/check" || json.NewDecoder(req.Body).Decode(&body) != nil {
			t.Error("authority contract")
			w.WriteHeader(400)
			return
		}
		ok := allowed.Load() && body.Subject != "denied-subject" && body.Permission == "platform.products.publish"
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": ok, "user_id": actor})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	keys := &materialHTTPKeys{key: bytes.Repeat([]byte{7}, 32)}
	validator := &materialHTTPValidator{}
	principal := func(_ context.Context, token string) (identity.Principal, error) {
		when := time.Now()
		if token == "stale-subject" {
			when = when.Add(-16 * time.Minute)
		}
		return identity.Principal{Issuer: "controlled", Subject: token, AuthenticatedAt: when}, nil
	}
	service := NewService(r).WithRechargeProductCatalog(r).WithAuthorizer(authorizer).WithProductAdmissionMaterials(r, keys, validator)
	handler := httpserver.HandlerWithApplication(config.Config{ServiceName: "billing-service", Environment: "test"}, service.Handler("controlled-service", principal), nil)
	base := "/admin/v1/recharge-products/" + item.CatalogItemID.String()
	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	upload := base + "/admission-materials?expected_version=" + fmt.Sprint(item.Version) + "&intended_expires_at=" + url.QueryEscape(expiry.Format(time.RFC3339Nano))
	call := func(h http.Handler, method, path, token, key string, body []byte) *materialHTTPRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "image/png")
		req.Header.Set("Idempotency-Key", key)
		out := &materialHTTPRecorder{ResponseRecorder: httptest.NewRecorder()}
		h.ServeHTTP(out, req)
		return out
	}
	key := uuid.NewString()
	original := []byte("controlled-original-private-bytes")
	for _, test := range []struct {
		token string
		code  int
	}{{"", 401}, {"denied-subject", 403}, {"stale-subject", 401}} {
		out := call(handler, "POST", upload, test.token, key, original)
		if out.Code != test.code || !out.deadline.IsZero() {
			t.Fatal(test.token, out.Code, out.Body.String())
		}
	}
	if validator.calls != 0 {
		t.Fatal("denied request reached validator")
	}
	started := time.Now()
	out := call(handler, "POST", upload, "external-oidc-subject-not-uuid", key, original)
	if out.Code != 200 {
		t.Fatal(out.Code, out.Body.String())
	}
	if out.deadline.Before(started.Add(65*time.Second)) || out.deadline.After(time.Now().Add(65*time.Second)) {
		t.Fatalf("upload response deadline = %v", out.deadline)
	}
	var m ProductAdmissionMaterial
	if err = json.Unmarshal(out.Body.Bytes(), &m); err != nil || m.UploadedBy != actor {
		t.Fatal("canonical Account user not used", m, err)
	}
	if m.SHA256 == "" || bytes.Contains(out.Body.Bytes(), original) {
		t.Fatal("invalid metadata or original leakage")
	}
	out = call(handler, "POST", upload, "external-oidc-subject-not-uuid", key, original)
	var replay ProductAdmissionMaterial
	_ = json.Unmarshal(out.Body.Bytes(), &replay)
	if out.Code != 200 || replay.MaterialID != m.MaterialID {
		t.Fatal("stable upload replay", out.Code, out.Body.String())
	}
	out = call(handler, "POST", upload, "external-oidc-subject-not-uuid", key, []byte("different"))
	if out.Code != 409 {
		t.Fatal("changed upload facts", out.Code, out.Body.String())
	}
	path := base + "/admission-materials/" + m.MaterialID.String()
	out = call(handler, "GET", path, "external-oidc-subject-not-uuid", "", nil)
	if out.Code != 200 || bytes.Contains(out.Body.Bytes(), original) || !out.deadline.IsZero() {
		t.Fatal("metadata", out.Code, out.Body.String())
	}
	out = call(handler, "GET", base+"/admission-material-receipts/"+key, "external-oidc-subject-not-uuid", "", nil)
	if out.Code != 200 {
		t.Fatal("receipt", out.Code, out.Body.String())
	}
	out = call(handler, "GET", path+"/content", "external-oidc-subject-not-uuid", "", nil)
	if out.Code != 200 || !bytes.Equal(out.Body.Bytes(), original) || out.Header().Get("Cache-Control") != "no-store" || out.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(out.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("original download", out.Code, out.Header())
	}
	var readsBefore int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_material_audits WHERE material_id=$1 AND action='READ'`, m.MaterialID).Scan(&readsBefore); err != nil || readsBefore != 1 {
		t.Fatal(readsBefore, err)
	}
	keys.afterOpen = func() { allowed.Store(false) }
	out = call(handler, "GET", path+"/content", "external-oidc-subject-not-uuid", "", nil)
	if out.Code != 403 || bytes.Contains(out.Body.Bytes(), original) {
		t.Fatal("postdecrypt revocation disclosed", out.Code, out.Body.String())
	}
	var readsAfter int
	if err = r.pool.QueryRow(ctx, `SELECT count(*) FROM product_admission_material_audits WHERE material_id=$1 AND action='READ'`, m.MaterialID).Scan(&readsAfter); err != nil || readsAfter != readsBefore {
		t.Fatal("failed disclosure audited as read", readsAfter, err)
	}
	allowed.Store(true)
	keys.afterOpen = nil
	before := validator.calls
	out = call(handler, "POST", upload, "external-oidc-subject-not-uuid", uuid.NewString(), make([]byte, evidenceenvelope.MaxPlaintextBytes+1))
	if out.Code != 413 || validator.calls != before {
		t.Fatal("raw body bound", out.Code, validator.calls)
	}
	validator.reject = true
	out = call(handler, "POST", upload, "external-oidc-subject-not-uuid", uuid.NewString(), original)
	if out.Code != 422 {
		t.Fatal("validator failure", out.Code, out.Body.String())
	}
	disabled := NewService(r).WithRechargeProductCatalog(r).WithAuthorizer(authorizer).Handler("controlled-service", principal)
	out = call(disabled, "POST", upload, "external-oidc-subject-not-uuid", uuid.NewString(), original)
	if out.Code != 404 {
		t.Fatal("disabled facility", out.Code, out.Body.String())
	}
}
