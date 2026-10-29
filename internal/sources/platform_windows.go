package sources

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockFile(f *os.File, exclusive bool) error {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	e := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(e, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func diskFree(name string) (uint64, error) {
	p, e := windows.UTF16PtrFromString(name)
	if e != nil {
		return 0, e
	}
	var available, total, free uint64
	e = windows.GetDiskFreeSpaceEx(p, &available, &total, &free)
	return available, e
}
func replaceFile(from, to string) error {
	a, e := windows.UTF16PtrFromString(from)
	if e != nil {
		return e
	}
	b, e := windows.UTF16PtrFromString(to)
	if e != nil {
		return e
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// File data is flushed explicitly; Windows directory handles do not support File.Sync.
func syncDir(string) error { return nil }
