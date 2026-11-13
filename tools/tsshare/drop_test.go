package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestDropCachesOnLastRelease(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "a.zip")
	big := bytes.Repeat([]byte("compressible data 0123456789\n"), 20000) // ~580 KB inflated
	writeZipFile(t, zp, map[string][]byte{"big.txt": big, "small.txt": []byte("x")})
	var builds atomic.Int32
	m := NewManager(ManagerOptions{NoTicker: true, OnBuild: func(SourceSpec) { builds.Add(1) }})
	defer m.Close()
	l1, err := m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	v := l1.Volume()
	zs := v.src.(*zipSource)
	g := v.Geometry()
	dataOff := (g.PartStart + g.DataStart) * sectorSize // the file data, not the zero-filled rest
	want := make([]byte, 2<<20)
	if _, err := v.ReadAt(want, dataOff); err != nil {
		t.Fatal(err)
	}
	if held := zs.cache.bytesHeld(); held < int64(len(big)) {
		t.Fatalf("cache holds %d bytes after a full read", held)
	}
	l1.Release()
	if zs.cache.bytesHeld() == 0 {
		t.Fatal("cache dropped while a lease is still open")
	}
	l2.Release()
	if held := zs.cache.bytesHeld(); held != 0 {
		t.Fatalf("cache holds %d bytes after the last release", held)
	}
	// the volume is kept: same one, no rebuild, reads inflate again
	l3, err := m.Acquire(SourceSpec{Path: zp, Zip: true})
	if err != nil {
		t.Fatal(err)
	}
	defer l3.Release()
	if l3.Volume() != v || builds.Load() != 1 {
		t.Fatalf("volume rebuilt (%d builds)", builds.Load())
	}
	got := make([]byte, len(want))
	if _, err := v.ReadAt(got, dataOff); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read after drop: %v, equal %v", err, bytes.Equal(got, want))
	}
	if zs.cache.bytesHeld() == 0 {
		t.Fatal("cache not refilled")
	}
}

func TestByteCacheDropAllPinned(t *testing.T) {
	c := newByteCache(1 << 20)
	data := bytes.Repeat([]byte{7}, 100000)
	inside := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- c.with(1, func() (*cdata, error) { return cdataOf(data), nil }, func(d *cdata) {
			close(inside)
			<-release
			if !bytes.Equal(d.bytes(), data) {
				t.Error("pinned data changed by dropAll")
			}
		})
	}()
	<-inside
	c.dropAll()
	if c.bytesHeld() == 0 {
		t.Fatal("pinned entry's bytes vanished from the accounting")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if held := c.bytesHeld(); held != 0 {
		t.Fatalf("held %d after release", held)
	}
}

func TestSymlinkedZipSharesVolume(t *testing.T) {
	dir := t.TempDir()
	dir, _ = filepath.EvalSymlinks(dir)
	zp := filepath.Join(dir, "a.zip")
	writeZipFile(t, zp, map[string][]byte{"x": []byte("x")})
	link := filepath.Join(dir, "link.zip")
	if err := os.Symlink(zp, link); err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(dir, ".", "sub", "..", "a.zip")
	var builds atomic.Int32
	m := NewManager(ManagerOptions{NoTicker: true, MaxVolumes: 3, OnBuild: func(SourceSpec) { builds.Add(1) }})
	defer m.Close()
	var vols []*Volume
	for _, p := range []string{zp, link, rel} {
		l, err := m.Acquire(SourceSpec{Path: p, Zip: true})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		defer l.Release()
		vols = append(vols, l.Volume())
	}
	if builds.Load() != 1 || vols[0] != vols[1] || vols[0] != vols[2] {
		t.Fatalf("%d builds for three names of one zip", builds.Load())
	}
	if n, _ := m.Stats(); n != 1 {
		t.Fatalf("%d volumes", n)
	}
}
