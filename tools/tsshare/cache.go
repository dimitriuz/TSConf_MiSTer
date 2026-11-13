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

// blockSize is the unit in which inflated entries are stored and recycled.
const blockSize = 16 << 10

// cdata is the inflated contents of one entry: n bytes in blockSize blocks.
type cdata struct {
	blk [][]byte
	n   int
}

// copyAt copies from offset off into p and reports the byte count.
func (d *cdata) copyAt(p []byte, off int64) int {
	total := 0
	for total < len(p) && off < int64(d.n) {
		bi, bo := int(off/blockSize), int(off%blockSize)
		end := min(blockSize, d.n-bi*blockSize)
		k := copy(p[total:], d.blk[bi][bo:end])
		total += k
		off += int64(k)
	}
	return total
}

// bytes returns the contents as one new slice.
func (d *cdata) bytes() []byte {
	out := make([]byte, d.n)
	d.copyAt(out, 0)
	return out
}

// cdataOf wraps b (copied into blocks).
func cdataOf(b []byte) *cdata {
	d := &cdata{n: len(b)}
	for off := 0; off < len(b); off += blockSize {
		blk := make([]byte, blockSize)
		copy(blk, b[off:])
		d.blk = append(d.blk, blk)
	}
	return d
}

func (d *cdata) size() int64 { return int64(len(d.blk)) * blockSize }

// zentry is one inflated entry of the byte cache. done closes when data/err
// are set.
type zentry struct {
	id   int
	done chan struct{}
	data *cdata
	err  error
	elem *list.Element

	// guarded by byteCache.mu
	pins   int  // readers inside with()
	dead   bool // evicted while pinned: the last release recycles data
	shared bool // data escaped through get(): never recycle
}

// byteCache is an LRU of inflated entries bounded by total bytes (of blocks,
// so the bound is real memory), with one load per entry at a time
// (singleflight).
//
// Entries live in 16 KiB blocks and the blocks of evicted entries are
// recycled through a free list bounded by freeMax, so reading a large archive
// does not turn every inflated entry into garbage (a plain []byte per entry
// let the Go heap grow to about twice the cache: see docs/PERF.md). Data
// handed out by with() is pinned and recycled only after the last reader.
type byteCache struct {
	mu      sync.Mutex
	budget  int64
	total   int64
	peak    int64 // highest total seen, before eviction
	m       map[int]*zentry
	lru     *list.List
	free    [][]byte
	freeMax int64
}

func newByteCache(budget int64) *byteCache {
	return &byteCache{budget: budget, m: map[int]*zentry{}, lru: list.New(),
		freeMax: min(budget/16, 4<<20)}
}

// alloc returns room for n bytes, recycling blocks when possible. Hand it
// back with discard if it is not going to be cached.
func (c *byteCache) alloc(n int) *cdata {
	d := &cdata{n: n}
	nb := (n + blockSize - 1) / blockSize
	d.blk = make([][]byte, nb)
	c.mu.Lock()
	for i := range d.blk {
		if k := len(c.free); k > 0 {
			d.blk[i] = c.free[k-1]
			c.free = c.free[:k-1]
		}
	}
	c.mu.Unlock()
	for i := range d.blk {
		if d.blk[i] == nil {
			d.blk[i] = make([]byte, blockSize)
		}
	}
	return d
}

// discard recycles data from alloc that never reached the cache.
func (c *byteCache) discard(d *cdata) {
	c.mu.Lock()
	c.recycleLocked(d)
	c.mu.Unlock()
}

func (c *byteCache) recycleLocked(d *cdata) {
	for _, b := range d.blk {
		if int64(len(c.free)+1)*blockSize > c.freeMax || len(b) != blockSize {
			break // over the bound: leave the rest to the GC
		}
		c.free = append(c.free, b)
	}
	d.blk = nil
}

// acquire returns the entry for id, loading it once for concurrent callers.
// With pin the entry is pinned for the caller (see release). Failed loads are
// not cached.
func (c *byteCache) acquire(id int, pin bool, load func() (*cdata, error)) (*zentry, error) {
	c.mu.Lock()
	if e := c.m[id]; e != nil {
		if e.elem != nil {
			c.lru.MoveToFront(e.elem)
		}
		if pin {
			e.pins++
		} else {
			e.shared = true
		}
		c.mu.Unlock()
		<-e.done
		if e.err != nil {
			c.release(e, pin)
			return nil, e.err
		}
		return e, nil
	}
	e := &zentry{id: id, done: make(chan struct{})}
	if pin {
		e.pins = 1
	} else {
		e.shared = true
	}
	c.m[id] = e
	c.mu.Unlock()

	var data *cdata
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
		if pin {
			e.pins--
		}
	} else {
		e.elem = c.lru.PushFront(e)
		c.total += data.size()
		if c.total > c.peak {
			c.peak = c.total
		}
		for c.total > c.budget && c.lru.Len() > 1 {
			el := c.lru.Back()
			v := el.Value.(*zentry)
			c.lru.Remove(el)
			delete(c.m, v.id)
			c.total -= v.data.size()
			switch {
			case v.shared:
			case v.pins > 0:
				v.dead = true
			default:
				c.recycleLocked(v.data)
			}
		}
	}
	c.mu.Unlock()
	close(e.done)
	if err != nil {
		return nil, err
	}
	return e, nil
}

// release drops a pin taken by acquire.
func (c *byteCache) release(e *zentry, pinned bool) {
	if !pinned {
		return
	}
	c.mu.Lock()
	e.pins--
	if e.pins == 0 && e.dead {
		c.recycleLocked(e.data)
		e.dead = false
	}
	c.mu.Unlock()
}

// with calls fn on the cached data of id, loading it first through load
// (which should take its blocks from alloc). The data is valid only during
// fn. It returns the load error, if any.
func (c *byteCache) with(id int, load func() (*cdata, error), fn func(d *cdata)) error {
	e, err := c.acquire(id, true, load)
	if err != nil {
		return err
	}
	defer c.release(e, true)
	fn(e.data)
	return nil
}

// get returns a copy of the contents of entry id. It is for tests; the
// production path is with.
func (c *byteCache) get(id int, load func() ([]byte, error)) ([]byte, error) {
	e, err := c.acquire(id, false, func() (*cdata, error) {
		b, err := load()
		if err != nil {
			return nil, err
		}
		return cdataOf(b), nil
	})
	if err != nil {
		return nil, err
	}
	return e.data.bytes(), nil
}
