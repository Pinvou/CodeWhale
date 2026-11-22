package token

import (
	"encoding/json"

	"github.com/google/uuid"
)

type DispatchEnvelope struct {
	// Local acquisition identity only; it cannot be supplied by stored/provider JSON.
	leaseOwner                          string
	BaseURL                             string          `json:"base_url"`
	Provider                            string          `json:"provider"`
	AdapterVersion                      string          `json:"adapter_version"`
	Credential                          []byte          `json:"credential"`
	RequestBody                         json.RawMessage `json:"request_body"`
	Price                               PriceVersion    `json:"price"`
	Stream                              bool            `json:"stream"`
	MaximumCost                         int64           `json:"maximum_cost"`
	MaximumOutput                       int64           `json:"maximum_output"`
	ContextType                         string          `json:"context_type,omitempty"`
	OrganizationID                      *uuid.UUID      `json:"organization_id,omitempty"`
	CanPurchaseOrganizationEntitlements bool            `json:"can_purchase_organization_entitlements,omitempty"`
}
