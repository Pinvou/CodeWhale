//go:build linux || darwin

package main

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func duplicateDescriptor(fd int, name string) (*os.File, error) {
	if fd < 0 {
		return nil, errors.New("invalid controlled descriptor")
	}
	owned, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(owned), name)
	if file == nil {
		unix.Close(owned)
		return nil, errors.New("invalid controlled descriptor")
	}
	return file, nil
}
