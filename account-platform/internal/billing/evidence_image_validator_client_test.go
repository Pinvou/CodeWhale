package billing

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func fakeImageValidator(t *testing.T, peer func(net.Conn)) *evidenceImageValidator {
	t.Helper()
	return &evidenceImageValidator{path: "test", workerUID: 123, slots: make(chan struct{}, 1), timeout: time.Second,
		dial: func(context.Context, string) (net.Conn, error) { a, b := net.Pipe(); go peer(b); return a, nil }, verify: func(net.Conn, uint32) bool { return true }}
}

func TestEvidenceImageValidatorDialFailure(t *testing.T) {
	for _, media := range []string{"image/png", "image/jpeg", "application/pdf"} {
		t.Run(media, func(t *testing.T) {
			payload := []byte("original-private-evidence")
			before := append([]byte(nil), payload...)
			dialError := errors.New("private socket transport diagnostic")
			calls := 0
			v := &evidenceImageValidator{path: "missing.sock", workerUID: 123, slots: make(chan struct{}, 1), timeout: time.Second,
				dial:   func(context.Context, string) (net.Conn, error) { calls++; return nil, dialError },
				verify: func(net.Conn, uint32) bool { t.Fatal("authenticated a connection after failed dial"); return false }}
			// A second invocation also proves that failure releases the admission slot.
			for attempt := 0; attempt < 2; attempt++ {
				if err := v.Validate(context.Background(), media, payload); err != errImageValidatorUnavailable {
					t.Fatalf("dial failure must return only the public unavailable error: %v", err)
				}
				if !bytes.Equal(payload, before) || len(v.slots) != 0 {
					t.Fatal("dial failure changed evidence or retained an admission slot")
				}
			}
			if calls != 2 {
				t.Fatalf("wanted one dial per invocation without retry, got %d", calls)
			}
		})
	}
}

func TestEvidenceImageValidatorResponseBinding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   byte
		mutation string
		want     error
	}{
		{"valid", 1, "", nil}, {"invalid", 2, "", errImageValidatorInvalid}, {"unavailable", 3, "", errImageValidatorUnavailable},
		{"unknown", 9, "", errImageValidatorUnavailable}, {"nonce", 1, "nonce", errImageValidatorUnavailable}, {"digest", 1, "digest", errImageValidatorUnavailable},
		{"reserved", 1, "reserved", errImageValidatorUnavailable}, {"extra", 1, "extra", errImageValidatorUnavailable}, {"truncated", 1, "truncated", errImageValidatorUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := []byte("fixture-contents")
			original := append([]byte(nil), payload...)
			checked := make(chan bool, 1)
			v := fakeImageValidator(t, func(conn net.Conn) {
				defer conn.Close()
				header := make([]byte, 60)
				if _, e := io.ReadFull(conn, header); e != nil {
					checked <- false
					return
				}
				body := make([]byte, len(payload))
				if _, e := io.ReadFull(conn, body); e != nil {
					checked <- false
					return
				}
				sum := sha256.Sum256(body)
				checked <- string(header[:4]) == "PIV1" && header[4] == 1 && bytes.Equal(body, original) && bytes.Equal(header[24:56], sum[:])
				response := make([]byte, 56)
				copy(response, "PIV1")
				response[4] = tc.status
				copy(response[8:24], header[8:24])
				copy(response[24:], sum[:])
				switch tc.mutation {
				case "nonce":
					response[8] ^= 1
				case "digest":
					response[24] ^= 1
				case "reserved":
					response[5] = 1
				case "extra":
					response = append(response, 1)
				case "truncated":
					response = response[:20]
				}
				_, _ = conn.Write(response)
			})
			got := v.Validate(context.Background(), "image/png", payload)
			if !errors.Is(got, tc.want) || (tc.want == nil && got != nil) || !bytes.Equal(payload, original) {
				t.Fatalf("incorrect validation result %v", got)
			}
			if !<-checked {
				t.Fatal("wire payload was incorrect")
			}
		})
	}
}
func TestEvidenceImageValidatorAuthenticatesBeforeSending(t *testing.T) {
	observed := make(chan int, 1)
	v := fakeImageValidator(t, func(conn net.Conn) {
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		observed <- n
	})
	v.verify = func(net.Conn, uint32) bool { return false }
	if !errors.Is(v.Validate(context.Background(), "image/png", []byte("private")), errImageValidatorUnavailable) {
		t.Fatal("unverified peer accepted")
	}
	if n := <-observed; n != 0 {
		t.Fatal("input sent before peer authentication")
	}
}
func TestEvidenceImageValidatorCapacityCancellationAndFormat(t *testing.T) {
	done := make(chan struct{})
	v := fakeImageValidator(t, func(c net.Conn) { defer close(done); defer c.Close(); _, _ = io.Copy(io.Discard, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !errors.Is(v.Validate(ctx, "image/jpeg", []byte("fixture")), errImageValidatorUnavailable) {
		t.Fatal("deadline did not fail closed")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancel did not close socket")
	}
	v.slots <- struct{}{}
	if !errors.Is(v.Validate(context.Background(), "image/png", []byte("fixture")), errImageValidatorUnavailable) {
		t.Fatal("capacity did not fail closed")
	}
	<-v.slots
	if !errors.Is(v.Validate(context.Background(), "image/gif", []byte("fixture")), errImageValidatorInvalid) {
		t.Fatal("unknown format sent to validator")
	}
}

func TestEvidencePDFProtocolVersionAndStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		magic  string
		status byte
		want   error
	}{
		{"valid", "PIV2", 1, nil}, {"invalid", "PIV2", 2, errImageValidatorInvalid},
		{"unavailable", "PIV2", 3, errImageValidatorUnavailable}, {"password", "PIV2", 4, errEvidencePDFPasswordRequired},
		{"old-response", "PIV1", 1, errImageValidatorUnavailable}, {"unknown", "PIV2", 9, errImageValidatorUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte("%PDF-controlled")
			before := append([]byte(nil), data...)
			observed := make(chan bool, 1)
			v := fakeImageValidator(t, func(c net.Conn) {
				defer c.Close()
				header := make([]byte, 60)
				if _, e := io.ReadFull(c, header); e != nil {
					observed <- false
					return
				}
				body := make([]byte, len(data))
				if _, e := io.ReadFull(c, body); e != nil {
					observed <- false
					return
				}
				sum := sha256.Sum256(body)
				observed <- string(header[:4]) == "PIV2" && header[4] == 3 && bytes.Equal(body, before) && bytes.Equal(header[24:56], sum[:])
				reply := make([]byte, 56)
				copy(reply, tc.magic)
				reply[4] = tc.status
				copy(reply[8:24], header[8:24])
				copy(reply[24:], sum[:])
				_, _ = c.Write(reply)
			})
			err := v.Validate(context.Background(), "application/pdf", data)
			if !errors.Is(err, tc.want) || (tc.want == nil && err != nil) || !bytes.Equal(before, data) {
				t.Fatalf("wrong PDF outcome: %v", err)
			}
			if !<-observed {
				t.Fatal("PDF protocol mismatch")
			}
		})
	}
}
