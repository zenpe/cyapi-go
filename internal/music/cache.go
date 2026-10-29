package music

import (
	"container/list"
	"context"
	"sync"
	"time"
)

type Response struct {
	Status                            int
	Body                              []byte
	ContentType, Location, RetryAfter string
	TTL                               time.Duration
	Cacheable                         bool
}
type entry struct {
	key      string
	response Response
	expires  time.Time
	weight   int64
}
type call struct {
	done     chan struct{}
	cancel   context.CancelFunc
	refs     int
	finished bool
	response Response
}
type cache struct {
	mu                      sync.Mutex
	items                   map[string]*list.Element
	lru                     *list.List
	calls                   map[string]*call
	used, maxBytes, maxItem int64
	maxEntries              int
	now                     func() time.Time
}

func newCache(bytes int64, entries int, item int64) *cache {
	return &cache{items: map[string]*list.Element{}, lru: list.New(), calls: map[string]*call{}, maxBytes: bytes, maxEntries: entries, maxItem: item, now: time.Now}
}
func (c *cache) remove(e *list.Element) {
	v := e.Value.(*entry)
	delete(c.items, v.key)
	c.used -= v.weight
	c.lru.Remove(e)
}
func (c *cache) get(key string) (Response, bool) {
	e := c.items[key]
	if e == nil {
		return Response{}, false
	}
	v := e.Value.(*entry)
	if !c.now().Before(v.expires) {
		c.remove(e)
		return Response{}, false
	}
	c.lru.MoveToFront(e)
	return v.response, true
}
func (c *cache) put(key string, r Response) {
	weight := int64(len(r.Body) + len(r.Location) + len(r.ContentType) + len(r.RetryAfter) + len(key) + 128)
	if !r.Cacheable || r.TTL <= 0 || r.Status != 200 || c.maxEntries <= 0 || weight > c.maxItem || weight > c.maxBytes {
		return
	}
	if old := c.items[key]; old != nil {
		c.remove(old)
	}
	for c.lru.Len() >= c.maxEntries || c.used+weight > c.maxBytes {
		c.remove(c.lru.Back())
	}
	v := &entry{key, r, c.now().Add(r.TTL), weight}
	c.items[key] = c.lru.PushFront(v)
	c.used += weight
}

// Calls share a bounded task. Cancellation of the last waiter cancels upstream work.
func (c *cache) load(ctx context.Context, key string, bypass bool, timeout time.Duration, build func(context.Context) Response) (Response, string) {
	if bypass {
		work, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return build(work), "BYPASS"
	}
	c.mu.Lock()
	if r, ok := c.get(key); ok {
		c.mu.Unlock()
		return r, "HIT"
	}
	f := c.calls[key]
	label := "COALESCED"
	if f == nil {
		if len(c.calls) >= 32 {
			c.mu.Unlock()
			return jsonResponse(503, map[string]any{"status": 503, "code": "MUSIC_BUSY", "message": "音乐服务繁忙，请稍后重试"}), "MISS"
		}
		label = "MISS"
		work, cancel := context.WithTimeout(context.Background(), timeout)
		f = &call{done: make(chan struct{}), cancel: cancel}
		c.calls[key] = f
		go func() {
			r := build(work)
			c.mu.Lock()
			f.response = r
			f.finished = true
			if c.calls[key] == f {
				delete(c.calls, key)
				if work.Err() == nil {
					c.put(key, r)
				}
			}
			close(f.done)
			c.mu.Unlock()
			cancel()
		}()
	}
	f.refs++
	c.mu.Unlock()
	select {
	case <-f.done:
		return f.response, label
	case <-ctx.Done():
		c.mu.Lock()
		f.refs--
		if f.refs == 0 && !f.finished {
			f.cancel()
			if c.calls[key] == f {
				delete(c.calls, key)
			}
		}
		c.mu.Unlock()
		return jsonResponse(504, map[string]any{"status": 504, "code": "MUSIC_TIMEOUT", "message": "请求超时，请重试"}), label
	}
}
