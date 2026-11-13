package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// closeSource counts Close calls of the wrapped source.
type closeSource struct {
	Source
	closed *atomic.Int32
}

func (c *closeSource) Close() error {
	c.closed.Add(1)
	return c.Source.Close()
}

type mgrEnv struct {
	m      *Manager
	clock  *fakeClock
	builds atomic.Int32
	opened atomic.Int32
	closed atomic.Int32
}

func newMgrEnv(t *testing.T, o ManagerOptions) *mgrEnv {
	t.Helper()
	e := &mgrEnv{clock: &fakeClock{t: time.Unix(1_700_000_000, 0)}}
	o.Now = e.clock.Now
	o.NoTicker = true
	o.OnBuild = func(SourceSpec) { e.builds.Add(1) }
	if o.Open == nil {
		o.Open = func(p string, so SourceOptions) (Source, error) {
			s, err := OpenSource(p, so)
			if err != nil {
				return nil, err
			}
			e.opened.Add(1)
			return &closeSource{s, &e.closed}, nil
		}
	}
	e.m = NewManager(o)
	t.Cleanup(e.m.Close)
	return e
}

func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for n, c := range files {
		f, err := w.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(c))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil { // new inode: old readers keep the old file
		t.Fatal(err)
	}
}

func makeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	d := t.TempDir()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(d, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

func diskBytes(t *testing.T, v *Volume) []byte {
	t.Helper()
	// Header, FAT and the start of the data area; not the whole (large) disk.
	g := v.Geometry()
	n := (g.PartStart + g.DataStart + 2048) * sectorSize
	if n > v.Size() {
		n = v.Size()
	}
	b := make([]byte, n)
	if _, err := v.ReadAt(b, 0); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAcquireBuildsOnce(t *testing.T) {
	gate := make(chan struct{})
	var e *mgrEnv
	e = newMgrEnv(t, ManagerOptions{Open: func(p string, so SourceOptions) (Source, error) {
		<-gate
		return OpenSource(p, so)
	}})
	dir := makeDir(t, map[string]string{"a.txt": "hello"})
	const n = 20
	leases := make([]*Lease, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = e.m.Acquire(SourceSpec{Path: dir})
		}(i)
	}
	close(gate)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if leases[i].Volume() != leases[0].Volume() {
			t.Fatal("different volumes")
		}
	}
	if b := e.builds.Load(); b != 1 {
		t.Fatalf("builds = %d", b)
	}
	if v, l := e.m.Stats(); v != 1 || l != 1 {
		t.Fatalf("stats %d %d", v, l)
	}
	for _, l := range leases {
		l.Release()
	}
	// Releasing all leases (counted per waiter) leaves the volume idle.
	if _, l := e.m.Stats(); l != 0 {
		t.Fatalf("still leased: %d", l)
	}
}

func TestReleaseKeepsIdleThenSweeps(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{IdleTTL: time.Minute})
	zp := filepath.Join(t.TempDir(), "a.zip")
	writeZip(t, zp, map[string]string{"x.txt": "xx"})
	l, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	e.clock.Advance(59 * time.Second)
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 1 || e.closed.Load() != 0 {
		t.Fatalf("freed too early: %d volumes, %d closed", v, e.closed.Load())
	}
	l2, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true}) // reuse, no rebuild
	if err != nil {
		t.Fatal(err)
	}
	if e.builds.Load() != 1 {
		t.Fatalf("builds = %d", e.builds.Load())
	}
	l2.Release()
	e.clock.Advance(59 * time.Second) // idle time restarted at the release
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 1 {
		t.Fatal("idle clock not restarted by release")
	}
	e.clock.Advance(2 * time.Second)
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 0 || e.closed.Load() != 1 {
		t.Fatalf("not freed: %d volumes, %d closed", v, e.closed.Load())
	}
}

func TestZipChangedRebuilds(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	zp := filepath.Join(t.TempDir(), "a.zip")
	writeZip(t, zp, map[string]string{"x.txt": strings.Repeat("old ", 100)})
	l1, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	before := diskBytes(t, l1.Volume())
	writeZip(t, zp, map[string]string{"x.txt": strings.Repeat("new!", 500), "y.txt": "yy"})
	l2, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	if l2.Volume() == l1.Volume() || e.builds.Load() != 2 {
		t.Fatal("zip change not detected")
	}
	if e.closed.Load() != 0 {
		t.Fatal("old volume closed while leased")
	}
	if !bytes.Equal(diskBytes(t, l1.Volume()), before) {
		t.Fatal("old lease reads different data")
	}
	if bytes.Equal(diskBytes(t, l2.Volume()), before) {
		t.Fatal("new volume shows old data")
	}
	if v, l := e.m.Stats(); v != 2 || l != 2 {
		t.Fatalf("stats %d %d", v, l)
	}
	l1.Release()
	if e.closed.Load() != 1 {
		t.Fatalf("old volume not closed after release: %d", e.closed.Load())
	}
	l2.Release()
	if e.closed.Load() != 1 {
		t.Fatal("current volume closed on release")
	}
	// Idle old generation is closed at once on replacement.
	writeZip(t, zp, map[string]string{"z.txt": "z"})
	l3, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l3.Release()
	if e.closed.Load() != 2 {
		t.Fatalf("idle stale volume not closed: %d", e.closed.Load())
	}
}

func TestDirRescanOnReacquire(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	dir := makeDir(t, map[string]string{"a.txt": "a"})
	l1, err := e.m.Acquire(SourceSpec{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	files1 := l1.Volume().Geometry().Files
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	l2, err := e.m.Acquire(SourceSpec{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if l2.Volume() != l1.Volume() || l2.Volume().Geometry().Files != files1 {
		t.Fatal("rescanned while leased")
	}
	l1.Release()
	l2.Release()
	l3, err := e.m.Acquire(SourceSpec{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Release()
	if got := l3.Volume().Geometry().Files; got != files1+1 {
		t.Fatalf("files %d, want %d", got, files1+1)
	}
	if e.builds.Load() != 2 || e.closed.Load() != 1 {
		t.Fatalf("builds %d closed %d", e.builds.Load(), e.closed.Load())
	}
}

func TestBusyAtLimit(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{MaxVolumes: 3})
	var ls []*Lease
	for i := 0; i < 3; i++ {
		l, err := e.m.Acquire(SourceSpec{Path: makeDir(t, map[string]string{"a": "a"})})
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
	}
	d4 := makeDir(t, map[string]string{"a": "a"})
	if _, err := e.m.Acquire(SourceSpec{Path: d4}); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if v, _ := e.m.Stats(); v != 3 {
		t.Fatalf("volumes %d", v)
	}
	ls[0].Release()
	l4, err := e.m.Acquire(SourceSpec{Path: d4}) // evicts the idle one
	if err != nil {
		t.Fatal(err)
	}
	if v, l := e.m.Stats(); v != 3 || l != 3 {
		t.Fatalf("stats %d %d", v, l)
	}
	if e.closed.Load() != 1 {
		t.Fatalf("closed %d", e.closed.Load())
	}
	l4.Release()
}

func TestEvictsOldestIdle(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{MaxVolumes: 2})
	var dirs []string
	for i := 0; i < 3; i++ {
		dirs = append(dirs, makeDir(t, map[string]string{"a": "a"}))
	}
	for i := 0; i < 2; i++ {
		l, err := e.m.Acquire(SourceSpec{Path: dirs[i]})
		if err != nil {
			t.Fatal(err)
		}
		l.Release()
		e.clock.Advance(time.Second)
	}
	l, err := e.m.Acquire(SourceSpec{Path: dirs[2]})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if v, _ := e.m.Stats(); v != 2 {
		t.Fatalf("volumes %d", v)
	}
	// dirs[1] is still cached (dirs[0] was oldest): idle dir rebuilds anyway,
	// so check through the entry map instead.
	e.m.mu.Lock()
	_, has0 := e.m.entries[dirs[0]]
	_, has1 := e.m.entries[dirs[1]]
	e.m.mu.Unlock()
	if has0 || !has1 {
		t.Fatalf("evicted wrong volume: has0=%v has1=%v", has0, has1)
	}
}

func TestBuildErrorNotCached(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	zp := filepath.Join(t.TempDir(), "bad.zip")
	os.WriteFile(zp, []byte("this is not a zip file at all"), 0o644)
	_, err1 := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	_, err2 := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
		t.Fatalf("errors: %v / %v", err1, err2)
	}
	if e.builds.Load() != 2 {
		t.Fatalf("builds %d", e.builds.Load())
	}
	if v, _ := e.m.Stats(); v != 0 {
		t.Fatalf("volumes %d", v)
	}
	writeZip(t, zp, map[string]string{"x": "x"})
	l, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
}

func TestBuildPanicIsError(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{Open: func(string, SourceOptions) (Source, error) {
		panic("boom")
	}})
	dir := makeDir(t, map[string]string{"a": "a"})
	for i := 0; i < 2; i++ {
		_, err := e.m.Acquire(SourceSpec{Path: dir})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	}
	if v, _ := e.m.Stats(); v != 0 {
		t.Fatal("volume cached")
	}
}

func TestConcurrentWaitersShareError(t *testing.T) {
	gate := make(chan struct{})
	e := newMgrEnv(t, ManagerOptions{Open: func(string, SourceOptions) (Source, error) {
		<-gate
		return nil, errors.New("nope")
	}})
	dir := makeDir(t, map[string]string{"a": "a"})
	var wg sync.WaitGroup
	errs := make([]error, 10)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.m.Acquire(SourceSpec{Path: dir})
		}(i)
	}
	close(gate)
	wg.Wait()
	for _, err := range errs {
		if err == nil || err.Error() != "nope" {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestDoubleReleaseAndClose(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	dir := makeDir(t, map[string]string{"a": "a"})
	l1, err := e.m.Acquire(SourceSpec{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := e.m.Acquire(SourceSpec{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	l1.Release()
	l1.Release() // must not drop l2's count
	if _, l := e.m.Stats(); l != 1 {
		t.Fatalf("leased %d", l)
	}
	v := l2.Volume()
	e.m.Close()
	if e.closed.Load() != 0 {
		t.Fatal("leased volume closed by Close")
	}
	if _, err := v.ReadAt(make([]byte, 512), 0); err != nil {
		t.Fatal("lease unusable after Close:", err)
	}
	l2.Release()
	l2.Release()
	if e.closed.Load() != 1 {
		t.Fatalf("closed %d", e.closed.Load())
	}
	if _, err := e.m.Acquire(SourceSpec{Path: dir}); err == nil {
		t.Fatal("Acquire after Close succeeded")
	}
	e.m.Close() // idempotent
	if v, _ := e.m.Stats(); v != 0 {
		t.Fatal("volumes left")
	}
}

func TestCloseClosesIdle(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	l, err := e.m.Acquire(SourceSpec{Path: makeDir(t, map[string]string{"a": "a"})})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	e.m.Close()
	if e.closed.Load() != 1 {
		t.Fatalf("closed %d", e.closed.Load())
	}
}

func TestManagerMaxEntries(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{MaxEntries: 3})
	dir := makeDir(t, map[string]string{"a": "a", "b": "b", "c": "c", "d": "d"})
	_, err := e.m.Acquire(SourceSpec{Path: dir})
	var te *tooManyEntries
	if !errors.As(err, &te) || !strings.Contains(err.Error(), "-max-entries") {
		t.Fatalf("err = %v", err)
	}
	if v, _ := e.m.Stats(); v != 0 || e.closed.Load() != e.opened.Load() {
		t.Fatalf("volumes %d, opened %d closed %d", v, e.opened.Load(), e.closed.Load())
	}
}

func TestTickerSweeps(t *testing.T) {
	var closed atomic.Int32
	m := NewManager(ManagerOptions{IdleTTL: 10 * time.Millisecond, Open: func(p string, so SourceOptions) (Source, error) {
		s, err := OpenSource(p, so)
		if err != nil {
			return nil, err
		}
		return &closeSource{s, &closed}, nil
	}})
	defer m.Close()
	l, err := m.Acquire(SourceSpec{Path: makeDir(t, map[string]string{"a": "a"})})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	deadline := time.Now().Add(5 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if closed.Load() != 1 {
		t.Fatal("ticker did not sweep within 5 s")
	}
}

func TestManagerStress(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{MaxVolumes: 2})
	dirs := []string{makeDir(t, map[string]string{"a": "a"}), makeDir(t, map[string]string{"b": "b"}), makeDir(t, map[string]string{"c": "c"})}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				l, err := e.m.Acquire(SourceSpec{Path: dirs[(g+i)%3]})
				if errors.Is(err, ErrBusy) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				buf := make([]byte, 512)
				if _, err := l.Volume().ReadAt(buf, 0); err != nil {
					t.Error(err)
				}
				l.Release()
				e.clock.Advance(time.Millisecond)
				if i%7 == 0 {
					e.m.Sweep()
				}
			}
		}(g)
	}
	wg.Wait()
	if _, l := e.m.Stats(); l != 0 {
		t.Fatalf("leaked leases: %d", l)
	}
	e.m.Close()
	if e.opened.Load() != e.closed.Load() {
		t.Fatalf("opened %d closed %d", e.opened.Load(), e.closed.Load())
	}
}
