package main

import (
	"io"
	"os"
	"runtime"
	"testing"
)

// Every wrapper must own a duplicate rather than a caller-owned descriptor.
// GC is deliberate: the original defect closed a still-live caller handle.
func TestControlledDescriptorOwnership(t *testing.T) {
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("controlled descriptors unsupported on this platform")
	}
	for _, operation := range []string{"validate", "deliver", "credential", "password"} {
		t.Run(operation, func(t *testing.T) {
			owner, err := os.CreateTemp(t.TempDir(), "controlled-")
			if err != nil {
				t.Fatal("owned descriptor creation failed")
			}
			defer owner.Close()
			fd := int(owner.Fd())
			switch operation {
			case "validate":
				if err := validateDeliveryFD(fd); err != nil {
					t.Fatal("controlled file rejected")
				}
			case "deliver":
				if err := writeSecret(fd, "PUBLIC_SYNTHETIC_DELIVERY"); err != nil {
					t.Fatal("controlled delivery failed")
				}
				if _, err := owner.Seek(0, 0); err != nil {
					t.Fatal("original descriptor was closed")
				}
				content, err := io.ReadAll(owner)
				if err != nil || string(content) != "PUBLIC_SYNTHETIC_DELIVERY\n" {
					t.Fatal("delivery content mismatch")
				}
			case "credential", "password":
				input := "PUBLIC_SYNTHETIC_CREDENTIAL\n"
				if operation == "password" {
					input = "PUBLIC_SYNTHETIC_PASSWORD\nPUBLIC_SYNTHETIC_PASSWORD\n"
				}
				if _, err := io.WriteString(owner, input); err != nil {
					t.Fatal("controlled input failed")
				}
				if _, err := owner.Seek(0, 0); err != nil {
					t.Fatal("controlled input seek failed")
				}
				var value string
				if operation == "password" {
					value, err = readConfirmedPassword(fd)
				} else {
					value, err = readSingleSecret(fd)
				}
				expected := "PUBLIC_SYNTHETIC_CREDENTIAL"
				if operation == "password" {
					expected = "PUBLIC_SYNTHETIC_PASSWORD"
				}
				if err != nil || value != expected {
					t.Fatal("controlled input mismatch")
				}
			}
			runtime.GC()
			if _, err := owner.WriteString("PUBLIC_SYNTHETIC_OWNER_RETAINED"); err != nil {
				t.Fatal("temporary wrapper closed caller descriptor")
			}
			if err := owner.Sync(); err != nil {
				t.Fatal("caller descriptor is no longer durable")
			}
			runtime.KeepAlive(owner)
		})
	}
}

func TestDeliveryRejectsStandardDescriptorsAndTheirAliases(t *testing.T) {
	for _, fd := range []int{-1, 0, 1, 2, int(os.Stdin.Fd()), int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if err := validateDeliveryFD(fd); err == nil {
			t.Fatal("standard stream accepted for credential delivery")
		}
	}
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("controlled descriptors unsupported on this platform")
	}
	// A duplicate has a different FD value but points to the same standard file.
	alias, err := duplicateDescriptor(int(os.Stdout.Fd()), "controlled-test-stdout-alias")
	if err != nil {
		t.Fatal("standard alias creation failed")
	}
	defer alias.Close()
	if err := validateDeliveryFD(int(alias.Fd())); err == nil {
		t.Fatal("standard stream alias accepted")
	}
}
