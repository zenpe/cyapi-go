package sourcecheck

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

// A delegated cgroup-v2 subtree is required for a hard per-worker memory limit.
// Failure is explicit; GOMEMLIMIT or RLIMIT_AS are not substitutes.
func isolateWorker(allow bool) (string, func(), error) {
	contents, e := os.ReadFile("/proc/self/cgroup")
	if e == nil {
		for _, line := range strings.Split(string(contents), "\n") {
			if !strings.HasPrefix(line, "0::/") {
				continue
			}
			rel := strings.TrimPrefix(line, "0::/")
			if strings.Contains(rel, "..") {
				continue
			}
			parent := filepath.Join("/sys/fs/cgroup", rel)
			child := filepath.Join(parent, fmt.Sprintf("cyapi-probe-%d", os.Getpid()))
			if e = os.Mkdir(child, 0700); e != nil {
				break
			}
			pid := []byte(fmt.Sprint(os.Getpid()))
			cleanup := func() { _ = os.WriteFile(filepath.Join(parent, "cgroup.procs"), pid, 0600); _ = os.Remove(child) }
			e = os.WriteFile(filepath.Join(child, "memory.max"), []byte("536870912"), 0600)
			if e == nil {
				e = os.WriteFile(filepath.Join(child, "memory.swap.max"), []byte("0"), 0600)
			}
			if e == nil {
				e = os.WriteFile(filepath.Join(child, "cgroup.procs"), pid, 0600)
			}
			if e == nil {
				return "cgroup_v2_512MiB", cleanup, nil
			}
			cleanup()
			break
		}
	}
	if allow {
		return "explicitly_unconfined", func() {}, nil
	}
	return "", func() {}, errors.New("worker requires delegated cgroup v2 memory controller; manual diagnostics may explicitly use --allow-unconfined-worker")
}
func lockFile(f *os.File) error {
	e := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(e, unix.EWOULDBLOCK) || errors.Is(e, unix.EAGAIN) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File)             { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }
func replaceFile(from, to string) error { return os.Rename(from, to) }
func syncDirectory(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

// The parent removes an empty child cgroup even if the worker was killed before defer ran.
func cleanupWorker(pid int) {
	b, e := os.ReadFile("/proc/self/cgroup")
	if e != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "0::/") {
			rel := strings.TrimPrefix(line, "0::/")
			if strings.Contains(rel, "..") {
				return
			}
			_ = os.Remove(filepath.Join("/sys/fs/cgroup", rel, fmt.Sprintf("cyapi-probe-%d", pid)))
			return
		}
	}
}
