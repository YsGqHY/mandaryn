//go:build linux || darwin

package storage

import (
	"os"

	"golang.org/x/sys/unix"
)

func lockRootFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
