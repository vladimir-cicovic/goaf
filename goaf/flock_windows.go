//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockExclusive places an advisory exclusive lock, blocking until acquired.
func lockExclusive(f *os.File) error {
	// Lock the whole file: low/high max DWORD = entire file range.
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

// unlockFile releases the lock.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
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
