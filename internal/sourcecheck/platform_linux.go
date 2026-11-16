package sourcecheck

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const runtimeCgroup = "cyapi-runtime"

var cgroupSetup sync.Mutex

func currentCgroup() (string, error) {
	contents, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, "0::/") {
			rel := strings.TrimPrefix(line, "0::/")
			if !strings.Contains(rel, "..") {
				return filepath.Join("/sys/fs/cgroup", rel), nil
			}
		}
	}
	return "", errors.New("cgroup v2 is unavailable")
}

func writeCgroup(path, value string) error {
	// Do not create ordinary files if a controller is absent.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteString(value)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func hasController(path, name string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, controller := range strings.Fields(string(b)) {
		if controller == name {
			return true
		}
	}
	return false
}

// Enabling a domain controller requires an empty parent cgroup. Keep the API
// process in a leaf and put bounded workers in sibling leaves of the delegated
// service subtree. Do this before spawning workers, never in their inherited leaf.
func prepareWorkerParent(allow bool) error {
	cgroupSetup.Lock()
	defer cgroupSetup.Unlock()
	err := prepareCgroup()
	if err == nil {
		current, e := currentCgroup()
		if e == nil {
			var group string
			group, e = makeWorkerCgroup(current, fmt.Sprintf("cyapi-preflight-%d", os.Getpid()))
			if e == nil {
				e = os.Remove(group)
			}
		}
		err = e
	}
	if allow {
		return nil
	}
	return err
}

func prepareCgroup() error {
	current, err := currentCgroup()
	if err != nil {
		return err
	}
	if filepath.Base(current) == runtimeCgroup {
		if hasController(filepath.Join(filepath.Dir(current), "cgroup.subtree_control"), "memory") {
			return nil
		}
		return errors.New("delegated memory controller is not enabled")
	}
	if !hasController(filepath.Join(current, "cgroup.controllers"), "memory") {
		return errors.New("service requires cgroup v2 Delegate=memory")
	}
	leaf := filepath.Join(current, runtimeCgroup)
	if err = os.Mkdir(leaf, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create delegated runtime cgroup: %w", err)
	}
	pid := fmt.Sprint(os.Getpid())
	if err = writeCgroup(filepath.Join(leaf, "cgroup.procs"), pid); err != nil {
		_ = os.Remove(leaf)
		return fmt.Errorf("move runtime into delegated leaf: %w", err)
	}
	if err = writeCgroup(filepath.Join(current, "cgroup.subtree_control"), "+memory"); err != nil {
		_ = writeCgroup(filepath.Join(current, "cgroup.procs"), pid)
		_ = os.Remove(leaf)
		return fmt.Errorf("enable delegated memory controller: %w", err)
	}
	return nil
}

func workerCgroupRoot(current string) string {
	if filepath.Base(current) == runtimeCgroup {
		return filepath.Dir(current)
	}
	return current
}

func makeWorkerCgroup(current, name string) (string, error) {
	child := filepath.Join(workerCgroupRoot(current), name)
	if err := os.Mkdir(child, 0700); err != nil {
		return "", err
	}
	err := writeCgroup(filepath.Join(child, "memory.max"), "536870912")
	if err == nil {
		err = writeCgroup(filepath.Join(child, "memory.swap.max"), "0")
	}
	if err != nil {
		_ = os.Remove(child)
		return "", fmt.Errorf("configure worker memory limit: %w", err)
	}
	return child, nil
}

// A delegated cgroup-v2 subtree is required for a hard per-worker memory limit.
func isolateWorker(allow bool) (string, func(), error) {
	current, err := currentCgroup()
	if err == nil {
		var child string
		child, err = makeWorkerCgroup(current, fmt.Sprintf("cyapi-probe-%d", os.Getpid()))
		if err == nil {
			pid := fmt.Sprint(os.Getpid())
			cleanup := func() { _ = writeCgroup(filepath.Join(current, "cgroup.procs"), pid); _ = os.Remove(child) }
			err = writeCgroup(filepath.Join(child, "cgroup.procs"), pid)
			if err == nil {
				return "cgroup_v2_512MiB", cleanup, nil
			}
			cleanup()
		}
	}
	if allow {
		return "explicitly_unconfined", func() {}, nil
	}
	return "", func() {}, fmt.Errorf("worker requires delegated cgroup v2 memory controller: %w", err)
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
	current, err := currentCgroup()
	if err == nil {
		_ = os.Remove(filepath.Join(workerCgroupRoot(current), fmt.Sprintf("cyapi-probe-%d", pid)))
	}
}
