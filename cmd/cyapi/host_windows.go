package main

import (
	"golang.org/x/sys/windows"
	"os"
	"os/signal"
)

func platformLock(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}
func platformUnlock(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func terminalPassword(read func() (string, error)) (string, error) {
	h := windows.Handle(os.Stdin.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return "", err
	}
	if err := windows.SetConsoleMode(h, old&^windows.ENABLE_ECHO_INPUT); err != nil {
		return "", err
	}
	defer windows.SetConsoleMode(h, old)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	finished := make(chan struct{})
	defer close(finished)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			_ = windows.SetConsoleMode(h, old)
			os.Exit(130)
		case <-finished:
		}
	}()
	return read()
}

// Windows mode bits do not expose ACLs; actual startup performs the write/open checks.
func checkWritableDirectory(path string) error {
	info, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}

func availableBytes(path string) (uint64, error) {
	p, e := windows.UTF16PtrFromString(path)
	if e != nil {
		return 0, e
	}
	var available, total, free uint64
	e = windows.GetDiskFreeSpaceEx(p, &available, &total, &free)
	return available, e
}
