package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"cyapi-go/internal/config"
	"cyapi-go/internal/sourcecheck"
	"cyapi-go/internal/sources"
)

func sourceProbeWorker(args []string) error {
	allow := false
	if len(args) > 0 {
		if len(args) != 1 || args[0] != "--allow-unconfined-worker" {
			return errors.New("invalid worker arguments")
		}
		allow = true
	}
	return sourcecheck.WorkerMain(context.Background(), os.Stdin, os.Stdout, allow)
}

func runSourceCheck(ctx context.Context, c config.Config, manager *sources.Manager, path, platform, samples string, allow bool, log *slog.Logger) error {
	service := sourcecheck.Service{Directory: filepath.Join(c.RuntimeDirectory, "source-checks"), Sources: manager, Runner: sourcecheck.Executor{AllowUnconfined: allow}}
	report, err := service.Run(ctx, sourcecheck.RunOptions{Path: path, Platform: platform, SamplesFile: samples, Progress: func(r sourcecheck.Result, completed, total int) {
		log.Info("source check progress", "completed", completed, "total", total, "path", r.ScriptPath, "platform", r.Platform, "sample", r.SampleID, "initialization", r.Initialization, "resolution", r.Resolution, "resource", r.Resource.Status, "assessment", r.Resource.Assessment, "reason", r.Reason, "elapsedMs", r.ElapsedMS)
	}})
	if report.RunID != "" {
		if e := json.NewEncoder(os.Stdout).Encode(report); e != nil {
			return e
		}
		if err == nil || errors.Is(err, context.Canceled) {
			log.Info("source check report saved", "runId", report.RunID, "state", report.State, "path", filepath.Join(service.Directory, "runs", report.RunID+".json"))
		} else {
			log.Warn("source check stopped; persisted report may be incomplete", "runId", report.RunID, "error", err.Error())
		}
	}
	return err
}
