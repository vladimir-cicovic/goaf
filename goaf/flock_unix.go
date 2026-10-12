//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockExclusive places an advisory exclusive lock, blocking until acquired.
func lockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile releases the lock.
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// withLockedFile opens path, locks it exclusively, runs fn, then unlocks.
// Used for state/audit/facts writes so parallel runs cannot corrupt them.
func withLockedFile(path string, flag int, perm os.FileMode, fn func(f *os.File) error) error {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lockExclusive(f); err != nil {
		return err
	}
	defer unlockFile(f)
	return fn(f)
}
