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

// renameat2Sysnum returns the renameat2 syscall number for the running
// GOARCH, or 0 when unknown (the caller then uses the portable fallback).
func renameat2Sysnum() uintptr {
	switch runtime.GOARCH {
	case "amd64":
		return 316
	case "arm64", "riscv64", "loong64":
		return 276
	case "386":
		return 353
	case "arm":
		return 382
	case "mips64", "mips64le":
		return 5311
	case "mips", "mipsle":
		return 4353
	case "ppc64", "ppc64le":
		return 357
	case "s390x":
		return 347
	default:
		return 0
	}
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
