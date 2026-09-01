//go:build !unix

// statfs_other.go — no statfs on this platform; the §6.2 pre-flight reports
// blocked (fail-closed).
package maintain

import "errors"

func freeBytes(dir string) (uint64, error) {
	return 0, errors.New("statfs unsupported on this platform")
}
