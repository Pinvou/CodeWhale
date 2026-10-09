package operator

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockRotationFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func unlockRotationFile(file *os.File) { _ = unix.Flock(int(file.Fd()), unix.LOCK_UN) }
