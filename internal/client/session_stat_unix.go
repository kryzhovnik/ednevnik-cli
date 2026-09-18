//go:build !windows

package client

import (
	"os"
	"syscall"
)

func sessionOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}
