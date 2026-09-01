//go:build linux

// fs_linux.go — FICLONE reflink for the §6.2 scratch copy (XFS/btrfs). No
// new dependency: raw syscall, with errReflinkUnsupported on any failure so
// callers fall back to a plain copy.
package maintain

import (
	"os"
	"syscall"
)

// FICLONE is the ioctl to clone a file's extents (linux/fs.h); 0x40049409 =
// _IOW(0x94, 9, int).
const fsiclone = 0x40049409

// reflinkClone clones src into the already-open dst fd.
func reflinkClone(dst, src *os.File) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), fsiclone, src.Fd()); errno != 0 {
		return errReflinkUnsupported
	}
	return nil
}
