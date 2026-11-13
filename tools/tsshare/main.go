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
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	maxShownWarnings  = 30
	defaultMaxEntries = 250000
	maxCacheMB        = 256
)

// checkCacheMB validates -cache-mb: 0 means the default, else 1..maxCacheMB.
func checkCacheMB(mb int) error {
	if mb < 0 || mb > maxCacheMB {
		return fmt.Errorf("-cache-mb %d out of range: use 1 to %d (0 = the default, 32)", mb, maxCacheMB)
	}
	return nil
}

// preferOOMKill asks the kernel to pick this process before Main_MiSTer when
// memory runs out. Best effort.
func preferOOMKill(verbose bool) {
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte("1000"), 0o644); err != nil {
		if verbose {
			log.Printf("oom_score_adj: %v", err)
		}
	} else if verbose {
		log.Printf("oom_score_adj set to 1000")
	}
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

// shutdown coordinates signals with startup. The first signal records an exit
// status and closes cancelled; before the mount exists, main checks it between
// stages and exits without mounting. After the mount, the first signal unmounts
// (lazily if busy) and a second one forces a lazy unmount and exit 1.
type shutdown struct {
	mu        sync.Mutex
	code      int // 0 until the first signal; 130 SIGINT, 143 SIGTERM
	cancelled chan struct{}
	srv       *fuse.Server
	mnt       string
	cleanup   func() // closes log and source; set before the first stage that needs it
}

func (sd *shutdown) isCancelled() bool {
	select {
	case <-sd.cancelled:
		return true
	default:
		return false
	}
}

func (sd *shutdown) run(sig <-chan os.Signal) {
	s := <-sig
	log.Printf("got %v", s)
	sd.mu.Lock()
	sd.code = 143
	if s == syscall.SIGINT {
		sd.code = 130
	}
	close(sd.cancelled)
	srv := sd.srv
	sd.mu.Unlock()
	if srv != nil {
		go sd.unmount(srv)
	}
	s = <-sig
	log.Printf("got %v again, exiting", s)
	sd.mu.Lock()
	srv = sd.srv
	sd.mu.Unlock()
	if srv != nil {
		if err := lazyUnmount(sd.mnt); err != nil {
			log.Printf("lazy unmount: %v", err)
		}
	}
	if sd.cleanup != nil {
		sd.cleanup()
	}
	os.Exit(1)
}

func (sd *shutdown) unmount(srv *fuse.Server) {
	if err := srv.Unmount(); err != nil {
		log.Printf("unmount: %v; trying lazy unmount", err)
		if err := lazyUnmount(sd.mnt); err != nil {
			log.Printf("lazy unmount: %v", err)
		}
	}
}

// setServer publishes the mounted server; if a signal arrived during Mount it
// starts the unmount at once.
func (sd *shutdown) setServer(srv *fuse.Server) {
	sd.mu.Lock()
	sd.srv = srv
	c := sd.isCancelled()
	sd.mu.Unlock()
	if c {
		go sd.unmount(srv)
	}
}

// abortIfCancelled exits with the signal's status, without mounting, once a
// signal has arrived.
func (sd *shutdown) abortIfCancelled() {
	if sd.isCancelled() {
		log.Printf("cancelled before mounting")
		if sd.cleanup != nil {
			sd.cleanup()
		}
		os.Exit(sd.code)
	}
}

func main() {
	src := flag.String("src", "", "directory or zip file to share (single-source mode; or use -view)")
	view := flag.String("view", "", "view mode: root folder whose zips are shown as <name>.vhd, built when opened")
	shared := flag.String("shared", "", "view mode: folder shown as shared.vhd (default <view>/shared, created if missing)")
	idleMin := flag.Int("idle-min", 10, "view mode: minutes an unused volume is kept, 1 to 1440")
	mnt := flag.String("mount", "", "mount point directory (required)")
	label := flag.String("label", defaultLabel, "FAT volume label")
	cacheMB := flag.Int("cache-mb", 32, "zip: MiB of inflated entries kept in memory, 1 to 256 (0 = the default, 32)")
	maxEntries := flag.Int("max-entries", defaultMaxEntries, "refuse sources with more files and directories than this (0 = no limit)")
	fdLimit := flag.Int("fd-limit", 64, "directory: open file descriptors kept")
	logPath := flag.String("log", "", "log file for the read log and warnings (default: none)")
	verbose := flag.Bool("v", false, "write one line per read to the -log file")
	memMB := flag.Int("mem-limit-mb", 192, "soft Go heap limit in MiB (0 = off)")
	flag.Parse()
	if *view != "" && *src != "" {
		fatal("-view and -src cannot be used together")
	}
	if (*src == "" && *view == "") || *mnt == "" {
		flag.Usage()
		os.Exit(2)
	}
	if *view == "" && *shared != "" {
		fatal("-shared needs -view")
	}
	if *idleMin < 1 || *idleMin > 1440 {
		fatal("-idle-min %d out of range: use 1 to 1440", *idleMin)
	}
	if *verbose && *logPath == "" {
		fatal("-v needs -log <file>")
	}
	if err := checkCacheMB(*cacheMB); err != nil {
		fatal("%v", err)
	}
	if *maxEntries < 0 {
		fatal("-max-entries %d: use 0 (no limit) or a positive number", *maxEntries)
	}
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("tsshare: ")
	preferOOMKill(*verbose)
	if os.Getenv("GOGC") == "" {
		// The live heap is mostly the cache and the tree; at the default 100%
		// the collector would let it double (about 220 MB on SD.zip).
		debug.SetGCPercent(10)
	}
	if *memMB > 0 {
		debug.SetMemoryLimit(int64(*memMB) << 20)
	}

	// Signals are handled from the very start, so a long scan can be cancelled.
	sd := &shutdown{cancelled: make(chan struct{}), mnt: *mnt}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go sd.run(sig)

	if st, err := os.Stat(*mnt); err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		fatal("bad -mount %s: %v", *mnt, err)
	}

	if *view != "" {
		runView(sd, viewConfig{
			zipRoot: *view, shared: *shared, mount: *mnt, label: *label, cacheMB: *cacheMB,
			maxEntries: *maxEntries, fdLimit: *fdLimit, logPath: *logPath, verbose: *verbose,
			idle: time.Duration(*idleMin) * time.Minute,
		})
		return
	}

	var rl *readLog
	var lf *os.File
	var vol *Volume
	var s Source
	sd.cleanup = func() {
		rl.close()
		if vol != nil {
			vol.Close()
		} else if s != nil {
			s.Close()
		}
		if lf != nil {
			lf.Close()
		}
	}
	if *logPath != "" {
		var err error
		lf, err = os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			fatal("%v", err)
		}
	}

	t0 := time.Now()
	var err error
	s, err = OpenSource(*src, SourceOptions{CacheMB: *cacheMB, FDLimit: *fdLimit})
	if err != nil {
		fatal("cannot open source: %v", err)
	}
	kind := "directory"
	if st, err := os.Stat(*src); err == nil && !st.IsDir() {
		kind = "zip file"
	}
	fmt.Fprintf(os.Stderr, "source: %s %s\n", kind, *src)
	sd.abortIfCancelled()
	vol, warns, err := NewVolume(s, VolumeOptions{Label: *label, MaxEntries: *maxEntries})
	if err != nil {
		s.Close()
		fatal("cannot build a FAT32 volume from %s: %v", *src, err)
	}
	// Test hook: stretch the build so tests can signal during it.
	if d, err := time.ParseDuration(os.Getenv("TSSHARE_TEST_BUILD_DELAY")); err == nil {
		select {
		case <-time.After(d):
		case <-sd.cancelled:
		}
	}
	sd.abortIfCancelled()
	built := time.Since(t0)
	debug.FreeOSMemory() // hand the scan's garbage (zip directory, name maps) back to the OS
	g := vol.Geometry()
	fmt.Fprintf(os.Stderr, "tree: %d files, %d directories (including the root)\n", g.Files, g.Dirs)
	fmt.Fprintf(os.Stderr, "volume: %d sectors/cluster, %d clusters, %d FAT sectors, partition at sector %d, %d reserved, data at partition sector %d, disk %d bytes\n",
		g.SectorsPerCluster, g.Clusters, g.FATSectors, g.PartStart, g.Reserved, g.DataStart, g.DiskBytes)
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
	srv, err := mountFS(*mnt, &root{vhd: v})
	if err != nil {
		log.Printf("mount: %v", err)
		sd.cleanup()
		os.Exit(1)
	}
	sd.setServer(srv)
	fmt.Fprintf(os.Stderr, "serving share.vhd (%d bytes) at %s\n", vol.Size(), *mnt)
	srv.Wait()
	sd.cleanup()
	log.Printf("exiting")
}

// mountFS mounts r. As root it uses go-fuse's strict direct mount(2), so a
// missing /dev/fuse or unsupported kernel gives the real error rather than
// "fusermount3 not found" (the board has no fusermount). Strict has one
// downside: it never falls back, so if mount(2) is refused for a reason that
// fusermount3 would handle, we retry with fusermount3 and report both errors
// if that fails too. Non-root users go through fusermount3 directly.
func mountFS(mnt string, r fs.InodeEmbedder) (*fuse.Server, error) {
	opts := func(strict bool) *fs.Options {
		return &fs.Options{MountOptions: fuse.MountOptions{
			DirectMountStrict: strict,
			Name:              "tsshare",
			FsName:            "tsshare",
		}}
	}
	if os.Geteuid() != 0 {
		return fs.Mount(mnt, r, opts(false))
	}
	srv, err := fs.Mount(mnt, r, opts(true))
	if err == nil {
		return srv, nil
	}
	srv, err2 := fs.Mount(mnt, r, opts(false))
	if err2 != nil {
		return nil, fmt.Errorf("direct mount: %v; fusermount fallback: %v", err, err2)
	}
	log.Printf("direct mount failed (%v); mounted through fusermount", err)
	return srv, nil
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
