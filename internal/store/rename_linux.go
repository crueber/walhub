//go:build linux

// renameat2(RENAME_NOREPLACE) via the stdlib syscall (no cgo) — the Linux
// Create-if-absent primitive of 03_store_backends.md §4. The stdlib syscall
// package is frozen and lacks SYS_RENAMEAT2 on several arches, so the syscall
// numbers live here.
package store

import (
	"runtime"
	"syscall"
	"unsafe"
)

// renameat2Sysnums maps GOARCH → the renameat2 syscall number. Arches absent
// from the table fall back to the portable lock+stat+rename path.
var renameat2Sysnums = map[string]uintptr{
	"amd64":    316,
	"arm64":    276,
	"riscv64":  276,
	"loong64":  276,
	"386":      353,
	"arm":      382,
	"mips64":   5311,
	"mips64le": 5311,
	"mips":     4353,
	"mipsle":   4353,
	"ppc64":    357,
	"ppc64le":  357,
	"s390x":    347,
}

// renameat2Sysnum returns the renameat2 syscall number for the running
// GOARCH, or 0 when unknown (the caller then uses the portable fallback).
func renameat2Sysnum() uintptr {
	return renameat2Sysnums[runtime.GOARCH]
}

// renameNoReplace renames old to new, failing with EEXIST when new exists.
// Returns syscall.ENOSYS when the kernel/arch lacks renameat2 (the caller
// then falls back to lock+stat+rename).
func renameNoReplace(old, new string) error {
	if forcePortableRename.Load() {
		return syscall.ENOSYS // test hook: exercise the portable fallback
	}
	num := renameat2Sysnum()
	if num == 0 {
		return syscall.ENOSYS
	}
	oldP, err := syscall.BytePtrFromString(old)
	if err != nil {
		return err
	}
	newP, err := syscall.BytePtrFromString(new)
	if err != nil {
		return err
	}
	const renameNOREPLACE = 1 << 0 // fail with EEXIST instead of overwriting
	atFDCWD := int32(-100)         // renameat2 dirfd: paths relative to CWD
	cwdFD := uintptr(atFDCWD)
	_, _, errno := syscall.Syscall6(
		num,
		cwdFD, uintptr(unsafe.Pointer(oldP)),
		cwdFD, uintptr(unsafe.Pointer(newP)),
		renameNOREPLACE, 0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
