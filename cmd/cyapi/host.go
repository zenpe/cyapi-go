package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

func runtimeVersion() string { return runtime.Version() }
func targetOS() string       { return runtime.GOOS }
func targetArch() string     { return runtime.GOARCH }

func acquireLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return nil, errors.New("lock path must be a regular file")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = platformLock(f); err != nil {
		f.Close()
		return nil, errors.New("another API or management operation holds the instance lock")
	}
	var once sync.Once
	return func() { once.Do(func() { platformUnlock(f); f.Close() }) }, nil
}
func writeNew(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err = f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".cyapi-write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(name, path)
}
func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(b, '\n'), 0600)
}
func regularDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("managed path must be a real directory")
	}
	return nil
}

type rotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func newRotatingLog(path string) (*rotatingLog, error) {
	if err := regularDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	r := &rotatingLog{path: path}
	err := r.open()
	return r, err
}
func (r *rotatingLog) open() error {
	if info, e := os.Lstat(r.path); e == nil && !info.Mode().IsRegular() {
		return errors.New("log path must be a regular file")
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.file = f
	r.size = info.Size()
	return nil
}
func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > 10<<20 {
		if err := r.file.Close(); err != nil {
			return 0, err
		}
		r.file = nil
		for i := 3; i >= 1; i-- {
			dst := r.path + string(rune('0'+i))
			src := r.path
			if i > 1 {
				src += string(rune('0' + i - 1))
			}
			if _, e := os.Lstat(dst); e == nil {
				if e = os.Remove(dst); e != nil {
					return 0, e
				}
			}
			if e := os.Rename(src, dst); e != nil && !errors.Is(e, os.ErrNotExist) {
				return 0, e
			}
		}
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	r.size += int64(n)
	return n, err
}
func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	e := r.file.Close()
	r.file = nil
	return e
}

var _ io.WriteCloser = (*rotatingLog)(nil)
