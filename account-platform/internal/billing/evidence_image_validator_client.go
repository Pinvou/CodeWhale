package billing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"time"
)

var errImageValidatorUnavailable = errors.New("image validator unavailable")
var errImageValidatorInvalid = errors.New("invalid evidence image")
var errEvidencePDFPasswordRequired = errors.New("evidence PDF requires a password")

// evidenceImageValidator is an opt-in local client. Nothing constructs or
// enables it by default. The caller keeps data unchanged until Validate returns.
type evidenceImageValidator struct {
	path      string
	workerUID uint32
	slots     chan struct{}
	dial      func(context.Context, string) (net.Conn, error)
	verify    func(net.Conn, uint32) bool
	timeout   time.Duration
}

func newEvidenceImageValidator(path string, workerUID uint32) (*evidenceImageValidator, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || workerUID == 0 || !validImageValidatorUID(workerUID) {
		return nil, errImageValidatorUnavailable
	}
	return &evidenceImageValidator{path: path, workerUID: workerUID, slots: make(chan struct{}, 1), timeout: 45 * time.Second,
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}, verify: verifyImageValidatorPeer}, nil
}

// Validate retains PIV1 for images and uses PIV2 for the opt-in PDF candidate.
// It never retries or reports raw transport errors and does not mutate data.
func (v *evidenceImageValidator) Validate(ctx context.Context, media string, data []byte) error {
	if ctx == nil || ctx.Err() != nil || v == nil || len(data) == 0 || len(data) > 10<<20 {
		return errImageValidatorUnavailable
	}
	var kind byte
	switch media {
	case "image/png":
		kind = 1
	case "image/jpeg":
		kind = 2
	case "application/pdf":
		kind = 3
	default:
		return errImageValidatorInvalid
	}
	select {
	case v.slots <- struct{}{}:
		defer func() { <-v.slots }()
	default:
		return errImageValidatorUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	conn, err := v.dial(ctx, v.path)
	if err != nil {
		return errImageValidatorUnavailable
	}
	defer conn.Close()
	// Peer authentication precedes any request header or original bytes.
	if !v.verify(conn, v.workerUID) {
		return errImageValidatorUnavailable
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if d, ok := ctx.Deadline(); ok {
		if conn.SetDeadline(d) != nil {
			return errImageValidatorUnavailable
		}
	}
	var header [60]byte
	copy(header[:4], "PIV1")
	if kind == 3 {
		copy(header[:4], "PIV2")
	}
	header[4] = kind
	if _, err = rand.Read(header[8:24]); err != nil {
		return errImageValidatorUnavailable
	}
	digest := sha256.Sum256(data)
	copy(header[24:56], digest[:])
	binary.BigEndian.PutUint32(header[56:60], uint32(len(data)))
	if err = writeImageValidatorBytes(conn, header[:]); err != nil {
		return errImageValidatorUnavailable
	}
	if err = writeImageValidatorBytes(conn, data); err != nil {
		return errImageValidatorUnavailable
	}
	// Do not CloseWrite: the worker treats a half-close as cancellation.
	var response [56]byte
	if _, err = io.ReadFull(conn, response[:]); err != nil {
		return errImageValidatorUnavailable
	}
	if subtle.ConstantTimeCompare(response[:4], header[:4]) != 1 || response[5] != 0 || response[6] != 0 || response[7] != 0 || subtle.ConstantTimeCompare(response[8:24], header[8:24]) != 1 || subtle.ConstantTimeCompare(response[24:], digest[:]) != 1 {
		return errImageValidatorUnavailable
	}
	// Exactly one frame followed by EOF; extra bytes or a stalled peer fail closed.
	var extra [1]byte
	n, readErr := conn.Read(extra[:])
	if n != 0 || readErr != io.EOF || ctx.Err() != nil {
		return errImageValidatorUnavailable
	}
	switch response[4] {
	case 1:
		return nil
	case 2:
		return errImageValidatorInvalid
	case 4:
		if kind == 3 {
			return errEvidencePDFPasswordRequired
		}
		return errImageValidatorUnavailable
	default:
		return errImageValidatorUnavailable
	}
}
func writeImageValidatorBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
