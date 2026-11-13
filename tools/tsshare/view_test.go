package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestViewName(t *testing.T) {
	type step struct {
		in   string
		want string // "" = not shown
	}
	cases := []struct {
		name  string
		taken []string // lower-cased names already taken
		steps []step
	}{
		{"basic", nil, []step{{"SD.zip", "SD.vhd"}, {"A.ZIP", "A.vhd"}, {"b.Zip", "b.vhd"}}},
		{"case twins", nil, []step{{"a.zip", "a.vhd"}, {"A.ZIP", "A (2).vhd"}, {"a.Zip", "a (3).vhd"}}},
		{"double zip", nil, []step{{"x.zip.zip", ""}, {"x.ZIP.zip", ""}, {"a.zipx.zip", ""}}},
		{"hidden", nil, []step{{".x.zip", ""}, {".zip", ""}, {"..zip", ""}}},
		{"not a zip", nil, []step{{"x.vhd", ""}, {"x.zip.d", ""}, {"zip", ""}, {"x.7z", ""}, {"X.ZIPP", ""}}},
		{"unicode", nil, []step{{"Игра.zip", "Игра.vhd"}, {"игра.ZIP", "игра (2).vhd"}, {"日本 語.zip", "日本 語.vhd"}}},
		{"taken by a directory", []string{"games.vhd"}, []step{{"games.zip", "games (2).vhd"}, {"Games.zip", "Games (3).vhd"}}},
		{"taken by shared", []string{"shared.vhd"}, []step{{"shared.zip", "shared (2).vhd"}, {"SHARED.ZIP", "SHARED (3).vhd"}}},
		{"suffix already taken", []string{"a (2).vhd"}, []step{{"a.zip", "a.vhd"}, {"A.zip", "A (3).vhd"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			taken := map[string]bool{}
			for _, n := range c.taken {
				taken[n] = true
			}
			for _, s := range c.steps {
				got, ok := viewName(s.in, taken)
				if s.want == "" {
					if ok {
						t.Errorf("viewName(%q) = %q, want hidden", s.in, got)
					}
					continue
				}
				if !ok || got != s.want {
					t.Errorf("viewName(%q) = %q, %v; want %q", s.in, got, ok, s.want)
				}
			}
		})
	}
}

func mkfile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeZipFile writes a zip with the given files.
func writeZipFile(t *testing.T, p string, files map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(files[n])
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	mkfile(t, p, buf.Bytes())
}

func shownNames(es []viewEntry) []string {
	var r []string
	for _, e := range es {
		r = append(r, e.Name)
	}
	return r
}

func TestShownEntries(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	small := map[string][]byte{"a.txt": []byte("a")}
	for _, d := range []string{"games", "Docs", "foo.zip.d", ".hidden", "mnt", "shared", "shared.vhd", "real", "Docs.VHD"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "linkdir"))
	writeZipFile(t, filepath.Join(root, "SD.zip"), small)
	writeZipFile(t, filepath.Join(root, "a.zip"), small)
	writeZipFile(t, filepath.Join(root, "A.ZIP"), small)
	writeZipFile(t, filepath.Join(root, "x.zip.zip"), small)
	writeZipFile(t, filepath.Join(root, ".x.zip"), small)
	writeZipFile(t, filepath.Join(root, "shared.zip"), small)
	writeZipFile(t, filepath.Join(root, "docs.zip"), small)
	writeZipFile(t, filepath.Join(root, "shared", "inner.zip"), small)
	mkfile(t, filepath.Join(root, "plain.vhd"), []byte("x"))
	mkfile(t, filepath.Join(root, "notes.txt"), []byte("x"))
	os.Symlink(filepath.Join(root, "SD.zip"), filepath.Join(root, "link.zip"))
	os.Symlink(filepath.Join(root, "nowhere.zip"), filepath.Join(root, "dangling.zip"))

	excl := []string{filepath.Join(root, "mnt"), filepath.Join(root, "shared")}
	es, err := shownEntries(root, excl, 0, "shared.vhd")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(shownNames(es), "|")
	// directories first (sorted case-insensitively), then zips in real-name order
	want := "Docs|Docs.VHD|games|real|A.vhd|a (2).vhd|docs (2).vhd|link.vhd|SD.vhd|shared (2).vhd"
	if got != want {
		t.Fatalf("shown\n got %s\nwant %s", got, want)
	}
	byName := map[string]viewEntry{}
	for _, e := range es {
		byName[e.Name] = e
	}
	if e := byName["a (2).vhd"]; e.Real != "a.zip" || e.IsDir || e.Size <= 0 {
		t.Errorf("entry %+v", e)
	}
	if e := byName["A.vhd"]; e.Real != "A.ZIP" {
		t.Errorf("entry %+v", e)
	}
	// a second scan maps the same way
	es2, _ := shownEntries(root, excl, 0, "shared.vhd")
	if strings.Join(shownNames(es2), "|") != want {
		t.Errorf("mapping not deterministic: %v", shownNames(es2))
	}
	// without exclusions the mount dir and shared dir show up
	es3, _ := shownEntries(root, nil, 0)
	g3 := strings.Join(shownNames(es3), "|")
	for _, n := range []string{"mnt", "shared", "shared.vhd"} {
		if !strings.Contains("|"+g3+"|", "|"+n+"|") {
			t.Errorf("%q missing without exclusion: %s", n, g3)
		}
	}
	// a symlinked exclusion path (lexically different) still excludes
	link := filepath.Join(t.TempDir(), "lnk")
	os.Symlink(root, link)
	es4, err := shownEntries(link, excl, 0)
	if err != nil || strings.Contains("|"+strings.Join(shownNames(es4), "|")+"|", "|mnt|") {
		t.Errorf("symlinked root: %v %v", shownNames(es4), err)
	}
	// missing dir is an error
	if _, err := shownEntries(filepath.Join(root, "nope"), nil, 0); err == nil {
		t.Error("missing dir gave no error")
	}
}

func TestShownEntriesCache(t *testing.T) {
	root := t.TempDir()
	writeZipFile(t, filepath.Join(root, "a.zip"), map[string][]byte{"x": []byte("x")})
	ttl := 150 * time.Millisecond
	es, _ := shownEntries(root, nil, ttl)
	if len(es) != 1 {
		t.Fatalf("%v", shownNames(es))
	}
	writeZipFile(t, filepath.Join(root, "b.zip"), map[string][]byte{"x": []byte("x")})
	if es, _ = shownEntries(root, nil, ttl); len(es) != 1 {
		t.Fatalf("cache ignored: %v", shownNames(es))
	}
	time.Sleep(ttl + 50*time.Millisecond)
	if es, _ = shownEntries(root, nil, ttl); len(es) != 2 {
		t.Fatalf("cache not expired: %v", shownNames(es))
	}
	// errors are not cached
	gone := filepath.Join(root, "gone")
	if _, err := shownEntries(gone, nil, time.Minute); err == nil {
		t.Fatal("want error")
	}
	os.Mkdir(gone, 0o755)
	if _, err := shownEntries(gone, nil, time.Minute); err != nil {
		t.Fatalf("error was cached: %v", err)
	}
}

// ---- FUSE tests ----

// startView runs the daemon in view mode with the given flags (including
// -mount, which must be given as the mount path used for readiness).
func startView(t *testing.T, mnt string, cwd string, args ...string) *daemon {
	t.Helper()
	needFuse(t)
	d := startViewRaw(t, mnt, cwd, args...)
	probe := filepath.Join(mnt, sharedName)
	if !filepath.IsAbs(mnt) {
		probe = filepath.Join(cwd, probe)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(probe); err == nil {
			return d
		}
		select {
		case err := <-d.done:
			t.Fatalf("daemon exited early: %v\n%s", err, d.sb)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("view did not appear\n%s", d.sb)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startViewRaw(t *testing.T, mnt, cwd string, args ...string) *daemon {
	t.Helper()
	needFuse(t)
	abs := mnt
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, mnt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, daemonBin, args...)
	cmd.Dir = cwd
	d := &daemon{cmd: cmd, mnt: abs, stderr: &bytes.Buffer{}, done: make(chan error, 1), cancel: cancel}
	sb := &syncBuf{}
	cmd.Stderr = sb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { d.done <- cmd.Wait(); close(d.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-d.done:
		case <-time.After(5 * time.Second):
		}
		exec.Command("fusermount3", "-uz", abs).Run()
	})
	d.sb = sb
	return d
}

type viewEnv struct {
	root, shared, mnt string
	d                 *daemon
}

var (
	aFiles = treeFiles
	bFiles = map[string][]byte{"readme.txt": []byte("second zip\n"), "dir/x.bin": bytes.Repeat([]byte{9, 8, 7}, 3000)}
	sFiles = map[string][]byte{"s1.txt": []byte("shared one\n"), "inner/s2.txt": []byte("shared two\n")}
)

// newViewEnv builds <root>/{A.zip, b.zip, sub/c.zip, shared/..} and mounts the
// view at <root>/share, so the mount point lies inside the zip root.
func newViewEnv(t *testing.T, extra ...string) *viewEnv {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	writeZipFile(t, filepath.Join(root, "A.zip"), aFiles)
	writeZipFile(t, filepath.Join(root, "b.zip"), bFiles)
	writeZipFile(t, filepath.Join(root, "sub", "c.zip"), bFiles)
	for p, b := range sFiles {
		mkfile(t, filepath.Join(root, "shared", p), b)
	}
	mnt := filepath.Join(root, "share")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-view", root, "-mount", mnt}, extra...)
	e := &viewEnv{root: root, shared: filepath.Join(root, "shared"), mnt: mnt}
	e.d = startView(t, mnt, root, args...)
	return e
}

func (e *viewEnv) p(name string) string { return filepath.Join(e.mnt, name) }

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var r []string
	for _, d := range des {
		r = append(r, d.Name())
	}
	sort.Strings(r)
	return r
}

func openErr(path string) error {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
	}
	return err
}

func checkDisk(t *testing.T, img string, files map[string][]byte) {
	t.Helper()
	f, err := os.Open(img) // the size is the built one only on an open handle
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(t.TempDir(), "part.img")
	if out, err := exec.Command("dd", "if="+img, "of="+part, "bs=64k", "iflag=skip_bytes,count_bytes",
		"skip=65536", fmt.Sprintf("count=%d", st.Size()-65536)).CombinedOutput(); err != nil {
		t.Fatalf("dd: %v\n%s", err, out)
	}
	if out, err := exec.Command("fsck.vfat", "-n", part).CombinedOutput(); err != nil {
		t.Fatalf("fsck.vfat %s: %v\n%s", img, err, out)
	}
	os.Remove(part)
	for p, b := range files {
		if len(b) == 0 {
			continue
		}
		if g := mcopyOut(t, img, "::"+p); !bytes.Equal(g, b) {
			t.Errorf("%s: %s content mismatch (%d vs %d bytes)", img, p, len(g), len(b))
		}
	}
}

func TestViewMountListsAndReads(t *testing.T) {
	needFuse(t)
	needTool(t, "fsck.vfat", "mdir", "mcopy", "dd")
	e := newViewEnv(t)
	got := dirNames(t, e.mnt)
	want := []string{"A.vhd", "b.vhd", "shared.vhd", "sub"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("root listing %q, want %q", got, want)
	}
	if got := dirNames(t, e.p("sub")); strings.Join(got, "|") != "c.vhd" {
		t.Fatalf("sub listing %q", got)
	}
	st, err := os.Stat(e.p("A.vhd"))
	if err != nil || st.Mode().Perm() != 0o444 || !st.Mode().IsRegular() {
		t.Fatalf("A.vhd: %v %v", st, err)
	}
	if st, err := os.Stat(e.p("sub")); err != nil || !st.IsDir() {
		t.Fatalf("sub: %v %v", st, err)
	}
	checkDisk(t, e.p("A.vhd"), aFiles)
	checkDisk(t, e.p("b.vhd"), bFiles)
	checkDisk(t, e.p("sub/c.vhd"), bFiles)
	checkDisk(t, e.p("shared.vhd"), sFiles)
	for _, n := range []string{"share", "shared", "A.zip", "b.zip"} {
		if _, err := os.Lstat(e.p(n)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should not exist: %v", n, err)
		}
	}
	if err := e.d.stop(t); exitCode(err) != 0 {
		t.Fatalf("exit: %v\n%s", err, e.d.sb)
	}
	out := e.d.sb.String()
	for _, s := range []string{"view: zip root " + e.root, "view: shared folder " + e.shared, "view: mount " + e.mnt,
		"serving view at " + e.mnt + " (volumes are built when a file is opened)", "build "} {
		if !strings.Contains(out, s) {
			t.Errorf("stderr lacks %q:\n%s", s, out)
		}
	}
}

func TestViewSizes(t *testing.T) {
	needFuse(t)
	e := newViewEnv(t)
	zp := filepath.Join(e.root, "A.zip")
	zst, _ := os.Stat(zp)
	st, err := os.Stat(e.p("A.vhd"))
	if err != nil || st.Size() != zst.Size() {
		t.Fatalf("before open: size %v (%v), zip %d", st, err, zst.Size())
	}
	if st.ModTime().Sub(zst.ModTime()).Abs() > time.Second {
		t.Errorf("mtime %v, zip %v", st.ModTime(), zst.ModTime())
	}
	// what -src reports for the same zip
	ref := startDaemon(t, zp)
	rst, err := os.Stat(ref.vhd())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(e.p("A.vhd"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fst, err := f.Stat()
	if err != nil || fst.Size() != rst.Size() {
		t.Fatalf("fstat after open: %v (%v), want %d", fst, err, rst.Size())
	}
	if end, err := f.Seek(0, io.SeekEnd); err != nil || end != rst.Size() {
		t.Fatalf("SEEK_END = %d, %v; want %d", end, err, rst.Size())
	}
	if st, err := os.Stat(e.p("A.vhd")); err != nil || st.Size() != rst.Size() {
		t.Fatalf("second stat while open: %v %v, want %d", st, err, rst.Size())
	}
	f.Close()
	// after the last close the real zip size is shown again
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, err := os.Stat(e.p("A.vhd"))
		if err == nil && st.Size() == zst.Size() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("size after close: %v %v, want %d", st, err, zst.Size())
		}
		time.Sleep(20 * time.Millisecond)
	}
	// shared.vhd: 0 before any open, the built size while open
	if st, err := os.Stat(e.p("shared.vhd")); err != nil || st.Size() != 0 {
		t.Fatalf("shared.vhd before open: %v %v", st, err)
	}
	sf, err := os.Open(e.p("shared.vhd"))
	if err != nil {
		t.Fatal(err)
	}
	defer sf.Close()
	if st, err := sf.Stat(); err != nil || st.Size() <= 0 {
		t.Fatalf("shared.vhd after open: %v %v", st, err)
	}
}

func TestViewReopenRescan(t *testing.T) {
	needFuse(t)
	needTool(t, "mdir", "mcopy")
	e := newViewEnv(t)
	has := func(img, name string) bool {
		for _, n := range mdirList(t, img) {
			if n == name {
				return true
			}
		}
		return false
	}
	if has(e.p("shared.vhd"), "new.txt") {
		t.Fatal("new.txt present too early")
	}
	mkfile(t, filepath.Join(e.shared, "new.txt"), []byte("brand new\n"))
	if !has(e.p("shared.vhd"), "new.txt") {
		t.Fatal("new.txt missing after reopen")
	}
	if g := mcopyOut(t, e.p("shared.vhd"), "::new.txt"); string(g) != "brand new\n" {
		t.Fatalf("new.txt = %q", g)
	}
	// a zip modified on disk
	if !has(e.p("b.vhd"), "readme.txt") || has(e.p("b.vhd"), "extra.txt") {
		t.Fatal("b.vhd precondition")
	}
	nb := map[string][]byte{"readme.txt": []byte("changed\n"), "extra.txt": bytes.Repeat([]byte("e"), 777)}
	writeZipFile(t, filepath.Join(e.root, "b.zip"), nb)
	if !has(e.p("b.vhd"), "extra.txt") {
		t.Fatal("extra.txt missing after the zip changed")
	}
	if g := mcopyOut(t, e.p("b.vhd"), "::readme.txt"); string(g) != "changed\n" {
		t.Fatalf("readme.txt = %q", g)
	}
	// a zip added shows up (after the listing cache, 5 s) -- not tested here;
	// a removed zip fails its open
	os.Remove(filepath.Join(e.root, "b.zip"))
	if err := openErr(e.p("b.vhd")); err == nil || errors.Is(err, syscall.EBUSY) {
		t.Fatalf("open of a removed zip: %v", err)
	}
}

func hashFile(t *testing.T, p string) [32]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 6<<20)); err != nil { // header, FAT and the first data
		t.Fatal(err)
	}
	var r [32]byte
	copy(r[:], h.Sum(nil))
	return r
}

func TestViewSwitch(t *testing.T) {
	needFuse(t)
	e := newViewEnv(t)
	a1 := hashFile(t, e.p("A.vhd"))
	b1 := hashFile(t, e.p("b.vhd"))
	if a1 == b1 {
		t.Fatal("A and b are the same")
	}
	fa, err := os.Open(e.p("A.vhd"))
	if err != nil {
		t.Fatal(err)
	}
	fa.Close()
	fb, err := os.Open(e.p("b.vhd"))
	if err != nil {
		t.Fatal(err)
	}
	fa2, err := os.Open(e.p("A.vhd"))
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	io.Copy(h, io.LimitReader(fa2, 6<<20))
	var a2 [32]byte
	copy(a2[:], h.Sum(nil))
	h.Reset()
	io.Copy(h, io.LimitReader(fb, 6<<20))
	var b2 [32]byte
	copy(b2[:], h.Sum(nil))
	fa2.Close()
	fb.Close()
	if a1 != a2 || b1 != b2 {
		t.Fatal("content changed after switching")
	}
	if hashFile(t, e.p("shared.vhd")) != hashFile(t, e.p("shared.vhd")) {
		t.Fatal("shared.vhd not stable")
	}
}

func TestViewReadOnly(t *testing.T) {
	needFuse(t)
	e := newViewEnv(t)
	ro := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) {
			t.Errorf("%s: %v, want EROFS or EACCES", what, err)
		}
	}
	for _, n := range []string{"A.vhd", "shared.vhd"} {
		_, err := os.OpenFile(e.p(n), os.O_WRONLY, 0)
		ro("open O_WRONLY "+n, err)
		_, err = os.OpenFile(e.p(n), os.O_RDWR, 0)
		ro("open O_RDWR "+n, err)
		_, err = os.OpenFile(e.p(n), os.O_RDONLY|os.O_TRUNC, 0)
		ro("open O_TRUNC "+n, err)
		ro("truncate "+n, os.Truncate(e.p(n), 0))
		ro("chmod "+n, os.Chmod(e.p(n), 0o666))
		ro("remove "+n, os.Remove(e.p(n)))
		ro("rename "+n, os.Rename(e.p(n), e.p("z.vhd")))
	}
	_, err := os.Create(e.p("new.vhd"))
	ro("create", err)
	ro("mkdir", os.Mkdir(e.p("d"), 0o755))
	ro("mkdir sub/d", os.Mkdir(e.p("sub/d"), 0o755))
	ro("symlink", os.Symlink("x", e.p("l")))
	ro("rmdir", os.Remove(e.p("sub")))
	ro("link", os.Link(e.p("A.vhd"), e.p("hard")))
	if _, err := os.Stat(e.p("A.vhd")); err != nil {
		t.Fatal(err)
	}
}

func TestViewBusy(t *testing.T) {
	needFuse(t)
	e := newViewEnv(t)
	var open []*os.File
	for _, n := range []string{"A.vhd", "b.vhd", "sub/c.vhd"} {
		f, err := os.Open(e.p(n))
		if err != nil {
			t.Fatalf("open %s: %v", n, err)
		}
		open = append(open, f)
		defer f.Close()
	}
	if err := openErr(e.p("shared.vhd")); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("4th open: %v, want EBUSY", err)
	}
	// the same file again does not need a slot
	f2, err := os.Open(e.p("A.vhd"))
	if err != nil {
		t.Fatalf("second handle of A.vhd: %v", err)
	}
	f2.Close()
	for i, f := range open {
		buf := make([]byte, 512)
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Errorf("read %d: %v", i, err)
		}
	}
	open[0].Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := openErr(e.p("shared.vhd"))
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EBUSY) || time.Now().After(deadline) {
			t.Fatalf("open after a release: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestViewNeverRecurses(t *testing.T) {
	needFuse(t)
	tmp := t.TempDir()
	tmp, _ = filepath.EvalSymlinks(tmp)
	mk := func(name string) string {
		root := filepath.Join(tmp, name)
		writeZipFile(t, filepath.Join(root, "A.zip"), aFiles)
		os.MkdirAll(filepath.Join(root, "sub", "m"), 0o755)
		os.MkdirAll(filepath.Join(root, "share"), 0o755)
		return root
	}
	check := func(t *testing.T, mnt string, nested bool) {
		t.Helper()
		got := dirNames(t, mnt)
		for _, n := range got {
			if n == "share" || n == "shared" {
				t.Errorf("mount listing shows %q: %v", n, got)
			}
		}
		if strings.Join(got, "|") != "A.vhd|shared.vhd|sub" {
			t.Errorf("listing %v", got)
		}
		sub := dirNames(t, filepath.Join(mnt, "sub"))
		if nested && len(sub) != 0 {
			t.Errorf("sub shows %v; the mount point sub/m must be hidden", sub)
		}
		if _, err := os.Lstat(filepath.Join(mnt, "share")); err == nil {
			t.Error("share is reachable inside the mount")
		}
	}
	t.Run("absolute", func(t *testing.T) {
		root := mk("abs")
		mnt := filepath.Join(root, "share")
		startView(t, mnt, root, "-view", root, "-mount", mnt)
		check(t, mnt, false)
	})
	t.Run("relative", func(t *testing.T) {
		root := mk("rel")
		startView(t, "share", root, "-view", ".", "-mount", "share")
		check(t, filepath.Join(root, "share"), false)
	})
	t.Run("relative dotdot", func(t *testing.T) {
		root := mk("rel2")
		startView(t, "../share", filepath.Join(root, "sub"), "-view", "..", "-mount", "../share")
		check(t, filepath.Join(root, "share"), false)
	})
	t.Run("symlinked mount", func(t *testing.T) {
		root := mk("sym")
		link := filepath.Join(tmp, "sym-link") // a symlink to the zip root; fusermount refuses a symlink as mount point itself
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		mnt := filepath.Join(link, "share")
		startView(t, mnt, root, "-view", root, "-mount", mnt)
		check(t, filepath.Join(root, "share"), false)
	})
	t.Run("symlinked zip root", func(t *testing.T) {
		root := mk("symroot")
		link := filepath.Join(tmp, "symroot-link")
		if err := os.Symlink(root, link); err != nil {
			t.Fatal(err)
		}
		mnt := filepath.Join(root, "share")
		startView(t, mnt, root, "-view", link, "-mount", mnt)
		check(t, mnt, false)
	})
	t.Run("nested mount point", func(t *testing.T) {
		root := mk("nest")
		mnt := filepath.Join(root, "sub", "m")
		startView(t, mnt, root, "-view", root, "-mount", mnt)
		got := dirNames(t, mnt)
		if strings.Join(got, "|") != "A.vhd|share|shared.vhd|sub" {
			t.Errorf("listing %v", got)
		}
		check2 := dirNames(t, filepath.Join(mnt, "sub"))
		if len(check2) != 0 {
			t.Errorf("sub shows %v", check2)
		}
	})
}

func TestViewBuildFailure(t *testing.T) {
	needFuse(t)
	e := newViewEnv(t)
	mkfile(t, filepath.Join(e.root, "bad.zip"), bytes.Repeat([]byte("this is not a zip file "), 100))
	mkfile(t, filepath.Join(e.root, "tiny.zip"), nil)
	// the listing cache is 5 s; wait until the new names are visible
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(e.p("bad.vhd")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bad.vhd never listed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, n := range []string{"bad.vhd", "tiny.vhd"} {
		for i := 0; i < 2; i++ {
			if err := openErr(e.p(n)); !errors.Is(err, syscall.EIO) {
				t.Fatalf("open %s: %v, want EIO", n, err)
			}
		}
	}
	if _, err := os.ReadDir(e.mnt); err != nil {
		t.Fatalf("listing after failures: %v", err)
	}
	if st, err := os.Stat(e.p("bad.vhd")); err != nil || st.Size() <= 0 {
		t.Fatalf("stat of a bad zip: %v %v", st, err)
	}
	if err := openErr(e.p("A.vhd")); err != nil {
		t.Fatalf("good entry after failures: %v", err)
	}
	select {
	case err := <-e.d.done:
		t.Fatalf("daemon exited: %v", err)
	default:
	}
	if !strings.Contains(e.d.sb.String(), "build ") {
		t.Errorf("no build failure logged:\n%s", e.d.sb)
	}
	// fixing the zip makes the open work (failures are not cached)
	writeZipFile(t, filepath.Join(e.root, "bad.zip"), bFiles)
	if err := openErr(e.p("bad.vhd")); err != nil {
		t.Fatalf("open after fixing: %v", err)
	}
}

func TestViewFlags(t *testing.T) {
	needFuse(t)
	run := func(args ...string) (int, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var sb syncBuf
		cmd := exec.CommandContext(ctx, daemonBin, args...)
		cmd.Stderr = &sb
		err := cmd.Run()
		return exitCode(err), sb.String()
	}
	root := t.TempDir()
	mnt := filepath.Join(t.TempDir(), "m")
	os.Mkdir(mnt, 0o755)
	src := makeZip(t)

	if c, out := run("-view", root, "-src", src, "-mount", mnt); c != 2 || !strings.Contains(out, "-view") || !strings.Contains(out, "-src") {
		t.Errorf("-view with -src: exit %d\n%s", c, out)
	}
	if c, _ := run("-view", root); c != 2 {
		t.Errorf("-view without -mount: exit %d", c)
	}
	for _, v := range []string{"0", "1441", "-5"} {
		if c, out := run("-view", root, "-mount", mnt, "-idle-min", v); c != 2 || !strings.Contains(out, "idle-min") {
			t.Errorf("-idle-min %s: exit %d\n%s", v, c, out)
		}
	}
	if c, _ := run("-view", filepath.Join(root, "nope"), "-mount", mnt); c != 2 {
		t.Errorf("missing -view dir: exit %d", c)
	}
	if c, _ := run("-view", root, "-mount", filepath.Join(root, "nomount")); c != 2 {
		t.Errorf("missing mount dir: exit %d", c)
	}
	if c, _ := run("-shared", root, "-src", src, "-mount", mnt); c != 2 {
		t.Errorf("-shared without -view: exit %d", c)
	}
	mkfile(t, filepath.Join(root, "afile"), []byte("x"))
	if c, _ := run("-view", root, "-shared", filepath.Join(root, "afile"), "-mount", mnt); c != 2 {
		t.Errorf("-shared is a file: exit %d", c)
	}
	if c, _ := run("-view", root, "-shared", filepath.Join(root, "afile", "x"), "-mount", mnt); c != 2 {
		t.Errorf("-shared cannot be created: exit %d", c)
	}

	// default shared folder is created
	d := startView(t, mnt, root, "-view", root, "-mount", mnt, "-idle-min", "1")
	if st, err := os.Stat(filepath.Join(root, "shared")); err != nil || !st.IsDir() {
		t.Errorf("default shared folder not created: %v", err)
	}
	if !strings.Contains(d.sb.String(), "created shared folder") {
		t.Errorf("no creation line:\n%s", d.sb)
	}
	d.stop(t)
	// explicit missing shared folder, nested
	sh := filepath.Join(t.TempDir(), "a", "b", "myshared")
	mnt2 := filepath.Join(t.TempDir(), "m")
	os.Mkdir(mnt2, 0o755)
	d = startView(t, mnt2, root, "-view", root, "-shared", sh, "-mount", mnt2)
	if st, err := os.Stat(sh); err != nil || !st.IsDir() {
		t.Errorf("-shared not created: %v", err)
	}
	if !strings.Contains(d.sb.String(), "view: shared folder "+sh) {
		t.Errorf("startup lines:\n%s", d.sb)
	}
}
