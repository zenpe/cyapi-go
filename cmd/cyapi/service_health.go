package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

func verifyServiceDefinition(i installation) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, e := exec.CommandContext(ctx, "systemctl", "show", i.Unit, "-p", "User", "--value").Output()
	if e != nil || strings.TrimSpace(string(b)) != i.User {
		return errors.New("existing service user differs from installation; inspect systemctl cat before repair")
	}
	b, e = exec.CommandContext(ctx, "systemctl", "show", i.Unit, "-p", "ExecStart", "--value").Output()
	if e != nil || !strings.Contains(string(b), "path="+i.Binary+" ;") || !strings.Contains(string(b), "--config "+i.Config) {
		return errors.New("existing service command differs from installation")
	}
	return nil
}
func waitManagedReady(i installation, listen, expected string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		b, e := exec.CommandContext(ctx, "systemctl", "show", i.Unit, "-p", "MainPID", "--value").Output()
		cancel()
		pid := strings.TrimSpace(string(b))
		if e == nil && pid != "" && pid != "0" {
			client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := client.Get(healthOrigin(listen) + "/health/ready")
			if err == nil {
				ok := res.StatusCode == 200 && res.Header.Get("X-Cyapi-PID") == pid && (expected == "" || res.Header.Get("X-Cyapi-Version") == expected)
				res.Body.Close()
				if ok && serviceActive(i.Unit) {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("target service %s did not become ready on %s", i.Unit, listen)
		}
		time.Sleep(250 * time.Millisecond)
	}
}
