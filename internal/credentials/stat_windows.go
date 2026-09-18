//go:build windows

package credentials

import "os"

func ownedByCurrentUser(os.FileInfo) bool { return true }
