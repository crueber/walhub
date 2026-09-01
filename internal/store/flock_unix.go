//go:build unix

// flock support (03_store_backends.md §4): advisory LOCK_EX on the sidecar.
package store

import (
	"os"
	"syscall"
)

// flockExclusive takes an exclusive advisory lock, blocking until acquired.
func flockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}
