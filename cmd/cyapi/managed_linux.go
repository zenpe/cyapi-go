package main

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

func securePath(path, account string, mode os.FileMode) error {
	info, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return fmt.Errorf("refusing non-regular managed path: %s", path)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && !info.IsDir() && st.Nlink != 1 {
		return fmt.Errorf("refusing hard-linked managed file: %s", path)
	}
	u, e := user.Lookup(account)
	if e != nil {
		return e
	}
	uid, e := strconv.Atoi(u.Uid)
	if e != nil {
		return e
	}
	gid, e := strconv.Atoi(u.Gid)
	if e != nil {
		return e
	}
	if e = os.Chown(path, uid, gid); e != nil {
		return fmt.Errorf("chown %s: %w", path, e)
	}
	// Program-managed business directories must not propagate inherited default
	// ACLs that make newly created files unreadable by their own service user.
	if info.IsDir() && account != "root" {
		e = unix.Removexattr(path, "system.posix_acl_default")
		if e != nil && !errors.Is(e, unix.ENODATA) && !errors.Is(e, unix.ENOTSUP) {
			return fmt.Errorf("clear inherited default ACL at %s: %w", path, e)
		}
	}
	if e = os.Chmod(path, mode); e != nil {
		return fmt.Errorf("chmod %s: %w", path, e)
	}
	return nil
}
func checkRootOwned(path string, dir bool) error {
	info, e := os.Lstat(path)
	if e != nil {
		return e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || info.Mode().Perm()&0022 != 0 || info.IsDir() != dir || (!dir && !info.Mode().IsRegular()) {
		return fmt.Errorf("management path must be root-owned and not writable by other users: %s", path)
	}
	return nil
}
func serviceCommand(ctx context.Context, account, binary string, args ...string) (*exec.Cmd, error) {
	u, e := user.Lookup(account)
	if e != nil {
		return nil, e
	}
	uid, e := strconv.ParseUint(u.Uid, 10, 32)
	if e != nil || uid == 0 {
		return nil, fmt.Errorf("invalid service UID for %s", account)
	}
	gid, e := strconv.ParseUint(u.Gid, 10, 32)
	if e != nil {
		return nil, e
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	if os.Geteuid() == 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{uint32(gid)}}}
	} else if os.Geteuid() != int(uid) {
		return nil, fmt.Errorf("run with sudo to check service user %s", account)
	}
	// Match systemd's environment instead of inheriting temporary shell credentials/config overrides.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + u.HomeDir, "USER=" + account, "LOGNAME=" + account, "LANG=C.UTF-8"}
	cmd.Dir = "/"
	return cmd, nil
}

func platformDefaults() { unix.Umask(0077) }
