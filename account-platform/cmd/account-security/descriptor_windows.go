package main

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func duplicateDescriptor(fd int, name string) (*os.File, error) {
	if fd < 0 {
		return nil, errors.New("invalid controlled descriptor")
	}
	process := windows.CurrentProcess()
	var owned windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(fd), process, &owned, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(owned), name)
	if file == nil {
		windows.CloseHandle(owned)
		return nil, errors.New("invalid controlled descriptor")
	}
	return file, nil
}
