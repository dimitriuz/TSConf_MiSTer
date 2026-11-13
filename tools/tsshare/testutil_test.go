package main

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// memFile describes one file of a memSource. Without Data the contents are
// generated from the path and the offset, so large files cost no memory.
type memFile struct {
	Size    int64
	Data    []byte
	ModTime time.Time
}

// memSource is an in-memory Source built from a path -> file map. A key ending
// in "/" is an (empty) directory.
type memSource struct {
	files map[string]memFile
	paths []string // by Node.ID
	// failPath, when set, makes reads of that file fail.
	failPath string
}

func newMemSource(files map[string]memFile) *memSource {
	return &memSource{files: files}
}

func genByte(p string, off int64) byte {
	h := fnv.New64a()
	h.Write([]byte(p))
	x := h.Sum64() + uint64(off)*0x9E3779B97F4A7C15
	x ^= x >> 29
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 32
	return byte(x >> 8)
}

// expected returns the bytes the source serves for p at off.
func (s *memSource) expected(p string, off int64, n int) []byte {
	f := s.files[p]
	out := make([]byte, n)
	for i := range out {
		if f.Data != nil {
			out[i] = f.Data[off+int64(i)]
		} else {
			out[i] = genByte(p, off+int64(i))
		}
	}
	return out
}

func (s *memSource) Tree() (*Node, []string, error) {
	root := &Node{IsDir: true}
	dirs := map[string]*Node{"": root}
	var mk func(dir string) *Node
	mk = func(dir string) *Node {
		if n, ok := dirs[dir]; ok {
			return n
		}
		parent := root
		if pd := path.Dir(dir); pd != "." {
			parent = mk(pd)
		}
		n := &Node{Name: path.Base(dir), IsDir: true}
		parent.Children = append(parent.Children, n)
		dirs[dir] = n
		return n
	}
	keys := make([]string, 0, len(s.files))
	for k := range s.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s.paths = nil
	for _, k := range keys {
		f := s.files[k]
		if strings.HasSuffix(k, "/") {
			d := mk(strings.TrimSuffix(k, "/"))
			d.ModTime = f.ModTime
			continue
		}
		dir := path.Dir(k)
		parent := root
		if dir != "." {
			parent = mk(dir)
		}
		size := f.Size
		if f.Data != nil {
			size = int64(len(f.Data))
		}
		s.paths = append(s.paths, k)
		parent.Children = append(parent.Children, &Node{
			Name: path.Base(k), Size: size, ModTime: f.ModTime, ID: len(s.paths) - 1,
		})
	}
	return root, nil, nil
}

func (s *memSource) ReadFileAt(n *Node, p []byte, off int64) (int, error) {
	pa := s.paths[n.ID]
	if pa == s.failPath {
		return 0, fmt.Errorf("injected failure reading %s", pa)
	}
	f := s.files[pa]
	size := f.Size
	if f.Data != nil {
		size = int64(len(f.Data))
	}
	if off >= size {
		return 0, fmt.Errorf("read past end of %s", pa)
	}
	k := int(min(int64(len(p)), size-off))
	copy(p, s.expected(pa, off, k))
	return k, nil
}

func (s *memSource) Close() error { return nil }

// dumpVolume writes the whole virtual disk to a temp file and returns its path.
func dumpVolume(t *testing.T, v *Volume) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "disk.img")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for off := int64(0); off < v.Size(); off += int64(len(buf)) {
		n := int(min(int64(len(buf)), v.Size()-off))
		if _, err := v.ReadAt(buf[:n], off); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(buf[:n]); err != nil {
			t.Fatal(err)
		}
	}
	return name
}

func needTool(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			t.Skipf("%s not installed; skipping", n)
		}
	}
}

// fsck runs fsck.vfat -n on the partition of the dumped disk (extracted with
// dd, since fsck.vfat has no --offset) and fails on any non-zero exit. It
// returns fsck's output.
func fsck(t *testing.T, img string, v *Volume) string {
	t.Helper()
	needTool(t, "fsck.vfat", "dd")
	part := filepath.Join(t.TempDir(), "part.img")
	out, err := exec.Command("dd", "if="+img, "of="+part, "bs=512",
		fmt.Sprintf("skip=%d", v.partStart), fmt.Sprintf("count=%d", v.partSectors)).CombinedOutput()
	if err != nil {
		t.Fatalf("dd: %v\n%s", err, out)
	}
	out, err = exec.Command("fsck.vfat", "-n", part).CombinedOutput()
	os.Remove(part)
	if err != nil {
		t.Fatalf("fsck.vfat -n failed: %v\n%s", err, out)
	}
	return string(out)
}

func mtoolsEnv() []string {
	return append(os.Environ(), "LC_ALL=C.UTF-8", "MTOOLS_SKIP_CHECK=1")
}

// mdirList returns the long names of everything under the image, recursively,
// as paths without a leading slash (directories without trailing slash).
func mdirList(t *testing.T, img string) []string {
	t.Helper()
	needTool(t, "mdir")
	cmd := exec.Command("mdir", "-i", img+"@@65536", "-/", "-b", "::")
	cmd.Env = mtoolsEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("mdir: %v\n%s", err, out)
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		res = append(res, strings.TrimPrefix(l, "::/"))
	}
	sort.Strings(res)
	return res
}

// mcopyOut copies a file out of the image and returns its bytes. src is like
// "::dir/file".
func mcopyOut(t *testing.T, img, src string) []byte {
	t.Helper()
	needTool(t, "mcopy")
	dst := filepath.Join(t.TempDir(), "out")
	cmd := exec.Command("mcopy", "-i", img+"@@65536", src, dst)
	cmd.Env = mtoolsEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("mcopy %s: %v\n%s", src, err, out)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readAll(t *testing.T, v *Volume, off int64, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	got, err := v.ReadAt(b, off)
	if err != nil || got != n {
		t.Fatalf("ReadAt(%d,%d) = %d, %v", off, n, got, err)
	}
	return b
}

var _ = bytes.Equal
