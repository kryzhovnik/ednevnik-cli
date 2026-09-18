//go:build windows

package client

import "os"

func sessionOwnedByCurrentUser(os.FileInfo) bool { return true }
