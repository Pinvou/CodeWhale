package token

import (
	"crypto/x509"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/transit"
	"net/http"
)

var errTransitCA = transit.ErrCA

func newTransitTransport(path string) (*http.Transport, error) { return transit.NewTransport(path) }
func readTransitCA(path string) (*x509.CertPool, error)        { return transit.ReadCA(path) }
