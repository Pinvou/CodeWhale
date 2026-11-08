//go:build !linux && !windows

package operator

import "os"

func lockRotationFile(*os.File) error { return ErrRotatePreflight }
func unlockRotationFile(*os.File)     {}
