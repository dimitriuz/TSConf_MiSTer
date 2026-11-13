package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
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
	open := o.Open
	if open == nil {
		open = OpenSource
	}
	o.Open = func(p string, so SourceOptions) (Source, error) {
		s, err := open(p, so)
		if err != nil {
			return nil, err
		}
		e.opened.Add(1)
		return &closeSource{s, &e.closed}, nil
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
	e := newMgrEnv(t, ManagerOptions{IdleTTL: time.Minute, Open: func(p string, so SourceOptions) (Source, error) {
		<-gate
		return OpenSource(p, so)
	}})
	dir := makeDir(t, map[string]string{"a.txt": "hello"})
	const n = 20
	leases := make([]*Lease, n)
	errs := make([]error, n)
	datas := make([][]byte, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			leases[i], errs[i] = e.m.Acquire(SourceSpec{Path: dir})
			if errs[i] == nil {
				datas[i] = diskBytes(t, leases[i].Volume())
			}
		}(i)
	}
	close(gate)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if leases[i].Volume() != leases[0].Volume() || !bytes.Equal(datas[i], datas[0]) {
			t.Fatal("different volumes")
		}
	}
	if b := e.builds.Load(); b != 1 {
		t.Fatalf("builds = %d", b)
	}
	if v, l := e.m.Stats(); v != 1 || l != 1 {
		t.Fatalf("stats %d %d", v, l)
	}
	// Each lease keeps the volume alive and readable until the last release.
	for i, l := range leases {
		if got := diskBytes(t, l.Volume()); !bytes.Equal(got, datas[0]) {
			t.Fatalf("lease %d unreadable or wrong", i)
		}
		l.Release()
		wantLeased := 1
		if i == n-1 {
			wantLeased = 0
		}
		if _, got := e.m.Stats(); got != wantLeased {
			t.Fatalf("after release %d: leased %d, want %d", i, got, wantLeased)
		}
		e.clock.Advance(2 * time.Minute)
		e.m.Sweep() // frees the volume only once it is idle
		if v, _ := e.m.Stats(); (i < n-1) != (v == 1) {
			t.Fatalf("after release %d: %d volumes", i, v)
		}
	}
	if e.closed.Load() != 1 {
		t.Fatalf("closed %d", e.closed.Load())
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
	base := runtime.NumGoroutine()
	e := newMgrEnv(t, ManagerOptions{MaxVolumes: 2})
	dirs := []string{
		makeDir(t, map[string]string{"a": "aaaa", "x": "1"}),
		makeDir(t, map[string]string{"b": "bbbbbbbb"}),
		makeDir(t, map[string]string{"c": "c", "d": "dd", "e": "eee"}),
	}
	ref := make([][]byte, len(dirs))
	for i, d := range dirs {
		s, err := OpenSource(d, SourceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := NewVolume(s, VolumeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ref[i] = diskBytes(t, v)
		v.Close()
	}
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				k := (g + i) % 3
				l, err := e.m.Acquire(SourceSpec{Path: dirs[k]})
				if errors.Is(err, ErrBusy) {
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(diskBytes(t, l.Volume()), ref[k]) {
					t.Errorf("goroutine %d: wrong bytes for dir %d", g, k)
				}
				l.Release()
				e.clock.Advance(time.Millisecond)
				if i%7 == 0 {
					e.m.Sweep()
				}
			}
		}(g)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("stress test hung")
	}
	if _, l := e.m.Stats(); l != 0 {
		t.Fatalf("leaked leases: %d", l)
	}
	e.m.Close()
	if e.opened.Load() != e.closed.Load() {
		t.Fatalf("opened %d closed %d", e.opened.Load(), e.closed.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutines: %d, started with %d", n, base)
	}
}

// waitRefs waits until build of key has n callers.
func waitRefs(t *testing.T, m *Manager, key string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		e := m.entries[key]
		ok := e != nil && e.building != nil && e.building.refs == n
		m.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("build never reached %d callers", n)
}

func waitStats(t *testing.T, m *Manager, wantV, wantL int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if v, l := m.Stats(); v == wantV && l == wantL {
			return
		}
		time.Sleep(time.Millisecond)
	}
	v, l := m.Stats()
	t.Fatalf("stats %d/%d, want %d/%d", v, l, wantV, wantL)
}

func gatedEnv(t *testing.T, o ManagerOptions) (*mgrEnv, chan struct{}) {
	gate := make(chan struct{})
	o.Open = func(p string, so SourceOptions) (Source, error) {
		<-gate
		return OpenSource(p, so)
	}
	return newMgrEnv(t, o), gate
}

func TestAcquireCtxCancelWaiter(t *testing.T) {
	e, gate := gatedEnv(t, ManagerOptions{})
	dir := makeDir(t, map[string]string{"a": "a"})
	key, _ := filepath.Abs(dir)
	var builder *Lease
	var berr error
	bdone := make(chan struct{})
	go func() {
		defer close(bdone)
		builder, berr = e.m.Acquire(SourceSpec{Path: dir})
	}()
	waitRefs(t, e.m, key, 1)
	ctx, cancel := context.WithCancel(context.Background())
	wdone := make(chan error, 1)
	go func() {
		_, err := e.m.AcquireCtx(ctx, SourceSpec{Path: dir})
		wdone <- err
	}()
	waitRefs(t, e.m, key, 2)
	cancel()
	if err := <-wdone; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter err = %v", err)
	}
	// a second waiter that is not cancelled still gets the volume
	var w2 *Lease
	w2done := make(chan error, 1)
	go func() {
		var err error
		w2, err = e.m.Acquire(SourceSpec{Path: dir})
		w2done <- err
	}()
	waitRefs(t, e.m, key, 2)
	close(gate)
	<-bdone
	if berr != nil {
		t.Fatal(berr)
	}
	if err := <-w2done; err != nil {
		t.Fatal(err)
	}
	if w2.Volume() != builder.Volume() {
		t.Fatal("different volumes")
	}
	if v, l := e.m.Stats(); v != 1 || l != 1 {
		t.Fatalf("stats %d/%d", v, l)
	}
	builder.Release()
	w2.Release()
	waitStats(t, e.m, 1, 0) // no lease leaked by the cancelled waiter
	e.clock.Advance(11 * time.Minute)
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 0 || e.closed.Load() != 1 {
		t.Fatalf("not idle-freed: %d volumes, %d closed", v, e.closed.Load())
	}
}

func TestAcquireCtxCancelBuilder(t *testing.T) {
	e, gate := gatedEnv(t, ManagerOptions{})
	zp := filepath.Join(t.TempDir(), "a.zip")
	writeZip(t, zp, map[string]string{"x": "x"})
	key, _ := filepath.Abs(zp)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.m.AcquireCtx(ctx, SourceSpec{Path: zp, Zip: true})
		done <- err
	}()
	waitRefs(t, e.m, key, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	close(gate)
	waitStats(t, e.m, 1, 0) // the build finished, idle
	start := time.Now()
	l, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Release()
	if e.builds.Load() != 1 {
		t.Fatalf("builds = %d: retry rebuilt", e.builds.Load())
	}
	if time.Since(start) > time.Second {
		t.Fatal("retry not instant")
	}
}

func TestAcquireCtxAlreadyCancelled(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	dir := makeDir(t, map[string]string{"a": "a"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.m.AcquireCtx(ctx, SourceSpec{Path: dir}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if e.builds.Load() != 0 {
		t.Fatal("built for a cancelled context")
	}
	if v, _ := e.m.Stats(); v != 0 {
		t.Fatal("volume created")
	}
}

// A Logf that calls back into the manager must not deadlock it.
func TestLogfReentrant(t *testing.T) {
	var m *Manager
	var lines atomic.Int32
	e := newMgrEnv(t, ManagerOptions{MaxVolumes: 2, Logf: func(string, ...any) {
		lines.Add(1)
		m.Stats()
	}})
	m = e.m
	done := make(chan struct{})
	go func() {
		defer close(done)
		zp := filepath.Join(t.TempDir(), "a.zip")
		writeZip(t, zp, map[string]string{"x": "x"})
		l, err := m.Acquire(SourceSpec{Path: zp})
		if err != nil {
			t.Error(err)
			return
		}
		writeZip(t, zp, map[string]string{"x": "xxxx"})
		l2, err := m.Acquire(SourceSpec{Path: zp}) // rebuild log
		if err != nil {
			t.Error(err)
			return
		}
		l.Release()
		l2.Release()
		d1 := makeDir(t, map[string]string{"a": "a"})
		d2 := makeDir(t, map[string]string{"a": "a"})
		for _, d := range []string{d1, d2} { // evict log
			l, err := m.Acquire(SourceSpec{Path: d})
			if err != nil {
				t.Error(err)
				return
			}
			l.Release()
		}
		e.clock.Advance(time.Hour)
		m.Sweep()
		m.Close()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("deadlock with re-entrant Logf")
	}
	if lines.Load() == 0 {
		t.Fatal("Logf never called")
	}
}

func TestZipMtimeOnlyChange(t *testing.T) {
	e := newMgrEnv(t, ManagerOptions{})
	zp := filepath.Join(t.TempDir(), "a.zip")
	writeZip(t, zp, map[string]string{"x": "x"})
	l, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if e.builds.Load() != 1 {
		t.Fatal("setup")
	}
	l, _ = e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	l.Release()
	if e.builds.Load() != 1 {
		t.Fatal("unchanged zip rebuilt")
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(zp, later, later); err != nil {
		t.Fatal(err)
	}
	l, err = e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	if e.builds.Load() != 2 {
		t.Fatalf("builds = %d: mtime-only change not detected", e.builds.Load())
	}
}

func TestIdleTTLBoundary(t *testing.T) {
	const ttl = time.Minute
	e := newMgrEnv(t, ManagerOptions{IdleTTL: ttl})
	zp := filepath.Join(t.TempDir(), "a.zip")
	writeZip(t, zp, map[string]string{"x": "x"})
	l, err := e.m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l.Release()
	e.clock.Advance(ttl - time.Nanosecond)
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 1 {
		t.Fatal("evicted one nanosecond early")
	}
	e.clock.Advance(time.Nanosecond)
	e.m.Sweep()
	if v, _ := e.m.Stats(); v != 0 || e.closed.Load() != 1 {
		t.Fatalf("not evicted at exactly the TTL: %d volumes, %d closed", v, e.closed.Load())
	}
}

// TestAcquireRacesClose: builds that finish after Close must not install a
// volume nobody closes.
func TestAcquireRacesClose(t *testing.T) {
	base := runtime.NumGoroutine()
	rng := rand.New(rand.NewSource(1))
	dirs := []string{
		makeDir(t, map[string]string{"a": "a"}), makeDir(t, map[string]string{"b": "b"}),
		makeDir(t, map[string]string{"c": "c"}), makeDir(t, map[string]string{"d": "d"}),
	}
	for round := 0; round < 50; round++ {
		gate := make(chan struct{})
		var mu sync.Mutex
		var counters []*atomic.Int32
		var started atomic.Int32
		m := NewManager(ManagerOptions{MaxVolumes: 16, NoTicker: true,
			OnBuild: func(SourceSpec) { started.Add(1) },
			Open: func(p string, so SourceOptions) (Source, error) {
				<-gate
				s, err := OpenSource(p, so)
				if err != nil {
					return nil, err
				}
				c := new(atomic.Int32)
				mu.Lock()
				counters = append(counters, c)
				mu.Unlock()
				return &closeSource{s, c}, nil
			}})
		var wg sync.WaitGroup
		type res struct {
			l   *Lease
			err error
		}
		results := make([]res, 2*len(dirs))
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i].l, results[i].err = m.Acquire(SourceSpec{Path: dirs[i%len(dirs)]})
			}(i)
		}
		deadline := time.Now().Add(10 * time.Second)
		for started.Load() < int32(len(dirs)) && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if started.Load() < int32(len(dirs)) {
			t.Fatal("builds did not start")
		}
		closeFirst := rng.Intn(2) == 0
		if closeFirst {
			m.Close()
			close(gate)
		} else {
			// Close and the end of the builds race, in random order.
			go close(gate)
			runtime.Gosched()
			m.Close()
		}
		wg.Wait()
		for i, r := range results {
			if r.err != nil {
				if !errors.Is(r.err, errManagerClosed) {
					t.Fatalf("round %d caller %d: err = %v", round, i, r.err)
				}
				continue
			}
			if closeFirst {
				t.Fatalf("round %d: caller %d got a lease on a closed manager", round, i)
			}
			r.l.Release()
		}
		if v, _ := m.Stats(); v != 0 {
			t.Fatalf("round %d: %d volumes left", round, v)
		}
		mu.Lock()
		for i, c := range counters {
			if c.Load() != 1 {
				t.Fatalf("round %d: source %d closed %d times", round, i, c.Load())
			}
		}
		mu.Unlock()
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Fatalf("goroutines: %d, started with %d", n, base)
	}
}
