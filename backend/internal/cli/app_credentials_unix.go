//go:build !windows

package cli

import (
	"os"
	"syscall"
)

func validateAppCredentialFile(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return errAppCredential
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int64(st.Uid) != int64(os.Geteuid()) {
		return errAppCredential
	}
	if directory {
		if !info.IsDir() || info.Mode().Perm() != 0o700 {
			return errAppCredential
		}
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || st.Nlink != 1 {
		return errAppCredential
	}
	return nil
}
