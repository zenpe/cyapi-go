package music

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("condition timed out")
		}
	}
}
func TestCacheCapacityTTLAndBypass(t *testing.T) {
	c := newCache(1000, 2, 500)
	now := time.Now()
	c.now = func() time.Time { return now }
	calls := 0
	build := func(context.Context) Response {
		calls++
		return Response{Status: 200, Body: []byte("response"), Cacheable: true, TTL: time.Minute}
	}
	c.load(context.Background(), "a", false, time.Second, build)
	if _, label := c.load(context.Background(), "a", false, time.Second, build); label != "HIT" || calls != 1 {
		t.Fatal(label, calls)
	}
	c.load(context.Background(), "b", false, time.Second, build)
	c.load(context.Background(), "c", false, time.Second, build)
	if c.used > c.maxBytes || len(c.items) != 2 {
		t.Fatal("cache exceeded limits")
	}
	c.load(context.Background(), "a", false, time.Second, build)
	if calls != 4 {
		t.Fatal("LRU did not evict oldest", calls)
	}
	now = now.Add(2 * time.Minute)
	c.load(context.Background(), "a", false, time.Second, build)
	if calls != 5 {
		t.Fatal("TTL not enforced", calls)
	}
	if _, label := c.load(context.Background(), "a", true, time.Second, build); label != "BYPASS" || calls != 6 {
		t.Fatal(label, calls)
	}
	large := func(context.Context) Response {
		return Response{Status: 200, Body: make([]byte, 501), Cacheable: true, TTL: time.Minute}
	}
	c.load(context.Background(), "large", false, time.Second, large)
	c.mu.Lock()
	_, cached := c.get("large")
	c.mu.Unlock()
	if cached {
		t.Fatal("oversized item cached")
	}
}
func TestCoalescingAndLastWaiterCancellation(t *testing.T) {
	c := newCache(1<<20, 100, 1<<20)
	release := make(chan struct{})
	started := make(chan struct{})
	var calls atomic.Int32
	build := func(ctx context.Context) Response {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return Response{Status: 200, Body: []byte("ok"), Cacheable: true, TTL: time.Minute}
		case <-ctx.Done():
			return Response{Status: 504}
		}
	}
	var wg sync.WaitGroup
	results := make(chan Response, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := c.load(context.Background(), "same", false, time.Second, build)
			results <- r
		}()
	}
	<-started
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.calls["same"] != nil && c.calls["same"].refs == 12
	})
	close(release)
	wg.Wait()
	close(results)
	for r := range results {
		if r.Status != 200 {
			t.Fatal(r)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("upstream stampede", calls.Load())
	}
	c = newCache(1<<20, 100, 1<<20)
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.load(ctx, "cancel", false, time.Minute, func(work context.Context) Response {
			close(entered)
			<-work.Done()
			close(cancelled)
			return Response{Status: 504}
		})
		close(done)
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waiter did not cancel")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("orphaned upstream request")
	}
}
func TestCancellingOneWaiterPreservesOthers(t *testing.T) {
	c := newCache(1<<20, 100, 1<<20)
	entered := make(chan struct{})
	release := make(chan struct{})
	build := func(ctx context.Context) Response {
		close(entered)
		select {
		case <-release:
			return Response{Status: 200, Body: []byte("ok")}
		case <-ctx.Done():
			return Response{Status: 504}
		}
	}
	first, cancel := context.WithCancel(context.Background())
	doneFirst := make(chan Response, 1)
	doneSecond := make(chan Response, 1)
	go func() { r, _ := c.load(first, "shared", false, time.Second, build); doneFirst <- r }()
	<-entered
	go func() { r, _ := c.load(context.Background(), "shared", false, time.Second, build); doneSecond <- r }()
	waitFor(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.calls["shared"] != nil && c.calls["shared"].refs == 2
	})
	cancel()
	if r := <-doneFirst; r.Status != 504 {
		t.Fatal(r)
	}
	close(release)
	if r := <-doneSecond; r.Status != 200 {
		t.Fatal("one cancellation affected another client", r)
	}
}
