// tsshare serves a directory or zip file as a read-only FAT32 disk image,
// share.vhd, through FUSE, for the MiSTer TSConf core's second SD slot.
//
// Page cache: share.vhd is opened with FOPEN_DIRECT_IO. The core issues
// overlapping reads at arbitrary offsets (see spikes/fuse-vhd/RESULTS.md) and
// the kernel page cache did not absorb them, while readahead only added
// speculative ReadAt calls on a slow source; direct I/O gives one ReadAt per
// request. Source-level caching is done by Volume's sources (cache.go).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const maxShownWarnings = 30

type root struct {
	fs.Inode
	vhd *vhdFile
}

func (r *root) OnAdd(ctx context.Context) {
	r.AddChild("share.vhd", r.NewPersistentInode(ctx, r.vhd, fs.StableAttr{Mode: fuse.S_IFREG}), true)
}

type vhdFile struct {
	fs.Inode
	vol       *Volume
	size      int64
	partStart int64 // bytes
	log       *readLog
}

var (
	_ = (fs.NodeGetattrer)((*vhdFile)(nil))
	_ = (fs.NodeSetattrer)((*vhdFile)(nil))
	_ = (fs.NodeOpener)((*vhdFile)(nil))
	_ = (fs.NodeReader)((*vhdFile)(nil))
	_ = (fs.NodeWriter)((*vhdFile)(nil))
)

// guard turns a panic in a FUSE operation into EIO.
func guard(op string, errno *syscall.Errno) {
	if r := recover(); r != nil {
		log.Printf("panic in %s: %v\n%s", op, r, debug.Stack())
		*errno = syscall.EIO
	}
}

func (v *vhdFile) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) (errno syscall.Errno) {
	defer guard("getattr", &errno)
	out.Mode = 0444
	out.Size = uint64(v.size)
	out.Nlink = 1
	return 0
}

func (v *vhdFile) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

func (v *vhdFile) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fl uint32, errno syscall.Errno) {
	defer guard("open", &errno)
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (v *vhdFile) Write(ctx context.Context, f fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	return 0, syscall.EROFS
}

// Read serves dest (the request's buffer from go-fuse's pool) straight from
// the volume. It takes no lock, so reads run concurrently.
func (v *vhdFile) Read(ctx context.Context, f fs.FileHandle, dest []byte, off int64) (res fuse.ReadResult, errno syscall.Errno) {
	start := time.Now()
	defer guard("read", &errno)
	if off < 0 || off >= v.size {
		return fuse.ReadResultData(nil), 0
	}
	if rem := v.size - off; int64(len(dest)) > rem {
		dest = dest[:rem]
	}
	if _, err := v.vol.ReadAt(dest, off); err != nil {
		log.Printf("read error at %d (+%d): %v", off, len(dest), err)
		v.log.add(time.Now().UnixNano(), off, len(dest), "error", time.Since(start))
		return nil, syscall.EIO
	}
	kind := "backing"
	if end := off + int64(len(dest)); off >= sectorSize && end <= v.partStart {
		kind = "synthetic" // lies wholly in the zero gap before the partition
	}
	v.log.add(time.Now().UnixNano(), off, len(dest), kind, time.Since(start))
	return fuse.ReadResultData(dest), 0
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "tsshare: "+format+"\n", a...)
	os.Exit(2)
}

func main() {
	src := flag.String("src", "", "directory or zip file to share (required)")
	mnt := flag.String("mount", "", "mount point directory (required)")
	label := flag.String("label", defaultLabel, "FAT volume label")
	cacheMB := flag.Int("cache-mb", 64, "zip: MiB of inflated entries kept in memory")
	fdLimit := flag.Int("fd-limit", 64, "directory: open file descriptors kept")
	logPath := flag.String("log", "", "log file for the read log and warnings (default: none)")
	verbose := flag.Bool("v", false, "write one line per read to the -log file")
	memMB := flag.Int("mem-limit-mb", 256, "soft Go heap limit in MiB (0 = off)")
	flag.Parse()
	if *src == "" || *mnt == "" {
		flag.Usage()
		os.Exit(2)
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("tsshare: ")
	if *memMB > 0 {
		debug.SetMemoryLimit(int64(*memMB) << 20)
	}

	// Install the handler before anything slow; the buffered channel queues a
	// signal that arrives while the tree is being scanned or Mount runs.
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	var rl *readLog
	var lf *os.File
	if *logPath != "" {
		var err error
		lf, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			fatal("%v", err)
		}
		defer lf.Close()
	}

	t0 := time.Now()
	s, err := OpenSource(*src, SourceOptions{CacheMB: *cacheMB, FDLimit: *fdLimit})
	if err != nil {
		fatal("cannot open source: %v", err)
	}
	kind := "directory"
	if st, err := os.Stat(*src); err == nil && !st.IsDir() {
		kind = "zip file"
	}
	fmt.Fprintf(os.Stderr, "source: %s %s\n", kind, *src)
	vol, warns, err := NewVolume(s, VolumeOptions{Label: *label})
	if err != nil {
		s.Close()
		fatal("cannot build a FAT32 volume from %s: %v", *src, err)
	}
	defer vol.Close()
	built := time.Since(t0)
	g := vol.Geometry()
	fmt.Fprintf(os.Stderr, "tree: %d files, %d directories (including the root)\n", g.Files, g.Dirs)
	fmt.Fprintf(os.Stderr, "volume: %d sectors/cluster, %d clusters, %d FAT sectors, partition at sector %d, disk %d bytes\n",
		g.SectorsPerCluster, g.Clusters, g.FATSectors, g.PartStart, g.DiskBytes)
	fmt.Fprintf(os.Stderr, "%d warnings\n", len(warns))
	for i, w := range warns {
		if i == maxShownWarnings {
			fmt.Fprintf(os.Stderr, "... and %d more warnings\n", len(warns)-maxShownWarnings)
			break
		}
		fmt.Fprintf(os.Stderr, "warn: %s\n", w)
	}
	fmt.Fprintf(os.Stderr, "built in %v\n", built.Round(time.Millisecond))
	if lf != nil {
		for _, w := range warns {
			fmt.Fprintf(lf, "warn: %s\n", w)
		}
		if *verbose {
			rl = newReadLog(lf)
		}
	}

	v := &vhdFile{vol: vol, size: vol.Size(), partStart: g.PartStart * sectorSize, log: rl}
	// DirectMount calls mount(2) as root and falls back to fusermount3 otherwise.
	srv, err := fs.Mount(*mnt, &root{vhd: v}, &fs.Options{
		MountOptions: fuse.MountOptions{
			DirectMount: true,
			Name:        "tsshare",
			FsName:      "tsshare",
		},
	})
	if err != nil {
		rl.close()
		log.Printf("mount: %v", err)
		os.Exit(1)
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
		vol.Close()
		os.Exit(1)
	}()
	fmt.Fprintf(os.Stderr, "serving share.vhd (%d bytes) at %s\n", vol.Size(), *mnt)
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
