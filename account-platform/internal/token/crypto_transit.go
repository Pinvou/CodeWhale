package token

import (
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/transit"
)

// transitMaximumValue remains the bound used by Token's protocol tests.
const transitMaximumValue = transit.MaximumValue

// TransitProvider retains Token's existing public provider contract while the
// native transport and crypto protocol are shared at the platform boundary.
type TransitProvider struct{ inner *transit.TransitProvider }

func NewTransitProvider(config SecretProviderConfig) (*TransitProvider, error) {
	inner, err := transit.NewTransitProvider(transit.Config{
		Environment: config.Environment, KeyID: config.KeyID, RemoteURL: config.RemoteURL,
		RemoteToken: config.RemoteToken, RemoteTokenFile: config.RemoteTokenFile, TransitCAFile: config.TransitCAFile,
	})
	if err != nil {
		return nil, mapTransitError(err)
	}
	return &TransitProvider{inner: inner}, nil
}
func mapTransitError(err error) error {
	if errors.Is(err, transit.ErrInvalidSecret) {
		return ErrInvalidSecret
	}
	return err
}
func (p *TransitProvider) KeyID() string { return p.inner.KeyID() }
func (p *TransitProvider) Encrypt(plaintext, aad []byte) ([]byte, error) {
	result, err := p.inner.Encrypt(plaintext, aad)
	return result, mapTransitError(err)
}
func (p *TransitProvider) Decrypt(ciphertext, aad []byte) ([]byte, error) {
	result, err := p.inner.Decrypt(ciphertext, aad)
	return result, mapTransitError(err)
}
func (p *TransitProvider) DecryptWithKeyID(keyID string, ciphertext, aad []byte) ([]byte, error) {
	result, err := p.inner.DecryptWithKeyID(keyID, ciphertext, aad)
	return result, mapTransitError(err)
}
