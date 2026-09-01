//go:build !linux

// Non-Linux GOOS have no renameat2 in the stdlib syscall package; the caller
// falls back to lock+stat+rename (03_store_backends.md §4 portable fallback).
package store

import "syscall"

// renameNoReplace always reports ENOSYS off Linux.
func renameNoReplace(old, new string) error {
	return syscall.ENOSYS
}
