package account

import (
	"context"
	"net/http"
	"net/url"
)

// AccountClosureIdentityClient is the private identity deletion authority used
// by the durable closure worker. Ordinary registration needs no delete method.
type AccountClosureIdentityClient interface {
	DeleteUser(context.Context, string) error
}

func (p *HTTPIdentityProvisioner) DeleteUser(ctx context.Context, subject string) error {
	return p.request(ctx, http.MethodDelete, "/internal/v1/users/"+url.PathEscape(subject), nil, nil)
}
