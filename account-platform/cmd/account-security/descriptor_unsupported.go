//go:build !windows && !linux && !darwin

package main

import (
	"errors"
	"os"
)

func duplicateDescriptor(fd int, name string) (*os.File, error) {
	return nil, errors.New("controlled descriptors are unsupported on this platform")
}
