package billing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProductAdmissionOriginal identifies an immutable, server-owned original.
// The submission contains its digest and metadata, never the document bytes.
type ProductAdmissionOriginal struct {
	DocumentID uuid.UUID `json:"document_id"`
	Version    int64     `json:"version"`
	MediaType  string    `json:"media_type"`
	Size       int64     `json:"size"`
	SHA256     string    `json:"sha256"`
}

// productAdmissionPublication preserves the facts used by catalogFingerprint.
// This readable copy does not replace or change that historical hash format.
type productAdmissionPublication struct {
	CatalogItemID      uuid.UUID                   `json:"catalog_item_id"`
	CatalogVersion     int64                       `json:"catalog_version"`
	ProductID          string                      `json:"product_id"`
	DisplayNameZhCN    string                      `json:"display_name_zh_cn"`
	DisplayNameEN      string                      `json:"display_name_en"`
	DisplayNameJA      string                      `json:"display_name_ja"`
	BasePoints         int64                       `json:"base_points"`
	BonusPoints        int64                       `json:"bonus_points"`
	ValidityDays       int                         `json:"validity_days"`
	InvoiceProductName string                      `json:"invoice_product_name"`
	Mapping            RechargeProductMappingDraft `json:"mapping"`
}

type productAdmissionSubmission struct {
	SchemaVersion      int                         `json:"schema_version"`
	Environment        string                      `json:"environment"`
	PublishFingerprint string                      `json:"publish_fingerprint"`
	Publication        productAdmissionPublication `json:"publication"`
	Original           ProductAdmissionOriginal    `json:"original"`
	ExpiresAt          time.Time                   `json:"expires_at"`
}

// freezeProductAdmissionSubmission receives publication facts loaded under the
// catalog owner lock, the server deployment environment, and a trusted original
// record. Client-provided snapshots or hashes must not be used as authority.
// The caller persists the returned bytes and digest in the same transaction.
func freezeProductAdmissionSubmission(environment string, item RechargeProductCatalogItem, product Product, original ProductAdmissionOriginal, expiresAt, now time.Time) ([]byte, string, error) {
	if environment == "" || environment != strings.TrimSpace(environment) || len(environment) > 128 || strings.ContainsAny(environment, "\r\n\t") || item.CatalogItemID == uuid.Nil || item.Version < 1 || product.ProductID != item.ProductID || item.Mapping.AdmissionClass != "FORMAL" || original.DocumentID == uuid.Nil || original.Version < 1 || original.Size < 1 || original.Size > 10<<20 || !expiresAt.After(now) || expiresAt.Year() > 9999 {
		return nil, "", ErrConflict
	}
	switch original.MediaType {
	case "image/png", "image/jpeg", "application/pdf":
	default:
		return nil, "", ErrConflict
	}
	digest, err := hex.DecodeString(original.SHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != original.SHA256 {
		return nil, "", ErrConflict
	}
	publication := productAdmissionPublication{
		CatalogItemID: item.CatalogItemID, CatalogVersion: item.Version, ProductID: item.ProductID,
		DisplayNameZhCN: item.DisplayNameZhCN, DisplayNameEN: item.DisplayNameEN, DisplayNameJA: item.DisplayNameJA,
		BasePoints: product.BasePoints, BonusPoints: product.BonusPoints, ValidityDays: product.ValidityDays,
		InvoiceProductName: product.InvoiceProductName, Mapping: item.Mapping,
	}
	publication.Mapping.Channels = append([]string(nil), item.Mapping.Channels...)
	facts := productAdmissionSubmission{
		SchemaVersion: 1, Environment: environment, PublishFingerprint: catalogFingerprint(item, product),
		Publication: publication, Original: original, ExpiresAt: expiresAt.UTC(),
	}
	body, err := json.Marshal(facts)
	if err != nil {
		return nil, "", ErrConflict
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:]), nil
}
