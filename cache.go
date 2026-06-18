package main

import (
	"sync"
	"time"
)

type cacheEntry[T any] struct {
	val    T
	expiry time.Time
}

// ttlCache is a tiny mutex-guarded map with per-entry expiry. Lazy eviction on
// read keeps it dependency-free; the working set (recently previewed URLs and
// validated tokens) is small, so we don't need a background sweeper.
type ttlCache[T any] struct {
	mu sync.Mutex
	m  map[string]cacheEntry[T]
}

func newTTLCache[T any]() *ttlCache[T] {
	return &ttlCache[T]{m: make(map[string]cacheEntry[T])}
}

func (c *ttlCache[T]) get(k string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		var zero T
		return zero, false
	}
	if time.Now().After(e.expiry) {
		delete(c.m, k)
		var zero T
		return zero, false
	}
	return e.val, true
}

func (c *ttlCache[T]) put(k string, v T, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = cacheEntry[T]{val: v, expiry: time.Now().Add(ttl)}
}
