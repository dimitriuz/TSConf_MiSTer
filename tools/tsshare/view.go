package main

// View mode: a FUSE tree that mirrors a zip root. Every directory is shown as
// a directory, every *.zip as <name>.vhd, and the shared folder as share.vhd.
// A file's content is a virtual FAT32 disk built (by the Manager) when the
// file is opened.
//
// Why X.vhd and not X.zip: Main_MiSTer's file picker treats every *.zip as a
// folder and any path containing ".zip" as zipped, so a name containing
// ".zip" can never be picked as a disk. See the design spec.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

const (
	sharedName     = "share.vhd"
	defaultListTTL = 5 * time.Second
	dirTimeout     = time.Second
	maxListCache   = 256
)

// ViewOptions configures the view.
type ViewOptions struct {
	ZipRoot, Shared, Mount string
	ListTTL                time.Duration // listing cache; 0 = 5 s
	Logf                   func(string, ...any)
	Log                    *readLog // read log (-v), may be nil
}

// viewEntry is one shown entry of a mirrored directory.
type viewEntry struct {
	Name    string // shown name
	Real    string // real file or directory name
	IsDir   bool
	Size    int64 // real zip size
	ModTime time.Time
}

// viewName maps a real zip file name to its shown name. taken holds the
// lower-cased names already used in the directory; the result is added to it.
// ok is false for names that must not be shown: not a .zip file, hidden, or a
// name that still contains ".zip" after the mapping.
func viewName(zipName string, taken map[string]bool) (string, bool) {
	if strings.HasPrefix(zipName, ".") || len(zipName) <= 4 ||
		!strings.EqualFold(zipName[len(zipName)-4:], ".zip") {
		return "", false
	}
	base := zipName[:len(zipName)-4]
	if base == "" || strings.Contains(strings.ToLower(base), ".zip") {
		return "", false
	}
	name := base + ".vhd"
	for n := 2; taken[strings.ToLower(name)]; n++ {
		suffix := " (" + strconv.Itoa(n) + ").vhd"
		name = cutUTF8(base, maxNameBytes-len(suffix)) + suffix
	}
	taken[strings.ToLower(name)] = true
	return name, true
}

// maxNameBytes is the longest name FUSE and Linux accept.
const maxNameBytes = 255

// cutUTF8 shortens s to at most n bytes, on a UTF-8 boundary.
func cutUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

var fallbackMtime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// sharedMtime is the mtime of the shared folder, or a fixed date.
func sharedMtime(p string) time.Time {
	if st, err := os.Stat(p); err == nil {
		return st.ModTime()
	}
	return fallbackMtime
}

// resolvePath returns the absolute path with symlinks resolved, or just the
// absolute path when it does not exist (yet).
func resolvePath(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		a = filepath.Clean(p)
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r
	}
	return a
}

// listing is one scanned directory: the entries in order and a name index.
type listing struct {
	entries []viewEntry
	byName  map[string]int
}

func newListing(es []viewEntry) *listing {
	l := &listing{entries: es, byName: make(map[string]int, len(es))}
	for i, e := range es {
		l.byName[e.Name] = i
	}
	return l
}

// find returns the entry shown under name.
func (l *listing) find(name string) (viewEntry, bool) {
	i, ok := l.byName[name]
	if !ok {
		return viewEntry{}, false
	}
	return l.entries[i], true
}

type listCacheEnt struct {
	at  time.Time // when the scan finished
	lst *listing
}

// flight is a scan in progress; waiters get its result.
type flight struct {
	done chan struct{}
	lst  *listing
	err  error
}

// lister reads and caches mirrored directory listings.
type lister struct {
	logf   func(string, ...any)
	scanFn func(dir string, excl, reserved []string) ([]viewEntry, error) // default scan; tests inject
	mu     sync.Mutex
	c      map[string]listCacheEnt
	fl     map[string]*flight
	seen   map[string]bool // skip warnings already logged
}

var defaultLister = &lister{}

// shownEntries lists dir for the view: sub-directories (not symlinks, not
// hidden, no ".zip" in the name, not in excl) and the zips as shown names. excl
// are resolved absolute paths. reserved names (case-insensitive, e.g.
// "share.vhd") are taken: a directory with such a name is hidden and a zip
// mapping to it gets a numeric suffix. Results are cached for ttl (0 = not).
// The order is by real name (case-insensitive, then bytewise), so a name maps
// to the same zip on every call.
func shownEntries(dir string, excl []string, ttl time.Duration, reserved ...string) ([]viewEntry, error) {
	return defaultLister.list(dir, excl, ttl, reserved)
}

func (l *lister) warn(dir, name, why string) {
	if l.logf == nil {
		return
	}
	key := dir + "\x00" + name
	l.mu.Lock()
	if l.seen == nil || len(l.seen) > 4096 {
		l.seen = map[string]bool{}
	}
	dup := l.seen[key]
	l.seen[key] = true
	l.mu.Unlock()
	if !dup {
		l.logf("view: not shown: %s (%s)", filepath.Join(dir, name), why)
	}
}

func (l *lister) list(dir string, excl []string, ttl time.Duration, reserved []string) ([]viewEntry, error) {
	lst, err := l.listing(dir, excl, ttl, reserved)
	if err != nil {
		return nil, err
	}
	return lst.entries, nil
}

func listKey(dir string, excl, reserved []string) string {
	return dir + "\x00" + strings.Join(excl, "\x01") + "\x00" + strings.Join(reserved, "\x01")
}

// peek returns the cached scan if it is still fresh; it never scans.
func (l *lister) peek(dir string, excl []string, ttl time.Duration, reserved []string) (*listing, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.c[listKey(dir, excl, reserved)]; ok && ttl > 0 && time.Since(e.at) < ttl {
		return e.lst, true
	}
	return nil, false
}

// listing returns the cached scan if it is younger than ttl, else scans. Only
// one scan per key runs at a time; concurrent callers share its result. No
// lock is held while scanning, a failed scan is not cached, and the cache
// entry is stamped when the scan finished.
func (l *lister) listing(dir string, excl []string, ttl time.Duration, reserved []string) (*listing, error) {
	key := listKey(dir, excl, reserved)
	l.mu.Lock()
	if ttl > 0 {
		if e, ok := l.c[key]; ok && time.Since(e.at) < ttl {
			l.mu.Unlock()
			return e.lst, nil
		}
	}
	if f := l.fl[key]; f != nil {
		l.mu.Unlock()
		<-f.done
		return f.lst, f.err
	}
	f := &flight{done: make(chan struct{})}
	if l.fl == nil {
		l.fl = map[string]*flight{}
	}
	l.fl[key] = f
	l.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				f.err = fmt.Errorf("panic scanning %s: %v", dir, r)
			}
			l.mu.Lock()
			delete(l.fl, key)
			if f.err == nil && ttl > 0 {
				now := time.Now()
				if l.c == nil || len(l.c) >= maxListCache {
					for k, e := range l.c {
						if now.Sub(e.at) >= ttl {
							delete(l.c, k)
						}
					}
					if l.c == nil || len(l.c) >= maxListCache {
						l.c = map[string]listCacheEnt{}
					}
				}
				l.c[key] = listCacheEnt{at: now, lst: f.lst}
			}
			l.mu.Unlock()
			close(f.done)
		}()
		scan := l.scan
		if l.scanFn != nil {
			scan = l.scanFn
		}
		var es []viewEntry
		es, f.err = scan(dir, excl, reserved)
		if f.err == nil {
			f.lst = newListing(es)
		}
	}()
	return f.lst, f.err
}

func (l *lister) scan(dir string, excl []string, reserved []string) ([]viewEntry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	base := resolvePath(dir)
	sort.Slice(des, func(i, j int) bool {
		a, b := des[i].Name(), des[j].Name()
		la, lb := strings.ToLower(a), strings.ToLower(b)
		if la != lb {
			return la < lb
		}
		return a < b
	})
	taken := map[string]bool{}
	for _, r := range reserved {
		taken[strings.ToLower(r)] = true
	}
	var out []viewEntry
	var zips []os.DirEntry
	for _, de := range des {
		name := de.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if de.IsDir() {
			p := filepath.Join(base, name)
			skip := false
			for _, x := range excl {
				if p == x {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			if strings.Contains(strings.ToLower(name), ".zip") {
				l.warn(dir, name, "directory name contains .zip")
				continue
			}
			if taken[strings.ToLower(name)] {
				l.warn(dir, name, "name is reserved")
				continue
			}
			taken[strings.ToLower(name)] = true
			out = append(out, viewEntry{Name: name, Real: name, IsDir: true})
			continue
		}
		if len(name) > 4 && strings.EqualFold(name[len(name)-4:], ".zip") {
			zips = append(zips, de)
		}
	}
	for _, de := range zips {
		name := de.Name()
		info, err := os.Stat(filepath.Join(dir, name)) // follows symlinks
		if err != nil || !info.Mode().IsRegular() {
			continue // vanished, dangling or not a file
		}
		shown, ok := viewName(name, taken)
		if !ok {
			l.warn(dir, name, "name contains .zip after mapping")
			continue
		}
		out = append(out, viewEntry{Name: shown, Real: name, Size: info.Size(), ModTime: info.ModTime()})
	}
	return out, nil
}

// viewShared is the state shared by all nodes of one view.
type viewShared struct {
	opt   ViewOptions
	m     *Manager
	ls    *lister
	excl  []string
	ttl   time.Duration
	shard string // shared folder (as given, made absolute)
	// statFn stats a real zip (Open, and Getattr when no fresh listing has
	// it); tests count calls. Default os.Stat.
	statFn func(string) (os.FileInfo, error)
}

func (v *viewShared) stat(p string) (os.FileInfo, error) {
	if v.statFn != nil {
		return v.statFn(p)
	}
	return os.Stat(p)
}

func (v *viewShared) logf(format string, a ...any) {
	if v.opt.Logf != nil {
		v.opt.Logf(format, a...)
	}
}

// newViewRoot returns the root node of the view. The mount point and the
// shared folder are excluded from the mirror by their resolved paths.
func newViewRoot(opt ViewOptions, m *Manager) fs.InodeEmbedder {
	v := &viewShared{opt: opt, m: m, ttl: opt.ListTTL}
	if v.ttl <= 0 {
		v.ttl = defaultListTTL
	}
	v.ls = &lister{logf: opt.Logf}
	v.excl = []string{resolvePath(opt.Mount), resolvePath(opt.Shared)}
	v.shard, _ = filepath.Abs(opt.Shared)
	return &viewDir{v: v, path: resolvePath(opt.ZipRoot), root: true}
}

func errnoOf(err error) syscall.Errno {
	if errors.Is(err, os.ErrNotExist) {
		return syscall.ENOENT
	}
	return syscall.EIO
}

// viewDir is a mirrored directory.
type viewDir struct {
	fs.Inode
	v    *viewShared
	path string // resolved real directory
	root bool
}

var (
	_ = (fs.NodeGetattrer)((*viewDir)(nil))
	_ = (fs.NodeSetattrer)((*viewDir)(nil))
	_ = (fs.NodeReaddirer)((*viewDir)(nil))
	_ = (fs.NodeLookuper)((*viewDir)(nil))
	_ = (fs.NodeMkdirer)((*viewDir)(nil))
	_ = (fs.NodeCreater)((*viewDir)(nil))
	_ = (fs.NodeUnlinker)((*viewDir)(nil))
	_ = (fs.NodeRmdirer)((*viewDir)(nil))
	_ = (fs.NodeRenamer)((*viewDir)(nil))
	_ = (fs.NodeSymlinker)((*viewDir)(nil))
	_ = (fs.NodeMknoder)((*viewDir)(nil))
	_ = (fs.NodeLinker)((*viewDir)(nil))
)

func (d *viewDir) reserved() []string {
	if d.root {
		return []string{sharedName}
	}
	return nil
}

func (d *viewDir) listing() (*listing, error) {
	return d.v.ls.listing(d.path, d.v.excl, d.v.ttl, d.reserved())
}

// cachedEntry returns the entry shown under name from a fresh listing, without
// touching the real directory.
func (d *viewDir) cachedEntry(name string) (viewEntry, bool) {
	lst, ok := d.v.ls.peek(d.path, d.v.excl, d.v.ttl, d.reserved())
	if !ok {
		return viewEntry{}, false
	}
	return lst.find(name)
}

func (d *viewDir) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) (errno syscall.Errno) {
	defer guard("getattr", &errno)
	st, err := os.Stat(d.path)
	if err != nil {
		return errnoOf(err)
	}
	out.Mode = 0555
	out.Nlink = 2
	mt := st.ModTime()
	out.SetTimes(nil, &mt, nil)
	out.SetTimeout(dirTimeout)
	return 0
}

func (d *viewDir) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

func (d *viewDir) Readdir(ctx context.Context) (ds fs.DirStream, errno syscall.Errno) {
	defer guard("readdir", &errno)
	lst, err := d.listing()
	if err != nil {
		d.v.logf("view: readdir failed: %v", err)
		return nil, errnoOf(err)
	}
	es := lst.entries
	list := make([]fuse.DirEntry, 0, len(es)+1)
	for _, e := range es {
		mode := uint32(fuse.S_IFREG)
		if e.IsDir {
			mode = fuse.S_IFDIR
		}
		list = append(list, fuse.DirEntry{Name: e.Name, Mode: mode})
	}
	if d.root {
		list = append(list, fuse.DirEntry{Name: sharedName, Mode: fuse.S_IFREG})
	}
	return fs.NewListDirStream(list), 0
}

func (d *viewDir) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (ch *fs.Inode, errno syscall.Errno) {
	defer guard("lookup", &errno)
	if d.root && name == sharedName {
		return d.file(ctx, name, d.v.shard, true, viewEntry{Name: sharedName, ModTime: sharedMtime(d.v.shard)}, out), 0
	}
	lst, err := d.listing()
	if err != nil {
		d.v.logf("view: lookup failed: %v", err)
		return nil, errnoOf(err)
	}
	if e, ok := lst.find(name); ok {
		if e.IsDir {
			p := filepath.Join(d.path, e.Real)
			if c := d.GetChild(name); c != nil {
				if cd, ok := c.Operations().(*viewDir); ok && cd.path == p {
					out.Mode = 0555
					out.Nlink = 2
					out.SetEntryTimeout(dirTimeout)
					out.SetAttrTimeout(dirTimeout)
					return c, 0
				}
			}
			nd := &viewDir{v: d.v, path: p}
			out.Mode = 0555
			out.Nlink = 2
			out.SetEntryTimeout(dirTimeout)
			out.SetAttrTimeout(dirTimeout)
			return d.NewInode(ctx, nd, fs.StableAttr{Mode: fuse.S_IFDIR}), 0
		}
		return d.file(ctx, name, filepath.Join(d.path, e.Real), false, e, out), 0
	}
	return nil, syscall.ENOENT
}

// file returns the inode of a disk file, reusing the existing one when it
// still stands for the same real path.
func (d *viewDir) file(ctx context.Context, name, real string, shared bool, e viewEntry, out *fuse.EntryOut) *fs.Inode {
	var vf *viewFile
	var ino *fs.Inode
	if c := d.GetChild(name); c != nil {
		if cf, ok := c.Operations().(*viewFile); ok && cf.real == real {
			vf, ino = cf, c
		}
	}
	if vf == nil {
		vf = &viewFile{v: d.v, parent: d, name: name, real: real, shared: shared}
		ino = d.NewInode(ctx, vf, fs.StableAttr{Mode: fuse.S_IFREG})
	}
	size, mt := vf.listed(e)
	out.Mode = 0444
	out.Nlink = 1
	out.Size = uint64(size)
	out.SetTimes(nil, &mt, nil)
	out.SetEntryTimeout(0)
	out.SetAttrTimeout(0)
	return ino
}

func (d *viewDir) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (d *viewDir) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, syscall.EROFS
}
func (d *viewDir) Unlink(ctx context.Context, name string) syscall.Errno { return syscall.EROFS }
func (d *viewDir) Rmdir(ctx context.Context, name string) syscall.Errno  { return syscall.EROFS }
func (d *viewDir) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	return syscall.EROFS
}
func (d *viewDir) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (d *viewDir) Mknod(ctx context.Context, name string, mode uint32, dev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (d *viewDir) Link(ctx context.Context, target fs.InodeEmbedder, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}

// viewFile is a disk image file: a zip (as X.vhd) or share.vhd.
type viewFile struct {
	fs.Inode
	v      *viewShared
	parent *viewDir // the directory listing that shows this file
	name   string
	real   string // zip path, or the shared folder
	shared bool

	mu    sync.Mutex
	opens int   // open handles
	built int64 // size of the last volume built for an open (0 = none yet)
	mtime time.Time
}

var (
	_ = (fs.NodeGetattrer)((*viewFile)(nil))
	_ = (fs.NodeSetattrer)((*viewFile)(nil))
	_ = (fs.NodeOpener)((*viewFile)(nil))
	_ = (fs.NodeReader)((*viewFile)(nil))
	_ = (fs.NodeWriter)((*viewFile)(nil))
	_ = (fs.NodeReleaser)((*viewFile)(nil))
)

// viewHandle holds one lease; released exactly once.
type viewHandle struct {
	lease     *Lease
	vol       *Volume
	size      int64
	partStart int64
	once      sync.Once
}

// listed gives the size and mtime to report in a Lookup answer, from the
// listing entry when no handle is open.
func (f *viewFile) listed(e viewEntry) (int64, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opens > 0 || f.shared {
		mt := f.mtime
		if mt.IsZero() {
			mt = e.ModTime
		}
		return f.built, mt
	}
	return e.Size, e.ModTime
}

func (f *viewFile) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) (errno syscall.Errno) {
	defer guard("getattr", &errno)
	out.Mode = 0444
	out.Nlink = 1
	out.SetTimeout(0) // fstat after open must see the built size
	if h, ok := fh.(*viewHandle); ok && h != nil {
		f.mu.Lock()
		mt := f.mtime
		f.mu.Unlock()
		out.Size = uint64(h.size)
		out.SetTimes(nil, &mt, nil)
		return 0
	}
	if f.shared {
		f.mu.Lock()
		out.Size = uint64(f.built)
		mt := f.mtime
		f.mu.Unlock()
		if mt.IsZero() {
			mt = sharedMtime(f.real)
		}
		out.SetTimes(nil, &mt, nil)
		return 0
	}
	// A fresh listing already holds the zip's size and mtime: no stat of the
	// real file (a CIFS round trip) per attribute request.
	if f.parent != nil {
		if e, ok := f.parent.cachedEntry(f.name); ok && !e.IsDir && e.Real == filepath.Base(f.real) {
			out.Size = uint64(e.Size)
			out.SetTimes(nil, &e.ModTime, nil)
			return 0
		}
	}
	st, err := f.v.stat(f.real)
	if err != nil {
		return errnoOf(err)
	}
	mt := st.ModTime()
	out.Size = uint64(st.Size())
	out.SetTimes(nil, &mt, nil)
	return 0
}

func (f *viewFile) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}

func (f *viewFile) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	return 0, syscall.EROFS
}

// Open acquires a lease, building the volume if needed. It holds no lock while
// waiting; a kernel interrupt cancels ctx and abandons the wait.
func (f *viewFile) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fl uint32, errno syscall.Errno) {
	defer guard("open", &errno)
	if flags&(syscall.O_WRONLY|syscall.O_RDWR|syscall.O_APPEND|syscall.O_TRUNC) != 0 {
		return nil, 0, syscall.EROFS
	}
	l, err := f.v.m.AcquireCtx(ctx, SourceSpec{Path: f.real, Zip: !f.shared})
	if err != nil {
		switch {
		case errors.Is(err, ErrBusy):
			f.v.logf("view: open %s: busy (too many sources in use)", f.name)
			return nil, 0, syscall.EBUSY
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			f.v.logf("view: open %s: interrupted", f.name)
			return nil, 0, syscall.EINTR
		}
		f.v.logf("view: open %s failed: %v", f.name, err)
		return nil, 0, errnoOf(err)
	}
	vol := l.Volume()
	h := &viewHandle{lease: l, vol: vol, size: vol.Size(), partStart: vol.Geometry().PartStart * sectorSize}
	f.mu.Lock()
	f.opens++
	f.built = h.size
	if f.shared {
		f.mtime = time.Now()
	} else if st, err := f.v.stat(f.real); err == nil {
		f.mtime = st.ModTime()
	}
	f.mu.Unlock()
	return h, fuse.FOPEN_DIRECT_IO, 0
}

// Release gives the lease back, once, even if called after unmount began.
func (f *viewFile) Release(ctx context.Context, fh fs.FileHandle) syscall.Errno {
	if h, ok := fh.(*viewHandle); ok && h != nil {
		h.once.Do(func() {
			f.mu.Lock()
			f.opens--
			f.mu.Unlock()
			h.lease.Release()
		})
	}
	return 0
}

// Read serves dest straight from the leased volume; no lock is taken.
func (f *viewFile) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (res fuse.ReadResult, errno syscall.Errno) {
	start := time.Now()
	defer guard("read", &errno)
	h, ok := fh.(*viewHandle)
	if !ok || h == nil {
		return nil, syscall.EBADF
	}
	rl := f.v.opt.Log
	if off < 0 || off >= h.size {
		return fuse.ReadResultData(nil), 0
	}
	if rem := h.size - off; int64(len(dest)) > rem {
		dest = dest[:rem]
	}
	if _, err := h.vol.ReadAt(dest, off); err != nil {
		log.Printf("read error at %d (+%d): %v", off, len(dest), err)
		rl.add(time.Now().UnixNano(), off, len(dest), "error", time.Since(start))
		return nil, syscall.EIO
	}
	kind := "backing"
	if end := off + int64(len(dest)); off >= sectorSize && end <= h.partStart {
		kind = "synthetic"
	}
	rl.add(time.Now().UnixNano(), off, len(dest), kind, time.Since(start))
	return fuse.ReadResultData(dest), 0
}

// viewConfig carries the flags of view mode.
type viewConfig struct {
	zipRoot, shared, mount string
	label                  string
	cacheMB, maxEntries    int
	fdLimit                int
	logPath                string
	verbose                bool
	idle                   time.Duration
	maxVolumes             int
}

// runView mounts the view and serves it until unmounted. Nothing is scanned at
// start; volumes are built when files are opened.
func runView(sd *shutdown, c viewConfig) {
	if st, err := os.Stat(c.zipRoot); err != nil || !st.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		fatal("bad -view %s: %v", c.zipRoot, err)
	}
	if c.shared == "" {
		c.shared = filepath.Join(c.zipRoot, "share")
	}
	if resolvePath(c.zipRoot) == resolvePath(c.mount) {
		fatal("-mount must not be the -view folder itself")
	}
	if _, err := os.Stat(c.shared); err != nil {
		if err := os.MkdirAll(c.shared, 0o755); err != nil {
			fatal("cannot create -shared %s: %v", c.shared, err)
		}
		fmt.Fprintf(os.Stderr, "view: created share folder %s\n", c.shared)
	} else if st, _ := os.Stat(c.shared); !st.IsDir() {
		fatal("bad -shared %s: not a directory", c.shared)
	}

	if rm, rs := resolvePath(c.mount), resolvePath(c.shared); rm == rs || strings.HasPrefix(rm, rs+string(filepath.Separator)) {
		fatal("-mount %s lies inside the -shared folder %s; the shared disk would contain its own mount", c.mount, c.shared)
	}

	var rl *readLog
	var lf *os.File
	var mgr *Manager
	var lfMu sync.Mutex
	sd.cleanup = func() {
		rl.close()
		if mgr != nil {
			mgr.Close()
		}
		if lf != nil {
			lf.Close()
		}
	}
	if c.logPath != "" {
		var err error
		lf, err = os.OpenFile(c.logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			fatal("%v", err)
		}
		if c.verbose {
			rl = newReadLog(lf)
		}
	}
	logf := func(format string, a ...any) {
		log.Printf(format, a...)
		if lf == nil {
			return
		}
		line := time.Now().Format("15:04:05.000000") + " tsshare: " + fmt.Sprintf(format, a...) + "\n"
		if rl != nil {
			rl.line(line) // one writer on the file
			return
		}
		lfMu.Lock()
		lf.WriteString(line)
		lfMu.Unlock()
	}
	fmt.Fprintf(os.Stderr, "view: zip root %s\n", c.zipRoot)
	fmt.Fprintf(os.Stderr, "view: shared folder %s\n", c.shared)
	fmt.Fprintf(os.Stderr, "view: mount %s\n", c.mount)
	fmt.Fprintf(os.Stderr, "view: up to %d volumes alive\n", c.maxVolumes)
	sd.abortIfCancelled()

	mgr = NewManager(ManagerOptions{
		MaxVolumes: c.maxVolumes,
		IdleTTL:    c.idle,
		Source:     SourceOptions{CacheMB: c.cacheMB, FDLimit: c.fdLimit},
		Volume:     VolumeOptions{Label: c.label, MaxEntries: c.maxEntries},
		MaxEntries: c.maxEntries,
		Logf:       logf,
	})
	root := newViewRoot(ViewOptions{
		ZipRoot: c.zipRoot, Shared: c.shared, Mount: c.mount,
		ListTTL: defaultListTTL, Logf: logf, Log: rl,
	}, mgr)
	srv, err := mountFS(c.mount, root)
	if err != nil {
		log.Printf("mount: %v", err)
		sd.cleanup()
		os.Exit(1)
	}
	sd.setServer(srv)
	fmt.Fprintf(os.Stderr, "serving view at %s (volumes are built when a file is opened)\n", c.mount)
	srv.Wait()
	sd.cleanup()
	log.Printf("exiting")
}
