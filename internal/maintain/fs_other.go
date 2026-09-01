//go:build !linux

// fs_other.go — non-linux: no FICLONE; copyDir always plain-copies (§6.2).
package maintain

import "os"

func reflinkClone(dst, src *os.File) error {
	return errReflinkUnsupported
}
