package router

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/sausheong/harness/llm"
)

const (
	defaultClassifierCacheCapacity = 1024
	defaultClassifierCacheTTL      = 5 * time.Minute
)

// ClassifierCacheStats is a point-in-time view of the process-local classifier
// cache. Misses count provider calls, while Coalesced counts callers that shared
// an already-running call for the same key.
type ClassifierCacheStats struct {
	Hits      uint64
	Misses    uint64
	Coalesced uint64
	Stores    uint64
	Evictions uint64
	Entries   int
}

type classifierCacheEntry struct {
	key       string
	profile   TaskProfile
	expiresAt time.Time
}

type classifierFlight struct {
	done    chan struct{}
	profile TaskProfile
	ok      bool
}

// classifierMemo stores only hashed keys and parsed profiles. In particular,
// it never retains the classifier input or any user prompt text.
type classifierMemo struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	entries  map[string]*list.Element
	lru      *list.List
	flights  map[string]*classifierFlight
	stats    ClassifierCacheStats
}

func newClassifierMemo(capacity int, ttl time.Duration) *classifierMemo {
	return &classifierMemo{
		capacity: capacity,
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]*list.Element),
		lru:      list.New(),
		flights:  make(map[string]*classifierFlight),
	}
}

// do returns cached=true only for a completed cache hit. A coalesced waiter
// receives the shared profile but no usage, because only one provider request
// was made and its usage is attributed to the caller that made it.
func (c *classifierMemo) do(
	ctx context.Context,
	key string,
	load func(context.Context) (TaskProfile, *llm.Usage, bool),
) (profile TaskProfile, usage *llm.Usage, cached, coalesced bool) {
	c.mu.Lock()
	if elem, found := c.entries[key]; found {
		entry := elem.Value.(*classifierCacheEntry)
		if c.now().Before(entry.expiresAt) {
			c.lru.MoveToFront(elem)
			c.stats.Hits++
			profile = entry.profile
			c.mu.Unlock()
			return profile, nil, true, false
		}
		c.removeLocked(elem)
	}
	if flight, found := c.flights[key]; found {
		c.stats.Coalesced++
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return DefaultProfile(), nil, false, true
		case <-flight.done:
			if !flight.ok {
				return DefaultProfile(), nil, false, true
			}
			return flight.profile, nil, false, true
		}
	}

	flight := &classifierFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.stats.Misses++
	c.mu.Unlock()

	profile, usage, ok := load(ctx)

	c.mu.Lock()
	if ok && c.capacity > 0 && c.ttl > 0 {
		c.storeLocked(key, profile)
	}
	flight.profile = profile
	flight.ok = ok
	delete(c.flights, key)
	close(flight.done)
	c.mu.Unlock()

	return profile, usage, false, false
}

func (c *classifierMemo) storeLocked(key string, profile TaskProfile) {
	if elem, found := c.entries[key]; found {
		entry := elem.Value.(*classifierCacheEntry)
		entry.profile = profile
		entry.expiresAt = c.now().Add(c.ttl)
		c.lru.MoveToFront(elem)
		return
	}
	entry := &classifierCacheEntry{key: key, profile: profile, expiresAt: c.now().Add(c.ttl)}
	c.entries[key] = c.lru.PushFront(entry)
	c.stats.Stores++
	for c.lru.Len() > c.capacity {
		c.removeLocked(c.lru.Back())
		c.stats.Evictions++
	}
}

func (c *classifierMemo) removeLocked(elem *list.Element) {
	if elem == nil {
		return
	}
	entry := elem.Value.(*classifierCacheEntry)
	delete(c.entries, entry.key)
	c.lru.Remove(elem)
}

func (c *classifierMemo) snapshot() ClassifierCacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	stats := c.stats
	stats.Entries = len(c.entries)
	return stats
}

func (c *classifierMemo) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]*list.Element)
	c.lru.Init()
	c.stats = ClassifierCacheStats{}
}

var processClassifierCache = newClassifierMemo(defaultClassifierCacheCapacity, defaultClassifierCacheTTL)

// CurrentClassifierCacheStats returns process-local cache counters for
// diagnostics and Insights integration.
func CurrentClassifierCacheStats() ClassifierCacheStats {
	return processClassifierCache.snapshot()
}
