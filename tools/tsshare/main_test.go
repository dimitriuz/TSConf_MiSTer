package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var daemonBin string

func TestMain(m *testing.M) {
	code := func() int {
		dir, err := os.MkdirTemp("", "tsshare-bin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		daemonBin = filepath.Join(dir, "tsshare")
		if out, err := exec.Command("go", "build", "-o", daemonBin, ".").CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
			return 1
		}
		return m.Run()
	}()
	os.Exit(code)
}

func needFuse(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skip("/dev/fuse not available; skipping FUSE test")
	}
	needTool(t, "fusermount3")
}

var treeFiles = map[string][]byte{
	"hello.txt":              []byte("hello, TSConf\n"),
	"sub/deep/data.bin":      bytes.Repeat([]byte{0, 1, 2, 3, 250, 251}, 5000),
	"sub/Long Name File.txt": []byte("long name\n"),
	"empty.dat":              {},
}

func makeTree(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "src")
	for p, b := range treeFiles {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func makeZip(t *testing.T) string {
	zp := filepath.Join(t.TempDir(), "src.zip")
	f, err := os.Create(zp)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for p, b := range treeFiles {
		w, err := zw.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(b)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return zp
}

type daemon struct {
	cmd    *exec.Cmd
	mnt    string
	stderr *bytes.Buffer
	done   chan error
	cancel context.CancelFunc
}

func (d *daemon) vhd() string { return filepath.Join(d.mnt, "share.vhd") }

// startDaemon runs the daemon on src and waits for share.vhd to appear.
func startDaemon(t *testing.T, src string, extra ...string) *daemon {
	t.Helper()
	d := startDaemonRaw(t, src, extra...)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(d.vhd()); err == nil {
			return d
		}
		select {
		case err := <-d.done:
			t.Fatalf("daemon exited early: %v\n%s", err, d.stderr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("share.vhd did not appear\n%s", d.stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startDaemonRaw(t *testing.T, src string, extra ...string) *daemon {
	t.Helper()
	needFuse(t)
	mnt := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(mnt, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	args := append([]string{"-src", src, "-mount", mnt}, extra...)
	cmd := exec.CommandContext(ctx, daemonBin, args...)
	d := &daemon{cmd: cmd, mnt: mnt, stderr: &bytes.Buffer{}, done: make(chan error, 1), cancel: cancel}
	cmd.Stderr = d.stderr
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
		exec.Command("fusermount3", "-uz", mnt).Run()
	})
	return d
}

// stop sends SIGTERM and waits for exit.
func (d *daemon) stop(t *testing.T) error {
	t.Helper()
	d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-d.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("daemon did not exit after SIGTERM\n%s", d.stderr)
	}
	return nil
}

func isMounted(mnt string) bool {
	b, _ := os.ReadFile("/proc/self/mountinfo")
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && f[4] == mnt {
			return true
		}
	}
	return false
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func TestMountAndRead(t *testing.T) {
	needFuse(t)
	needTool(t, "fsck.vfat", "mdir", "mcopy", "dd")
	for name, src := range map[string]string{"dir": makeTree(t), "zip": makeZip(t)} {
		t.Run(name, func(t *testing.T) {
			d := startDaemon(t, src)
			st, err := os.Stat(d.vhd())
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0o444 || st.Size() <= 0 {
				t.Fatalf("mode %v size %d", st.Mode(), st.Size())
			}
			// fsck on the partition extracted through the mount
			part := filepath.Join(t.TempDir(), "part.img")
			if out, err := exec.Command("dd", "if="+d.vhd(), "of="+part, "bs=512", "skip=128",
				fmt.Sprintf("count=%d", st.Size()/512-128)).CombinedOutput(); err != nil {
				t.Fatalf("dd: %v\n%s", err, out)
			}
			if out, err := exec.Command("fsck.vfat", "-n", part).CombinedOutput(); err != nil {
				t.Fatalf("fsck.vfat: %v\n%s", err, out)
			}
			got := mdirList(t, d.vhd())
			want := []string{"empty.dat", "hello.txt", "sub/", "sub/Long Name File.txt", "sub/deep/", "sub/deep/data.bin"}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("listing\n got %q\nwant %q", got, want)
			}
			for p, b := range treeFiles {
				if len(b) == 0 {
					continue
				}
				if g := mcopyOut(t, d.vhd(), "::"+p); !bytes.Equal(g, b) {
					t.Errorf("%s: content mismatch (%d vs %d bytes)", p, len(g), len(b))
				}
			}
			if err := d.stop(t); exitCode(err) != 0 {
				t.Fatalf("exit: %v\n%s", err, d.stderr)
			}
			for _, s := range []string{"serving share.vhd", "warnings"} {
				if !strings.Contains(d.stderr.String(), s) {
					t.Errorf("startup output lacks %q:\n%s", s, d.stderr)
				}
			}
		})
	}
}

func TestReadLog(t *testing.T) {
	needFuse(t)
	lp := filepath.Join(t.TempDir(), "reads.log")
	d := startDaemon(t, makeTree(t), "-v", "-log", lp)
	f, err := os.Open(d.vhd())
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := f.ReadAt(buf, 512); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := d.stop(t); exitCode(err) != 0 {
		t.Fatalf("exit: %v\n%s", err, d.stderr)
	}
	b, _ := os.ReadFile(lp)
	if !strings.Contains(string(b), " backing ") && !strings.Contains(string(b), " synthetic ") {
		t.Fatalf("no read lines in log:\n%s", b)
	}
}

func TestNoWrite(t *testing.T) {
	d := startDaemon(t, makeTree(t))
	_, err := os.OpenFile(d.vhd(), os.O_WRONLY, 0)
	if !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) {
		t.Fatalf("O_WRONLY open: %v", err)
	}
	_, err = os.OpenFile(d.vhd(), os.O_RDWR, 0)
	if !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) {
		t.Fatalf("O_RDWR open: %v", err)
	}
	err = os.Truncate(d.vhd(), 0)
	if !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := os.Create(filepath.Join(d.mnt, "other")); err == nil {
		t.Fatal("created a second file")
	}
	d.stop(t)
}

func TestShutdown(t *testing.T) {
	d := startDaemon(t, makeTree(t))
	if !isMounted(d.mnt) {
		t.Fatal("not in mountinfo")
	}
	start := time.Now()
	if err := d.stop(t); exitCode(err) != 0 {
		t.Fatalf("exit: %v\n%s", err, d.stderr)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("shutdown took %v", time.Since(start))
	}
	if isMounted(d.mnt) {
		t.Fatal("still mounted after exit")
	}
}

// TestShutdownBusy: a process with its cwd inside the mount makes the clean
// unmount fail. The daemon then lazily unmounts (the mount leaves
// /proc/self/mountinfo) but keeps serving until the last user leaves; a second
// signal ends it with status exactly 1.
func TestShutdownBusy(t *testing.T) {
	d := startDaemon(t, makeTree(t))
	sh := exec.Command("sh", "-c", "cd \"$1\" && exec sleep 60", "sh", d.mnt)
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sh.Process.Kill(); sh.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cwd, _ := os.Readlink(fmt.Sprintf("/proc/%d/cwd", sh.Process.Pid))
		if cwd == d.mnt {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell never entered the mount (cwd %q)", cwd)
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.cmd.Process.Signal(syscall.SIGTERM)
	deadline = time.Now().Add(5 * time.Second)
	for isMounted(d.mnt) {
		if time.Now().After(deadline) {
			t.Fatalf("mount not detached after first signal\n%s", d.stderr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case err := <-d.done:
		t.Fatalf("daemon exited on the first signal while busy: %v\n%s", err, d.stderr)
	case <-time.After(300 * time.Millisecond):
	}
	d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-d.done:
		if c := exitCode(err); c != 1 {
			t.Fatalf("exit code %d, want 1\n%s", c, d.stderr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("second signal did not end the daemon\n%s", d.stderr)
	}
	if !strings.Contains(d.stderr.String(), "lazy unmount") {
		t.Errorf("no lazy unmount in output:\n%s", d.stderr)
	}
}

func TestSignalDuringBuild(t *testing.T) {
	needFuse(t)
	for sigName, tc := range map[string]struct {
		sig  syscall.Signal
		want int
	}{"term": {syscall.SIGTERM, 143}, "int": {syscall.SIGINT, 130}} {
		t.Run(sigName, func(t *testing.T) {
			mnt := filepath.Join(t.TempDir(), "mnt")
			os.Mkdir(mnt, 0o755)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, daemonBin, "-src", makeTree(t), "-mount", mnt)
			cmd.Env = append(os.Environ(), "TSSHARE_TEST_BUILD_DELAY=20s")
			var se syncBuf
			cmd.Stderr = &se
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// "source:" is printed after the signal handlers are installed and
			// before the build delay, so the signal arrives inside the delay
			// however loaded the machine is.
			for dl := time.Now().Add(20 * time.Second); !strings.Contains(se.String(), "source:"); time.Sleep(10 * time.Millisecond) {
				if time.Now().After(dl) {
					t.Fatalf("daemon never reached the build\n%s", se.String())
				}
			}
			start := time.Now()
			cmd.Process.Signal(tc.sig)
			err := cmd.Wait()
			if c := exitCode(err); c != tc.want {
				t.Fatalf("exit %d, want %d\n%s", c, tc.want, se.String())
			}
			if time.Since(start) > 10*time.Second {
				t.Errorf("took %v to cancel", time.Since(start))
			}
			if isMounted(mnt) {
				t.Fatal("mounted")
			}
			if _, err := os.Stat(filepath.Join(mnt, "share.vhd")); err == nil {
				t.Fatal("share.vhd present")
			}
			if strings.Contains(se.String(), "serving") {
				t.Errorf("reached serving:\n%s", se.String())
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	needFuse(t)
	src := makeTree(t)
	file := filepath.Join(t.TempDir(), "regular")
	os.WriteFile(file, nil, 0o644)
	for name, args := range map[string][]string{
		"mount-file":    {"-src", src, "-mount", file},
		"mount-missing": {"-src", src, "-mount", filepath.Join(t.TempDir(), "nope")},
		"v-without-log": {"-src", src, "-mount", t.TempDir(), "-v"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, daemonBin, args...)
			var se bytes.Buffer
			cmd.Stderr = &se
			if c := exitCode(cmd.Run()); c != 2 {
				t.Fatalf("exit %d, want 2\n%s", c, se.String())
			}
			if !strings.Contains(se.String(), "tsshare:") {
				t.Errorf("unclear message %q", se.String())
			}
			for _, a := range args {
				if isMounted(a) {
					t.Fatalf("%s mounted", a)
				}
			}
		})
	}
}

func TestBadSource(t *testing.T) {
	needFuse(t)
	tmp := t.TempDir()
	garbage := filepath.Join(tmp, "garbage.zip")
	os.WriteFile(garbage, []byte("this is not a zip file"), 0o644)
	empty := filepath.Join(tmp, "empty.zip")
	os.WriteFile(empty, nil, 0o644)
	emptyDir := filepath.Join(tmp, "emptydir")
	os.Mkdir(emptyDir, 0o755)
	for name, src := range map[string]string{
		"missing": filepath.Join(tmp, "nonexistent"),
		"garbage": garbage,
		"empty":   empty,
	} {
		t.Run(name, func(t *testing.T) {
			mnt := filepath.Join(t.TempDir(), "mnt")
			os.Mkdir(mnt, 0o755)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, daemonBin, "-src", src, "-mount", mnt)
			var se bytes.Buffer
			cmd.Stderr = &se
			cmd.Stdout = io.Discard
			err := cmd.Run()
			if exitCode(err) != 2 {
				t.Fatalf("exit %v, want 2\n%s", err, se.String())
			}
			if !strings.Contains(se.String(), "tsshare:") {
				t.Errorf("unclear message: %q", se.String())
			}
			if isMounted(mnt) {
				t.Fatal("left a mount")
			}
		})
	}
}

// syncBuf is a bytes.Buffer safe for the exec copier goroutine and a polling
// reader.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
