//go:build !windows

package rpc

import "golang.org/x/sys/unix"

func checkExecutable(path string) error {
	return unix.Faccessat(unix.AT_FDCWD, path, unix.X_OK, unix.AT_EACCESS)
}
