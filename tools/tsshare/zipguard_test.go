package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeStoredZip(t *testing.T, path string, content map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for n, c := range content {
		f, err := w.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(c))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipNode(t *testing.T, s Source, name string) *Node {
	t.Helper()
	root, _, err := s.Tree()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range root.Children {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no node %s", name)
	return nil
}

// TestZipChangeGuard: a zip rewritten in place under an open source must give
// an error, never other bytes; replacing it by rename keeps the old inode.
func TestZipChangeGuard(t *testing.T) {
	const old = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	cases := []struct {
		name    string
		mutate  func(t *testing.T, path string)
		wantErr bool
	}{
		{"same-size rewrite", func(t *testing.T, p string) {
			f, err := os.OpenFile(p, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			// the stored data of a.bin sits after its local header; rewrite
			// every 'A' with 'B' by patching the whole file
			b, _ := os.ReadFile(p)
			b = bytes.ReplaceAll(b, []byte("A"), []byte("B"))
			if _, err := f.WriteAt(b, 0); err != nil {
				t.Fatal(err)
			}
			os.Chtimes(p, time.Now(), time.Now().Add(10*time.Second))
		}, true},
		{"grow", func(t *testing.T, p string) {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte("trailing"))
			f.Close()
		}, true},
		{"truncate", func(t *testing.T, p string) {
			st, _ := os.Stat(p)
			if err := os.Truncate(p, st.Size()-10); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"replace by rename", func(t *testing.T, p string) {
			writeStoredZip(t, p+".new", map[string]string{"a.bin": "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC"})
			if err := os.Rename(p+".new", p); err != nil {
				t.Fatal(err)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "a.zip")
			writeStoredZip(t, p, map[string]string{"a.bin": old, "b.bin": old})
			src, err := NewZipSource(p, SourceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close()
			zs := src.(*zipSource)
			clk := time.Unix(1_700_000_000, 0)
			zs.now = func() time.Time { return clk }
			a, b := zipNode(t, src, "a.bin"), zipNode(t, src, "b.bin")
			buf := make([]byte, len(old))
			if _, err := src.ReadFileAt(a, buf, 0); err != nil || string(buf) != old {
				t.Fatalf("baseline read: %v %q", err, buf)
			}
			tc.mutate(t, p)
			clk = clk.Add(500 * time.Millisecond)
			// Inside the throttle window no fstat happens (the read may see
			// new bytes; that window is the documented limit).
			if _, err := src.ReadFileAt(a, buf, 0); err != nil && !tc.wantErr {
				t.Fatalf("read within throttle window: %v", err)
			}
			if tc.wantErr {
				if zs.changed.Load() {
					t.Fatal("fstat done inside the throttle window")
				}
			}
			clk = clk.Add(time.Second)
			for _, n := range []*Node{a, b, a} {
				for i := range buf {
					buf[i] = 0
				}
				_, err := src.ReadFileAt(n, buf, 0)
				if tc.wantErr {
					if !errors.Is(err, errZipChanged) {
						t.Fatalf("err = %v, want errZipChanged (bytes %q)", err, buf)
					}
					clk = clk.Add(time.Hour) // sticky, whatever the clock
				} else if err != nil || string(buf) != old {
					t.Fatalf("old inode read: %v %q", err, buf)
				}
			}
		})
	}
}

// TestZipGuardThrottle: one fstat per interval, however many reads.
func TestZipGuardThrottle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.zip")
	writeStoredZip(t, p, map[string]string{"a.bin": "0123456789"})
	src, err := NewZipSource(p, SourceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	zs := src.(*zipSource)
	clk := time.Unix(1_700_000_000, 0)
	zs.now = func() time.Time { return clk }
	n := zipNode(t, src, "a.bin")
	buf := make([]byte, 10)
	src.ReadFileAt(n, buf, 0)
	first := zs.lastChk.Load()
	for i := 0; i < 100; i++ {
		clk = clk.Add(5 * time.Millisecond) // 500 ms in all
		src.ReadFileAt(n, buf, 0)
	}
	if zs.lastChk.Load() != first {
		t.Fatal("fstat repeated inside the interval")
	}
	clk = clk.Add(600 * time.Millisecond)
	src.ReadFileAt(n, buf, 0)
	if zs.lastChk.Load() == first {
		t.Fatal("no fstat after the interval")
	}
}
