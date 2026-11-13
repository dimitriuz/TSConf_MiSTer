package main

import (
	"container/list"
	"fmt"
	"os"
	"sync"
)

// SourceOptions tunes the directory and zip sources.
type SourceOptions struct {
	CacheMB int // zip: total size of inflated entries kept in memory (default 64)
	FDLimit int // directory: open descriptors kept (default 64)
}

func (o SourceOptions) withDefaults() SourceOptions {
	if o.CacheMB <= 0 {
		o.CacheMB = 64
	}
	if o.FDLimit <= 0 {
		o.FDLimit = 64
	}
	return o
}

// windowSize is the read-ahead / sliding window size of both sources.
const windowSize = 256 << 10

// fdEntry is one file of the descriptor cache. mu guards f and the window.
type fdEntry struct {
	id   int
	refs int           // guarded by fdCache.mu
	elem *list.Element // guarded by fdCache.mu

	mu     sync.Mutex
	f      *os.File
	win    []byte
	winOff int64
	winLen int
}

// fdCache keeps at most limit descriptors open (more only while every one of
// them is in use). A descriptor is closed only when nobody holds the entry.
type fdCache struct {
	mu    sync.Mutex
	limit int
	m     map[int]*fdEntry
	lru   *list.List // front = most recent
}

func newFDCache(limit int) *fdCache {
	return &fdCache{limit: limit, m: map[int]*fdEntry{}, lru: list.New()}
}

func (c *fdCache) get(id int) *fdEntry {
	c.mu.Lock()
	e := c.m[id]
	if e == nil {
		e = &fdEntry{id: id}
		e.elem = c.lru.PushFront(e)
		c.m[id] = e
	} else {
		c.lru.MoveToFront(e.elem)
	}
	e.refs++
	victims := c.shrinkLocked()
	c.mu.Unlock()
	closeEntries(victims)
	return e
}

func (c *fdCache) put(e *fdEntry) {
	c.mu.Lock()
	e.refs--
	victims := c.shrinkLocked()
	c.mu.Unlock()
	closeEntries(victims)
}

func (c *fdCache) shrinkLocked() []*fdEntry {
	var victims []*fdEntry
	for el := c.lru.Back(); el != nil && len(c.m) > c.limit; {
		prev := el.Prev()
		e := el.Value.(*fdEntry)
		if e.refs == 0 {
			c.lru.Remove(el)
			delete(c.m, e.id)
			victims = append(victims, e)
		}
		el = prev
	}
	return victims
}

func closeEntries(es []*fdEntry) {
	for _, e := range es {
		e.mu.Lock()
		if e.f != nil {
			e.f.Close()
			e.f = nil
		}
		e.win = nil
		e.mu.Unlock()
	}
}

func (c *fdCache) closeAll() {
	c.mu.Lock()
	var all []*fdEntry
	for _, e := range c.m {
		all = append(all, e)
	}
	c.m = map[int]*fdEntry{}
	c.lru.Init()
	c.mu.Unlock()
	closeEntries(all)
}

// openCount returns the number of cached entries (for tests).
func (c *fdCache) openCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// zentry is one inflated entry of the byte cache. done closes when data/err
// are set.
type zentry struct {
	id   int
	done chan struct{}
	data []byte
	err  error
	elem *list.Element
}

// byteCache is an LRU of inflated entries bounded by total bytes, with one
// load per entry at a time (singleflight).
type byteCache struct {
	mu     sync.Mutex
	budget int64
	total  int64
	peak   int64 // highest total seen, before eviction
	m      map[int]*zentry
	lru    *list.List
}

func newByteCache(budget int64) *byteCache {
	return &byteCache{budget: budget, m: map[int]*zentry{}, lru: list.New()}
}

// get returns the data of entry id, calling load once for concurrent callers
// of a cold entry. Failed loads are not cached.
func (c *byteCache) get(id int, load func() ([]byte, error)) ([]byte, error) {
	c.mu.Lock()
	if e := c.m[id]; e != nil {
		if e.elem != nil {
			c.lru.MoveToFront(e.elem)
		}
		c.mu.Unlock()
		<-e.done
		return e.data, e.err
	}
	e := &zentry{id: id, done: make(chan struct{})}
	c.m[id] = e
	c.mu.Unlock()

	var data []byte
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				data, err = nil, fmt.Errorf("tsshare: cache load panicked: %v", r)
			}
		}()
		data, err = load()
	}()

	c.mu.Lock()
	e.data, e.err = data, err
	if err != nil {
		delete(c.m, id)
	} else {
		e.elem = c.lru.PushFront(e)
		c.total += int64(len(data))
		if c.total > c.peak {
			c.peak = c.total
		}
		for c.total > c.budget && c.lru.Len() > 1 {
			el := c.lru.Back()
			v := el.Value.(*zentry)
			c.lru.Remove(el)
			delete(c.m, v.id)
			c.total -= int64(len(v.data))
		}
	}
	c.mu.Unlock()
	close(e.done)
	return data, err
}
