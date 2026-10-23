// Package tilecache provides a byte-bounded, in-memory LRU cache for
// encoded MVT tiles.
package tilecache

import (
	"container/list"
	"sync"
)

type entry struct {
	key   string
	value []byte
}

type Stats struct {
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Evictions uint64 `json:"evictions"`
	Entries   int    `json:"entries"`
	Bytes     int64  `json:"bytes"`
	MaxBytes  int64  `json:"max_bytes"`
}

type Cache struct {
	mu       sync.Mutex
	maxBytes int64
	bytes    int64
	ll       *list.List
	items    map[string]*list.Element
	hits     uint64
	misses   uint64
	evicts   uint64
}

// New returns a cache bounded by maxBytes. Entries larger than the whole
// budget are not cached.
func New(maxBytes int64) *Cache {
	if maxBytes < 0 {
		maxBytes = 0
	}
	return &Cache{
		maxBytes: maxBytes,
		ll:       list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (c *Cache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	c.ll.MoveToFront(el)
	return el.Value.(*entry).value, true
}

func (c *Cache) Put(key string, value []byte) {
	if len(value) == 0 || c.maxBytes <= 0 || int64(len(value)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry)
		c.bytes += int64(len(value)) - int64(len(e.value))
		e.value = value
		c.ll.MoveToFront(el)
	} else {
		el := c.ll.PushFront(&entry{key: key, value: value})
		c.items[key] = el
		c.bytes += int64(len(value))
	}
	for c.bytes > c.maxBytes {
		c.removeOldestLocked()
	}
}

func (c *Cache) removeOldestLocked() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	e := el.Value.(*entry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.bytes -= int64(len(e.value))
	c.evicts++
}

func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits:      c.hits,
		Misses:    c.misses,
		Evictions: c.evicts,
		Entries:   len(c.items),
		Bytes:     c.bytes,
		MaxBytes:  c.maxBytes,
	}
}
