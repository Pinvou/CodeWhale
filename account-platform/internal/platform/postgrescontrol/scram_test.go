package postgrescontrol

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func authenticationFrame(method uint32) []byte {
	frame := make([]byte, 9)
	frame[0] = 'R'
	binary.BigEndian.PutUint32(frame[1:5], 8)
	binary.BigEndian.PutUint32(frame[5:], method)
	return frame
}

func TestSCRAMObserverRejectsOtherAuthenticationBeforeResponse(t *testing.T) {
	for _, method := range []uint32{0, 3, 5, 7, 11, 12, 99} {
		r := &scramReader{reader: bytes.NewReader(authenticationFrame(method))}
		if n, err := r.Read(make([]byte, 32)); err == nil || n != 0 {
			t.Fatalf("authentication method %d reached the client", method)
		}
	}
	var wire []byte
	for _, method := range []uint32{10, 11, 12, 0} {
		wire = append(wire, authenticationFrame(method)...)
	}
	r := &scramReader{reader: bytes.NewReader(wire)}
	var observed []byte
	buffer := make([]byte, 2) // every header and method crosses read boundaries
	for {
		n, err := r.Read(buffer)
		observed = append(observed, buffer[:n]...)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if r.stage != 4 || !bytes.Equal(observed, wire) {
		t.Fatal("complete SCRAM authentication changed wire bytes or lost evidence")
	}
	for _, malformed := range [][]byte{{'R', 0, 0, 0, 3}, {'R', 0, 0, 0, 4}, {'R', 127, 0, 0, 0}, {'R', 0, 0}} {
		r := &scramReader{reader: bytes.NewReader(malformed)}
		if _, err := io.ReadAll(r); err == nil {
			t.Fatal("malformed authentication accepted")
		}
	}
}

func TestSCRAMVerifierGeneratedCredentialContract(t *testing.T) {
	password := strings.Repeat("fixture-only-", 3)
	first, err := SCRAMVerifier(password)
	if err != nil {
		t.Fatal(err)
	}
	second, err := SCRAMVerifier(password)
	if err != nil || first == second || !strings.HasPrefix(first, "SCRAM-SHA-256$4096:") || strings.Contains(first, password) || strings.ContainsAny(first, "'\r\n") {
		t.Fatal("verifier format, random salt or secrecy contract failed")
	}
	for _, password := range []string{"short", strings.Repeat("x", 129), strings.Repeat("x", 32) + "\n", strings.Repeat("x", 32) + "\u00e9"} {
		if _, err := SCRAMVerifier(password); err == nil || strings.Contains(err.Error(), password) {
			t.Fatal("invalid generated credential accepted or disclosed")
		}
	}
}
