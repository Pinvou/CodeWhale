package portal

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

const productAdmissionMaterialLimit int64 = 10 << 20

// Keep the ordinary API at 15s. The original upload includes Billing's
// bounded validation, Transit encryption and persistence after body ingress.
const productAdmissionMaterialUpstreamTimeout = 70 * time.Second
const productAdmissionMaterialWriteTimeout = 105 * time.Second

func productAdmissionMaterialPathAllowed(method, resource string) bool {
	parts := strings.Split(strings.TrimPrefix(resource, "/"), "/")
	if len(parts) < 5 || parts[0] != "admin" || parts[1] != "v1" || parts[2] != "recharge-products" || !pathUUID(parts[3]) {
		return false
	}
	read := method == http.MethodGet || method == http.MethodHead
	if parts[4] == "admission-material-receipts" {
		return len(parts) == 6 && read && safeMaterialOperationKey(parts[5])
	}
	if parts[4] != "admission-materials" {
		return false
	}
	if len(parts) == 5 {
		return method == http.MethodPost
	}
	if !pathUUID(parts[5]) {
		return false
	}
	return len(parts) == 6 && read || len(parts) == 7 && (parts[6] == "content" && read || parts[6] == "legal-hold" && method == http.MethodPost)
}

func safeMaterialOperationKey(key string) bool {
	if len(key) < 16 || len(key) > 128 {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func productAdmissionMaterialUpload(method, resource string) bool {
	return method == http.MethodPost && strings.HasSuffix(resource, "/admission-materials") && productAdmissionMaterialPathAllowed(method, resource)
}

func productAdmissionMaterialContent(method, resource string) bool {
	return (method == http.MethodGet || method == http.MethodHead) && strings.HasSuffix(resource, "/content") && productAdmissionMaterialPathAllowed(method, resource)
}

// This bounded attachment is reusable, unlike the one-use large audit stream.
// Validate the complete response before disclosing any original bytes.
func forwardProductAdmissionOriginal(w http.ResponseWriter, r *http.Request, response *http.Response) {
	media, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	extension := map[string]string{"image/png": "png", "image/jpeg": "jpg", "application/pdf": "pdf"}[media]
	length := response.ContentLength
	if err != nil || len(params) != 0 || extension == "" || length < 1 || length > productAdmissionMaterialLimit {
		api.WriteError(w, http.StatusBadGateway, "ADMISSION_MATERIAL_UNAVAILABLE", "product admission original is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body []byte
	if r.Method != http.MethodHead {
		body, err = io.ReadAll(io.LimitReader(response.Body, productAdmissionMaterialLimit+1))
		defer clear(body)
		if err != nil || int64(len(body)) != length {
			api.WriteError(w, http.StatusBadGateway, "ADMISSION_MATERIAL_UNAVAILABLE", "product admission original is unavailable", httpserver.RequestID(r.Context()))
			return
		}
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="product-admission.`+extension+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}
