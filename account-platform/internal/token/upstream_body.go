package token

import (
	"errors"
	"io"
)

var errUpstreamBodyTooLarge = errors.New("upstream body exceeds existing response limit")

// A response is complete only if EOF was observed within the existing bound.
// Neither an overflow prefix nor bytes preceding a read failure may be replayed.
func readBoundedUpstreamBody(body io.Reader, limit int64) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(payload)) > limit {
		return nil, errUpstreamBodyTooLarge
	}
	return payload, nil
}
