package events

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNATSPrivateCABundleRejectsEveryMalformedBlock(t *testing.T) {
	first, _, _ := natsFixtureTLS(t)
	second, _, _ := natsFixtureTLS(t)
	malformed := []byte("-----BEGIN CERTIFICATE-----\nnot valid base64!\n-----END CERTIFICATE-----\n")
	unclosed := []byte("-----BEGIN CERTIFICATE-----\nnot valid base64!\n")
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"malformed_first", bytes.Join([][]byte{malformed, first}, nil)},
		{"malformed_middle", bytes.Join([][]byte{first, malformed, second}, nil)},
		{"unclosed_first", bytes.Join([][]byte{unclosed, first}, nil)},
		{"unclosed_middle", bytes.Join([][]byte{first, unclosed, second}, nil)},
		{"malformed_last", bytes.Join([][]byte{first, malformed}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(file, test.body, 0600); err != nil {
				t.Fatal(err)
			}
			pool, err := natsRoots(file)
			if !errors.Is(err, ErrNATSResources) || pool != nil {
				t.Fatal("malformed bundle must fail without returning partial roots")
			}
		})
	}
}

func TestNATSPrivateCABundleRetainsEachValidRoot(t *testing.T) {
	first, firstLeaf, _ := natsFixtureTLS(t)
	second, secondLeaf, _ := natsFixtureTLS(t)
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"two_roots_LF", bytes.Join([][]byte{first, second}, nil)},
		{"two_roots_CRLF", bytes.ReplaceAll(bytes.Join([][]byte{first, second}, nil), []byte("\n"), []byte("\r\n"))},
		{"mixed_line_endings_and_whitespace", bytes.Join([][]byte{[]byte(" \r\n\t"), first, []byte("\n \t"), bytes.ReplaceAll(second, []byte("\n"), []byte("\r\n")), []byte(" \r\n")}, nil)},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "ca.pem")
			if err := os.WriteFile(file, test.body, 0600); err != nil {
				t.Fatal(err)
			}
			pool, err := natsRoots(file)
			if err != nil || pool == nil {
				t.Fatal("valid CA rotation bundle rejected")
			}
			for _, leafPEM := range [][]byte{firstLeaf, secondLeaf} {
				block, _ := pem.Decode(leafPEM)
				if block == nil {
					t.Fatal("fixture certificate missing")
				}
				leaf, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
					t.Fatal("bundle dropped a valid trust root")
				}
			}
		})
	}
}
