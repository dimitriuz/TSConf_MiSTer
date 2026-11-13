// fuse-vhd: spike daemon exposing one read-only file, share.vhd, whose
// content is a backing image padded with zeros up to -size.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type readLog struct {
	ch      chan string
	stop    chan struct{}
	done    chan struct{}
	dropped atomic.Int64
	once    sync.Once
}

func newReadLog(f *os.File) *readLog {
	l := &readLog{ch: make(chan string, 1<<16), stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		w := bufio.NewWriter(f)
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case s := <-l.ch:
				w.WriteString(s)
			case <-tk.C:
				w.Flush()
			case <-l.stop:
				for {
					select {
					case s := <-l.ch:
						w.WriteString(s)
					default:
						w.Flush()
						return
					}
				}
			}
		}
	}()
	return l
}

// add never blocks the reply path: the line is dropped (and counted) if the queue is full.
func (l *readLog) add(ts int64, off int64, n int, kind string, lat time.Duration) {
	if l == nil {
		return
	}
	select {
	case l.ch <- fmt.Sprintf("%d %d %d %s %d\n", ts, off, n, kind, lat.Microseconds()):
	default:
		l.dropped.Add(1)
	}
}

func (l *readLog) close() {
	if l == nil {
		return
	}
	l.once.Do(func() { close(l.stop) })
	<-l.done
	log.Printf("read log closed, dropped %d lines", l.dropped.Load())
}

type root struct {
	fs.Inode
	vhd *vhdFile
}

func (r *root) OnAdd(ctx context.Context) {
	r.AddChild("share.vhd", r.NewPersistentInode(ctx, r.vhd, fs.StableAttr{Mode: fuse.S_IFREG}), true)
}

type vhdFile struct {
	fs.Inode
	backing *os.File
	size    int64
	delay   time.Duration
	log     *readLog
}

var (
	_ = (fs.NodeGetattrer)((*vhdFile)(nil))
	_ = (fs.NodeSetattrer)((*vhdFile)(nil))
	_ = (fs.NodeOpener)((*vhdFile)(nil))
	_ = (fs.NodeReader)((*vhdFile)(nil))
	_ = (fs.NodeWriter)((*vhdFile)(nil))
)

func (v *vhdFile) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0444
	out.Size = uint64(v.size)
	out.Nlink = 1
	return 0
}

func (v *vhdFile) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

func (v *vhdFile) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (v *vhdFile) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	return 0, syscall.EROFS
}

func (v *vhdFile) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	start := time.Now()
	if off < 0 || off >= v.size {
		return fuse.ReadResultData(nil), 0
	}
	if rem := v.size - off; int64(len(dest)) > rem {
		dest = dest[:rem]
	}
	if v.delay > 0 {
		time.Sleep(v.delay)
	}
	n, err := v.backing.ReadAt(dest, off)
	if err != nil && err != io.EOF {
		log.Printf("backing read error at %d: %v", off, err)
		v.log.add(time.Now().UnixNano(), off, len(dest), "error", time.Since(start))
		return nil, syscall.EIO
	}
	for i := n; i < len(dest); i++ {
		dest[i] = 0
	}
	kind := "backing"
	if n == 0 {
		kind = "synthetic"
	}
	v.log.add(time.Now().UnixNano(), off, len(dest), kind, time.Since(start))
	return fuse.ReadResultData(dest), 0
}

func main() {
	backing := flag.String("backing", "", "backing image file (required)")
	mnt := flag.String("mount", "", "mount point directory (required)")
	size := flag.Int64("size", 4<<30, "virtual size of share.vhd in bytes")
	logPath := flag.String("log", "", "read log file (default: none)")
	delay := flag.Duration("delay", 0, "artificial latency per read, e.g. 50ms")
	flag.Parse()
	if *backing == "" || *mnt == "" {
		flag.Usage()
		os.Exit(2)
	}
	bf, err := os.Open(*backing)
	if err != nil {
		log.Fatal(err)
	}
	defer bf.Close()
	var rl *readLog
	if *logPath != "" {
		lf, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			log.Fatal(err)
		}
		defer lf.Close()
		rl = newReadLog(lf)
	}
	v := &vhdFile{backing: bf, size: *size, delay: *delay, log: rl}
	// Install the handler before mounting; the buffered channel queues a signal
	// that arrives while Mount is still running.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	// DirectMount falls back to fusermount when not root (local testing).
	srv, err := fs.Mount(*mnt, &root{vhd: v}, &fs.Options{
		MountOptions: fuse.MountOptions{
			DirectMount: true,
			AllowOther:  false,
			Name:        "fuse-vhd",
			FsName:      "fuse-vhd",
		},
	})
	if err != nil {
		log.Fatalf("mount: %v", err)
	}
	go func() {
		s := <-sig
		log.Printf("got %v, unmounting", s)
		if err := srv.Unmount(); err != nil {
			log.Printf("unmount: %v; trying lazy unmount", err)
			if err := lazyUnmount(*mnt); err != nil {
				log.Printf("lazy unmount: %v", err)
			}
		}
		s = <-sig
		log.Printf("got %v again, lazy unmount and exit", s)
		if err := lazyUnmount(*mnt); err != nil {
			log.Printf("lazy unmount: %v", err)
		}
		rl.close()
		os.Exit(1)
	}()
	log.Printf("serving %s (%d bytes) from %s at %s", "share.vhd", *size, *backing, *mnt)
	srv.Wait()
	rl.close()
	log.Printf("exiting")
}

func lazyUnmount(mnt string) error {
	if os.Geteuid() == 0 {
		return syscall.Unmount(mnt, syscall.MNT_DETACH)
	}
	out, err := exec.Command("fusermount3", "-uz", mnt).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
