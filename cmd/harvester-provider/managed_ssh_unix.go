//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func secureManagedDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed SSH directory must not be a symlink")
	}
	return checkManagedPermissions(path, info)
}

func checkManagedPermissions(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("managed SSH state must be accessible only to its owner")
	}
	return nil
}

func protectManagedCreatedFile(path string) error {
	return checkManagedFile(path)
}

func openManagedLockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

func tryManagedLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}

func releaseManagedLock(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
