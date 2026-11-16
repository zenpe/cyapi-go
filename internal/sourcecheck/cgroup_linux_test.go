package sourcecheck

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDelegatedCgroupWorkerLifecycle(t *testing.T) {
	if os.Getenv("CYAPI_DELEGATED_CGROUP_TEST") != "1" {
		t.Skip("requires a disposable non-root systemd service with Delegate=memory")
	}
	if os.Geteuid() == 0 {
		t.Fatal("test must prove delegation works without root")
	}
	executor := Executor{}
	if err := executor.Preflight(context.Background()); err != nil {
		t.Fatal(err)
	}
	current, err := currentCgroup()
	if err != nil || filepath.Base(current) != runtimeCgroup {
		t.Fatalf("runtime must occupy its own leaf: %s %v", current, err)
	}
	root := workerCgroupRoot(current)
	if b, err := os.ReadFile(filepath.Join(root, "cgroup.procs")); err != nil || len(strings.Fields(string(b))) != 0 {
		t.Fatalf("delegated parent contains processes: %s %v", b, err)
	}
	if !hasController(filepath.Join(root, "cgroup.subtree_control"), "memory") {
		t.Fatal("memory controller not enabled")
	}
	job := fixtureJob(t, `lx.send(lx.EVENT_NAMES.inited,{status:false,message:'isolated-worker-ran'});`)
	r := executor.Run(context.Background(), job)
	if r.Isolation != "cgroup_v2_512MiB" || r.FailureOrigin != "source" || !strings.Contains(r.Reason, "isolated-worker-ran") {
		t.Fatalf("bounded worker did not execute script: %+v", r)
	}
	job = fixtureJob(t, `setTimeout(()=>lx.send(lx.EVENT_NAMES.inited,{status:false}),10000);`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- executor.Run(ctx, job) }()
	found := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		groups, _ := filepath.Glob(filepath.Join(root, "cyapi-probe-*"))
		for _, group := range groups {
			memory, err := os.ReadFile(filepath.Join(group, "memory.max"))
			pids, _ := os.ReadFile(filepath.Join(group, "cgroup.procs"))
			if err == nil && strings.TrimSpace(string(memory)) == "536870912" && len(strings.Fields(string(pids))) == 1 {
				swap, err := os.ReadFile(filepath.Join(group, "memory.swap.max"))
				if err != nil || strings.TrimSpace(string(swap)) != "0" {
					t.Fatal("worker swap limit missing")
				}
				found = true
			}
		}
		if found {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case r := <-done:
		if !found || r.FailureOrigin != "probe" || !strings.Contains(r.Reason, "canceled") {
			t.Fatalf("worker cancellation failed: found=%v %+v", found, r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled worker was not reaped")
	}
	if groups, _ := filepath.Glob(filepath.Join(root, "cyapi-probe-*")); len(groups) != 0 {
		t.Fatalf("worker cgroups leaked: %v", groups)
	}
	// Force a real kernel OOM in a worker, then verify the API/test parent lives
	// and the parent's cleanup can remove the orphaned cgroup after SIGKILL.
	ctx, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "_source-probe-memory-fixture")
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	err = cmd.Wait()
	group := filepath.Join(root, "cyapi-probe-"+strconv.Itoa(pid))
	defer cleanupWorker(pid)
	if err == nil || ctx.Err() != nil {
		t.Fatalf("worker did not hit memory limit: %v", err)
	}
	events, readErr := os.ReadFile(filepath.Join(group, "memory.events"))
	fields := strings.Fields(string(events))
	oomKilled := false
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "oom_kill" && fields[i+1] != "0" {
			oomKilled = true
		}
	}
	if readErr != nil || !oomKilled {
		t.Fatalf("no kernel OOM evidence: %s %v", events, readErr)
	}
	cleanupWorker(pid)
	if _, err = os.Stat(group); !os.IsNotExist(err) {
		t.Fatalf("OOM cgroup leaked: %v", err)
	}
	if r = executor.Run(context.Background(), fixtureJob(t, `lx.send(lx.EVENT_NAMES.inited,{status:false,message:'still-alive'});`)); r.Isolation != "cgroup_v2_512MiB" || !strings.Contains(r.Reason, "still-alive") {
		t.Fatalf("parent did not recover after worker OOM: %+v", r)
	}
}
