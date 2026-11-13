package main

import (
	"context"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func newTestFile(t *testing.T, failPath string) *vhdFile {
	t.Helper()
	src := newMemSource(map[string]memFile{"a.bin": {Size: 100000}, "b.bin": {Size: 5000}})
	src.failPath = failPath
	vol, _, err := NewVolume(src, VolumeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &vhdFile{vol: vol, size: vol.Size(), partStart: vol.Geometry().PartStart * sectorSize}
}

func TestWriteHandlersEROFS(t *testing.T) {
	v := newTestFile(t, "")
	ctx := context.Background()
	for _, fl := range []uint32{syscall.O_WRONLY, syscall.O_RDWR, syscall.O_APPEND, syscall.O_RDONLY | syscall.O_TRUNC} {
		if _, _, e := v.Open(ctx, fl); e != syscall.EROFS {
			t.Errorf("Open(%#x) = %v, want EROFS", fl, e)
		}
	}
	if _, fl, e := v.Open(ctx, syscall.O_RDONLY); e != 0 || fl&fuse.FOPEN_DIRECT_IO == 0 {
		t.Errorf("read-only Open = %#x, %v", fl, e)
	}
	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	if e := v.Setattr(ctx, nil, in, &fuse.AttrOut{}); e != syscall.EROFS {
		t.Errorf("Setattr = %v, want EROFS", e)
	}
	if n, e := v.Write(ctx, nil, []byte("x"), 0); e != syscall.EROFS || n != 0 {
		t.Errorf("Write = %d, %v, want EROFS", n, e)
	}
}

func TestGuardPanicToEIO(t *testing.T) {
	f := func() (errno syscall.Errno) {
		defer guard("test", &errno)
		panic("boom")
	}
	if e := f(); e != syscall.EIO {
		t.Fatalf("got %v, want EIO", e)
	}
}

func TestReadHandler(t *testing.T) {
	ctx := context.Background()
	v := newTestFile(t, "")
	buf := make([]byte, 4096)
	res, e := v.Read(ctx, nil, buf, 0)
	if e != 0 {
		t.Fatal(e)
	}
	b, _ := res.Bytes(make([]byte, 4096))
	if len(b) != 4096 || b[510] != 0x55 || b[511] != 0xAA {
		t.Fatalf("MBR signature missing in first read (%d bytes)", len(b))
	}
	// past the end: empty, no error
	if res, e := v.Read(ctx, nil, buf, v.size+10); e != 0 || res.Size() != 0 {
		t.Fatalf("past end: %d, %v", res.Size(), e)
	}

	// A failing source file makes some read fail with EIO and no data.
	bad := newTestFile(t, "a.bin")
	var saw bool
	for off := int64(0); off < bad.size && !saw; off += 65536 {
		res, e := bad.Read(ctx, nil, make([]byte, 65536), off)
		if e != 0 {
			if e != syscall.EIO || res != nil {
				t.Fatalf("got %v, %v; want EIO and no data", res, e)
			}
			saw = true
		}
	}
	if !saw {
		t.Fatal("no read of the failing file returned EIO")
	}
}
