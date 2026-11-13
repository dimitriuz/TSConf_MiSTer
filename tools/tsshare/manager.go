package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrBusy is returned by Acquire when MaxVolumes volumes are in use.
var ErrBusy = errors.New("tsshare: too many sources in use")

// errManagerClosed is returned by Acquire after Close.
var errManagerClosed = errors.New("tsshare: volume manager is closed")

// SourceSpec names what to serve. Zip is a hint from the caller; the kind
// actually used is decided by stat (a directory, else a zip file).
type SourceSpec struct {
	Path string
	Zip  bool
}

// ManagerOptions configures a Manager. Zero values pick the defaults.
type ManagerOptions struct {
	MaxVolumes int           // volumes in use at once; default 3
	IdleTTL    time.Duration // an unused volume is freed after this; default 10m
	Source     SourceOptions // passed to OpenSource
	Volume     VolumeOptions // passed to NewVolume (Label etc.)
	MaxEntries int           // per volume (0 = no limit); overrides Volume.MaxEntries
	Now        func() time.Time
	Logf       func(format string, a ...any)

	// Open replaces OpenSource (default OpenSource); OnBuild is called when a
	// build starts; NoTicker disables the background sweeper. For tests.
	Open     func(path string, o SourceOptions) (Source, error)
	OnBuild  func(SourceSpec)
	NoTicker bool
}

// mvol is one built volume generation. All fields except v and key are
// guarded by Manager.mu.
type mvol struct {
	v        *Volume
	key      string
	isZip    bool
	size     int64
	mtime    time.Time
	leases   int
	retired  bool // no longer the current volume of its key; closed after the last lease
	lastUsed time.Time
}

// mbuild is a build in flight for one key.
type mbuild struct {
	done    chan struct{}
	waiters int // extra Acquires waiting for the result (under Manager.mu)
	vol     *mvol
	err     error
}

type mentry struct {
	cur      *mvol
	building *mbuild
}

// Manager builds volumes on demand, shares them between callers through
// leases, and frees idle ones.
type Manager struct {
	o  ManagerOptions
	mu sync.Mutex

	entries  map[string]*mentry
	live     map[*mvol]struct{} // every installed, not yet closed volume
	building int                // builds in flight
	closed   bool

	stop chan struct{}
	wg   sync.WaitGroup
}

// Lease keeps a volume alive until Release.
type Lease struct {
	m        *Manager
	v        *mvol
	released bool // under m.mu
}

// NewManager returns a manager and, unless o.NoTicker, starts its sweeper.
func NewManager(o ManagerOptions) *Manager {
	if o.MaxVolumes <= 0 {
		o.MaxVolumes = 3
	}
	if o.IdleTTL <= 0 {
		o.IdleTTL = 10 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Open == nil {
		o.Open = OpenSource
	}
	if o.MaxEntries != 0 {
		o.Volume.MaxEntries = o.MaxEntries
	}
	m := &Manager{o: o, entries: map[string]*mentry{}, live: map[*mvol]struct{}{}, stop: make(chan struct{})}
	if !o.NoTicker {
		iv := o.IdleTTL / 10
		if iv < time.Second {
			iv = time.Second
		}
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			t := time.NewTicker(iv)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					m.Sweep()
				case <-m.stop:
					return
				}
			}
		}()
	}
	return m
}

func (m *Manager) logf(format string, a ...any) {
	if m.o.Logf != nil {
		m.o.Logf(format, a...)
	}
}

func (l *Lease) Volume() *Volume { return l.v.v }

// Stats returns the number of installed volumes (including retired ones still
// leased) and how many of them are leased.
func (m *Manager) Stats() (volumes, leased int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for v := range m.live {
		volumes++
		if v.leases > 0 {
			leased++
		}
	}
	return
}

func (m *Manager) closeVol(v *mvol, why string) {
	m.logf("close %s: %s", v.key, why)
	if err := v.v.Close(); err != nil {
		m.logf("close %s: %v", v.key, err)
	}
}

// stale reports why the current volume cannot be reused, or "" if it can.
// stat is the fresh os.Stat of the path (nil for a directory).
func (v *mvol) stale(st os.FileInfo) string {
	if !v.isZip {
		if v.leases == 0 {
			return "directory rescan"
		}
		return ""
	}
	if st.Size() != v.size || !st.ModTime().Equal(v.mtime) {
		return "zip changed on disk"
	}
	return ""
}

// Acquire returns a lease on the volume for spec, building it on first use.
func (m *Manager) Acquire(spec SourceSpec) (*Lease, error) {
	p, err := filepath.Abs(spec.Path)
	if err != nil {
		return nil, err
	}
	key := filepath.Clean(p)
	st, err := os.Stat(key)
	if err != nil {
		return nil, err
	}
	isZip := !st.IsDir()

	var toClose []*mvol
	closeAll := func() {
		for _, v := range toClose {
			m.closeVol(v, "replaced")
		}
		toClose = nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errManagerClosed
	}
	e := m.entries[key]
	if e == nil {
		e = &mentry{}
		m.entries[key] = e
	}
	if v := e.cur; v != nil {
		if v.isZip != isZip {
			// the path changed kind; treat as stale
			m.retireLocked(e, &toClose)
			m.logf("rebuild %s: kind changed", key)
		} else if why := v.stale(st); why == "" {
			v.leases++
			m.mu.Unlock()
			return &Lease{m: m, v: v}, nil
		} else {
			m.logf("rebuild %s: %s", key, why)
			m.retireLocked(e, &toClose)
		}
	}
	if b := e.building; b != nil {
		b.waiters++
		m.mu.Unlock()
		closeAll()
		<-b.done
		if b.err != nil {
			return nil, b.err
		}
		return &Lease{m: m, v: b.vol}, nil
	}
	// This caller builds. Reserve a slot.
	leased := m.building
	for v := range m.live {
		if v.leases > 0 {
			leased++
		}
	}
	if leased >= m.o.MaxVolumes {
		m.cleanupEntryLocked(key, e)
		m.mu.Unlock()
		closeAll()
		return nil, ErrBusy
	}
	if len(m.live)+m.building >= m.o.MaxVolumes {
		// Room is short and some volume is idle: evict the oldest.
		var old *mvol
		for v := range m.live {
			if v.leases == 0 && (old == nil || v.lastUsed.Before(old.lastUsed)) {
				old = v
			}
		}
		if old != nil {
			m.logf("evict %s: idle, making room", old.key)
			m.dropLocked(old)
			toClose = append(toClose, old)
		}
	}
	b := &mbuild{done: make(chan struct{})}
	e.building = b
	m.building++
	m.mu.Unlock()
	closeAll()

	nv, err := m.build(spec, key, st, isZip)

	m.mu.Lock()
	m.building--
	e.building = nil
	if err == nil && m.closed {
		err = errManagerClosed
	}
	if err != nil {
		m.cleanupEntryLocked(key, e)
		b.err = err
		m.mu.Unlock()
		if nv != nil {
			m.closeVol(nv, "manager closed during build")
		}
		close(b.done)
		m.logf("build %s failed: %v", key, err)
		return nil, err
	}
	nv.leases = 1 + b.waiters
	nv.lastUsed = m.o.Now()
	e.cur = nv
	m.live[nv] = struct{}{}
	b.vol = nv
	m.mu.Unlock()
	close(b.done)
	return &Lease{m: m, v: nv}, nil
}

// build runs OpenSource and NewVolume without any manager lock held. A panic
// becomes an error. On error the returned volume is nil, except when the
// manager closed meanwhile (the caller closes it).
func (m *Manager) build(spec SourceSpec, key string, st os.FileInfo, isZip bool) (nv *mvol, err error) {
	var s Source
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tsshare: panic building %s: %v", key, r)
			nv = nil
			if s != nil {
				s.Close()
			}
		}
	}()
	if m.o.OnBuild != nil {
		m.o.OnBuild(spec)
	}
	t0 := time.Now()
	m.logf("build %s: start", key)
	s, err = m.o.Open(key, m.o.Source)
	if err != nil {
		return nil, err
	}
	vol, warns, err := NewVolume(s, m.o.Volume)
	if err != nil {
		s.Close()
		return nil, err
	}
	g := vol.Geometry()
	m.logf("build %s: done, %d nodes, %d warnings, %v", key, g.Files+g.Dirs, len(warns), time.Since(t0).Round(time.Millisecond))
	return &mvol{v: vol, key: key, isZip: isZip, size: st.Size(), mtime: st.ModTime()}, nil
}

// dropLocked removes v from the live set and from its entry.
func (m *Manager) dropLocked(v *mvol) {
	delete(m.live, v)
	if e := m.entries[v.key]; e != nil && e.cur == v {
		e.cur = nil
		m.cleanupEntryLocked(v.key, e)
	}
}

func (m *Manager) cleanupEntryLocked(key string, e *mentry) {
	if e.cur == nil && e.building == nil && m.entries[key] == e {
		delete(m.entries, key)
	}
}

// retireLocked detaches e.cur. An idle volume goes to toClose now; a leased
// one is closed by its last Release.
func (m *Manager) retireLocked(e *mentry, toClose *[]*mvol) {
	v := e.cur
	e.cur = nil
	v.retired = true
	if v.leases == 0 {
		delete(m.live, v)
		*toClose = append(*toClose, v)
	}
}

// Release gives the lease back; it is idempotent and safe after Close.
func (l *Lease) Release() {
	m := l.m
	m.mu.Lock()
	if l.released {
		m.mu.Unlock()
		return
	}
	l.released = true
	v := l.v
	v.leases--
	var doClose bool
	if v.leases == 0 {
		v.lastUsed = m.o.Now()
		if v.retired {
			delete(m.live, v)
			doClose = true
		}
	}
	m.mu.Unlock()
	if doClose {
		m.closeVol(v, "last lease released")
	}
}

// Sweep frees volumes idle for at least IdleTTL.
func (m *Manager) Sweep() {
	now := m.o.Now()
	var toClose []*mvol
	m.mu.Lock()
	for v := range m.live {
		if v.leases == 0 && !v.retired && now.Sub(v.lastUsed) >= m.o.IdleTTL {
			m.dropLocked(v)
			toClose = append(toClose, v)
		}
	}
	m.mu.Unlock()
	for _, v := range toClose {
		m.closeVol(v, "idle timeout")
	}
}

// Close stops the sweeper and releases everything; leased volumes are closed
// by their last Release. Later Acquires fail.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.stop)
	var toClose []*mvol
	for v := range m.live {
		v.retired = true
		if v.leases == 0 {
			delete(m.live, v)
			toClose = append(toClose, v)
		}
	}
	m.entries = map[string]*mentry{}
	m.mu.Unlock()
	m.wg.Wait()
	for _, v := range toClose {
		m.closeVol(v, "manager closed")
	}
}
