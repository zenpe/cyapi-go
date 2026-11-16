package sourcecheck

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
	"unsafe"
)

func prepareWorkerParent(bool) error { return nil }

func isolateWorker(allow bool) (string, func(), error) {
	job, e := windows.CreateJobObject(nil, nil)
	if e == nil {
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY
		info.ProcessMemoryLimit = 512 << 20
		_, e = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
		if e == nil {
			e = windows.AssignProcessToJobObject(job, windows.CurrentProcess())
		}
		if e == nil {
			return "windows_job_512MiB", func() { windows.CloseHandle(job) }, nil
		}
		windows.CloseHandle(job)
	}
	if allow {
		return "explicitly_unconfined", func() {}, nil
	}
	return "", func() {}, errors.New("worker memory isolation unavailable; manual diagnostics may explicitly use --allow-unconfined-worker")
}
func lockFile(f *os.File) error {
	e := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_FAIL_IMMEDIATELY|windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(e, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
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
func syncDirectory(string) error { return nil }

func cleanupWorker(int) {}
