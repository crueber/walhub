//go:build linux

// fs_linux.go — statfs + hard-link identity for eviction (05 §5.1.6).
package wal

import (
	"os"
	"syscall"
)

type fsStat struct {
	Blocks uint64
	Bfree  uint64
}

func statfs(path string) (*fsStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return nil, err
	}
	return &fsStat{Blocks: st.Blocks, Bfree: st.Bfree}, nil
}

// hardlinkKey keys a file by (dev, ino): dev<<32|ino, per walk.
func hardlinkKey(info os.FileInfo) uint64 {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(s.Dev)<<32 | uint64(s.Ino)&0xffffffff
	}
	return uint64(info.ModTime().UnixNano())
}
