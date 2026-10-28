//go:build !windows

package appdiagnostics

import (
	"errors"
	"os"
	"syscall"
)

func privateDir(name string) error {
	err := os.Mkdir(name, 0o700)
	if err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	return privateDirectoryInfo(info)
}
func privateDirectoryInfo(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || int64(st.Uid) != int64(os.Geteuid()) {
		return errors.New("unsafe diagnostic directory")
	}
	return nil
}
func privateFile(info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || int64(st.Uid) != int64(os.Geteuid()) || st.Nlink != 1 {
		return errors.New("unsafe diagnostic file")
	}
	return nil
}
func secureCreatedFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	return privateFile(info)
}

func secureOpenedFile(file *os.File, directory bool) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if directory {
		return privateDirectoryInfo(info)
	}
	return privateFile(info)
}
