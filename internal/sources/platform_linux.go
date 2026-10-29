package sources

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockFile(f *os.File, exclusive bool) error {
	mode := unix.LOCK_SH | unix.LOCK_NB
	if exclusive {
		mode = unix.LOCK_EX | unix.LOCK_NB
	}
	e := unix.Flock(int(f.Fd()), mode)
	if errors.Is(e, unix.EWOULDBLOCK) || errors.Is(e, unix.EAGAIN) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File) { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func diskFree(name string) (uint64, error) {
	var s unix.Statfs_t
	e := unix.Statfs(name, &s)
	return s.Bavail * uint64(s.Bsize), e
}
func replaceFile(from, to string) error { return os.Rename(from, to) }
func syncDir(name string) error {
	f, e := os.Open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
