package sourcecheck

import (
	"context"
	"testing"
	"time"
)

func TestControllerPersistsCancellationOrigin(t *testing.T) {
	for _, tc := range []struct{ origin, state, reason string }{
		{"user", "canceled", "user"},
		{"parent_shutdown", "interrupted", "shutdown"},
		{"close", "interrupted", "shutdown"},
		{"control", "interrupted", "control_changed"},
		{"timeout", "canceled", "timeout"},
	} {
		t.Run(tc.origin, func(t *testing.T) {
			manager, _ := sourceFixture(t)
			entered := make(chan struct{})
			svc := &Service{Directory: t.TempDir(), Sources: manager, Runner: runnerFunc(func(ctx context.Context, j Job) Result {
				close(entered)
				<-ctx.Done()
				return resultFor(j)
			})}
			parent, stopParent := context.WithCancel(context.Background())
			defer stopParent()
			controller := NewController(parent, svc)
			defer controller.Close(context.Background())
			opts := RunOptions{Platform: "qq"}
			if tc.origin == "timeout" {
				opts.Timeout = 100 * time.Millisecond
			}
			id, _, err := controller.Start(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not start")
			}
			switch tc.origin {
			case "user":
				err = controller.Cancel(id)
			case "control":
				err = controller.Interrupt(id)
			case "parent_shutdown":
				stopParent()
			case "close":
				err = controller.Close(context.Background())
			}
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for controller.ActiveID() != "" {
				if time.Now().After(deadline) {
					t.Fatal("worker did not stop")
				}
				time.Sleep(time.Millisecond)
			}
			report, err := svc.Report(id)
			if err != nil || report.State != tc.state || report.CancellationReason != tc.reason {
				t.Fatal(report.State, report.CancellationReason, err)
			}
		})
	}
}
