package sourcecheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

type Executor struct {
	Executable      string
	AllowUnconfined bool
}

func (e Executor) Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := prepareWorkerParent(e.AllowUnconfined); err != nil {
		return fmt.Errorf("%w: %v", ErrEnvironment, err)
	}
	return nil
}

type cappedWriter struct {
	bytes.Buffer
	limit    int
	exceeded bool
	cancel   context.CancelFunc
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.Len() {
		w.exceeded = true
		w.cancel()
		return 0, ErrLimit
	}
	return w.Buffer.Write(p)
}

func (e Executor) Run(ctx context.Context, j Job) Result {
	start := time.Now()
	r := resultFor(j)
	fail := func(err error) Result {
		r.FailureOrigin = "probe"
		r.Reason = safe(err)
		r.ElapsedMS = time.Since(start).Milliseconds()
		return r
	}
	if err := j.validate(); err != nil {
		return fail(err)
	}
	if err := e.Preflight(ctx); err != nil {
		return fail(err)
	}
	path := e.Executable
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return fail(err)
		}
	}
	work, cancel := context.WithTimeout(ctx, TaskTimeout)
	defer cancel()
	args := []string{"_source-probe-worker"}
	if e.AllowUnconfined {
		args = append(args, "--allow-unconfined-worker")
	}
	cmd := exec.CommandContext(work, path, args...)
	cmd.WaitDelay = 2 * time.Second
	// No service tokens, provider cookies or configured proxy enter the worker.
	for _, key := range []string{"SystemRoot", "WINDIR", "TMP", "TEMP", "TMPDIR"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	cmd.Env = append(cmd.Env, "GOMEMLIMIT=384MiB", "GOMAXPROCS=1")
	dir, err := os.MkdirTemp("", "cyapi-probe-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(dir)
	cmd.Dir = dir
	b, err := json.Marshal(j)
	if err != nil {
		return fail(err)
	}
	cmd.Stdin = bytes.NewReader(b)
	out := &cappedWriter{limit: 256 << 10, cancel: cancel}
	stderr := &cappedWriter{limit: 256 << 10, cancel: cancel}
	cmd.Stdout = out
	cmd.Stderr = stderr
	err = cmd.Run()
	if cmd.Process != nil {
		cleanupWorker(cmd.Process.Pid)
	}
	if out.exceeded || stderr.exceeded {
		return fail(errors.New("probe_limit: worker output"))
	}
	if work.Err() != nil {
		return fail(fmt.Errorf("worker terminated: %w", work.Err()))
	}
	if err != nil {
		return fail(fmt.Errorf("worker failed: %v %s", err, safe(stderr.String())))
	}
	var got Result
	if err = decodeLimited(bytes.NewReader(out.Bytes()), &got, 256<<10); err != nil {
		return fail(fmt.Errorf("invalid worker response: %w", err))
	}
	if got.Format != ProtocolVersion || got.JobID != j.ID || got.ScriptSHA256 != j.ScriptSHA256 || got.SampleID != j.Sample.ID || got.Platform != j.Sample.Platform || got.Quality != j.Quality || got.ScriptPath != j.ScriptPath || got.ProbeVersion != ProbeVersion {
		return fail(errors.New("worker identity mismatch"))
	}
	return got
}

// Keep protocol errors small and free of untrusted stack/output floods.
func WorkerError(w io.Writer, err error) { _, _ = io.WriteString(w, strings.TrimSpace(safe(err))+"\n") }
