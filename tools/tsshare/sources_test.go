package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func randBytes(seed int64, n int) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	// semi-compressible: runs of random bytes
	for i := 0; i < n; {
		v := byte(r.Intn(256))
		run := 1 + r.Intn(8)
		for j := 0; j < run && i < n; j++ {
			b[i] = v
			i++
		}
	}
	return b
}

// flatten returns "path:size" for every file and "path/" for every dir.
func flatten(n *Node, prefix string, out *[]string) {
	for _, c := range n.Children {
		p := prefix + c.Name
		if c.IsDir {
			*out = append(*out, p+"/")
			flatten(c, p+"/", out)
		} else {
			*out = append(*out, fmt.Sprintf("%s:%d", p, c.Size))
		}
	}
}

func listTree(t *testing.T, s Source) ([]string, []string, *Node) {
	t.Helper()
	root, warns, err := s.Tree()
	if err != nil {
		t.Fatal(err)
	}
	var l []string
	flatten(root, "", &l)
	sort.Strings(l)
	return l, warns, root
}

func findNode(n *Node, p string) *Node {
	for _, part := range strings.Split(p, "/") {
		var f *Node
		for _, c := range n.Children {
			if c.Name == part {
				f = c
			}
		}
		if f == nil {
			return nil
		}
		n = f
	}
	return n
}

func hasWarn(w []string, sub string) bool {
	for _, x := range w {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readNode(t *testing.T, s Source, n *Node, off int64, ln int) ([]byte, error) {
	b := make([]byte, ln)
	k, err := s.ReadFileAt(n, b, off)
	if k == ln && (err == nil || err == io.EOF) {
		return b, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return b[:k], err
}

func TestDirSourceTree(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), []byte("hello"))
	writeFile(t, filepath.Join(root, "sub/deep/b.bin"), []byte("xyz"))
	writeFile(t, filepath.Join(root, "sub/c"), nil)
	if err := os.Symlink("a.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(filepath.Join(root, "realdir"), 0o755)
	os.Symlink("realdir", filepath.Join(root, "linkdir"))
	os.Symlink("nowhere", filepath.Join(root, "broken"))
	secret := filepath.Join(root, "secret")
	writeFile(t, secret, []byte("x"))
	os.Chmod(secret, 0)
	defer os.Chmod(secret, 0o644)
	s, err := NewDirSource(root, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, warns, tree := listTree(t, s)
	want := []string{"a.txt:5", "link.txt:5", "realdir/", "secret:1", "sub/", "sub/c:0", "sub/deep/", "sub/deep/b.bin:3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tree = %v\nwant   %v", got, want)
	}
	if !hasWarn(warns, "linkdir") || !hasWarn(warns, "broken") {
		t.Errorf("missing warnings: %v", warns)
	}
	if hasWarn(warns, "link.txt") {
		t.Errorf("warning for a working file symlink: %v", warns)
	}
	// An unreadable file is listed; reading it fails (unless running as root)
	// and everything else keeps working.
	if _, err := readNode(t, s, findNode(tree, "secret"), 0, 1); err == nil && os.Geteuid() != 0 {
		t.Error("unreadable file read without error")
	}
	if b, err := readNode(t, s, findNode(tree, "link.txt"), 0, 5); err != nil || string(b) != "hello" {
		t.Errorf("link.txt = %q, %v", b, err)
	}
	for _, w := range warns {
		if !strings.HasPrefix(w, root) {
			t.Errorf("warning without source prefix: %q", w)
		}
	}
	if _, err := NewDirSource(filepath.Join(root, "a.txt"), SourceOptions{}); err == nil {
		t.Error("file accepted as directory")
	}
}

func TestDirSourceReadAt(t *testing.T) {
	root := t.TempDir()
	data := map[string][]byte{}
	for i := 0; i < 20; i++ {
		n := fmt.Sprintf("d%d/f%02d", i%3, i)
		data[n] = randBytes(int64(i), 100_000+i*37_000)
		writeFile(t, filepath.Join(root, n), data[n])
	}
	s, _ := NewDirSource(root, SourceOptions{FDLimit: 4})
	defer s.Close()
	_, _, tree := listTree(t, s)
	r := rand.New(rand.NewSource(1))
	for round := 0; round < 3; round++ {
		for name, d := range data {
			n := findNode(tree, name)
			for k := 0; k < 30; k++ {
				ln := 1 + r.Intn(300_000)
				off := int64(r.Intn(len(d)))
				if off+int64(ln) > int64(len(d)) {
					ln = int(int64(len(d)) - off)
				}
				b, err := readNode(t, s, n, off, ln)
				if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
					t.Fatalf("%s @%d+%d: err=%v", name, off, ln, err)
				}
			}
		}
	}
	if c := s.(*dirSource).cache.openCount(); c > 4 {
		t.Errorf("%d descriptors cached, limit 4", c)
	}
}

func TestDirSourceChangedFiles(t *testing.T) {
	root := t.TempDir()
	orig := randBytes(5, 300_000)
	writeFile(t, filepath.Join(root, "shrink"), orig)
	writeFile(t, filepath.Join(root, "grow"), orig[:100_000])
	s, _ := NewDirSource(root, SourceOptions{})
	defer s.Close()
	_, _, tree := listTree(t, s)
	// prime the windows, then change the files
	readNode(t, s, findNode(tree, "grow"), 0, 10)
	if err := os.WriteFile(filepath.Join(root, "grow"), append(append([]byte{}, orig[:100_000]...), orig[:50_000]...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(filepath.Join(root, "shrink"), 1000); err != nil {
		t.Fatal(err)
	}
	g := findNode(tree, "grow")
	buf := make([]byte, 200_000)
	k, err := s.ReadFileAt(g, buf, 0)
	if k != 100_000 || (err != nil && err != io.EOF) || !bytes.Equal(buf[:k], orig[:100_000]) {
		t.Errorf("grown file: %d bytes, %v", k, err)
	}
	if k, err := s.ReadFileAt(g, buf, 100_000); k != 0 || err == nil {
		t.Errorf("read past scanned size = %d, %v", k, err)
	}
	if _, err := readNode(t, s, findNode(tree, "shrink"), 0, 300_000); err == nil {
		t.Error("shrunk file: read of the missing tail succeeded")
	}
	if _, err := readNode(t, s, findNode(tree, "shrink"), 200_000, 50_000); err == nil {
		t.Error("shrunk file: read past new end succeeded")
	}
}

func TestDirSourceConcurrent(t *testing.T) {
	root := t.TempDir()
	data := map[string][]byte{}
	for i := 0; i < 12; i++ {
		n := fmt.Sprintf("f%02d", i)
		data[n] = randBytes(int64(100+i), 700_000)
		writeFile(t, filepath.Join(root, n), data[n])
	}
	s, _ := NewDirSource(root, SourceOptions{FDLimit: 2})
	defer s.Close()
	_, _, tree := listTree(t, s)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 200; k++ {
				name := fmt.Sprintf("f%02d", r.Intn(12))
				d := data[name]
				ln := 1 + r.Intn(5000)
				off := int64(r.Intn(len(d) - ln))
				b, err := readNode(t, s, findNode(tree, name), off, ln)
				if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
					t.Errorf("%s @%d+%d: %v", name, off, ln, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestDirTimes(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("test", 5*3600+1800)
	defer func() { time.Local = old }()
	root := t.TempDir()
	p := filepath.Join(root, "f")
	writeFile(t, p, []byte("x"))
	mt := time.Date(2021, 3, 4, 5, 6, 8, 0, time.Local)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	s, _ := NewDirSource(root, SourceOptions{})
	defer s.Close()
	_, _, tree := listTree(t, s)
	m := findNode(tree, "f").ModTime
	if m.Year() != 2021 || m.Month() != 3 || m.Day() != 4 || m.Hour() != 5 || m.Minute() != 6 || m.Second() != 8 {
		t.Errorf("ModTime fields = %v, want 2021-03-04 05:06:08 local wall clock", m)
	}
}

// zent describes a zip fixture entry.
type zent struct {
	name   string
	data   []byte
	method uint16
	utf8   bool // set the UTF-8 flag (otherwise NonUTF8)
	flags  uint16
	mod    time.Time
	raw    bool // write with CreateRaw (data is the stored bytes; crc from data)
}

func buildZip(t *testing.T, p string, ents []zent) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range ents {
		h := &zip.FileHeader{Name: e.name, Method: e.method, NonUTF8: !e.utf8, Modified: e.mod}
		if e.raw {
			h.Flags = e.flags
			h.CompressedSize64 = uint64(len(e.data))
			h.UncompressedSize64 = uint64(len(e.data))
			h.CRC32 = crc32.ChecksumIEEE(e.data)
			w, err := zw.CreateRaw(h)
			if err != nil {
				t.Fatal(err)
			}
			w.Write(e.data)
			continue
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(e.data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZipSourceTree(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.zip")
	buildZip(t, p, []zent{
		{name: "win\\sub\\a.txt", data: []byte("a"), method: zip.Deflate},
		{name: "./dot/b.txt", data: []byte("bb"), method: zip.Store},
		{name: "/abs/c.txt", data: []byte("ccc"), method: zip.Deflate},
		{name: "../evil.txt", data: []byte("e"), method: zip.Store},
		{name: "a/../evil2.txt", data: []byte("e"), method: zip.Store},
		{name: "dup.txt", data: []byte("first"), method: zip.Deflate},
		{name: "dup.txt", data: []byte("second!"), method: zip.Deflate},
		{name: "clash/", method: zip.Store},
		{name: "clash", data: []byte("f"), method: zip.Store},
		{name: "fileparent", data: []byte("f"), method: zip.Store},
		{name: "fileparent/child", data: []byte("f"), method: zip.Store},
		{name: "emptydir/", method: zip.Store},
		{name: "cp\x82\x8a.txt", data: []byte("cp"), method: zip.Store},
		{name: "utf8-é.txt", data: []byte("u"), method: zip.Store, utf8: true},
		{name: "enc.bin", data: []byte("secret"), method: zip.Store, raw: true, flags: 1},
		{name: "weird.bin", data: []byte("w"), method: 12, raw: true},
	})
	s, err := NewZipSource(p, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, warns, _ := listTree(t, s)
	want := []string{
		"abs/", "abs/c.txt:3", "clash/", "cpéè.txt:2", "dot/", "dot/b.txt:2", "dup.txt:5",
		"emptydir/", "fileparent:1", "utf8-é.txt:1", "win/", "win/sub/", "win/sub/a.txt:1",
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tree = %v\nwant   %v", got, want)
	}
	for _, sub := range []string{"../evil.txt", "a/../evil2.txt", "clash", "fileparent/child", "encrypted", "unsupported method", "second!"[:0] + "dup.txt"} {
		if !hasWarn(warns, sub) {
			t.Errorf("no warning mentioning %q in %v", sub, warns)
		}
	}
	for _, w := range warns {
		if !strings.HasPrefix(w, p) {
			t.Errorf("warning without source prefix: %q", w)
		}
	}
	// duplicate keeps the first
	_, _, tree := listTree(t, s)
	b, err := readNode(t, s, findNode(tree, "dup.txt"), 0, 5)
	if err != nil || string(b) != "first" {
		t.Errorf("dup = %q, %v", b, err)
	}
	if _, err := OpenSource(filepath.Join(t.TempDir(), "nope"), SourceOptions{}); err == nil {
		t.Error("missing path accepted")
	}
	notzip := filepath.Join(t.TempDir(), "n.zip")
	os.WriteFile(notzip, []byte("not a zip"), 0o644)
	if _, err := OpenSource(notzip, SourceOptions{}); err == nil {
		t.Error("non-zip accepted")
	}
}

func TestCP437Table(t *testing.T) {
	if n := len([]rune(cp437High)); n != 128 {
		t.Fatalf("table has %d runes", n)
	}
	if got := decodeCP437("\x80\x81\xe1\xfb A"); got != "Çüß√ A" {
		t.Errorf("got %q", got)
	}
}

func zipFixture(t *testing.T, cacheMB int) (Source, map[string][]byte, *Node, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.zip")
	data := map[string][]byte{
		"stored.bin":   randBytes(1, 900_000),
		"small.dat":    randBytes(2, 120_000),
		"d/deflate.1":  randBytes(3, 700_000),
		"d/deflate.2":  randBytes(4, 333_333),
		"empty":        {},
		"d/big.deflat": randBytes(5, 3_000_000),
	}
	var ents []zent
	for n, d := range data {
		m := uint16(zip.Deflate)
		if n == "stored.bin" {
			m = zip.Store
		}
		ents = append(ents, zent{name: n, data: d, method: m})
	}
	buildZip(t, p, ents)
	s, err := NewZipSource(p, SourceOptions{CacheMB: cacheMB})
	if err != nil {
		t.Fatal(err)
	}
	_, _, tree := listTree(t, s)
	return s, data, tree, p
}

func TestZipReadAt(t *testing.T) {
	s, data, tree, _ := zipFixture(t, 1) // d/big.deflat (3 MB) streams
	defer s.Close()
	r := rand.New(rand.NewSource(7))
	for name, d := range data {
		n := findNode(tree, name)
		if len(d) == 0 {
			if k, err := s.ReadFileAt(n, make([]byte, 4), 0); k != 0 || err == nil {
				t.Errorf("empty read = %d, %v", k, err)
			}
			continue
		}
		for k := 0; k < 60; k++ {
			ln := 1 + r.Intn(400_000)
			off := int64(r.Intn(len(d)))
			if off+int64(ln) > int64(len(d)) {
				ln = int(int64(len(d)) - off)
			}
			b, err := readNode(t, s, n, off, ln)
			if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
				t.Fatalf("%s @%d+%d: err=%v", name, off, ln, err)
			}
		}
		// explicit forward, then backward
		tail := min(len(d), 1000)
		for _, off := range []int64{int64(len(d) - tail), 0, int64(len(d) / 2), 0} {
			b, err := readNode(t, s, n, off, min(tail, len(d)-int(off)))
			if err != nil || !bytes.Equal(b, d[off:off+int64(len(b))]) {
				t.Fatalf("%s seek %d: %v", name, off, err)
			}
		}
	}
	// short read at the end
	n := findNode(tree, "small.dat")
	buf := make([]byte, 100)
	k, err := s.ReadFileAt(n, buf, int64(len(data["small.dat"])-10))
	if k != 10 || (err != io.EOF && err != nil) {
		t.Errorf("tail read = %d, %v", k, err)
	}
}

func TestZipConcurrent(t *testing.T) {
	s, data, tree, _ := zipFixture(t, 1)
	defer s.Close()
	var wg sync.WaitGroup
	names := []string{"stored.bin", "small.dat", "d/deflate.1", "d/deflate.2", "d/big.deflat"}
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 40; k++ {
				name := names[r.Intn(len(names))]
				d := data[name]
				ln := 1 + r.Intn(70_000)
				off := int64(r.Intn(len(d) - ln))
				b, err := readNode(t, s, findNode(tree, name), off, ln)
				if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
					t.Errorf("%s @%d+%d: %v", name, off, ln, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestZipSingleflight(t *testing.T) {
	s, data, tree, _ := zipFixture(t, 64)
	defer s.Close()
	n := findNode(tree, "d/deflate.1")
	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			b, err := readNode(t, s, n, int64(g*1000), 500)
			if err != nil || !bytes.Equal(b, data["d/deflate.1"][g*1000:g*1000+500]) {
				t.Errorf("g%d: %v", g, err)
			}
		}(g)
	}
	wg.Wait()
	if c := s.(*zipSource).inflations.Load(); c != 1 {
		t.Errorf("%d inflations for one entry, want 1", c)
	}
}

func TestZipCorruptEntry(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.zip")
	good := randBytes(11, 200_000)
	bad := randBytes(12, 200_000)
	buildZip(t, p, []zent{
		{name: "bad", data: bad, method: zip.Deflate},
		{name: "good", data: good, method: zip.Deflate},
	})
	raw, _ := os.ReadFile(p)
	zr, _ := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	off, _ := zr.File[0].DataOffset()
	raw[off+int64(zr.File[0].CompressedSize64)/2] ^= 0xFF
	os.WriteFile(p, raw, 0o644)
	for _, mb := range []int{64, 0} { // cached, and streaming when the budget is tiny
		s, err := NewZipSource(p, SourceOptions{CacheMB: max(mb, 1)})
		if err != nil {
			t.Fatal(err)
		}
		_, _, tree := listTree(t, s)
		if mb == 0 {
			s.(*zipSource).budget = 1000 // force streaming
		}
		if _, err := readNode(t, s, findNode(tree, "bad"), 0, 200_000); err == nil {
			t.Errorf("mb=%d: corrupt entry read without error", mb)
		}
		if _, err := readNode(t, s, findNode(tree, "bad"), 0, 10); err == nil && mb == 64 {
			t.Errorf("corruption not reported again")
		}
		b, err := readNode(t, s, findNode(tree, "good"), 5, 1000)
		if err != nil || !bytes.Equal(b, good[5:1005]) {
			t.Errorf("good entry: %v", err)
		}
		s.Close()
	}
}

func TestCacheBudget(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.zip")
	var ents []zent
	const entry = 300_000
	for i := 0; i < 30; i++ {
		ents = append(ents, zent{name: fmt.Sprintf("f%02d", i), data: randBytes(int64(i), entry), method: zip.Deflate})
	}
	buildZip(t, p, ents)
	s, _ := NewZipSource(p, SourceOptions{CacheMB: 1})
	defer s.Close()
	_, _, tree := listTree(t, s)
	for round := 0; round < 2; round++ {
		for i := 0; i < 30; i++ {
			if _, err := readNode(t, s, findNode(tree, fmt.Sprintf("f%02d", i)), 0, 100); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := s.(*zipSource).cache
	if c.peak > c.budget+entry {
		t.Errorf("peak %d exceeds budget %d by more than one entry (%d)", c.peak, c.budget, entry)
	}
	if c.total > c.budget {
		t.Errorf("total %d > budget %d", c.total, c.budget)
	}
	if got := s.(*zipSource).inflations.Load(); got <= 30 {
		t.Errorf("only %d inflations: eviction not exercised", got)
	}
}

func TestZipTimes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.zip")
	f, _ := os.Create(p)
	zw := zip.NewWriter(f)
	// DOS-only time: stored wall-clock fields, no zone.
	h := &zip.FileHeader{Name: "dos.txt", Method: zip.Store}
	h.ModifiedDate = uint16(2020-1980)<<9 | 5<<5 | 6
	h.ModifiedTime = 7<<11 | 8<<5 | 10/2
	w, _ := zw.CreateHeader(h)
	w.Write([]byte("x"))
	// Extended timestamp written by archive/zip: DOS fields are the wall clock
	// of the given time in its own zone; those are the stored fields.
	h = &zip.FileHeader{Name: "ext.txt", Method: zip.Store,
		Modified: time.Date(2019, 12, 31, 23, 58, 56, 0, time.FixedZone("z", -3*3600))}
	w, _ = zw.CreateHeader(h)
	w.Write([]byte("y"))
	// DOS time and the extended-timestamp (UT) field disagree: DOS wins.
	ut := time.Date(2020, 6, 1, 10, 0, 0, 0, time.UTC).Unix()
	h = &zip.FileHeader{Name: "both.txt", Method: zip.Store,
		Extra: []byte{0x55, 0x54, 5, 0, 1, byte(ut), byte(ut >> 8), byte(ut >> 16), byte(ut >> 24)}}
	h.ModifiedDate = uint16(2020-1980)<<9 | 6<<5 | 1
	h.ModifiedTime = 12<<11 | 0<<5 | 0
	w, _ = zw.CreateHeader(h)
	w.Write([]byte("z"))
	w, _ = zw.CreateHeader(&zip.FileHeader{Name: "dir/", Method: zip.Store})
	_ = w
	zw.Close()
	f.Close()
	s, err := NewZipSource(p, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _, tree := listTree(t, s)
	chk := func(name string, y int, mo time.Month, d, h, mi, sec int) {
		t.Helper()
		m := findNode(tree, name).ModTime
		if m.Year() != y || m.Month() != mo || m.Day() != d || m.Hour() != h || m.Minute() != mi || m.Second() != sec {
			t.Errorf("%s ModTime = %v, want %d-%02d-%02d %02d:%02d:%02d", name, m, y, mo, d, h, mi, sec)
		}
	}
	chk("dos.txt", 2020, 5, 6, 7, 8, 10)
	chk("ext.txt", 2019, 12, 31, 23, 58, 56)
	chk("both.txt", 2020, 6, 1, 12, 0, 0)
	if !findNode(tree, "dir").ModTime.IsZero() {
		t.Errorf("dir without time = %v", findNode(tree, "dir").ModTime)
	}
	// visible through a Volume + mdir
	needTool(t, "mdir")
	v, _, err := NewVolume(s, VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	img := dumpVolume(t, v)
	cmd := exec.Command("mdir", "-i", img+"@@65536", "::")
	cmd.Env = mtoolsEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mdir: %v\n%s", err, out)
	}
	if !regexp.MustCompile(`2020-05-06\s+7:08`).Match(out) {
		t.Errorf("FAT listing lacks 2020-05-06 7:08:\n%s", out)
	}
	if !regexp.MustCompile(`BOTH\s+TXT\s+1\s+2020-06-01\s+12:00`).Match(out) {
		t.Errorf("FAT listing lacks both.txt 2020-06-01 12:00 (DOS time, not UT):\n%s", out)
	}
}

func TestVolumeFromDirAndZip(t *testing.T) {
	needTool(t, "mdir", "mcopy", "fsck.vfat")
	root := t.TempDir()
	files := map[string][]byte{
		"readme.txt":                           []byte("hello\n"),
		"games/big.bin":                        randBytes(21, 1_500_000),
		"games/Long File Name with spaces.dat": randBytes(22, 70_000),
		"games/sub/x.tap":                      randBytes(23, 33_333),
		"empty.bin":                            {},
	}
	var ents []zent
	for n, d := range files {
		writeFile(t, filepath.Join(root, n), d)
		m := uint16(zip.Deflate)
		if strings.HasSuffix(n, ".tap") {
			m = zip.Store
		}
		ents = append(ents, zent{name: n, data: d, method: m})
	}
	os.Mkdir(filepath.Join(root, "emptydir"), 0o755)
	ents = append(ents, zent{name: "emptydir/", method: zip.Store})
	zp := filepath.Join(t.TempDir(), "v.zip")
	buildZip(t, zp, ents)

	var imgs [2]string
	for i, p := range []string{root, zp} {
		s, err := OpenSource(p, SourceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		v, _, err := NewVolume(s, VolumeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		imgs[i] = dumpVolume(t, v)
		fsck(t, imgs[i], v)
		v.Close()
	}
	l0, l1 := mdirList(t, imgs[0]), mdirList(t, imgs[1])
	if strings.Join(l0, "\n") != strings.Join(l1, "\n") {
		t.Fatalf("listings differ:\n%v\n%v", l0, l1)
	}
	for n, d := range files {
		for i := range imgs {
			if got := mcopyOut(t, imgs[i], "::"+n); !bytes.Equal(got, d) {
				t.Errorf("image %d: %s differs", i, n)
			}
		}
	}
}

func TestCacheLoadPanic(t *testing.T) {
	c := newByteCache(1 << 20)
	release := make(chan struct{})
	errs := make(chan error, 6)
	for i := 0; i < 5; i++ {
		go func() {
			_, err := c.get(1, func() ([]byte, error) { <-release; panic("boom") })
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond) // let the waiters queue up behind the leader
	close(release)
	for i := 0; i < 5; i++ {
		select {
		case err := <-errs:
			if err == nil || !strings.Contains(err.Error(), "boom") {
				t.Errorf("err = %v, want one mentioning the panic value", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a caller is blocked after the loader panicked")
		}
	}
	d, err := c.get(1, func() ([]byte, error) { return []byte("ok"), nil })
	if err != nil || string(d) != "ok" {
		t.Errorf("retry = %q, %v", d, err)
	}
}

func TestCacheLeaderFailureRetry(t *testing.T) {
	c := newByteCache(1 << 20)
	release := make(chan struct{})
	errs := make(chan error, 4)
	boom := errors.New("load failed")
	for i := 0; i < 4; i++ {
		go func() {
			_, err := c.get(7, func() ([]byte, error) { <-release; return nil, boom })
			errs <- err
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	for i := 0; i < 4; i++ {
		if err := <-errs; !errors.Is(err, boom) {
			t.Errorf("err = %v", err)
		}
	}
	calls := 0
	d, err := c.get(7, func() ([]byte, error) { calls++; return []byte("fine"), nil })
	if err != nil || string(d) != "fine" || calls != 1 {
		t.Errorf("retry = %q, %v, loader calls %d", d, err, calls)
	}
}

func TestZipConcurrentStream(t *testing.T) {
	s, data, tree, _ := zipFixture(t, 1)
	defer s.Close()
	n := findNode(tree, "d/big.deflat")
	d := data["d/big.deflat"]
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 12; k++ {
				ln := 1 + r.Intn(50_000)
				off := int64(r.Intn(len(d) - ln))
				b, err := readNode(t, s, n, off, ln)
				if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
					t.Errorf("@%d+%d: %v", off, ln, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestCdata(t *testing.T) {
	for _, n := range []int{0, 1, blockSize - 1, blockSize, blockSize + 1, 5*blockSize + 7} {
		b := randBytes(int64(n), n)
		d := cdataOf(b)
		if len(d.blk) != (n+blockSize-1)/blockSize || !bytes.Equal(d.bytes(), b) {
			t.Errorf("n=%d: %d blocks, contents differ", n, len(d.blk))
		}
		for _, off := range []int64{0, 1, blockSize - 1, blockSize, int64(n) - 1, int64(n), int64(n) + 5} {
			if off < 0 {
				continue
			}
			p := make([]byte, 3*blockSize/2)
			k := d.copyAt(p, off)
			want := 0
			if off < int64(n) {
				want = min(len(p), n-int(off))
			}
			if k != want || (k > 0 && !bytes.Equal(p[:k], b[off:int(off)+k])) {
				t.Errorf("n=%d off=%d: copied %d, want %d", n, off, k, want)
			}
		}
	}
}

// Buffers of evicted entries are recycled; a buffer pinned by a reader is
// never recycled under it.
func TestCacheRecycleSafe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "r.zip")
	var ents []zent
	data := map[string][]byte{}
	for i := 0; i < 40; i++ {
		n := fmt.Sprintf("f%02d", i)
		d := randBytes(int64(i), 60_000+i*1000)
		data[n] = d
		ents = append(ents, zent{name: n, data: d, method: zip.Deflate})
	}
	buildZip(t, p, ents)
	s, _ := NewZipSource(p, SourceOptions{CacheMB: 1})
	defer s.Close()
	_, _, tree := listTree(t, s)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for k := 0; k < 150; k++ {
				n := fmt.Sprintf("f%02d", r.Intn(40))
				d := data[n]
				ln := 1 + r.Intn(len(d)-1)
				off := int64(r.Intn(len(d) - ln))
				b, err := readNode(t, s, findNode(tree, n), off, ln)
				if err != nil || !bytes.Equal(b, d[off:off+int64(ln)]) {
					t.Errorf("%s @%d+%d: %v", n, off, ln, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	c := s.(*zipSource).cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.free) == 0 {
		t.Error("no buffer was recycled")
	}
	if int64(len(c.free))*blockSize > c.freeMax || c.total > c.budget {
		t.Errorf("free %d blocks (max %d bytes), total %d (budget %d)", len(c.free), c.freeMax, c.total, c.budget)
	}
	if len(c.m) != c.lru.Len() {
		t.Errorf("map %d != lru %d", len(c.m), c.lru.Len())
	}
}
