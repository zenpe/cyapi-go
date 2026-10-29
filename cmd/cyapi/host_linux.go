package main

import (
	"golang.org/x/sys/unix"
	"os"
	"os/signal"
	"syscall"
)

func platformLock(f *os.File) error { return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) }
func platformUnlock(f *os.File)     { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func terminalPassword(read func() (string, error)) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return "", err
	}
	next := *old
	next.Lflag &^= unix.ECHO
	if err = unix.IoctlSetTermios(fd, unix.TCSETS, &next); err != nil {
		return "", err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	finished := make(chan struct{})
	defer close(finished)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
			os.Exit(130)
		case <-finished:
		}
	}()
	return read()
}

func checkWritableDirectory(path string) error {
	info, e := os.Stat(path)
	if e != nil {
		return e
	}
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return unix.Access(path, unix.W_OK|unix.X_OK)
}

func availableBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if e := unix.Statfs(path, &stat); e != nil {
		return 0, e
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
