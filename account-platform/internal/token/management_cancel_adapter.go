package token

import ()

type ProviderCancelCapability string

// The currently supported OpenAI-compatible and New API adapters do not expose a
// provider-guaranteed idempotent cancellation API. Closing the client stream is
// therefore delivery-only and must never be represented as provider cancellation.
func (c *HTTPUpstreamClient) CancelCapability() ProviderCancelCapability {
	return ProviderCancelUnsupported
}

const (
	ProviderCancelExplicit             ProviderCancelCapability = "EXPLICIT_CANCEL"
	ProviderCancelStreamAbortConfirmed ProviderCancelCapability = "STREAM_ABORT_CONFIRMED_CANCEL"
	ProviderCancelUnsupported          ProviderCancelCapability = "UNSUPPORTED"
)
