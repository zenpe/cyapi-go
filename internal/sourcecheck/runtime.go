package sourcecheck

import (
	"context"
	"embed"
	"fmt"
	"github.com/dop251/goja"
	"net/http"
	"time"
)

//go:embed sandbox/*.js sandbox/LICENSE.flutter_js
var bridgeFiles embed.FS

type session struct {
	vm         *goja.Runtime
	ctx        context.Context
	cancel     context.CancelFunc
	jobs       chan func() error
	ready      map[string]any
	response   map[string]any
	asyncError string
	timers     map[int]*time.Timer
	requests   map[string]context.CancelFunc
	events     []map[string]any
	utilities  map[string]int
	client     *http.Client
	deadline   time.Time
	cpu        time.Duration
}

func newSession(parent context.Context, client *http.Client) (*session, error) {
	ctx, cancel := context.WithTimeout(parent, TaskTimeout)
	s := &session{vm: goja.New(), ctx: ctx, cancel: cancel, jobs: make(chan func() error, 256), timers: map[int]*time.Timer{}, requests: map[string]context.CancelFunc{}, utilities: map[string]int{}, client: client, deadline: time.Now().Add(12 * time.Second), cpu: 5 * time.Second}
	s.vm.SetMaxCallStackSize(2000)
	s.vm.Set("console", map[string]any{"log": func(...any) {}, "warn": func(...any) {}, "error": func(...any) {}})
	s.vm.Set("__lx_native_send__", func(call goja.FunctionCall) goja.Value {
		s.bridge(call.Argument(0).String(), call.Argument(1).Export())
		return goja.Undefined()
	})
	s.vm.Set("__lx_utils__", func(call goja.FunctionCall) goja.Value {
		a := obj(call.Argument(0).Export())
		s.utilities[str(a["operation"])]++
		return s.vm.ToValue(utility(a))
	})
	for _, name := range []string{"sandbox.js", "xhr.js", "browser.js", "fetch.js"} {
		b, e := bridgeFiles.ReadFile("sandbox/" + name)
		if e != nil {
			s.close()
			return nil, e
		}
		if e = s.eval(name, string(b)); e != nil {
			s.close()
			return nil, e
		}
	}
	return s, nil
}
func (s *session) close() {
	s.cancel()
	for _, t := range s.timers {
		t.Stop()
	}
	for _, c := range s.requests {
		c()
	}
	s.client.CloseIdleConnections()
}
func (s *session) eval(name, code string) error {
	finished := make(chan struct{})
	timer := time.AfterFunc(s.cpu, func() { s.vm.Interrupt("JS execution budget exceeded"); close(finished) })
	_, err := s.vm.RunScript(name, code)
	if !timer.Stop() {
		<-finished
	}
	s.vm.ClearInterrupt()
	return err
}
func (s *session) post(fn func() error) {
	select {
	case s.jobs <- fn:
	case <-s.ctx.Done():
	default:
		s.cancel()
	}
}
func (s *session) pump(until func() bool) error {
	timer := time.NewTimer(time.Until(s.deadline))
	defer timer.Stop()
	for !until() {
		if s.asyncError != "" {
			return fmt.Errorf("script error: %s", s.asyncError)
		}
		select {
		case fn := <-s.jobs:
			if err := fn(); err != nil {
				return err
			}
		case <-timer.C:
			return context.DeadlineExceeded
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
	return nil
}
func (s *session) bridge(name string, value any) {
	a := obj(value)
	switch name {
	case "lxOnInited":
		s.ready = a
	case "lxOnResponse":
		s.response = a
	case "lxOnError":
		s.asyncError = safe(value)
	case "lxSetTimeout":
		if len(s.timers) >= 256 {
			s.asyncError = "probe_limit: timer count"
			return
		}
		id := int(num(a["timerId"]))
		delay := time.Duration(num(a["delay"])) * time.Millisecond
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		if t := s.timers[id]; t != nil {
			t.Stop()
		}
		s.timers[id] = time.AfterFunc(delay, func() {
			s.post(func() error { delete(s.timers, id); return s.eval("timer", fmt.Sprintf("__lx_fireTimer__(%d)", id)) })
		})
	case "lxClearTimeout":
		id := int(num(a["timerId"]))
		if t := s.timers[id]; t != nil {
			t.Stop()
			delete(s.timers, id)
		}
	case "lxCancelHttp":
		if c := s.requests[str(a["requestId"])]; c != nil {
			c()
			delete(s.requests, str(a["requestId"]))
		}
	case "lxRequest":
		if len(s.requests)+len(s.events) >= 64 {
			s.asyncError = "probe_limit: request count"
			return
		}
		id := str(a["requestId"])
		deadline := s.deadline
		opts := obj(a["options"])
		if n := num(opts["timeout"]); n > 0 && time.Now().Add(time.Duration(n)*time.Millisecond).Before(deadline) {
			deadline = time.Now().Add(time.Duration(n) * time.Millisecond)
		}
		ctx, cancel := context.WithDeadline(s.ctx, deadline)
		if old := s.requests[id]; old != nil {
			old()
		}
		s.requests[id] = cancel
		go func() {
			result, event := performHTTP(ctx, s.client, str(a["url"]), opts)
			cancel()
			result["requestId"] = id
			s.post(func() error {
				delete(s.requests, id)
				s.events = append(s.events, event)
				return s.eval("http-callback", "__lx_handleHttpResponse__("+jsJSON(result)+")")
			})
		}()
	}
}
