package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
)

func securePath(path, account string, mode os.FileMode) error {
	return errors.New("managed permissions require Linux")
}
func checkRootOwned(path string, dir bool) error {
	return errors.New("managed installation requires Linux")
}
func serviceCommand(ctx context.Context, account, binary string, args ...string) (*exec.Cmd, error) {
	return nil, errors.New("service users require Linux")
}

func platformDefaults() {}
