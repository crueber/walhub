//go:build !linux

// fs_other.go — non-Linux fallbacks: disk-mode eviction is disabled and hard
// links are not deduped (best effort).
package wal

import "os"

type fsStat struct{ Blocks, Bfree uint64 }

func statfs(string) (*fsStat, error) { return nil, os.ErrNotExist }

func hardlinkKey(info os.FileInfo) uint64 { return uint64(info.ModTime().UnixNano()) }
